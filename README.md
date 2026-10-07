# Substrate — multi-tenant micro-VM sandbox platform

Architecture and **working implementation** of a platform that lets customers
programmatically create, run, pause, and destroy isolated compute sandboxes
(512MB–1GB) for their end users. Open-core + hosted service.

> [!WARNING]
> **This project is in active development and is not production-ready.**
> APIs, configuration, on-disk formats, and the wire protocol may change
> without notice, and the security hardening is still in progress. Do not run
> it with untrusted workloads or real customer data yet. Use at your own risk.

> "Substrate" is a working codename only.

## Status

The plan's Phase 0–2 build sequence is implemented and tested:

- **Isolation**: Firecracker driver behind a `VMDriver` interface (jailer,
  GA snapshots, VMGenID uniqueness protocol on every restore), plus an
  explicitly-labeled `process` tier so the whole stack runs on KVM-less boxes.
  gVisor `kernel` tier implemented (`driver.name: gvisor`) for hosts without
  KVM — real isolation via a userspace kernel, sub-second starts, no pause yet.
- **Cold start**: restore path implemented end-to-end (snapshot load → vsock
  handshake → RESEED → ports announce); latency targets (p50 ≤300ms) are
  enforced by `bench/phase0` on KVM hardware.
- **Density machinery**: 512KB content-addressed chunk store with zero-page
  elision + hotness index + LRU cache; measured-resident packing inputs.
- **Networking**: per-sandbox netns/tap with routed /32s from RFC6598 blocks;
  nftables with the IMDS drop as rule #1; per-tap egress chains.
- **Control plane**: Postgres cells, node enrollment + dial-out heartbeats,
  bin-packing scheduler with snapshot-cache affinity and BYO org isolation.
- **API**: full v1 REST surface with mandatory idempotency keys, SSE exec
  streams, sandbox-scoped JWTs for end-user agents, events ledger, usage API.
- **Ecosystem**: Python/TypeScript/Go SDKs, `subctl` CLI, stateless MCP server
  conforming to the 2026-07-28 spec.

## Contents

| File | What it is |
|---|---|
| [`substrate-plan.md`](substrate-plan.md) | The architecture plan (~13k words): decisions, prior art, isolation, density/cold-start, networking, control plane, agent API, security, open-core split, build sequence, risk register, bibliography. |
| [`docs/IMPLEMENTATION.md`](docs/IMPLEMENTATION.md) | What is built, repo layout, quickstart, test matrix, deliberate deviations. |
| [`docs/OPERATING.md`](docs/OPERATING.md) | Running it as a service: tenant onboarding, abuse response, backups, billing, and what is still your decision. |
| [`docs/ACCEPTABLE-USE.md`](docs/ACCEPTABLE-USE.md) | AUP draft — a starting point for legal review, not legal advice. |
| [`docs/CONFIGURATION.md`](docs/CONFIGURATION.md) | Every config key, its default, what it does — plus which keys are declared but inert, and the env-var overrides. |
| [`docs/OBSERVABILITY.md`](docs/OBSERVABILITY.md) | Every metric exposed, what it answers, what to alert on, and the one place the persisted ledger is knowingly weaker than Prometheus. |
| [`docs/protocol.md`](docs/protocol.md) | SGP/1 — the host↔agent wire protocol, both transports, and agent authentication. |
| [`docs/KVM-AVAILABILITY.md`](docs/KVM-AVAILABILITY.md) | How to tell whether a host can do hardware virt, and what to run when it cannot. |
| [`docs/openapi.yaml`](docs/openapi.yaml) | OpenAPI 3.1 spec of the public v1 surface. |
| [`research/`](research/) | Six research passes behind the plan (Firecracker, runtimes, economics, competitors, snapshots/side-channels, agent APIs). |

## Headline positions

- **Isolation:** Firecracker primary behind a `VMDriver` interface; tier never silently downgraded. gVisor `kernel` tier for hosts without `/dev/kvm` — shared read-only template, size-capped per-sandbox overlay, authenticated agent.
- **Cold start:** p50 ≤300ms / p99 ≤1s (create-from-template and local resume); object-storage resume p50 ≤3s — measured by `bench/phase0`, not vibes.
- **Density:** 2,000 resident sandboxes per 1TB bare-metal host (~$0.47/resident-sandbox-month); paused = 0 RAM.
- **Security:** IMDS structurally unreachable (4 layers); no KSM ever; core-scheduling cookies; snapshot hygiene as a security control (§7.4).
- **Open-core:** Apache-2.0 engine (`noded --standalone`, `guestd`, SDKs, MCP server); hosted = multi-cell control plane, gateway fleet, build farm.

## Quickstart

See [`docs/IMPLEMENTATION.md`](docs/IMPLEMENTATION.md). Short version:

```sh
make build guestd
SUBSTRATE_INSECURE_DEV=1 SUBSTRATE_API_KEY=sk_dev noded   # standalone mode
subctl sandboxes create --template demo
```
