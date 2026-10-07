# Substrate — implementation

This repository implements the architecture in [`substrate-plan.md`](../substrate-plan.md).
Status: **Phases 0–2 of the build sequence are implemented and tested**; the
Firecracker data path is validated against a fake VMM (this host has no KVM)
and the full stack runs end-to-end on the process tier.

## Layout

| Path | What it is |
|---|---|
| `cmd/noded` | Node agent: VMM supervision, reconciler, REST API, embedded gateway. Modes: `standalone` (SQLite) and `node` (dials out to a cell). |
| `cmd/cell` | Control-plane cell: Postgres registry, scheduler, node enrollment, placement-routing API. |
| `cmd/subctl` | CLI. |
| `cmd/shim` | Guest agent for the `process` and `kernel` tiers (Go twin of `guestd`, same wire protocol). Listens on a unix socket for `process`; **dials out** for `kernel`, where a socket bound inside the sandbox is invisible to the host. Must be built `CGO_ENABLED=0` — it runs inside a tenant rootfs. |
| `cmd/templates-build` | Rootfs → content-addressed chunk store packager. |
| `guestd/` | Guest agent in Rust (~530KB static): exec (pty+pipe), fs, port announce, RESEED uniqueness protocol, metrics. vsock-only. |
| `internal/proto` | SGP/1 framed multiplexed protocol shared by noded/guestd/shim (`docs/protocol.md`). |
| `internal/vmm` | `VMDriver` interface; Firecracker driver (jailer, snapshots, UFFD hook, VMGenID uniqueness) + process driver + fake-VMM test double. |
| `internal/vmm/gvisor` | `kernel`-tier driver: hand-written OCI bundles over `runsc`, shared read-only templates with size-capped per-sandbox overlays, authenticated dial-out agent. The tier for hosts with no `/dev/kvm`. |
| `internal/netns` | Per-sandbox netns/tap//32 networking, nftables policy with the IMDS drop as rule #1. |
| `internal/snapstore` | 512KB content-addressed chunks, zero-page elision, hotness index, LRU cache, object-storage demotion tier. |
| `internal/reconcile` | Level-triggered supervisor: desired-vs-actual convergence, idle pause, TTLs, pressure-pause OOM policy. |
| `internal/api` | v1 REST surface: auth (3 credential grades), mandatory idempotency, SSE exec streams, fs, ports, tokens, events, usage. |
| `internal/gateway` | Preview URLs `{port}-{sandbox}.domain`, default-private with bearer auth, 30s route cache. |
| `internal/cell` | Node registry, bin-packing scheduler (snapshot-cache affinity), BYO org isolation, placement proxy. |
| `sdk/python`, `sdk/typescript`, `sdk/go` | Client SDKs (tested against mock servers). |
| `mcp/` | Stateless MCP server (2026-07-28 spec): 10 tools, deterministic listing, confirm-guarded destroy. |
| `bench/phase0` | Density/restore spike harness — KVM-gated, refuses to produce fake numbers. |

## Isolation tiers

`driver.name` decides what a sandbox is. Full key reference in
[`CONFIGURATION.md`](CONFIGURATION.md); how to tell whether a host can do
hardware virt at all is in [`KVM-AVAILABILITY.md`](KVM-AVAILABILITY.md).

| tier | `driver.name` | boundary | needs | pause/snapshot |
|---|---|---|---|---|
| `hv` | `firecracker` | hardware virtualisation | `/dev/kvm` | yes |
| `kernel` | `gvisor` | userspace kernel (sentry) | `runsc` | not yet |
| `process` | `process` | **none — dev only** | nothing | pause only |

### The `kernel` tier

Written because the honest answer to "can users create sandboxes on a cheap
KVM-less server?" was *no*, and that answer was a missing driver rather than a
missing capability. On such a host the only alternative was `process`, which is
not a boundary at all.

Design points worth knowing before operating it:

- **The rootfs is not a per-sandbox writable directory.** One node-owned
  template tree is shared read-only by every sandbox using it, with a private
  `--overlay2` layer on top capped by `driver.gvisor_overlay_size_mib`. Guest
  writes never reach the template, and `df` inside reports the cap rather than
  the node's volume. Prepared templates live in `{data_dir}/gvisor-templates`
  and are expected to be immutable trees.
- **The agent is bind-mounted read-only**, not copied in, so guest code cannot
  replace or relink it.
- **The host listens and the agent dials out**, because a unix socket bound
  inside a gVisor sandbox lives in the sentry's VFS and the gofer does not
  propagate it. The agent authenticates with a token it reads and unlinks before
  any guest code can exist — see [`protocol.md`](protocol.md).
- **The agent reconnects.** The sandbox and its filesystem survive a transport
  error; execs do not (v1 contract: execs die with connections).
- No pause/snapshot yet. `runsc checkpoint/restore` exists and is unused;
  `Caps().Pause` reports `false` so the reconciler never attempts it.

Measured on this host (`sudo make gvisor-test`), 15 creates:

```
create p50=191.6ms  p95=205.6ms   (plan target: p50 ≤ 300ms, p99 ≤ 1s)
  stage_rootfs      2.1ms
  vmm_spawn       153.9ms   <- 80% of create
  agent_connect     8.0ms
  guest_handshake   1.9ms
```

## Quickstart (no KVM needed)

```sh
make build guestd

# terminal 1
SUBSTRATE_INSECURE_DEV=1 SUBSTRATE_API_KEY=sk_dev noded --config - <<'EOF'
mode: standalone
node: {data_dir: /tmp/substrate}
api: {listen: :8080}
gateway: {enabled: true, listen: :8081, base_domain: run.localhost}
driver: {name: process, allow_process: true}
EOF

# terminal 2
export SUBSTRATE_URL=http://localhost:8080 SUBSTRATE_KEY=sk_dev
subctl templates create --name demo
SB=$(subctl sandboxes create --template demo | jq -r .id)
subctl exec $SB -- sh -c 'echo hello from $HOSTNAME'
```

On a KVM host, switch the driver to `firecracker` for real microVMs.

## Tests

```sh
go test ./internal/...              # unit + integration (process tier)
go test ./internal/api -tags e2e    # full-stack E2E via real binary
cargo test --manifest-path guestd/Cargo.toml
(cd sdk/python && python3 test_sdk.py)
(cd sdk/typescript && npm test)
(cd sdk/go && go test ./...)
(cd mcp && node test/test.mjs)
SUBSTRATE_TEST_DB_URL=postgres://... go test ./internal/cell/inttest -tags cell
sudo make gvisor-test               # kernel tier, real runsc sandboxes (root)
sudo make live-netns                # netns/nftables against the real kernel
make pg-test                        # cell integration against Postgres
```

`make gvisor-test` is the one that exercises real isolation: it runs 20 live
tests against actual gVisor sandboxes, including attack attempts (guest tries to
overwrite its own agent, exceed its write cap, read another tenant's files,
impersonate the agent during the reconnect window) rather than inspecting code.

## Deviations from the plan (deliberate)

- **gRPC → SGP/1 over mTLS**: the plan's internal control-plane channel uses a
  custom framed multiplexed protocol instead of gRPC to avoid protobuf
  toolchain dependencies; semantics (dial-out, multiplexed streams) match §5.3.
- **Cell data plane**: gateways/nodes exchange data directly; the cell proxies
  only control operations (§5.6 degradation contract preserved by design).
- **UFFD restore** is implemented behind config (`snapshots` + driver UFFD
  socket) but cannot be exercised without KVM hardware; file-backed snapshots
  are the validated default.
