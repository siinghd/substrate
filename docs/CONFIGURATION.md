# Configuration reference

Every key `substrate` reads, its default from `config.Defaults()`, and what it
actually does. YAML file passed with `--config`; a few secrets come from the
environment instead and are marked as such.

Keys are grouped exactly as they nest in the file:

```yaml
mode: standalone
node: { ... }
api: { ... }
gateway: { ... }
driver: { ... }
network: { ... }
snapshots: { ... }
cell: { ... }
limits: { ... }
log: { ... }
```

---

## `mode`

| key | default | meaning |
|---|---|---|
| `mode` | `standalone` | `standalone` (API + node in one process), `node` (agent only, joins a cell), `cell` (control plane only) |

## `node`

| key | default | meaning |
|---|---|---|
| `node.id` | `nd_{hostname}` | Stable node id, derived from the hostname when empty. |
| `node.name` | *hostname* | Display name. |
| `node.region` | `local` | Placement label the cell scheduler reads. |
| `node.data_dir` | `/var/lib/substrate` | Everything durable: sqlite db, artifacts, gVisor bundles and templates. |
| `node.jail_dir` | `/srv/jail` | Firecracker jailer chroot root. `hv` tier only. |
| `node.cache_dir` | `/var/cache/substrate` | Snapshot chunk cache. |
| `node.advertise_addr` | *empty* | Address the cell reaches this node on. Reported at enrolment. |
| `node.mem_total_mib` | `0` | RAM this node advertises to the cell scheduler, which subtracts live sandboxes from it. Reported verbatim at enrolment — it is **not** probed, so leaving it `0` advertises a node with no capacity. |

## `api`

| key | default | meaning |
|---|---|---|
| `api.listen` | `:8080` | Control-plane HTTP listener. |
| `api.public_base_url` | *empty* | Absolute URL used when building links back to the API. |
| `SUBSTRATE_JWT_SECRET` | **required** | **Environment, not YAML.** Signing key for API tokens. |
| `SUBSTRATE_ADMIN_KEY` | *empty* | **Environment, not YAML.** Gates `/v1/admin/*` (creating orgs, projects, keys; suspending orgs). **Empty disables the admin API entirely** — fail closed. Never stored in the database, so a database compromise cannot mint tenants. |

## `gateway`

Public ingress that routes `{port}-{sandbox}.{base_domain}` to a sandbox port.

| key | default | meaning |
|---|---|---|
| `gateway.enabled` | `true` | |
| `gateway.listen` | `:8081` | |
| `gateway.base_domain` | `run.localhost` | e.g. `run.example.dev`. Hostnames are `{port}-{sandbox_id}.{base_domain}`. |
| `gateway.allow_process_tier_previews` | `false` | **Leave false in production.** The `process` tier is not an isolation boundary; serving public previews from it exposes the host. |
| `gateway.tls_cert` / `gateway.tls_key` | *empty* | Serve TLS directly instead of behind a terminator. |

## `driver`

Which isolation tier this node runs. See *Isolation tiers* below for how to choose.

| key | default | meaning |
|---|---|---|
| `driver.name` | `process` | `firecracker` \| `gvisor` \| `process` |
| `driver.allow_process` | `true` | Guard rail: set `false` in production so a misconfiguration cannot silently fall back to the no-boundary tier. |
| `driver.firecracker_bin` | `/usr/local/bin/firecracker` | `hv` tier. |
| `driver.jailer_bin` | `/usr/local/bin/jailer` | `hv` tier. |
| `driver.kernel_image` | `/var/lib/substrate/artifacts/vmlinux` | Guest kernel, `hv` tier. |
| `driver.runsc_bin` | `runsc` | gVisor runtime, resolved on `PATH` by default. |
| `driver.gvisor_platform` | `systrap` | gVisor platform. `systrap` needs no `/dev/kvm` and is the reason this tier works on nested VPS hosts. |
| `driver.gvisor_sock_dir` | `/run/substrate-gvisor/sock` | Per-sandbox agent sockets. Keep it **short**: a unix socket path is capped near 108 bytes, and a long path fails with a bare `bind: invalid argument` that names nothing useful. |
| `driver.gvisor_overlay_size_mib` | `2048` | **How much one sandbox may write.** Guest writes go to a private overlay, never to the shared template. Without a cap one guest can fill the node's data volume for every other tenant on it, and `df` inside the sandbox reports the host's real free space. |

## `network`

| key | default | meaning |
|---|---|---|
| `network.node_block_cidr` | `100.64.0.0/22` | RFC6598 block this node hands out to sandboxes. |
| `network.mtu` | `1500` | |
| `network.enabled` | `true` | Per-sandbox namespaces, NAT and egress policy. Off means sandboxes have **no network**. Ignored on the `process` tier, which shares the host stack. |
| `network.uplink_interface` | *empty* | Host NIC egress is masqueraded out of. Empty = the default route's interface. |
| `network.egress_policy` | `open` | `open` \| `allowlist`. **Not wired** — see *Declared but inert* below. |
| `network.bandwidth_mbit` | `100` | Per-sandbox rate limit. |

## `snapshots`

| key | default | meaning |
|---|---|---|
| `snapshots.chunk_size_kb` | `512` | Content-addressed chunk size. |
| `snapshots.cache_size_gb` | `50` | Local chunk cache ceiling. |
| `snapshots.object_store_url` | *empty* | Remote chunk store; local-only when empty. |

## `cell`

Control-plane process only (`mode: cell`).

| key | default | meaning |
|---|---|---|
| `cell.database_url` | *empty* | `postgres://…`. SQLite is used when empty. |
| `cell.listen_http` | | Control-plane API listener. |
| `cell.listen_nodes` | | mTLS tunnel listener nodes connect back on. |
| `cell.tls_cert` / `cell.tls_key` | | Node-facing TLS material. |
| `SUBSTRATE_ENROLL_TOKEN` | | **Environment, not YAML.** Shared secret a node presents when joining. |

## `limits`

These are the ceilings a tenant request is checked against. **A limit of `0`
means unlimited** — every one of these is enforced only when positive.

| key | default | meaning |
|---|---|---|
| `limits.max_sandboxes_per_project` | `1000` | |
| `limits.max_sandboxes_per_user` | `20` | Per `external_user_id`, i.e. per *your* end user. |
| `limits.max_mem_mib` | `16384` | Ceiling on `mem_mib`. Without it one request can declare more RAM than the host has **and** poison the node's advertised capacity so the scheduler places nothing else there. |
| `limits.max_vcpus` | `16` | Ceiling on `vcpus`. |
| `limits.max_ttl_absolute_secs` | `2592000` (30d) | Ceiling on **both** `ttl_absolute_secs` and `ttl_idle_secs`. A sandbox may not pin resources indefinitely, and `time.Duration` is int64 nanoseconds — values above ~9.2e9 seconds wrap negative and invert the comparison. |
| `limits.default_idle_pause_secs` | `86400` | Applied when a request sends `ttl_idle_secs: 0`, so a tenant cannot disable reclaim by omitting it. |
| `limits.retention_days` | `30` | Intended retention for usage rows. **Not wired** — see *Declared but inert* below. |
| `limits.memory_overcommit_ratio` | `3` | Intended placement overcommit factor. **Not wired** — see *Declared but inert* below. |

## `backup`

| key | default | meaning |
|---|---|---|
| `backup.enabled` | `true` | Scheduled, **verified** store snapshots. |
| `backup.dir` | `/var/backups/substrate` | Put it on a **different volume** from `node.data_dir`: a backup on the disk that failed is not a backup. |
| `backup.interval_mins` | `60` | |
| `backup.keep` | `48` | Retention count. Unbounded backups fill the disk the node also runs sandboxes on. |

## `billing`

Prices are **nano-cents** per unit (1e-9 of a cent), so a per-second price does
not round to zero. All default to 0: an unpriced deployment invoices zero
rather than guessing.

| key | default | meaning |
|---|---|---|
| `billing.plan_id` / `billing.currency` | `default` / `usd` | Node-wide plan; per-org plans need a schema migration. |
| `billing.sandbox_second_nano_cents` | `0` | Residency: the sandbox existed and held resources. |
| `billing.cpu_second_nano_cents` | `0` | CPU actually consumed. |
| `billing.gib_hour_nano_cents` | `0` | Peak GiB held over the running duration. |
| `billing.egress_gb_nano_cents` | `0` | **Outbound only.** Billing inbound bytes would let anyone on the internet run up a tenant's bill. |
| `billing.included_cents` | `0` | Free-tier credit; cannot make a total negative. |
| `billing.minimum_cents` | `0` | Floors a **non-zero** invoice only; never charges an idle org. |

## `log`

| key | default | meaning |
|---|---|---|
| `log.level` | `info` | `debug` \| `info` \| `warn` \| `error` |
| `log.json` | `false` | Structured output. |

---

## Declared but inert

These keys parse, take a default, and are read by **nothing**. They are listed
here rather than quietly documented as working, because a setting that looks
like a control and is not is worse than a missing one — you would set
`egress_policy: allowlist`, see no error, and believe egress was restricted.

| key | status |
|---|---|
| `network.egress_policy` | Declared only. Egress filtering is implemented in `internal/netns` (per-sandbox nftables chains, RFC1918 drops, peer isolation, rate limit) but that package is **not yet wired into `noded`**, and nothing reads this key to choose a policy. |
| `network.bandwidth_mbit` | Same: the rate limit exists in the nftables rules, but the value comes from the rules, not from this key. |
| `limits.retention_days` | Declared only. Nothing prunes usage rows on a schedule. |
| `limits.memory_overcommit_ratio` | Declared only. Placement does not consult it. |

## Environment overrides

Applied **after** the YAML file, so they win. Only these keys have one; anything
else must come from the file.

| variable | overrides |
|---|---|
| `SUBSTRATE_MODE` | `mode` |
| `SUBSTRATE_NODE_ID` | `node.id` |
| `SUBSTRATE_ADVERTISE_ADDR` | `node.advertise_addr` |
| `SUBSTRATE_REGION` | `node.region` |
| `SUBSTRATE_DATA_DIR` | `node.data_dir` |
| `SUBSTRATE_LISTEN` | `api.listen` |
| `SUBSTRATE_PUBLIC_BASE_URL` | `api.public_base_url` |
| `SUBSTRATE_JWT_SECRET` | `api.jwt_secret` — **no YAML equivalent** |
| `SUBSTRATE_DRIVER` | `driver.name` |
| `SUBSTRATE_FIRECRACKER_BIN` | `driver.firecracker_bin` |
| `SUBSTRATE_JAILER_BIN` | `driver.jailer_bin` |
| `SUBSTRATE_KERNEL_IMAGE` | `driver.kernel_image` |
| `SUBSTRATE_NODE_BLOCK` | `network.node_block_cidr` |
| `SUBSTRATE_GATEWAY_LISTEN` | `gateway.listen` |
| `SUBSTRATE_GATEWAY_DOMAIN` | `gateway.base_domain` |
| `SUBSTRATE_DB_URL` | `cell.database_url` |
| `SUBSTRATE_CELL_HTTP` | `cell.listen_http` |
| `SUBSTRATE_CELL_NODES` | `cell.listen_nodes` |
| `SUBSTRATE_ENROLL_TOKEN` | `cell.enroll_token` — **no YAML equivalent** |
| `SUBSTRATE_IDLE_PAUSE_SECS` | `limits.default_idle_pause_secs` |
| `SUBSTRATE_MAX_SB_PER_PROJECT` | `limits.max_sandboxes_per_project` |
| `SUBSTRATE_CACHE_GB` | `snapshots.cache_size_gb` |

Note there is **no** env override for `driver.runsc_bin`, the gVisor keys, or
`limits.max_mem_mib` / `max_vcpus` / `max_ttl_absolute_secs`.

---

## Isolation tiers

`driver.name` decides what a sandbox actually is. This is the most consequential
setting in the file.

| tier | `driver.name` | boundary | needs | pause/snapshot |
|---|---|---|---|---|
| `hv` | `firecracker` | hardware virtualisation | `/dev/kvm` | yes |
| `kernel` | `gvisor` | userspace kernel (sentry) | nothing special | not yet |
| `process` | `process` | **none** | nothing | pause only |

**`process` is not an isolation boundary.** It exists so the stack runs on
KVM-less dev boxes and CI. Guest code runs as the node agent's user on the host;
on a machine with passwordless sudo that is host root in one command. Set
`driver.allow_process: false` and `gateway.allow_process_tier_previews: false`
anywhere real.

**`kernel` (gVisor) is the tier for hosts without `/dev/kvm`** — nested VPS
instances, most shared cloud hardware. See `KVM-AVAILABILITY.md` for how to tell
whether a given host can do hardware virt at all.

### What the `kernel` tier gives a sandbox

- Its own kernel: the guest sees `4.19.0-gvisor`, not the host's.
- Empty capability sets across all five, `noNewPrivileges`, no privileged
  devices, and exactly **one** reachable host path — the sandbox's own agent
  socket directory.
- A process ceiling (`RLIMIT_NPROC` + `pids.limit`, 512) so a fork bomb inside
  cannot exhaust host memory through sentry tasks.
- A **shared, read-only template** with a private, size-capped overlay per
  sandbox. The template is copied once per node, not once per sandbox, and the
  guest agent is bind-mounted read-only rather than copied into a directory the
  guest can write.
- Writes bounded by `driver.gvisor_overlay_size_mib`; `df` inside reports that
  cap, not the node's volume.

### Preparing a node for the `kernel` tier

```yaml
driver:
  name: gvisor
  runsc_bin: /usr/local/bin/runsc
  gvisor_sock_dir: /run/substrate-gvisor/sock   # keep it short
  gvisor_overlay_size_mib: 2048
```

The guest agent must be **statically linked** — it runs inside a
tenant-supplied rootfs, and a dynamically linked binary needs a loader and libc
that a musl/distroless/scratch image does not have. `make build` and `make bin`
set `CGO_ENABLED=0`; the driver also refuses to stage a binary carrying a
`PT_INTERP` header rather than letting it fail 30 seconds later as a spurious
"guest agent never connected".

Prepared templates live under `{node.data_dir}/gvisor-templates`, keyed by
template path and mtime. Templates are expected to be **immutable trees**;
editing one in place without touching its top directory keeps the old key.
