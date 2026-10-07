# Substrate: Architecture and Implementation Plan

*A multi-tenant micro-VM sandbox platform — architecture and build plan. Drafted 2026-08-13. All sourced claims carry a bracketed citation with retrieval date; unverifiable claims are marked [UNVERIFIED] or [ESTIMATE] with arithmetic shown. ("Substrate" is a working codename only — it collides with Polkadot's framework and Google's new "Agent Substrate"; rename before anything public.)*

---

## Decisions up front

The six [DECIDE] items, answered so the rest of the plan can be concrete. Each: the pick, why, and the condition under which we revisit.

1. **Persistence: persistent by default, with scale-to-zero doing the economic work.** A sandbox keeps its filesystem and (best-effort at the SLA level, durable once demoted to object storage) its memory across idle periods; destroy is explicit or TTL'd. Why: "one sandbox per end user" is the product; per-user state is the reason a customer picks us over stuffing code into a stateless executor. Ephemeral per-task sandboxes fall out for free (`ttl_absolute: 1h`, no pause tier). *Revisit if* >80% of real usage is ephemeral single-exec — then the pause pipeline is over-built and a warm-pool-only mode should lead.
2. **Concurrency profile: mostly-idle, long-lived.** Design center: 100k defined sandboxes, ~10% resident, ~2–3% actively computing at any instant. Bursty short-lived load is served by the same machinery (template warm pools). *Revisit if* measured active ratio exceeds ~25% sustained — that flips the fleet toward CPU-heavy SKUs (§3.7 arithmetic inverts).
3. **Images: our templates + customer-supplied OCI images through our build pipeline, both in v1.** Customers hand us a Dockerfile/OCI image; the build farm flattens → chunks → boots → warms → snapshots (§3.5). Why v1 and not later: every design partner's first question is "can it run *our* stack"; templates-only kills adoption. What stays out: customer-supplied raw VM images/kernels (they'd break the snapshot/uniqueness pipeline and the security story). *Revisit* never, honestly — this one is structural.
4. **Public ingress: v1.** Preview URLs are table stakes across E2B/Cloudflare/Codespaces/Daytona (§4.4); an agent that builds a web app needs to show it to its human. Scope-limited: HTTP(S) only via the gateway, default-private with bearer tokens; raw TCP later. *Revisit* nothing; ship it.
5. **Our fleet: rented bare metal first (Latitude/Hetzner class), cloud metal + nested-capable instances for BYOC and compliance-pinned regions.** The verified economics (§2.4, §3.7): $0.92–2.80/GB-RAM/month bare metal vs $4.17–9.20 AWS metal, and a ~78× egress gap at 20TB/mo. AWS's Feb 2026 nested-virt-on-virtual change makes *BYOC in the customer's cloud account* cheap to support but doesn't change our own fleet's math. *Revisit if* bare-metal providers reprice toward cloud (Hetzner already 3×'d entry pricing in June 2026 — tracked as risk 7) or if enterprise demand concentrates in AWS regions.
6. **Languages: Go for `noded` and the control plane; Rust for `guestd`; Firecracker driven over its API socket.** Go: the entire orchestration ecosystem (and every candidate hire) speaks it; CSP-style concurrency fits a reconciler; static binaries deploy to BYO boxes. Rust for the guest agent: no GC in a 512MB guest, no runtime to exploit, ~2MB static binary. We do *not* embed rust-vmm in v1 — Firecracker-as-process via its API socket is the supported, jailed configuration. *Revisit* the rust-vmm question only if per-process overhead or spawn latency becomes the measured bottleneck (ZeroBoot's numbers hint at what a custom loader buys; §3.1 explains why not yet).

## Where the framing is wrong — three pushbacks

**1. "A sandbox per end user" conflates identity with residency.** The right product promise is: *the user's environment is always there*; the right implementation is: *a VM almost never is*. Persistent identity (volume + pause image + metadata) with ephemeral compute (restored on demand in <300ms) is what makes 4,000-users-per-customer affordable — a literal always-on tiny VPS per user would cost ~20× more (resident vs 10% resident). The plan builds the second thing while selling the first. Where this bites: workloads that hold long-lived external connections (DB tunnels, websockets) *feel* the pauses (§3.6). That's a real product seam — surfaced via `keep_warm: true` as a paid per-sandbox flag, not hidden.

**2. Per-session ephemeral is most of today's *demand*; per-user persistent is the *differentiator*. Build the second, serve the first.** The agent-sandbox market (E2B et al.) is overwhelmingly ephemeral today, and if we design only for persistent-idle we over-invest in pause plumbing before anyone pays for it. The resolution in this plan: the pause/restore machinery *is also* the fast-create machinery (both are snapshot-restore paths), so the investment serves both shapes. But the phasing (§10) proves ephemeral + fast-create first (Phases 0–1) and layers durable pause on top (Phase 2) — if the persistent bet is wrong, we've still built the right core.

**3. BYO-server should be v1-scoped but constrained to "capacity", and it must require KVM.** Keep it — it's the wedge nobody serves (§1) and it forces the dial-out agent + standalone mode that make the OSS story real. But: BYO nodes run only their owner's org (never mixed tenancy — we can't attest their hardware); they need /dev/kvm for tier-`hv` (a random VPS gets tier-`kernel` gVisor with an explicit downgrade warning); and they get no latency/durability SLA. What I'd *cut* from v1 instead: multi-region active-active anything, GPU, raw-TCP ingress, and Windows — none of them are on the critical path of the bet.

One non-pushback, confirmed after research: 512MB–1GB granularity is right. It matches E2B's floor (512MB) and Cloudflare's lite tier (256MB), and with resident-based packing the *configured* size is nearly free to be generous with.

## 0. Research grounding — method note

This plan was researched on **2026-08-13** via six parallel literature/web sweeps: (1) Firecracker's current capabilities and changelog, (2) alternative runtimes, (3) nested-virt availability and hardware economics on AWS/Azure/GCP/bare-metal, (4) the competitive landscape, (5) academic snapshot/restore/side-channel work through mid-2026, (6) agent-API design and sandbox network-security incidents. Rules applied throughout: primary sources preferred (project changelogs, vendor engineering blogs, USENIX/arXiv papers); every number carries its measurement conditions and source date; marketing claims are labeled as such; anything we could not trace to a primary source is marked **[UNVERIFIED]**, and anything we computed ourselves is marked **[ESTIMATE]** with arithmetic. Known gaps are flagged inline rather than papered over — the three largest: no primary per-VM memory-overhead figure exists for Cloud Hypervisor or gVisor (the circulating numbers trace to SEO content farms); AWS has no public statement of Lambda's exact SMT co-tenancy policy; and eBPF-vs-nftables scaling claims in 2026 tutorials conflate nftables with legacy iptables. A dated bibliography grouped by section closes the document.

## 1. Prior art

All claims below were verified against vendor primary sources on 2026-08-13 unless flagged. The single most useful meta-finding: **almost no published cold-start number in this market comes with test conditions**, and several famous ones don't appear on any primary source at all.

| Provider | Isolation primitive | Cold start / resume (published) | Persistence & fork | Self-host / BYO |
|---|---|---|---|---|
| E2B | Firecracker (+ Nomad/Consul) | resume ≈1s; pause ≈4s/GiB [docs]. The cited "~150–200ms" cold start: **[UNVERIFIED — third-party listicles only]** | pause/resume with memory+processes, kept indefinitely | Apache-2.0 infra repo, but self-host requires GCP/AWS + Terraform + a Cloudflare domain; **no bare-metal path** |
| Modal | **gVisor (runsc)** | containers ~1s; memory-snapshot restore: PyTorch import 5s→1.05s p50 (2025-01) | FS snapshots GA; **memory snapshots alpha**, same-instance-type restore only | none |
| Fly Machines | Firecracker | start stopped machine "well under a second"; **suspend-resume "a few hundred ms"**, suspend limited to ≤2GB RAM | stopped/suspended machines bill **storage only** (1GB rootfs·30d = $0.15); **resume explicitly not guaranteed** | none |
| Cloudflare | Workers: V8 isolates; Containers: "own VM," **VMM undisclosed** | Containers cold start "1–3s range" | ephemeral disk; snapshots "coming soon" | none |
| AWS Lambda SnapStart | Firecracker snapshots | "as low as sub-second"; AWS itself says use provisioned concurrency for double-digit-ms | per-function snapshots; runtime-restricted (Java/Python 3.12+/.NET 8+) | n/a |
| AWS Fargate | **contested in AWS's own sources**: NSDI'20 says Firecracker deployed in Lambda *and* Fargate; AWS's "Under the hood: Fargate data plane" (2020-04) describes containerd/runC with Firecracker as an optional runtime plugin — both cited, unresolved | n/a | n/a | n/a |
| Vercel Sandbox | Firecracker (explicit) | "milliseconds" — **marketing, no benchmark** | persistent by default, auto-save on stop; single region (iad1) | none |
| Together AI / CodeSandbox | Firecracker | **start-from-snapshot 500ms; cold 2.7s p95; resume 511ms p95; live-clone ~3s** (2025-05, the best-documented numbers in the field) | memory snapshot + hibernate + clone-from-live | none |
| Morph (Infinibranch) | "MorphVM," undisclosed | "<250ms," "instantaneous" branching — **no methodology** | branch-a-running-computation is the differentiator | none |
| Daytona | "dedicated kernel," VMM unnamed | "under 90ms" — vendor claim, no methodology | stateful snapshots | BYOC runners dial out to **their** control plane (Enterprise-gated); **OSS repo abandoned June 2026**, license unresolved |
| Northflank | Kata+CH primary; Firecracker; gVisor fallback | vendor benchmark: 100k sandboxes in 24s, p99 alloc 566ms **[vendor-cited]** | ephemeral + persistent modes | BYOC incl. on-prem/bare metal (commercial) |
| Koyeb | Firecracker | quotes Firecracker project numbers (125ms), no platform measurements | scale-to-zero | none |
| Beam (beta9) | **gVisor + runc** | "under a second" | stateful sandboxes | **AGPL, self-host incl. Hetzner** — closest to our BYO posture |
| Microsandbox | **libkrun**, embedded | "<100ms boot" (on an M1 — single-machine claim) | none documented | fully self-hostable, but no control plane at all |

*(Blaxel: sub-25ms resume-from-standby claim, auto-standby after ~15s idle, pay-storage-only while standby — the cleanest scale-to-zero pricing model in the field, VMM undisclosed. Runloop, Depot: microVMs, VMM undisclosed, no self-host.)*

**What we copy:**

1. **Firecracker as the substrate** — it's what E2B, Fly, Vercel, CodeSandbox, Koyeb, and Lambda all converged on independently. The NSDI'20 numbers (~3MB VMM overhead, production oversubscription up to 10x, tested to 20x) are from a secondary summary [acolyer 2020-03-02; USENIX PDF blocked — flagged], but the convergence itself is primary evidence.
2. **CodeSandbox/Together's snapshot pipeline shape** (snapshot → hibernate → clone, with userfaultfd CoW memory sharing) and their habit of publishing p95s — their 500ms/511ms figures are our external benchmark to beat.
3. **Fly's honesty contract**: suspend/resume is best-effort; SDKs must treat resume-or-cold-start as one operation with two latencies. We adopt this *as API semantics* (§6), not just as ops reality.
4. **Blaxel's scale-to-zero pricing shape** (idle = storage only) — this is what makes per-end-user sandboxes economically sane, and it matches our §3.6 design.
5. **Daytona's runner topology** (nodes dial out to the control plane; region-local snapshot storage so blobs never cross NAT) — the only published design solving our BYO topology. We adopt dial-out but keep the control plane self-hostable in standalone mode (§9), which is exactly what Daytona's abandonment of their OSS repo (June 2026) teaches: an open runner tethered to a closed control plane earns no trust when the vendor pivots.

**What we deliberately do differently:**

- **Nobody serves BYO bare metal behind NAT as a first-class, non-enterprise feature.** E2B's self-host needs two clouds and Cloudflare; Northflank and Daytona gate BYO behind commercial tiers; Beam is closest but gVisor-based. That's our wedge, and it forces the dial-out node agent (§5.3) and the tiered isolation story (§2.3) — design consequences, not afterthoughts.
- **Modal and Beam accept a userspace-kernel boundary; we don't for the default tier.** gVisor is a legitimate engineering choice (Google runs claude.ai's code execution, Cloudflare Pages builds, GKE Sandbox on it), but our two-level tenancy — arbitrary hostile end-user code, resold — argues for hardware virt as the default and gVisor only as the explicitly-labeled fallback tier.
- **Per-sandbox published pricing with idle ≈ storage** (vs Modal's 3x sandbox premium over functions).
- **Publish our latency numbers with conditions.** The field's habit of condition-free marketing numbers is an opening: a public, reproducible bench harness is cheap credibility.

## 2. Isolation primitive

### 2.1 The comparison

| | Boundary vs hostile kernel exploit | Memory overhead/instance | Boot | Snapshot/restore | Runs "anything" | Needs /dev/kvm |
|---|---|---|---|---|---|---|
| **Firecracker v1.16.1** (2026-07-02) | hardware virt, minimal device model (~no PCI by default, tiny attack surface; first escape-class CVE only in 2026) | ≤5MiB claimed (1vCPU/128MiB, spec conditions; stale M5D.metal test bed) | ≤125ms to init (serial off, minimal kernel) | **GA since v1.13.0** (2025-08); UFFD restore; diff snapshots still preview; no live migration | yes — real Linux kernel | yes |
| **Cloud Hypervisor v53** (2026-07-12) | hardware virt, wider device model (PCI hotplug, VFIO — more surface) | no primary figure exists [flagged gap]; NSDI'20-era secondary: ~13MB | no current primary figure | mature: offloaded snapshot daemon, UFFD on-demand, **postcopy live migration, mTLS migration** | yes, incl. Windows | yes (or MSHV) |
| **gVisor (runsc, systrap default)** | userspace kernel; host kernel still reachable through a filtered syscall surface — weaker than HW virt, but proven: GKE Sandbox, Cloud Run, Anthropic's claude.ai code exec, OpenAI, Cloudflare Pages [gvisor.dev/users, retrieved 2026-08-13] | "small, mostly fixed" per docs; no primary number [flagged] | ~container speed; GKE Agent Sandbox: p90 200ms *with warm pool* (2026-05) | checkpoint/restore incl. background restore + cross-CPU-feature; Modal built memory snapshots on it | almost — syscall subset; io_uring off, no nested KVM, partial iptables | **no** — systrap needs no KVM |
| **Kata 4.0** (2026-07-22) | hardware virt via QEMU/CH/FC | ~334MiB idle measured (AWS sample: QEMU 282 + shims) | 100s of ms [vendor-tier claims, UNVERIFIED] | via underlying VMM | yes | yes |
| **Hardened containers** (seccomp+userns+cgroups) | shared kernel — one kernel LPE = escape; not defensible for our threat model | ~0 | ~ms | CRIU (fragile) | yes | no |
| **WASM (wasmtime 47)** | in-process SFI; strong for what it runs | KBs–MBs | sub-ms instantiation (pooling/CoW) | module-level | **no** — WASI 0.3 (2026-06) still lacks threads/fork/dlopen; CPython needs Emscripten; "run this container" impossible | no |

### 2.2 The pick: Firecracker, with the VMM behind a driver interface

**Primary runtime: Firecracker.** The reasons, in order of weight:

1. **Smallest hardware-virt attack surface with production-proven multi-tenancy.** Lambda has run it across millions of workloads for seven years; its device model is a fraction of CH's (no PCI unless enabled, no VFIO, no hotplug by default). One escape-class CVE in its history (CVE-2026-5747, 2026) vs a Windows-capable general VMM's surface.
2. **The snapshot path is now GA** (v1.13.0) with UFFD restore — the exact mechanism our density design needs (§3.2) — plus VMGenID always-on and VMClock (v1.15.0) for the uniqueness protocol.
3. **Independent convergence**: E2B, Fly, Vercel, CodeSandbox/Together, Koyeb, Lambda all chose it (§1). Betting with the ecosystem means the restore-acceleration research (REAP/FaaSnap/Aquifer lineage) and the tooling target our VMM.
4. ≤5MiB per-VM overhead is the only *primary-source* per-VM figure any VMM publishes.

**What Firecracker costs us, named:** no live migration (CH has postcopy + mTLS migration as of v53) — our drain story is therefore pause/resume, which breaks long-running non-pausable connections during maintenance; no GPU/device passthrough (explicit non-goal for v1; GPU sandboxes would force CH or Kata later); diff snapshots still preview, so incremental pause images wait. **Hedge:** `noded` talks to VMMs through a narrow driver interface (create/restore/pause/snapshot/exec-attach); if live migration or devices become existential, a Cloud Hypervisor driver is a contained swap, not a rewrite. If we're wrong about Firecracker's feature velocity (diff snapshots and the secret-hiding/`guest_memfd` branch have been "in progress" for over a year — PRs active through 2026-08-12), the CH fallback is the revisit path.

```go
// The interface noded holds; Firecracker is driver #1, gVisor #2, CH if needed.
type VMDriver interface {
    Create(ctx, spec MachineSpec) (Machine, error)          // cold boot (builders only)
    Restore(ctx, snap SnapshotRef, uniq UniquenessSeed) (Machine, error)
    Pause(ctx, m Machine, dest SnapshotSink) (SnapshotRef, error)
    Exec(ctx, m Machine) (GuestConn, error)                 // vsock to guestd
    Kill(ctx, m Machine) error
    Capabilities() DriverCaps                               // isolation tier, migration, devices
}
```

### 2.3 The /dev/kvm problem — and the February 2026 plot twist

The map as of 2026-08-13 (all vendor primary sources, retrieved 2026-08-13):

- **AWS: nested virtualization now works on virtual instances** — announced 2026-02-16, and the supported list has already grown to 16 Intel families (C7i/M7i/R7i, C8i/M8i/R8i + id/flex variants, X8i, I7i). Opt-in per instance (`NestedVirtualization=enabled`). **Intel only — no Graviton, no AMD.** AWS itself steers perf-sensitive users to metal. Graviton exposes KVM on `.metal` only.
- **Azure: nested virt on mainstream families including AMD** (Dasv5 "Supported", doc updated 2026-07-27); **no general-purpose bare metal** (BareMetal Infrastructure is SAP/Oracle/Nutanix-scoped). On Azure we are *always* nested under Hyper-V.
- **GCP: nested virt Intel-only** (explicitly excluded on AMD, Arm, E2), with a vendor-documented penalty: "10% or greater decrease for CPU-bound, possibly greater for I/O-bound." **But GCP bare metal is now broad and GA**: C3/C4/C4D (AMD 384-core!)/C4A (Arm Axion, GA 2026-05-28)/Z3/X4 — with real caveats (single NIC, no live migration: host maintenance *terminates* the instance).
- **Nested penalty in general:** an L2 exit costs ~4 world switches vs 2; GCP's ≥10% is the only vendor number; there is **no published L0=Nitro/L1=KVM/L2=Firecracker benchmark anywhere** [gap — Phase 0 measures it on m8i/c8i].

Consequence: the "nested-virt forces us onto $10k/mo metal" premise from 2025 is dead on AWS and Azure for Intel/AMD respectively — hardware-virt sandboxes can run on ordinary instances in a customer's own AWS/Azure account. What survives of the problem: GCP-AMD/Arm-virtual, older instance families, and no-KVM BYO boxes (VPSes, some hosters).

### 2.4 The tiered model — different guarantees, said out loud

Two isolation tiers, surfaced in the API as a first-class field, never silently downgraded:

- **Tier `hv` (default): hardware virtualization.** Firecracker on bare metal (our fleet), on nested-capable cloud instances (BYOC), or on customer metal (BYO). One tenant's kernel exploit must additionally beat KVM + the jailer to reach anyone else.
- **Tier `kernel` (fallback, explicit): gVisor.** For placements with no /dev/kvm (GCP-AMD virtual, VPS-class BYO boxes) and for customers who choose it (faster start, no KVM dependency). The boundary is a userspace kernel plus seccomp — materially weaker against kernel exploits, and we say so in exactly those words in the API reference. Precedent that it's a *respectable* fallback, not a shameful one: Google runs GKE Agent Sandbox and Cloud Run on it; Modal and Beam run businesses on it.

Placement policy: `isolation: hv` sandboxes only schedule to hv-capable nodes; a create that can't be satisfied fails with a clear error rather than downgrading. BYO nodes carry `attestation: none` and only ever run their owner's workloads (§5.3), so the tier question on BYO is the customer's own risk decision about their own users. The dashboard shows, per sandbox, tier + node class + (for BYOC/BYO) whose hardware it's on.

Cost note for our own fleet (from §3.7's table): bare metal at ~$1–3/GB-RAM/month (Latitude m3.large 1TB at $0.92; Hetzner post-June-2026 ~$2.4–5.6) versus AWS metal at $4.17–9.20 — a 3–10× spread that, combined with egress (~$0.09/GB AWS vs ~free/1Gbit Hetzner: 20TB/mo ≈ $1,791 vs ~$23), settles the fleet question in bare metal's favor for the density tier; cloud metal/nested exists for BYOC and compliance-pinned regions.

## 3. Density and cold start

### 3.1 Cold-start target, stated and defended

**Target: p50 ≤ 300ms, p99 ≤ 1s from API call to a usable shell/exec, for create-from-template and resume-from-local-pause. Resume from object storage: p50 ≤ 3s.**

Why this number and not lower: the best *documented, conditioned* numbers in the field are CodeSandbox/Together's 500ms start-from-snapshot and 511ms p95 resume (2025-05-20 post) and E2B's first-party ~1s resume (docs, retrieved 2026-08-13). Sub-10ms claims exist — ZeroBoot demonstrated p50 0.79ms VM-create by mmap'ing a Firecracker-produced snapshot MAP_PRIVATE into a custom KVM loader (Show HN 2026-03-17) — but that prototype is single-vCPU, has **no networking**, no disclosed test hardware, and shares CSPRNG state across forks, which is precisely the multi-restore insecurity Firecracker's own docs warn about. The honest reading of the evidence: **VM-create in single-digit ms is real physics; a *usable, networked, unique* sandbox in under ~100ms is not demonstrated anywhere we could verify.** 300ms p50 beats every published production number while leaving room for the uniqueness protocol, network setup, and guestd handshake. "Feels instant" for an end user behind our customer's product is ~<400ms perceived; we sit under it.

The latency budget [ESTIMATE, to be validated in Phase 0]:

```
API + placement (cell-local, cache hit)         10 ms
jailer + Firecracker spawn, netns/tap attach    40 ms   (amortizable via pre-spawned pool)
UFFD restore: map snapshot, install hot set     60 ms   (hot set ≈5% of pages, §3.2; local NVMe)
vCPU resume → VMGenID reseed → guestd ready     80 ms   (reseed + identity regen, §7.4)
DHCP-less net config + route programming        10 ms
margin                                         100 ms
                                               ─────
                                               300 ms p50
```

### 3.2 Snapshot/restore — the mechanism

Base pipeline: build template rootfs → boot once on a builder → warm it (import the runtime, start the language server, whatever the template's `warm` hook declares) → **full snapshot** (Firecracker snapshots are GA since v1.13.0, 2025-08-28; diff snapshots remain developer-preview so v1 uses full snapshots + rootfs overlays, revisiting when diff+`guest_memfd` stabilizes [changelog, retrieved 2026-08-13]).

Restore path per sandbox: memory file served via **UFFD** (supported, documented) with the **hot working set eagerly installed and cold pages demand-paged**. This split is the single best-attested design point in the research: Aquifer (arXiv 2606.24079, 2026-06) measured across FunctionBench/SeBS that **82.8% of snapshot pages are zero, 72.7% of the non-zero pages are cold, and the hot set is ~5.5% of total pages, fragmented** — so the snapshot format must (a) never store zero pages, (b) index hot pages explicitly rather than as contiguous ranges, (c) install hot pages synchronously (`uffd.copy`) and stream the rest. REAP (ASPLOS'21, 3.7× faster than vanilla restore via working-set prefetch) and FaaSnap (EuroSys'22, async prefetch + zero-page minor faults) are the lineage; Lambda SnapStart in production uses 512KB chunks with recorded access patterns prefetched ahead of need, at ~1ms per chunk from node-local cache and single-digit ms from the AZ tier [AWS compute blog, 2025-08-19].

Our snapshot format, therefore: content-addressed **512KB chunks** (matching Lambda's ATC'23-documented design: convergent encryption per chunk, ~80% of uploads produce zero unique chunks, dedup up to 23×), a **hotness-ordered index** produced by recording UFFD faults during template warm-runs, zero pages elided entirely. Chunk store tiers: node-local NVMe cache → region object storage. Page-cache sharing across restores on one node is automatic: all sandboxes from one template fault the same chunk files, so read-only guest pages are backed by one page-cache copy per node — same-content memory sharing **by construction, without KSM** (§7.3).

The restore hot path, concretely (this is what Phase 0 times):

```
1. clone jail dir; open snapshot index (hot-page list + chunk map)     ~1 ms
2. spawn jailed Firecracker with vsock + tap fds inherited            ~40 ms  (or take one
                                                                        from the pre-spawned pool)
3. PUT /snapshot/load {mem_backend: {type: Uffd, path: uffd.sock}}
4. noded's UFFD handler: uffd.zeropage() ranges for elided zeros;
   uffd.copy() the hot set from node-local chunk cache                 ~40–60 ms for a
                                                                        ~50MB hot set on NVMe
5. PATCH /vm {state: Resumed}; VMGenID regenerates → guest reseeds
6. guestd wakes on vmgenid uevent: machine-id/hostkey/MAC regen,
   re-announce ports, ready heartbeat over vsock                       ~50–80 ms
7. cold pages fault in lazily via the same UFFD handler (background
   prefetch continues in hotness order)
```

**Multi-restore safety.** Firecracker's docs say restoring one snapshot more than once is insecure *absent a uniqueness protocol*. This is our densest-traffic code path, so the protocol is non-negotiable and mechanized (§7.4): VMGenID is always enabled and regenerated per restore (guest ≥5.18 reseeds kernel CSPRNG; our guests run 6.x), virtio-rng attached, `random-seed` files deleted pre-snapshot, machine-id/host-keys/MACs regenerated by `guestd` on first wake, and userspace PRNG reseeding hooks documented as a template contract (OpenSSL/numpy pools are the template author's obligation — stated, with helpers, because the kernel can't fix them). Golden snapshots contain no tenant state by construction (taken pre-user-code; Nanvix's rule, §7.4). User-touched snapshots restore into exactly one live sandbox at a time (lineage lock in the control plane); fork explicitly creates a new lineage and re-runs the uniqueness protocol.

### 3.3 Memory: overcommit without OOM cascades

Per-sandbox configured RAM is 512MB/1GB, but resident is what matters:

- **VMM overhead:** Firecracker's live spec claims ≤5MiB per microVM (1 vCPU/128MiB conditions, on an admittedly stale M5D.metal test bed — current claim, old hardware [SPECIFICATION.md, retrieved 2026-08-13]). Budget 8MiB [ESTIMATE, conservative].
- **Zero and cold pages never become resident:** UFFD restore + zero-page elision means an idle-ish sandbox touches its hot set only — Aquifer's data says ~5.5% hot + faulted-in cold tail. A "1GB" sandbox running a typical REPL/agent loop plausibly sits at 100–250MB resident [ESTIMATE — Phase 0 measures this; it is *the* number the economics hang on].
- **Reclaim:** virtio-balloon with **free-page reporting (GA in v1.14.0)** returns freed guest pages to the host (`MADV_DONTNEED`); we do not use free-page *hinting* (still preview with a documented memory-corruption race [ballooning docs, retrieved 2026-08-13]). Balloon inflation latency under churn is real (HotMem, arXiv 2411.12893: virtio-mem takes seconds to resize after instance recycling) — so reclaim is a background policy, never on the create path.
- **Overcommit ratio:** start at **3:1 configured:physical** on general hosts [ESTIMATE — Lambda's production ratio "up to 10x" per the NSDI'20-era secondary summary is for functions with tiny working sets; our persistent sandboxes are fatter]. The scheduler packs against *measured resident + headroom*, not configured size (§5.2).
- **Pressure response, in order:** `memory.high` throttling → balloon-inflate the fattest idle sandboxes → **pause** idle sandboxes to disk (§3.6) → refuse new placements → userspace-OOM policy kill (newest-first, §7.5). The cascade-killer is that pause is cheap and lossless, so pressure relief doesn't destroy user state.

### 3.4 CPU

cgroup v2 `cpu.max` quota per sandbox: default 1 vCPU with `cpu.max.burst` allowing short full-core bursts (interactive feel is burst-dominated); `cpu.weight` proportional under contention. vCPUs are deliberately overcommitted — idle sandboxes consume ~0 CPU, and the 100k-concurrent profile is mostly-idle by assumption (§ Decisions). No core pinning per sandbox (fragmentation kills density at 512MB granularity); instead **per-org core-scheduling cookies** so SMT siblings never mix organizations (§7.3), which costs low-single-digit % in scheduler fragmentation [ESTIMATE] versus ~30–50% for SMT-off. Dedicated physical cores are a paid tier (compliance/perf-sensitive customers), priced at the real density cost: a pinned pair of siblings removes ~2 hyperthreads from the shared pool.

### 3.5 Rootfs

Shared read-only base + per-sandbox writable overlay:

- Base images are **block devices, not filesystems-over-virtiofs**: an ext4 image per template layer stack, mounted read-only into every sandbox from one host file (page cache shared across all sandboxes on the node), with a per-sandbox **overlayfs upper** on a sparse per-sandbox ext4 (or device-mapper thin volume — Phase 0 decides; dm-thin has better discard semantics, overlayfs has simpler ops [decision deferred with a bias to dm-thin]).
- Customers supply images as **OCI images / Dockerfiles**; the build farm flattens them (Lambda ATC'23 style: flatten, chunk at 512KB, convergent-encrypt, dedup — their production data: mean 4.3% unique chunks among non-identical uploads) into template block images, then boots+warms+snapshots (§3.2). Customers never hand us running VMs; they hand us the same artifact they already know how to build.
- Cache warmth: the scheduler prefers nodes already holding the template's chunks (§5.2); the node agent prefetches the hot chunk set for any template with a running warm-pool policy; LRU eviction weighted by org activity.

### 3.6 Scale-to-zero — the feature the economics stand on

States: `running → idle-detected → paused(local NVMe) → paused(object storage) → archived`.

- **Idle signal:** no exec/API/ingress activity AND guest CPU below threshold for `ttl_idle` (default 5 min for the pool tier, configurable; the API/ingress part matters — an agent that left a server running behind a preview URL is *not* idle if traffic flows).
- **Pause:** vCPU pause → full snapshot to local NVMe (Firecracker full snapshot; E2B's first-party figure for the same operation is ~4s/GiB — our budget: ≤2s for a 1GB-configured sandbox whose resident set is ~200MB [ESTIMATE, Phase 0 validates]. Mechanism caveat, stated because it interacts with our §3.2 full-snapshot position: a Firecracker *full* snapshot nominally writes the configured memory size; our budget bets that pages never faulted in under UFFD restore produce a **sparse** memory file (and Cloud Hypervisor's v52 hole-punching shows the VMM ecosystem moving this way). Nobody has published data on full-snapshot sparseness after UFFD restore — Phase 0 measures the actual on-disk pause size, and if it comes back ≈configured size, pause cost rises ~5× until diff snapshots leave preview). VMM process exits; resident cost drops to **zero bytes of RAM**.
- **Demotion:** after `pause_demote` (default 1h) the pause image is chunked/dedup'd/uploaded to object storage and evicted from NVMe. After `retention` (default 30d) → archive/GC with warnings (§5.5).
- **Resume:** from local NVMe within the 300ms budget (§3.1); from object storage, hot-set chunks fetched first (Lambda's tiering: AZ-cache single-digit-ms/chunk, origin ~36ms median/chunk [ATC'23]) — p50 ≤3s, p99 ≤8s. **Resume is implicit**: any API call or ingress packet targeting a paused sandbox triggers it; the caller just sees a slower first response. This is Fly's suspend ergonomics with Blaxel's pricing shape (idle = storage only), and unlike Fly we make resume *durable* (pause images are replicated in object storage once demoted; only the local-NVMe window is best-effort, and that window is bounded).

Connections don't survive pause (TCP dies; Blaxel documents the same caveat). The SDK contract says so, and `guestd` re-announces ports on resume so preview URLs come back without customer code.

### 3.7 Density target — the arithmetic

Reference host: **Latitude.sh m3.large.x86** — EPYC 7543P, 32 cores/64 threads, 1,024GB RAM, NVMe, $938/mo with 20TB egress included ($0.92/GB-RAM/month — the best verified figure in the market scan; prices retrieved 2026-08-13). The deliberately RAM-heavy core:RAM ratio fits a mostly-idle fleet. (Hetzner post-June-2026 AX162 tiers land at ~$2.80–5.60/GB [config behind the new fixed tiers UNVERIFIED — their standardization FAQ didn't render]; AWS r7i.metal-48xl at 1yr RI is $3.99/GB. The arithmetic below scales linearly with $/GB.)

All per-sandbox numbers are [ESTIMATE] until Phase 0 measures them — that is Phase 0's entire job:

```
Host RAM                                    1,024 GB
− host reserve (OS, noded, cache floor)        −32 GB
= packable                                     992 GB

Resident sandbox (1GB configured, typical agent/REPL workload):
  hot+faulted working set  ~200 MB   (Aquifer: ~5.5% hot of non-zero pages;
                                      zero pages never resident; cold tail faults in)
  VMM + guestd overhead      ~10 MB   (Firecracker spec ≤5MiB + margin)
  ≈ 210 MB resident

Memory bound:   992 GB / 0.21 GB          ≈ 4,700 resident sandboxes
CPU bound:      64 threads / 2% avg util  ≈ 3,200 resident sandboxes
                (mostly-idle profile; interactive bursts absorbed by cpu.max.burst)

Target (with 30% safety margin on the binding constraint):
  ▶ 2,000 resident sandboxes per 1TB host
  ▶ paused sandboxes: 0 RAM — bounded by NVMe/object storage, not by the host
  ▶ at a 10% resident ratio (per the mostly-idle [DECIDE] profile),
    one host anchors ~20,000 defined sandboxes
```

Unit economics at target: $938 / 2,000 = **$0.47/resident-sandbox-month ≈ $0.00064/hour resident**; a *defined but mostly-paused* sandbox costs storage only (~1–2GB dedup'd pause image + overlay ≈ $0.03–0.06/month at object-storage rates [ESTIMATE]). For calibration against the only comparable published claim: Lambda ran production oversubscription "up to 10x" on function workloads [NSDI'20 via secondary summary — flagged]; our 3:1 memory overcommit with measured-resident packing is conservative by comparison. If Phase 0 finds resident sets at 400–500MB instead of ~210MB, density halves to ~1,000/host and cost doubles to ~$1/sandbox-month — which still works; the kill threshold is resident sets **>1GB actually resident per configured GB**, i.e., UFFD/zero-page sharing failing outright (§11, risk 2).

## 4. Networking

### 4.1 Per-sandbox topology

Every sandbox gets its own network namespace containing exactly one tap device bound to the Firecracker VMM process. No shared L2 bridge between sandboxes — bridges invite MAC/ARP games between guests. Instead: point-to-point routed /32s. The host side of each tap gets a link-local peer address; the guest's address is a /32 routed to that tap by the host routing table. Sandbox-to-sandbox traffic is impossible at L2 by construction and blocked at L3 by policy (guest /32s are not routed to each other; an nftables rule drops any inter-tap forwarding as backstop).

IP allocation at fleet scale: each node is assigned a /22 (1,024 addresses) of RFC 6598 carrier-grade NAT space (100.64.0.0/10, ~4.2M addresses — enough for 100k+ sandboxes across thousands of nodes without colliding with customer RFC 1918 expectations inside the guest). The node agent allocates /32s locally from its block — no fleet-wide IPAM chatter on the hot path; the control plane only assigns blocks to nodes. Guests that need to see a "normal" private IP get one: the same /32 model, NATed at the host for egress.

### 4.2 The metadata endpoint — treat as the top escape vector

This is the most common real-world compromise path for exactly this class of platform, and it stays current: MITRE tracks it as T1552.005; Capital One (2019) is the canonical SSRF→IMDS→credentials incident; in September 2025 CVE-2025-51591 was exploited in the wild — SSRF via Pandoc document conversion reaching EC2 IMDS, precisely the kind of tool our customers will run on untrusted input [The Hacker News, 2025-09; MITRE ATT&CK T1552.005, retrieved 2026-08-13].

Do not rely on IMDSv2's hop limit. Latacora's analysis (2025-10-02) shows hop-limit=1 only protects bridge-mode containers; anything sharing the host netns or one hop away still reaches IMDS. Our defense is structural, four layers:

1. **No route.** The guest's netns contains only its tap; 169.254.0.0/16 has no route anywhere in the guest's path. Azure/GCP equivalents (same IP, plus GCP's `metadata.google.internal`) are covered by the same rule.
2. **Explicit drop.** Host nftables: `ip daddr 169.254.0.0/16 iifname "tap*" drop` — first rule, not last.
3. **IMDSv2-only, hop limit 1** on cloud hosts, so even a full guest-to-host escape needs a second step to mint credentials.
4. **Minimal instance role.** Host instance profiles carry only node-agent enrollment permissions; customer-data access requires the control plane, never ambient instance credentials.

The same drop applies to the control plane's CIDRs, the host's own addresses (except the guest-agent vsock path, which isn't IP at all — see §6.2), and RFC 1918/CGN space other than the sandbox's own /32 peer.

### 4.3 Egress policy

**Position: default-open with a standing deny-list, per-sandbox configurable down to allow-list.** Default-deny kills the primary use case (agents installing packages, calling APIs); every serious competitor ships open-by-default with controls (E2B exposes `allowInternetAccess`, CIDR and domain-level allow/deny including wildcards, mutable on a running sandbox — docs retrieved 2026-08-13). We match that surface:

- **Always blocked:** link-local, RFC 1918/CGN (other than own gateway), host, control plane, other tenants. Outbound port 25 blocked unconditionally; 465/587 blocked by default, unblockable per-org after review (Fly.io's model — SMTP blocked by default, unblock on request, per their community threads).
- **Default:** everything else open, with per-sandbox bandwidth caps (default 100 Mbit/s [ESTIMATE — tune in beta]) and connection-rate limits (SYN/s per sandbox) to blunt DDoS-participation and scanning.
- **Configurable:** per-template or per-sandbox CIDR/domain allow- and deny-lists. Domain rules are enforced at our DNS resolver plus SNI inspection for TLS — same mechanism E2B documents.
- **Mining/abuse:** no published provider actually documents stratum-protocol blocking [UNVERIFIED — we found none]; the observed practice is behavioral. GitLab's 2021 credit-card gate was defeated by card reuse within weeks — payment gating is a weak signal. We detect instead: sustained 100%-CPU with near-zero I/O, connections to known pool endpoints (threat-intel feed), plus account-velocity signals. Enforcement is throttle-then-suspend with audit trail, not silent kill.

DNS: guests get one resolver IP (their /32 peer). The node runs a local caching resolver that is also the policy and logging point — per-sandbox query logs feed abuse detection, and domain allow/deny is enforced where the name is resolved, not just at connect time.

Mechanism: nftables with sets and verdict maps, one chain per sandbox keyed by tap interface. The "eBPF is O(1), nftables is O(n)" claim circulating in 2026 tutorials conflates legacy iptables with nftables — nftables sets/maps don't traverse linearly, and we found no primary benchmark showing nftables failing at our scale [UNVERIFIED claim, flagged]. Position: nftables now (boring, auditable, works on every kernel we target including BYO servers); revisit with eBPF only if per-host rule counts pass ~50k or we need flow-level metering the nft counters can't give us.

### 4.4 Ingress — preview URLs

Pattern is industry-converged: wildcard DNS + the port and sandbox ID as the leftmost label + SNI/Host routing at a gateway. E2B uses `{port}-{sandboxId}.e2b.app`; Cloudflare's Sandbox SDK uses `{port}-{id}-{token}.domain`; Codespaces uses `{name}-{port}.app.github.dev` (all docs retrieved 2026-08-13). We do:

```
https://{port}-{sandbox_id}.run.{region}.ourdomain.dev
```

- Wildcard DNS per region → anycast/GSLB to regional gateway fleet (Envoy). Gateway terminates TLS with the wildcard cert, maps `{sandbox_id}` → node via a routes cache (updated by the control plane, cached locally so ingress survives control-plane outage), and forwards over the fleet WireGuard mesh to the node, which DNATs to the sandbox's /32.
- **Auth: default private, real auth — the Codespaces model, not the capability-URL model.** Private ports require a bearer token (`Authorization` or `X-Sandbox-Token`) minted per sandbox; agents handle headers trivially. Cloudflare's token-in-hostname is a capability URL — anything that logs hostnames (proxies, TLS SNI, browser history) leaks it. E2B's URLs are effectively public-if-known [E2B docs, retrieved 2026-08-13 — no documented URL auth]. Customers can flip a port to `public` explicitly, per port, with org-level policy to forbid it.
- Custom domains (customer CNAMEs) are a hosted-tier feature: ACME per hostname at the gateway.

BYO-node ingress works identically because the node agent dials out (§5.3): the gateway reaches the node over the established tunnel; no inbound port on the customer's NAT needed.

## 5. Control plane

### 5.1 Object model — two-level tenancy is the schema, not a bolt-on

Our customer resells sandboxes to *their* users, so the hierarchy models both levels explicitly:

```
Organization (our customer)
 └── Project (their product/environment; quota boundary)
      ├── Template (image + resources + network policy + defaults)
      ├── ApiKey / SandboxToken (credential grades, §6.4)
      └── Sandbox
           ├── external_user_id   ← the customer's end-user identifier, indexed
           ├── volumes / snapshots (lineage-tracked, §7.4)
           └── execs, ports, events
```

`external_user_id` is a first-class, indexed, opaque string the customer sets. It buys three things: "give me the sandbox for my user U" as one API call (`GET /v1/sandboxes?external_user_id=U`), per-end-user quota enforcement (max sandboxes/user, so one of *their* users can't starve the rest of *their* fleet), and per-end-user usage rollups the customer can meter their own billing from (§8). Quotas exist at both levels: org→project (sandbox count, vCPU-hours, storage GB, egress GB) and project→external_user_id.

### 5.2 Scheduler and placement

Bin-packing thousands of 512MB–1GB instances is the easy version of scheduling — the items are tiny relative to hosts, so first-fit-decreasing gets within a few percent of optimal; we don't need a research scheduler. Placement inputs per node: allocatable memory (physical minus host reserve minus Σ resident guest working sets — *not* Σ configured guest RAM, since overcommit is the point, §3.3), pause-storage headroom, CPU load, snapshot-cache contents (prefer nodes that already hold the template's snapshot hot — this is the biggest cold-start lever), and taints (draining, BYO, isolation tier).

Scoring: filter (capacity, tier, region) → score = w₁·snapshot-cache-hit + w₂·bin-pack-tightness − w₃·correlated-risk. Anti-affinity: orgs can mark projects `isolation: dedicated-host` (compliance tier); we additionally spread any single org across ≥2 failure domains once it exceeds a handful of sandboxes. Draining is routine, not exceptional (kernel patch cadence, §7.2): cordon → pause-and-reschedule idle sandboxes → live-migrate or pause/resume active ones with a customer-visible maintenance event → reimage. Failure handling: node misses 3 heartbeats (15s) → control plane marks sandboxes `unreachable` (not dead — the node may be partitioned and still serving ingress via its tunnel); after a grace period, paused-state-backed sandboxes are resurrected elsewhere from their last consistent pause/snapshot; purely-ephemeral ones are reported lost. We do **not** auto-resurrect from possibly-stale state without surfacing it (`restored_from: {snapshot, age}` on the event).

### 5.3 Node agent — one binary, fleet and BYO identical

`noded` is a single static Go binary that: enrolls, supervises VMMs (via Firecracker's API socket + jailer), manages the snapshot/rootfs cache, programs nftables/DNS, runs the local reconciler, exports metrics, and executes the OOM/abuse policies. It is the *only* thing we install on a host; everything else (VMM binaries, kernels, snapshots) it fetches content-addressed from the artifact store.

**Connectivity: the node always dials out.** `noded` opens a single outbound mTLS gRPC stream to the regional control plane and keeps it alive; all commands multiplex over it. This is forced by the BYO requirement (customer server behind NAT, no inbound ports) — and once it exists, running our *own* fleet the same way removes an entire class of listening-socket attack surface on every node. Enrollment: operator runs `noded enroll --token <one-time-token>` (token minted in the dashboard, bound to org + expiry); the node generates a keypair, submits a CSR over the token, receives a short-lived (72h, auto-renewed) client certificate binding `node_id`→org. Compromised-node containment: a node's cert only authorizes operations on *its own* sandboxes; the control plane API is namespaced per node identity, so an owned node can lie about its own workloads but cannot touch another node's, and revocation is one row.

BYO nodes are schedulable only within their owning org (taint), with the isolation-tier caveat surfaced (§2): we cannot attest a customer's hardware, so BYO capacity is marked `attestation: none` and never receives another org's sandboxes. Ever.

### 5.4 State store and reconciliation

**Postgres.** Not etcd (we need relational queries — per-user rollups, quota sums, lineage — and etcd's 8GB practical ceiling is real at 100k+ sandboxes × events), not a bespoke KV. One regional Postgres cluster (Patroni/managed equivalent, synchronous replica + PITR) per control-plane cell; the fleet is sharded by *cell*, not one global database. A cell = one Postgres cluster + stateless API/scheduler/reconciler replicas + a NATS JetStream bus for events/watches. 100k sandboxes ≈ low-single-digit-GB of hot rows; Postgres yawns at this. The scaling unit is "add a cell" (a region can host several), and an org's projects pin to cells.

**Reconciliation over imperative RPC.** The control plane writes *desired* state (`sandbox 4f2a: running, node n-291`); `noded` holds *actual* state and continuously converges, reporting observed state back. Imperative fire-and-forget breaks exactly where this platform lives: nodes reboot mid-command, BYO links flap, pause/resume races with user requests. With a reconciler, a node that was partitioned for five minutes replays nothing — it compares desired vs actual and converges. The API layer still gives callers synchronous *feels* (create waits for `running` up to a deadline) but the substrate is level-triggered.

### 5.5 Lifecycle

`create → running ⇄ paused → destroyed`, plus `snapshot` (point-in-time, keeps lineage) and `fork` (create with `source: snapshot|sandbox`; forking a *running* sandbox does pause-lite: brief vCPU pause → diff snapshot → resume, cloning in O(dirty pages), §3.2). TTLs are first-class and default-on: `ttl_idle` (default 24h idle → auto-pause; pause is cheap so this is aggressive) and `ttl_absolute` (default none, settable). GC: paused sandboxes untouched for `retention` (default 30 days, org-configurable) get a warning event, then archive-to-object-storage, then deletion — every step visible in the events feed, because silently deleting a user's "persistent" sandbox is a trust-ending bug. Abandoned-resource sweeps (orphan taps, jail dirs, cache entries) run on-node hourly, reconciled against desired state.

### 5.6 Failure domains and control-plane blast radius

Cells are the failure domain. Losing a control-plane cell means: **running sandboxes keep running** (noded needs no control plane to keep VMMs alive), ingress keeps routing (gateways serve from their local route cache, marked possibly-stale), execs against *connected* nodes keep streaming (the data path node↔gateway doesn't traverse the control plane). What breaks: create/destroy/pause/resume, new enrollments, quota updates. This degradation contract is explicit in the SLA and in the architecture: the data plane holds no synchronous dependency on the control plane. Multi-region: cells are regional and independent (no global consensus); the global layer is only anycast DNS + a thin directory ("org O, project P → cell C") that is aggressively cacheable and replicated read-only to every region. Region evacuation for persistent sandboxes = pause-to-object-storage + cross-region replication of pause archives (hosted-tier feature, async, RPO hours not seconds — stated, not hidden).

## 6. API and agent interface

### 6.1 Core API: REST outside, gRPC inside

**Position: the public API is REST/JSON with SSE and WebSocket for streams; gRPC is internal only** (control plane ↔ node agent). Justification: the consumers are SDKs, curl-wielding humans, and LLM agents — all three are JSON-native; gRPC's streaming advantages matter on our internal hop, where we control both ends, not on the public edge where it complicates every proxy, browser, and agent-tool integration. Every competitor's public surface is REST for the same reason.

Surface (v1):

```
POST   /v1/sandboxes                      create (from template or snapshot)
GET    /v1/sandboxes/{id}                 state, endpoints, resource usage
POST   /v1/sandboxes/{id}/exec            start process → returns exec handle
GET    /v1/execs/{eid}                    poll status/exit code, tail output
GET    /v1/execs/{eid}/stream             SSE: stdout/stderr/exit, resumable by offset
POST   /v1/execs/{eid}/stdin              write stdin; DELETE kills
GET/PUT/DELETE /v1/sandboxes/{id}/fs/...  file read/write/delete; GET dir → listing
POST   /v1/sandboxes/{id}/ports           expose port → preview URL + token
POST   /v1/sandboxes/{id}/pause|resume    scale-to-zero controls (§3.6)
POST   /v1/sandboxes/{id}/snapshots       snapshot; POST /v1/sandboxes w/ source=snap forks
DELETE /v1/sandboxes/{id}                 destroy
```

**Idempotency is mandatory, not optional.** Every mutating endpoint accepts an `Idempotency-Key` header; creates without one are rejected for API-key principals (SDKs set it automatically). This isn't just courtesy: MCP's 2026-07-28 revision removed SSE resumability — a broken stream means the client re-issues the request — and specifies no idempotency mechanism of its own [MCP changelog, retrieved 2026-08-13]. The dedupe window is 24h, keyed on (principal, key), replaying the original response. Exec is the subtle case: replaying `exec` must return the *same exec handle*, not run the command twice — an agent retrying `rm -rf build && make` twice concurrently corrupts its own workspace.

Env/secret injection: env vars at create; secrets as tmpfs file mounts refreshed on resume, never baked into snapshots (a snapshot that contains a secret outlives the secret's rotation).

### 6.2 The in-sandbox guest agent

A single static ~2MB Rust binary (`guestd`), PID 1's child, listening **only on vsock** — no IP, no port, unreachable from the guest's network and from anything outside the host. The host node agent is the only peer. One multiplexed connection (yamux-style framing) carries: exec sessions (pty and pipe modes), file I/O, port-forward setup, health/metrics.

Keeping it from becoming an escape vector:

- vsock only. The guest agent cannot be reached by other sandboxes or by the sandbox's own untrusted network traffic.
- The node agent treats `guestd` as **hostile the moment the sandbox runs user code**: every message from the guest is length-checked, schema-validated, rate-limited; file paths in responses are never trusted or dereferenced on the host; output is treated as bytes, not structure.
- `guestd` has no authority the workload lacks — it runs inside the same VM boundary. Compromising it gains an attacker nothing beyond what they already have as root in the guest. All real enforcement (quotas, network policy, filesystem limits) lives host-side. This is the critical design rule: **the guest agent is a convenience, never a boundary.**
- It is replaceable by customers (it's OSS, §9); the platform works with any binary speaking the vsock protocol.

### 6.3 Agent ergonomics — MCP *and* OpenAPI, one thin layer apart

**Position: ship both; the MCP server is a stateless shim over the REST API, not a second brain.** The argument: OpenAPI-with-good-descriptions serves SDK-generation and the growing code-execution pattern — Cloudflare's Code Mode (2025-09-26) and Anthropic's code-execution-with-MCP post (2025-11-04, showing a 150k→2k token reduction by letting the model write code against an API instead of chaining tool calls) both argue models handle APIs-as-code better than long tool-call chains. But MCP is what agent frameworks actually mount today, and the 2026-07-28 spec revision made it fit us unusually well: protocol sessions were removed in favor of *server-minted handles passed as ordinary tool arguments* — exactly our `sandbox_id`/`exec_id` — and long-running work moved to the `tasks` extension (durable handles, poll via `tasks/get`) [MCP spec 2026-07-28, retrieved 2026-08-13]. We conform: stateless server, deterministic `tools/list` ordering and `CacheableResult` TTLs (both now spec'd, both improve prompt-cache hits), OAuth 2.1 resource-server auth with RFC 8707 audience binding.

The tool set (verbs namespaced per Anthropic's tool-writing guidance, 2025-09-11):

| Tool | Description (as the agent sees it) |
|---|---|
| `sandbox_create` | Create a sandbox from a template. Returns `sandbox_id`. Idempotent via `request_id`. |
| `sandbox_exec` | Run a command. Returns within 10s with output, or an `exec_id` task handle if still running. `mode: "concise"\|"full"` controls output size. |
| `sandbox_exec_status` | Poll a long-running exec: state, new output since `offset`, exit code. |
| `sandbox_exec_kill` | Terminate a running exec by `exec_id`. |
| `sandbox_fs_read` | Read a file (range-supported) or list a directory. Listings are names+sizes+types, one line each — not stat dumps. |
| `sandbox_fs_write` | Write/append a file, creating parents. |
| `sandbox_port_expose` | Expose a port; returns the preview URL and its access token. |
| `sandbox_pause` / `sandbox_resume` | Suspend to storage / resume. Resume is implicit on any other call, so agents rarely need these. |
| `sandbox_destroy` | Destroy the sandbox and its state. Requires the `sandbox_id` twice (`confirm` param) — cheap mistake-proofing for loops. |

Deliberately left out: `sandbox_list` (an end-user-scoped credential can see exactly one sandbox — listing is a customer-backend concern, and a leaked agent credential that can enumerate is worse than one that can't); snapshot/template management (control-plane operations, not agent-loop operations); raw VM controls; and a generic `sandbox_upload` (fs_write covers it; two tools that do the same thing make agents guess).

Token economy rules, enforced server-side: default `concise` responses (Anthropic reports concise formats at roughly ⅓ the tokens; Claude Code truncates tool results at 25k tokens — a response that big is already a failure); every potentially-large response is paginated with a continuation token and ends with an explicit `"...truncated, N more entries; pass cursor=X"` line, because an agent that can't see the truncation invents the rest; errors name the fix (`"path /workspace/out does not exist — parent /workspace has: src/, README.md"`), following the tool-writing guidance's error principle. Directory listings cap at 200 entries by default. Exec output returns the last 4KB + exit code in concise mode with the full log behind `sandbox_fs_read` on a well-known path.

### 6.4 Auth and credential scoping

Three credential grades:

1. **Org API keys** (customer backend only): full project CRUD. Never handed to end users.
2. **Project keys**: create/manage sandboxes within one project; for customers' server-side integration.
3. **Sandbox tokens**: short-lived JWTs (default 24h, renewable) whose audience is a single `sandbox_id`, minted by `POST /v1/sandboxes/{id}/tokens` with a scope mask (`exec,fs,ports` by default; no destroy). **This is the credential our customer hands to their end user's agent** — it can drive one sandbox and can't see the fleet, the project, or a neighbor. MCP connections authenticate with these as OAuth bearer tokens; the MCP server validates audience per RFC 8707 as the 2026-07-28 spec requires.

OIDC (SSO) is hosted-tier for human dashboards; the machine path is keys+JWTs throughout.

## 7. Security posture

### 7.1 Threat model

Four adversaries, in decreasing order of assumed capability:

1. **Hostile guest** (baseline assumption, always): root inside the VM, running exploit code against the VMM, the KVM interface, and microarchitectural state. This is not hypothetical hardening: 2026 delivered both **Januscape** (CVE-2026-53359, a 16-year-old use-after-free in KVM's x86 shadow MMU — the first KVM guest-to-host escape working on both Intel and AMD, patched 2026-07-04, with companion CVE-2026-46113 required as well) and Firecracker's own first escape-class advisory (CVE-2026-5747, OOB write in virtio-pci, fixed in 1.14.4/1.15.1) [The Hacker News 2026-07; GitHub advisory GHSA-776c-mpj7-jm3r, retrieved 2026-08-13]. The boundary we sell *will* have bugs; the posture must survive that.
2. **Hostile co-tenant**: a paying customer (or their end user) attacking neighbors via side channels or resource exhaustion.
3. **Compromised node agent / node**: a fully-owned host. Blast radius must be bounded to the sandboxes on that host.
4. **Malicious or negligent customer**: abuse (mining, spam, scanning, illegal content) that threatens our cloud accounts and IP reputation.

### 7.2 Defense in depth around the VMM

Each Firecracker process runs inside the jailer: dedicated unprivileged UID/GID per sandbox, chroot into an empty per-sandbox directory, own PID and network namespaces, cgroup-v2 limits on memory/CPU/pids, and a tight seccomp allowlist on the VMM process itself. No device passthrough of any kind in v1 (no GPU, no /dev/net/tun inside the guest's VMM beyond its tap fd, nothing hot-pluggable). Host mounts into the chroot are read-only except the per-sandbox overlay. The kernel is the next boundary: hosts run the latest LTS with livepatching, and the Januscape episode sets the operational bar — **a KVM CVE must be patchable fleet-wide within 24h**, which is an argument for the pause/migrate machinery in §3.6 being reliable enough to drain a host routinely.

Concretely, the jailer invocation per sandbox (the settled arguments, not pseudocode):

```
jailer --id {sandbox_id} --uid {per_sandbox_uid} --gid {per_sandbox_gid} \
       --chroot-base-dir /srv/jail --netns /var/run/netns/{sandbox_id} \
       --cgroup-version 2 --parent-cgroup sandboxes.slice \
       --daemonize -- firecracker --no-api ... --config-file vmconfig.json
```

with the VMM's built-in seccomp filters left at default-strict (Firecracker ships thread-specific allowlists; we add a wrapper filter denying `ptrace`, `process_vm_readv/writev`, `userfaultfd` creation by non-VMM threads, and all `*at`-family calls resolving outside the chroot).

### 7.3 Side channels — positions, with their density cost

- **KSM: off, permanently, across the whole fleet.** The dedup-timing side channel is real and repeatedly demonstrated (Lindemann & Fischer SAC'18 detected co-resident processes from KSM write latency; a 2023 result extends the attack remotely; Lindemann's ACSAC'24 defense exists but is research-grade). Firecracker's production-host guidance says disable KSM outright, and Nanvix (arXiv 2604.11669, 2026-04) states it as a design premise: no KSM, no cross-tenant sibling cores [all retrieved 2026-08-13]. The density loss is acceptable because we get our memory sharing *by construction* instead: all sandboxes restored from the same snapshot share read-only page-cache pages of provider-authored content (§3.2) — same-content sharing without a timing oracle on tenant-written memory.
- **SMT: leave it on, forbid cross-tenant sibling sharing via core scheduling.** Firecracker's prod guidance says disable SMT for tenant separation; disabling it costs ~30–50% of thread capacity [ESTIMATE — workload-dependent]. The mainstream middle path is Linux **core scheduling** with the tenant as the cookie: GKE Sandbox has shipped gVisor under core scheduling since 1.25.5 explicitly to mitigate cross-thread side channels [GKE docs, retrieved 2026-08-13]. We tag every vCPU thread with a per-*organization* cookie; siblings only ever co-run same-org workloads. Cost: scheduling fragmentation of a few percent [ESTIMATE], not 50%. Customers who need more buy the dedicated-cores tier (§3.4). If a Training-Solo-class attack (VUSec, S&P 2025 — self-training Spectre-v2 variants leaking up to 17 KB/s) is shown to work cross-core against KVM guests, we revisit toward SMT-off; that's the tripwire.
- **VM-exit branch-state flushing.** VMScape (CVE-2025-40300, ETH COMSEC 2025-09) demonstrated practical guest-to-host Spectre-BTI against unmodified KVM/QEMU on Zen 1–5 and Coffee Lake, leaking 32 B/s; the kernel mitigation is IBPB-on-VM-exit at ~1% compute cost but up to ~51% on I/O-heavy workloads [COMSEC writeup, retrieved 2026-08-13]. We take the mitigation fleet-wide and eat the I/O tax — and reduce exposure structurally by keeping VM exits rare (virtio with in-kernel vhost paths off; Firecracker's minimal device model already exits far less than QEMU).
- Standard host mitigations per Firecracker prod-host guidance: KPTI, L1TF/MDS mitigations on, SSBD, no plaintext swap (we run zram-less hosts; paused state is encrypted at rest), Rowhammer-aware DIMM selection on our own bare metal.

### 7.4 Snapshot hygiene as a security control

Restoring many sandboxes from one snapshot is our density engine and also a correctness/security trap: clones wake with identical entropy pools, identical `/etc/machine-id`, identical in-memory secrets and TLS session state. This is the exact subject of "Restoring Uniqueness in MicroVM Snapshots" (Brooker et al., arXiv 2102.12892) and Nanvix restates the rule: a cross-tenant-shared snapshot must contain **no tenant code and no state influenced by tenant code** (JIT/GC state included), and the guest must re-initialize entropy after restore. Our rules:

1. Golden snapshots are built from provider-authored or customer-template content only, taken *before* any end-user code runs.
2. Once a sandbox has executed user code, its snapshot is **private to that sandbox lineage** — pause/resume and forks stay within one end-user's chain; nothing user-touched is ever shared across tenants.
3. On every restore, `guestd` re-seeds: virtio-rng reseed + vmgenid notification consumed, regenerate machine-id, host keys, DHCP identity; MADV_WIPEONFORK-marked regions for anything secret-like we control.
4. Secrets are never in snapshots at all (tmpfs-mounted, injected on resume — §6.1).

### 7.5 Resource exhaustion as an attack on neighbors

cgroup v2 does the work: per-sandbox `memory.max` (VMM RSS + guest), `cpu.max` quota with burst (§3.4), `pids.max` (the VMM needs ~a dozen threads; cap at 64), `io.max` on the overlay device, plus the network caps of §4.3. The failure we design against is the **OOM cascade**: global host memory pressure OOM-killing arbitrary VMMs. Mitigations: overcommit policy bounded per host (§3.3), `memory.high` set below `memory.max` so reclaim throttles a ballooning guest before the OOM killer fires, node agent as the userspace OOM authority (kills/pauses the *newest or least-valuable* sandbox by policy, never random), and per-host reserve (host slice with `memory.min`) so the node agent itself never dies. A host that hits sustained pressure marks itself uncloseable-to-new-placements (§5.2) before it degrades.

### 7.6 Audit, forensics, incident response

Every control-plane mutation and every exec (argv, principal, timestamps — not payload contents by default) is written to an append-only per-org audit log. Per-sandbox forensics: on abuse flag, the node agent can snapshot the sandbox (memory + disk) to a quarantine bucket before termination — that snapshot is the evidence package. Abuse pipeline: detection signals (§4.3) → throttle → suspend + customer notification → quarantine snapshot + terminate, with the customer-facing timeline logged. IR for a suspected escape: the affected host is cordoned, live-migrated/paused off (victims first), memory-imaged, and reimaged before returning; we assume node compromise means all co-resident sandbox data on that host is disclosed and notify accordingly.

### 7.7 SOC 2, briefly and without theater

The design already produces the evidence SOC 2 wants: append-only audit logs (CC7), per-tenant isolation with documented threat model (CC6), enrollment-based node identity and short-lived credentials (CC6.1), change management via the reconciler's declarative state history (CC8). The two things to build *early because retrofitting hurts*: (a) audit log completeness — every API mutation through one middleware, no side doors; (b) access controls on production hosts — SSH via short-lived certificates through a bastion with session recording, from day one, because "who could touch a node" is the first auditor question and the BYO story makes it interesting (customer hosts are *their* SOC 2 problem; our attestation covers our fleet, and the shared-responsibility line is written into the BYO docs).

## 8. Observability and metering

Metering is not an add-on: the §5.1 object model exists so every resource-second can be attributed to (org, project, external_user_id). Instrumented from day one:

- **Per-sandbox, from the host, not the guest.** cgroup v2 gives us authoritative `cpu.stat` (usec, throttled), `memory.current`/`memory.peak`, `io.stat`; nftables counters give per-tap bytes in/out; the node agent samples every 15s into a local ring buffer and ships 1-minute rollups. Guest-reported metrics (via `guestd`) are advisory/diagnostic only — the guest lies (§7.1); billing-grade numbers come from cgroups the guest can't touch.
- **Lifecycle events as the metering ledger.** Every transition (create, resume, pause, destroy, tier change) is an immutable event with timestamps. Usage = f(events + resource rollups): running-seconds × footprint, paused-GB-hours, egress GB, per external_user_id. This is exactly the feed our customers need to bill *their* users, so it's exposed as an API (`GET /v1/usage?group_by=external_user_id`), not just a dashboard.
- **Fleet dashboards:** per-node density (defined/resident/running sandboxes), memory commit ratio vs actual, snapshot-cache hit rate on creates (the cold-start canary), restore latency histograms (p50/p95/p99 per source: local pause, remote pause, template), pause/resume rates, and abuse-signal rates.
- **Cost per sandbox-hour as a first-class tracked metric:** (node $/hr amortized + storage + egress) ÷ resident-sandbox-hours, computed per node class and published internally weekly. This number decides the fleet-mix question (§2.4) empirically rather than by argument; if it drifts above target on a node class, the scheduler's weights change, not the roadmap.

Stack: OpenTelemetry from `noded` and control plane → Prometheus-compatible store (Mimir/VictoriaMetrics) + ClickHouse for events/usage. Nothing exotic; the design constraint is only that **metering survives control-plane outages** (node-local ring buffers flush on reconnect, idempotent by event ID).

## 9. Open-core split

**Principle: the runtime is open, the fleet is the product.** The OSS piece must be genuinely useful standalone — a single `curl | install` on one Linux box with KVM yields the full sandbox API locally. A crippled OSS release earns neither adoption nor trust, and the BYO requirement means the node-side stack ships to customer hardware anyway; pretending it's secret is self-deception.

**Apache-2.0 (the "engine"):**
- `noded` (node agent: VMM supervision, jailer integration, snapshot cache, nftables/DNS programming, local reconciler)
- `guestd` (guest agent) + guest kernel configs + base rootfs build tooling
- The full REST API implementation in **single-node mode**: `noded --standalone` serves the same API the cloud does, backed by SQLite. This is the adoption wedge — an agent developer runs the whole thing on a Hetzner box tonight.
- SDKs (Python/TS/Go), CLI, and the MCP server. (MCP server closed would be strategic malpractice — it's the top of our funnel.)
- Reference Terraform/cloud-init for self-hosted multi-node.

**Proprietary (the hosted service):**
- Multi-cell/multi-region control plane, scheduler, and directory (§5)
- Managed ingress gateway fleet, wildcard TLS, custom domains
- Image build farm (Dockerfile → golden snapshot pipeline at scale), the warm snapshot CDN/cache tiers
- Org management: SSO/OIDC, quotas UI, audit-log retention, usage/metering APIs at fleet scale, abuse-detection models
- Attestation and compliance tooling (SOC 2 evidence, dedicated-host tiers)

The line is drawn where multi-tenancy begins: everything needed to run *your own* sandboxes on *your own* hardware is open; everything about running *other people's* mixed workloads on a shared global fleet is paid. Self-hosters who outgrow standalone mode become design partners or hosted customers — the same wedge Daytona (AGPL) and Microsandbox (OSS runtime) are betting on, but with a permissive license because AGPL suppresses exactly the embedded-in-your-product adoption our customer profile (companies reselling sandboxes to their users) requires.

## 9. Open-core split

## 10. Build sequence

**Phase 0 — the density/restore spike (2–3 weeks, 2 engineers, one rented box).**
Scope: one Latitude/Hetzner EPYC host; Firecracker v1.16.x; hand-rolled harness that builds a Python+Node template, snapshots it, then restores N sandboxes with the full uniqueness protocol (VMGenID reseed verified, machine-id regen), real tap networking, and a `guestd` stub answering exec.
De-risks: the two numbers the whole plan stands on — restore latency with networking + uniqueness (not ZeroBoot-style bare VM-create) and true resident-set size per sandbox under realistic agent workloads (run an LLM-agent trace, measure `memory.current` distribution over hours).
Exit criteria, measured not vibed: **p50 restore ≤300ms / p99 ≤1s at 50 concurrent restores; ≥500 sandboxes resident on 256GB with the workload trace running; resident p50 ≤250MB per 1GB-configured sandbox.** Also produced: the nested-virt benchmark nobody publishes (same harness on an m8i.2xlarge with `NestedVirtualization=enabled` — the L2-Firecracker penalty number, §2.3). Miss the exit criteria by >2× → stop and rethink (risk 2's tripwire).

**Phase 1 — standalone node = the OSS seed (6–8 weeks).**
Scope: `noded --standalone` (SQLite, single host) + `guestd` + the full v1 REST surface (create/exec/fs/ports/pause/resume/destroy, idempotency keys) + preview URLs via a bundled Caddy-class gateway + the CLI + Python/TS SDKs + MCP server. Everything in §6 except multi-node.
De-risks: API shape against real agent loops (dogfood: run coding agents against it daily); the guest-agent protocol; pause/resume durability on one box.
Exit: an external developer can `curl | install` on a Hetzner box and run 200 sandboxes from our docs in under 30 minutes; agents complete a benchmark task suite over MCP with zero human intervention; OSS repo public.

**Phase 2 — the cell (8–10 weeks).**
Scope: Postgres control plane, scheduler, dial-out mTLS node enrollment, multi-node placement, object-storage pause demotion + chunk store, the real Envoy gateway with route cache, usage metering pipeline (§8).
De-risks: reconciler correctness under partitions; the degradation contract (§5.6).
Exit: 5-node cell survives, with evidence: kill -9 a node (paused sandboxes resurrect elsewhere; running ones reported honestly), partition the control plane 10 minutes (running sandboxes + ingress unaffected; API returns clean 503s), drain a node for "kernel patch" in <15 min without losing a paused sandbox. 10k defined / 1k resident sandboxes on the cell.

**Phase 3 — hostile-tenant hardening (6 weeks, overlaps 2).**
Scope: gVisor tier-`kernel` driver; core-scheduling cookies; seccomp/jailer audit; the §7.4 uniqueness test suite (two restores of one snapshot must produce different machine-ids, SSH keys, CSPRNG streams — CI-enforced); abuse pipeline v1; external pentest focused on guest→host and tenant→tenant.
Exit: pentest report with all criticals/highs fixed; uniqueness suite green in CI; a mining workload detected and throttled in staging within 15 minutes.

**Phase 4 — hosted beta (ongoing).**
Scope: multi-cell control plane, org/SSO, quotas, dashboards, cost-per-sandbox-hour tracking live, 3–5 design partners (one of them BYO-server, one BYOC-in-their-AWS).
Exit: 30 days of a design partner's production traffic at ≥99.5% create-success, cost/resident-sandbox-hour within 2× of the §3.7 model, and the §8 usage API reconciling to within 1% of node-level ground truth.

**Phase 5 — BYO GA + second region.** Enrollment UX, BYO attestation-none guardrails, region evacuation runbook executed for real once.

The ordering principle: every phase ships something a customer (or the OSS community) can use, and the riskiest physics (density, restore latency) is bought down before any distributed-systems work begins. Team size implied: ~4 engineers through Phase 2, 6–8 by Phase 4 [ESTIMATE].

## 11. Risk register

| # | Risk | Likelihood | Impact | Response |
|---|---|---|---|---|
| 1 | **Guest→host escape** (KVM or VMM CVE — Januscape and CVE-2026-5747 prove the class is live) | Medium (a serious KVM/VMM CVE every 1–2 years is the observed base rate) | Existential if exploited cross-tenant | Mitigate: defense-in-depth (§7.2) so a VMM escape lands in an empty chroot as a nobody-UID; 24h fleet patch SLA exercised via routine drains; vuln-reward program at GA; IR runbook (§7.6). **Accept** the residual: we sell hardware-virt isolation, not formal verification. |
| 2 | **Density misses 3–5×** (resident sets much fatter than the ~210MB estimate) | Medium — the estimate leans on serverless-function studies (Aquifer), and persistent agent sandboxes are fatter | High: unit economics — $0.47 → $2+/sandbox-month | Phase 0 measures *before* fleet commitment; levers in order: more aggressive idle→pause, balloon+free-page-reporting reclaim, RAM-heavier SKUs (r7i-class at $3.99/GB), price floor changes. Kill-signal defined in §3.7. |
| 3 | **Nested-virt regression/withdrawal** (AWS's Feb 2026 feature is new; Intel-only everywhere; GCP AMD/Arm excluded) | Low-medium | Medium: BYOC coverage gaps | Own fleet is bare metal (unaffected); gVisor tier covers no-KVM placements *with explicit labeling*; GCP has GA metal as fallback. |
| 4 | **Abuse → cloud-account termination / IP-reputation death** (mining, spam, DDoS-participation; the PurpleUrchin pattern industrialized abuse of exactly our product shape) | High (it *will* be attempted continuously) | High | §4.3 egress posture + detection; velocity-based signup friction on the hosted tier (card gating alone demonstrably fails — GitLab 2021); separate IP pools per trust tier so one burned range doesn't poison the fleet; provider relationships with documented abuse-desk SLAs. |
| 5 | **Snapshot-uniqueness bug → cross-tenant key/entropy reuse** (Firecracker docs call multi-restore insecure without the protocol) | Low (protocol is mechanized) | Existential-adjacent (a "two customers minted the same TLS key" headline) | Uniqueness CI suite (Phase 3) runs on every template build; golden snapshots contain no tenant state by construction; lineage locks enforce restore-once for user-touched snapshots; annual external crypto review of the restore path. |
| 6 | **Firecracker feature stall** (diff snapshots preview >1yr; secret-hiding unmerged since 2025) | Medium | Medium: pause-image costs stay full-size; direct-map exposure persists | VMDriver interface (§2.2) keeps a Cloud Hypervisor swap contained; full-snapshot + chunk-dedup already gives most of diff-snapshots' win at the storage layer. |
| 7 | **Bare-metal repricing / ToS risk** (Hetzner 3×'d entry pricing June 2026; Latitude's $0.92/GB is one provider's current list; hoster ToS on multi-tenant resale unverified) | Medium | Medium | Multi-provider fleet from day one (≥2 of Latitude/Hetzner/OVH); cost-per-sandbox-hour tracked weekly (§8) with SKU-mix as the response; verify resale/virtualization ToS in writing before fleet commitment [flagged UNVERIFIED in research]. |
| 8 | **A BYO node is hostile** (customer's "own server" is an attacker's server: fake usage, poisoned snapshots, harvested images) | Medium | Medium (bounded by design) | BYO runs only its owner's org; node credentials scoped to own sandboxes (§5.3); BYO-produced snapshots never enter shared caches; usage from BYO nodes marked non-billable-grade. |
| 9 | **Side-channel disclosure forces SMT-off fleet-wide** (VMScape-class findings keep coming; core-scheduling is our current stance) | Low-medium | Medium: ~30–50% thread-capacity cut → CPU-bound density drops | Capacity model (§3.7) already CPU-bound at 3,200 vs memory 4,700 — SMT-off moves CPU bound to ~1,600, inside the 2,000 target's margin only if resident stays lean; pre-negotiated burst capacity with providers; dedicated-core tier already priced for the customers who'd demand it. Tripwire defined in §7.3. |
| 10 | **Open-core backfire** (a hyperscaler or competitor hosts our OSS engine better than we do) | Medium | Low-medium | Accepted deliberately: the moat is fleet ops, the build farm, the gateway network, and trust — not the runtime (§9). The alternative (crippled OSS) fails worse, per Daytona's abandoned-repo cautionary tale. Trademark + brand protection; hosted-only features chosen to be genuinely hard (multi-region, abuse ML), not artificially withheld. |

The two that keep me up: #2 (it invalidates the business quietly, which is why Phase 0 exists) and #4 (it invalidates the business loudly, and no single mitigation suffices — it's an operations muscle, not a feature).

## 11. Risk register

## Bibliography

All retrieved 2026-08-13. Format: source — date — what it establishes. **(P)** = primary source; **(S)** = secondary, used only where flagged.

### §1 Prior art
- (P) docs.e2b.dev/sandbox/persistence — undated, live — E2B first-party: resume ≈1s, pause ≈4s/GiB; refutes the third-party "150ms" figure.
- (P) github.com/e2b-dev/infra + self-host.md — live — Apache-2.0; Firecracker/Nomad/Consul; self-host needs GCP/AWS + Cloudflare domain.
- (P) modal.com/blog/mem-snapshots — 2025-01-28 — Modal on gVisor; snapshot restores: PyTorch import 5s→1.05s p50.
- (P) modal.com/docs/guide/sandbox-snapshots + /pricing — live — sandbox memory snapshots alpha, same-instance-type restore; sandbox pricing ~3× functions.
- (P) fly.io/docs/reference/suspend-resume + /about/pricing + /reference/architecture — live — Firecracker; resume "few hundred ms", ≤2GB limit, resume not guaranteed; stopped machines bill rootfs only.
- (P) developers.cloudflare.com/containers/platform-details — live — Containers: per-instance VM (VMM undisclosed), 1–3s cold start, snapshots "coming soon".
- (P) blog.cloudflare.com/cloudflare-containers-coming-2025 — 2025-04-11 — Durable Object per container as the control-plane sidecar.
- (P) docs.aws.amazon.com/lambda/latest/dg/snapstart.html — live — SnapStart runtime restrictions, caching model, "sub-second".
- (S) blog.acolyer.org/2020/03/02/firecracker — 2020-03-02 — NSDI'20 summary: ~3MB/microVM, 10× prod oversubscription (USENIX PDF blocked; flagged wherever used).
- (P) vercel.com/docs/sandbox + /pricing — updated 2026-08-04 — Firecracker explicit; persistent-by-default; $0.128/CPU-h; iad1 only.
- (P) together.ai/blog/code-sandbox — 2025-05-20 — best-conditioned numbers in the field: 500ms snapshot start, 2.7s cold p95, 511ms resume p95.
- (P) cloud.morph.so/docs/blog/developers — 2024-11-19 — Infinibranch claims (<250ms, "instantaneous") with no methodology.
- (P) daytona.io/docs/en/bring-your-own-compute + repo README — live — BYOC runners dial out to their control plane; **"no longer maintained as of June 2026"**.
- (P) northflank.com/blog (sandbox/microVM post) — 2025-07-10 — Kata+CH/Firecracker/gVisor per-workload lineup; 100k-sandbox vendor benchmark.
- (P) beam.cloud/blog/how-to-self-host-code-sandbox — 2026-06-16 — gVisor+runc; AGPL beta9; Hetzner self-host path.
- (P) github.com/microsandbox/microsandbox — live — libkrun-based, Apache-2.0, <100ms boot on M1, no control plane.

### §2 Isolation primitive
- (P) github.com/firecracker-microvm/firecracker — releases API + CHANGELOG.md — live — v1.16.1 (2026-07-02); snapshots GA v1.13.0 (2025-08-28); diff snapshots preview; PCI preview; balloon free-page-reporting GA v1.14.0.
- (P) firecracker SPECIFICATION.md — live claim, stale test bed (M5D/M6G.metal) — ≤125ms boot, ≤5MiB overhead, conditions attached.
- (P) cloudhypervisor.org/blog v53.0 — 2026-07-12 — offloaded snapshot daemon, postcopy + mTLS live migration, VFIO migration v2.
- (P) github.com/containers/libkrun — live — VMM+guest one security context; no snapshot in stable 1.x; compat non-goal.
- (P) katacontainers.io 4.0.0 release blog — 2026-07-22 — runtime-rs default, Go runtime deprecated.
- (P) github.com/aws-samples (Kata multi-tenancy doc) — live — the only concrete Kata overhead numbers: 160MiB podFixed, ~334MiB idle measured.
- (P) gvisor.dev — platforms/performance/checkpoint-restore guides + /users — live (systrap post 2023-04, flagged stale) — systrap default, no /dev/kvm needed, C/R features, production users incl. Anthropic/OpenAI/Cloudflare.
- (P) cloud.google.com blog, GKE Agent Sandbox — 2026-05-21 — gVisor-native: 300 sandboxes/s/cluster, p90 200ms with warm pool.
- (P) wasi.dev/roadmap + WASI 0.3 release — 2026-06-11 — async landed; threads/fork/dlopen still absent → arbitrary-binary workloads out of reach.
- (P) aws.amazon.com whats-new nested virt — 2026-02-16 — nested virtualization on virtual EC2 (C8i/M8i/R8i at launch).
- (P) docs.aws.amazon.com EC2 nested-virtualization guide — live — expanded 16-family Intel-only list; opt-in flag; perf guidance toward metal.
- (P) learn.microsoft.com Dasv5 series — updated 2026-07-27 — nested virt "Supported" on AMD; Azure has no general bare metal.
- (P) docs.cloud.google.com nested-virtualization/overview — live — Intel-only, AMD/Arm excluded, ≥10% documented penalty.
- (P) docs.cloud.google.com bare-metal-instances + C4A-metal blog — GA 2026-05-28 — C3/C4/C4D/C4A/Z3/X4 metal; single-NIC/no-live-migration caveats.
- (P) usenix.org/conference/atc25 HyperTurtle — July 2025 — nested-virt hot-path offload: 5× EPT-fault handling, bounds the nested overhead problem.

### §3 Density and cold start
- (P) arXiv 2606.24079 (Aquifer) — 2026-06-23 — 82.8% of snapshot pages zero; hot set ~5.5%, fragmented; hot-preinstall beats demand-paging 187× on uffd-copy time; CXL emulated (caveat).
- (P) arXiv 2305.13162 (ATC'23, Lambda container loading) — 2023-05 — 512KB chunks, convergent encryption, 80% of uploads fully dedup'd, tiered cache hit rates and per-chunk latencies.
- (P) AWS compute blog, SnapStart under the hood — 2025-08-19 — chunk-tier latencies (L1 ~1ms/512KB, L2 single-digit ms, S3 hundreds), working-set prefetch.
- (P) arXiv 2101.09355 (REAP, ASPLOS'21) — 2021-01 — working-set prefetch, 3.7× faster restore. FaaSnap (EuroSys'22) — async prefetch refinement.
- (S→P) USENIX ATC'24 (PASS) — July 2024 — abstract-level only (PDF 403'd): DAX-fault semantic gap, 72%/47% improvements — flagged.
- (P) arXiv 2509.14292 (Spice) — 2025-09 — minor faults dominate restore cost; OS co-design, 10.6× vs VM-based restores.
- (P) arXiv 2411.12893 (HotMem) — 2024-11 — virtio-balloon/virtio-mem reclaim latency under churn; why reclaim can't be on the create path.
- (P) firecracker docs: snapshot-support, handling-page-faults, ballooning, hugepages — live — UFFD restore mechanics; multi-restore "insecure" statement; free-page-hinting race; hugetlbfs constraints.
- (P) github.com/adammiribyan/zeroboot + HN thread — 2026-03-17 — 0.79ms p50 VM-create via MAP_PRIVATE snapshot + custom KVM loader; no networking, shared CSPRNG, no hardware disclosed.
- (P) latitude.sh/pricing — 2026-08-13 — m3.large.x86 32c/1TB $938/mo, 20TB egress included.
- (P) docs.hetzner.com price-adjustment + traffic — eff. 2026-06-15 — new fixed-tier pricing (configs behind tiers unverified); 1Gbit unlimited egress.
- (S) instances.vantage.sh — 2026-08-13 — AWS on-demand/RI pricing used in the comparison table.

### §4 Networking
- (P) MITRE ATT&CK T1552.005 — live — cloud instance-metadata credential theft as a tracked technique.
- (P) latacora.com ECS/IMDS hardening — 2025-10-02 — hop-limit=1 insufficient beyond bridge mode.
- (S) thehackernews.com CVE-2025-51591 — 2025-09 — in-the-wild SSRF→IMDS via Pandoc rendering.
- (P) docs.e2b.dev/sandbox/internet-access — live — the competitive bar for egress controls (CIDR+domain rules, mutable on running sandboxes).
- (P) fly.io community threads + network-policies doc — live — SMTP blocked by default, unblock on request.
- (P) forum.gitlab.com + theregister.com — 2021-05 — credit-card gating for CI abuse and its rapid bypass.
- (S) bleepingcomputer/Unit 42 PurpleUrchin — 2023-01 — industrialized freejacking across GitHub/Heroku/Buddy.
- (P) docs.github.com Codespaces port forwarding — live — default-private ports with real token auth: the model we adopt.
- (P) developers.cloudflare.com sandbox preview-urls + tunnels changelog — 2026-05-29 — token-in-hostname capability URLs; quick tunnels.

### §6 API and agent interface
- (P) modelcontextprotocol.io spec 2026-07-28 + changelog + authorization — live — sessions removed → server-minted handles; tasks extension for long-running ops; SSE resumability removed; OAuth 2.1 RS with RFC 8707 audience binding; CacheableResult; deterministic tools/list.
- (P) anthropic.com/engineering/writing-tools-for-agents — 2025-09-11 — namespacing, concise-vs-detailed response formats (~⅓ tokens), 25k-token truncation, actionable errors.
- (P) anthropic.com/engineering/code-execution-with-mcp — 2025-11-04 — API-as-code beats tool-call chaining; 150k→2k token example.
- (P) blog.cloudflare.com/code-mode — 2025-09-26 — models write code against APIs better than they chain tool calls.
- (P) daytona.io/docs/en/mcp — live — the toolset shape (fs/exec/git/preview) of a shipping sandbox MCP server.

### §7 Security posture
- (P) github.com/firecracker-microvm advisory GHSA-776c-mpj7-jm3r (CVE-2026-5747) — 2026-04-07 — Firecracker's first escape-class bug: virtio-pci OOB write.
- (S) thehackernews.com + corgea.com, Januscape CVE-2026-53359 — 2026-07 — 16-year-old KVM shadow-MMU UAF; first guest→host escape on both Intel and AMD; fixed 2026-07-04; companion CVE-2026-46113.
- (P) comsec.ethz.ch VMScape (CVE-2025-40300) — 2025-09 — practical guest→host Spectre-BTI on default KVM/QEMU; IBPB-on-exit mitigation costs ~1% compute / up to ~51% I/O-heavy.
- (P) vusec.net Training Solo — S&P 2025 — self-training Spectre-v2 bypassing domain isolation; up to 17KB/s.
- (P) firecracker prod-host-setup.md — live — disable KSM; disable SMT for tenant separation; KPTI/L1TF/SSBD; the authoritative hardening list.
- (P) docs.cloud.google.com GKE SMT config + sandbox-pods — live — core scheduling as the shipped SMT middle path (gVisor under core scheduling since GKE 1.25.5).
- (P) arXiv 2102.12892 (Restoring Uniqueness) — 2021-02 — why clones must regain uniqueness; MADV_WIPEONSUSPEND (unshipped) + SysGenId (shipped as VMGenID v1.8.0/v1.9.0).
- (P) arXiv 2604.11669 (Nanvix) — 2026-04 — cross-tenant snapshot rule (no tenant code, no tenant-influenced state); no-KSM/no-sibling-sharing stated as design premises.
- (P) Lindemann & Fischer SAC'18 + ACSAC'24 — 2018/2024 — KSM dedup-timing attacks demonstrated; research-grade defense only.
- (P) firecracker jailer.md + seccomp.md — live — chroot/ns/cgroup mechanics; per-thread seccomp allowlists on by default.

### Known gaps (research debt, carried forward)
1. No primary per-VM memory-overhead figure for Cloud Hypervisor or gVisor — every circulating number traces to SEO content farms. Phase 0 measures our own.
2. No public L0=Nitro/L1=KVM/L2=Firecracker nested-virt benchmark — Phase 0 produces one.
3. Lambda's SMT co-tenancy policy is not publicly documented — the *recommendation* (SMT off) is verified; the practice is inferred.
4. Hetzner's post-June-2026 fixed-tier RAM/disk configs, OVH current pricing, Hetzner RX (Arm) availability, and hoster ToS on multi-tenant resale — all [UNVERIFIED]; verify in writing before fleet commitment.
5. PASS (ATC'24) numbers are abstract-level (PDF fetch blocked); CodeSandbox's 2022–23 engineering posts likewise (403, via search snippets).
6. eBPF-vs-nftables scaling claims at density: no primary benchmark found; 2026 tutorial content conflates nftables with legacy iptables.
