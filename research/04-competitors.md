# Agent-sandbox / micro-VM competitive landscape — verified as of 2026-08-13

*Research report 4 of 6 for the Substrate architecture plan. Every number is from the vendor's own docs/blog/repo unless marked. Items per provider: (1) isolation (2) cold-start/resume (3) density/pricing (4) snapshot/fork (5) OSS (6) BYO bare metal (7) control plane.*

---

**E2B**
1. Firecracker microVMs + Nomad + Consul + Terraform (github.com/e2b-dev/infra, accessed 2026-08-13). E2B's own blog only says "companies like E2B use Firecracker" (e2b.dev/blog/firecracker-vs-qemu, 2025-03-03).
2. Primary docs give **resume ≈1s**, **pause ≈4s per 1 GiB RAM** (docs.e2b.dev/sandbox/persistence, undated, accessed 2026-08-13). The widely-cited "~150–200ms cold start" appears only in third-party listicles — **[UNVERIFIED]**, not found on e2b.dev primary pages fetched.
3. Per-second: $0.000014/vCPU/s; RAM $0.0000045/GiB/s (512–8192 MiB). Hobby 1h session / 20 concurrent; Pro 24h / 100 concurrent (burst to 1,100). Storage 10 GiB (Hobby) / 20 GiB (Pro) included (e2b.dev/pricing, accessed 2026-08-13).
4. Pause/resume persists **filesystem + memory + running processes**; paused sandboxes kept **indefinitely, no TTL**; runtime limit resets on resume.
5. **Apache-2.0** (e2b-dev/infra).
6. **Partial.** self-host.md documents **GCP and AWS only**, and requires Packer, Terraform 1.7.5, a **Cloudflare account with a domain**, Postgres, plus GCP quotas ≥2500 GB PD-SSD / 24 CPUs. No bare-metal or NAT path documented.
7. Nomad/Consul orchestration is the only public detail (repo README).

---

**Modal**
1. **gVisor `runsc`** — "gVisor implements a userspace kernel and gives the guest container access to that" (modal.com/blog/mem-snapshots, 2025-01-28).
2. "Containers boot in about one second" (docs/guide/cold-start). Memory-snapshot restore ~**2.5x faster** than cold container: PyTorch import ~5s → **1.05s p50 / 0.69s p0**; Stable Diffusion ~13s → **3.5s** (2025-01-28). GPU snapshots: "up to 10x faster" (modal.com/blog/gpu-mem-snapshots, 2025-07-30) — vendor benchmark.
3. Per-second: CPU $0.0000131/core/s (min 0.125 cores), memory $0.00000222/GiB/s. **Sandboxes priced higher**: $0.00003942/core/s, $0.00000667/GiB/s. Default idle `scaledown_window` 60s, configurable 2s–20min. "Never pay for idle" (modal.com/pricing, accessed 2026-08-13).
4. Two distinct things: **Function** memory snapshots (shipped Jan 2025) vs **Sandbox** snapshots — filesystem/directory snapshots **GA** (30-day retention); **Sandbox memory snapshots are alpha** (`_experimental_*`), 7-day fixed retention, terminate the sandbox on capture, no GPU, restore only on the **same exact instance type** (docs/guide/sandbox-snapshots).
5. Proprietary control plane; no self-host offering found.
6. **No.**
7. Detail on FUSE filesystem + gVisor prioritized background page loading cooperating to cut page-fault latency (2025-01-28 post). No multi-region scheduling post found.

---

**Fly.io Machines**
1. **Firecracker microVMs**, "based on strong hardware virtualization," on hosts of 8–32 cores / 32–256 GB (fly.io/docs/reference/architecture/).
2. Start a stopped Machine: "usually well under a second"; create-from-scratch "maybe low double digit seconds" (docs/machines/overview). **Resume from suspend: "a few hundred ms"** vs cold start ~2+s (fly.io/docs/reference/suspend-resume/).
3. Per-second while running: shared-cpu-1x/256MB = $0.00000078/s (~$2.02/mo); performance-16x/128GB = $0.00039113/s (~$1,013.80/mo). **Stopped and suspended Machines bill storage only: 1 GB rootfs for 30 days = $0.15** (~$0.005/GB/day) (fly.io/docs/about/pricing/).
4. Three states: running / **stopped** (components stay assembled on the worker host) / **suspended** (memory+disk to disk). Suspend requires **≤2 GB memory**. Critically: **resume is not guaranteed** — snapshots may be discarded on deploy, host migration, corruption or maintenance; "always design for both resume and cold start paths." `auto_stop_machines = "suspend"` gives autosuspend.
5. Proprietary platform (Firecracker itself is Apache-2.0 upstream).
6. **No.**
7. Public docs name per-region physical hosts; flyd/Corrosion internals not on the architecture page fetched.

---

**Cloudflare (three separate products)**
1. **Workers** = **V8 isolates** — "any given isolate can start around a hundred times faster than a Node process" (developers.cloudflare.com/workers/reference/how-workers-works/). **Containers** = "each container instance runs inside its own VM" — **the VMM is not disclosed** in docs. **Sandbox SDK** is a layer on Containers, "own isolated container with a full Linux environment."
2. Containers cold start **targeted at the "1-3 second range"**, depending on image size (containers/platform-details/).
3. Billed in **10ms increments** while running; charges stop when the instance sleeps. Workers Paid ($5/mo) includes 25 GiB-h memory, 375 vCPU-min, 200 GB-h disk; overage $0.0000025/GiB-s, $0.000020/vCPU-s, $0.00000007/GB-s. Six instance types from **lite (1/16 vCPU, 256 MiB)** to standard-4 (4 vCPU, 12 GiB). Default `sleepAfter` 10 min.
4. **None today** — "all disk is ephemeral"; "**Snapshots are coming soon**." Workaround is FUSE to R2.
5. Proprietary (workerd is OSS, but Containers/Sandbox platform is not).
6. **No.**
7. Best-documented control plane of the set: **Durable Objects as programmable sidecars**, one DO per container, boot-on-first-`fetch`, placement "in a Cloudflare location near the incoming request," autoscaling at 75% CPU (blog.cloudflare.com/cloudflare-containers-coming-2025/, 2025-04-11). Caveat from docs: DOs and their containers are **not guaranteed co-located**.

---

**AWS Lambda SnapStart + Firecracker fleet**
1. **Firecracker microVM snapshot** of memory+disk of the initialized execution environment, encrypted and cached (docs.aws.amazon.com/lambda/latest/dg/snapstart.html, accessed 2026-08-13).
2. "As low as sub-second startup." AWS explicitly recommends **provisioned concurrency** if you need "double-digit millisecond" starts — i.e. SnapStart is not that fast.
3. Supported runtimes: **Java 11+, Python 3.12+, .NET 8+ only**. Not nodejs24.x, not ruby4.0, not OS-only runtimes, not container images. No provisioned concurrency, no EFS, ephemeral storage ≤512 MB. Versions/aliases only ($LATEST unsupported). All commercial regions except ap-New Zealand and ap-Taipei.
4. Pricing: **free for Java**; other runtimes pay a **cache charge (min 3 hours, memory-scaled)** plus a **per-restore charge**. Snapshot uniqueness (RNG/entropy, connections) is the caller's problem.
5. Firecracker upstream is Apache-2.0; the fleet is proprietary.
6. **No.**
7. Density (NSDI'20 paper, Agache/Brooker et al.) — USENIX PDF 403'd twice; numbers via Adrian Colyer's summary (blog.acolyer.org, 2020-03-02), i.e. **secondary**: boot ~100ms (~150ms incl. API call), **~3 MB memory overhead per microVM** (vs 13 MB Cloud Hypervisor, 131 MB QEMU), **oversubscription tested to 20x, production ratios up to 10x**, block I/O capped ~13,000 IOPS, back-of-envelope "up to 8000 functions on a [1 TB] server." Amazon Science's page confirms venue/authors and "millions of production workloads, trillions of requests/month" but no density figures. Marc Brooker's *Ten Years of AWS Lambda* (2024-11-14) contains **no** density/oversubscription numbers — checked. **Fargate is contested**: the NSDI'20 paper says Firecracker is deployed in Lambda *and Fargate*; the AWS "Under the hood: Fargate data plane" post (2020-04-08) describes containerd/runC with Firecracker as an optional runtime plugin. Both cited, unresolved.

---

**Vercel Sandbox**
1. **Firecracker microVM**, explicit: "each sandbox runs in a secure Firecracker microVM with its own filesystem and network" (vercel.com/docs/sandbox, last_updated **2026-08-04**).
2. "Sandboxes start in milliseconds" — **no numeric benchmark published**; treat as marketing.
3. Pro/Enterprise: **Active CPU $0.128/hour** (I/O wait not billed), **provisioned memory $0.0212/GB-hour** (1-min minimum increments), creations $0.60/1M, egress $0.15/GB, **snapshot storage $0.08/GB-month**. Hobby free tier: 5 CPU-h, 420 GB-h, 5,000 creations. Max vCPU 4/8/32 by plan; 32 GB ephemeral NVMe fixed; max duration 45min/24h/24h; concurrency 10/10,000/10,000. **Only region: `iad1`.** Dynamic vCPU allocation quota ramping 150→5,000 vCPU/min (vercel.com/docs/sandbox/pricing, last_updated 2026-08-04).
4. **Persistent sandboxes are the default** — auto-save on stop, resume in place, no manual snapshot management. Explicit snapshots also exist, expiring 30 days after last use.
5. SDK/CLI OSS at github.com/vercel/sandbox; platform proprietary.
6. **No.**
7. Notable: published control-plane rate limits (1k–100k req/min) and a 20 req/s deletion cap — useful as a design reference for API quotas.

---

**Together AI / CodeSandbox SDK**
1. **Firecracker** — CodeSandbox's engineering blog describes `create_snapshot` producing `snapshot.snap` + `memory.snap` (codesandbox.io/blog/how-we-clone-a-running-vm-in-2-seconds; direct fetch 403'd, content via search index — treat as **near-primary**). Follow-ups on userfaultfd CoW memory sharing and low-latency memory decompression.
2. Together's post (2025-05-20): **start from snapshot 500 ms**, **cold start 2.7s P95**, **hibernate/resume 511 ms P95**; **VM clone from live VM or snapshot ~3s**. (CodeSandbox's older "2 seconds" clone figure is the earlier generation.)
3. Up to **64 vCPU / 128 GB per sandbox**; $0.0446/vCPU/hour + $0.0149/GiB RAM/hour (together.ai/blog/code-sandbox, 2025-05-20).
4. Strongest fork story after Morph: memory snapshot → hibernate → clone-from-live-VM.
5. Proprietary; CodeSandbox acquired by Together AI.
6. **No.**
7. Three engineering posts on microVM memory handling (userfaultfd CoW, memory decompression) — the deepest public writing on microVM density in this list.

---

**Morph Labs (MorphVM / Infinibranch)**
1. "**MorphVM**, a custom virtualization stack" + "**MorphFS**, a distributed copy-on-write filesystem." **Underlying hypervisor not disclosed** (cloud.morph.so/docs/blog/developers, 2024-11-19).
2. "**<250 ms startup**"; snapshot/branch/restore described as "**instantaneous**." **Marketing claims** — no benchmark methodology, test conditions or workload spec.
3. Usage-based pricing; "near-zero storage overhead for cloning" — unverified.
4. **Infinibranch** is the differentiator: snapshot a running computation at any point and instantly launch N instances continuing from that point; "branch-level security" analogized to row-level security. OCI-compatible, Docker-in-Docker supported.
5. Python/TS SDKs on GitHub (morph-labs/morph-python-sdk); platform proprietary.
6. **No** self-host documented.
7. Not disclosed.

---

**Daytona — headline: the OSS repo is dead**
1. "Complete isolation, a **dedicated kernel**, filesystem, network stack" — **no VMM is named anywhere fetched**. Do not infer Firecracker.
2. "**Under 90ms from code to execution**" (daytona.io/docs and repo README) — vendor claim, no methodology.
3. Only price extracted: "Windows $0.0858/vCPU/h" and "$200 free compute"; per-second CPU/RAM/disk rates not on the page fetched.
4. "Stateful environment snapshots… persistent captures of sandbox state you can use to create sandboxes"; Declarative Builder for images.
5. **License unresolved — [UNVERIFIED].** LICENSE file 404'd via three paths; no repo badge. The AGPL assumption is *not* confirmed. **README states verbatim: "This repository is no longer maintained. As of June 2026, Daytona's core development has moved to a private codebase"** — remains "public and free to use, fork, and build on… as is and without support or warranty."
6. **Yes, best-in-class on paper**: "Bring Your Own Compute" — define custom regions, attach **your own runner nodes**, optional region-local sandbox-proxy and snapshot-manager services, **official Helm charts** (daytona.io/docs/en/bring-your-own-compute/). Control plane stays Daytona's; BYOC is gated to the Enterprise tier.
7. Three planes: interface / control / compute (README).

---

**Northflank**
1. Broadest lineup, per-workload selectable: **Kata Containers + Cloud Hypervisor** (primary), **Firecracker** (minimal device model), **gVisor** (where syscall interception suffices or nested virt is unavailable) (northflank.com/blog/how-to-spin-up-a-secure-code-sandbox-and-microvm-in-seconds…, **2025-07-10**).
2. **Vendor-cited benchmark, no independent methodology**: "ComputeSDK 2026 Scale Invitational" — 100,000 concurrent sandboxes launched in 24s from cold, **P99 allocation 566ms, P99 readiness 733ms**.
3. Not on the engineering page; separate pricing page.
4. Ephemeral and persistent execution modes; snapshot/fork not described.
5. Proprietary.
6. **Yes — the strongest BYO story.** BYOC into customer VPCs across AWS, GCP, Azure, Civo, Oracle, CoreWeave, **and on-prem / bare metal**. Caveat: Northflank's blog is aggressively SEO-optimized and authors "best sandbox" listicles ranking itself — discount the framing, keep the primitives.
7. Not published.

---

**Koyeb**
1. **Firecracker** on KVM, chosen for "multi-tenancy security, fast startup for scale-to-zero and autoscaling, and increased density" (koyeb.com/blog/firecracker-microvms-lightweight-virtualization…).
2. Attribution correction: the "**125 ms to user-space/app code**" and "**up to 150 microVMs per second per host**" figures in Koyeb's posts are **Firecracker-project numbers quoted by Koyeb**, not Koyeb platform measurements. No Koyeb-measured cold start found.
3/4. Scale-to-zero is the stated design goal; no snapshot/fork feature found.
5/6. Proprietary; **no** self-host.
7. Not published beyond the Firecracker explainers.

---

**Blaxel**
1. "Lightweight, sandboxed virtual machines" with per-workload kernels — **VMM not named** in docs.blaxel.ai/Sandboxes/Overview.
2. **Sub-25 ms resume from standby**; initial creation from template ~200–600 ms (the latter from Blaxel's own blog, which is listicle-style — discount).
3. Distinctive model: auto-standby after **~15 s of network inactivity**; standby is **indefinite at zero compute cost** — you pay **snapshot + volume storage only**, no memory charge. Active mode bills memory + storage; CPU allocated without separate charge.
4. Standby automatically snapshots full FS-in-memory + running processes. Caveat stated in their docs: external network connections (DB, MQ, HTTP pools) time out and close across a snapshot.
5/6. Proprietary; self-hosting **not mentioned**.
7. Not published.

---

**Runloop**
1. "Isolated Linux **microVM**-backed sandbox environments"; "virtual machine technology" on a "custom bare-metal hypervisor" — **specific VMM not named** (docs.runloop.ai/docs/devboxes/overview).
2. "Startup takes a few seconds"; the "10 GB images boot in under 2 seconds" figure came from a third-party aggregation, **[UNVERIFIED]** against Runloop docs.
3. Machine sizes selectable; pricing not in the docs page fetched.
4. **Snapshot, suspend and resume** via API; **Blueprints** = Dockerfile-based team-shared image templates.
5/6. Proprietary; **no** self-host documented. SOC 2 Type II / HIPAA / GDPR claimed.
7. Not published.

---

**Beam (beta9) — the OSS/self-host outlier**
1. **gVisor + runc** — user-space kernel intercepts syscalls before the host. Beam argues gVisor is why they launch sandboxes with custom deps in under a second where Firecracker platforms must mount and boot a large image (beam.cloud/blog/how-to-self-host-code-sandbox, **2026-06-16**). Weaker boundary than a microVM — say so in the threat model.
2. "Under one second" for custom-dependency sandboxes; no p50/p99 published.
3. Not in this post.
4. Beam markets "stateful sandboxes"; snapshot mechanics not detailed in the post fetched.
5. **beta9 runtime is open source under AGPL-3.0.**
6. **Yes — the most BYO-friendly of the managed vendors.** Local, Kubernetes via Helm, or BYOC on AWS/GCP/Azure **and Hetzner**, with "the same sandbox API, self-hosted or managed." Hetzner support is the closest thing here to a cheap-bare-metal path.
7. Not published.

---

**Depot Sandbox SDK**
1. "**Real microVMs, not containers**," full syscall compatibility, Docker runs inside — **hypervisor undisclosed** (depot.dev/blog/now-available-the-depot-sandbox-sdk, **2026-06-23**).
2. Starts "within a few seconds" from a pre-cached base image.
3. Same compute rate as Depot CI, metered by resources × time.
4. **None yet** — "Snapshots, secrets, and persistent disk support are all on the way."
5/6. Proprietary; **private beta**, API may change before GA. No self-host.
7. Not published.

---

**Microsandbox (OSS)**
1. **libkrun** (credited in acknowledgements, alongside smoltcp) — KVM/HVF-backed microVM library, not Firecracker.
2. "Average boot times **under 100 milliseconds**," footnoted "on an M1 machine" — single-machine vendor claim, not a fleet number.
3. N/A — no hosted service; density is whatever your host gives you.
4. Not documented in the README.
5. **Apache-2.0** (note: the common "MIT" attribution is wrong — the repo footer says Apache 2.0). A SELF_HOSTING.md exists in-repo.
6. **Yes, trivially — the only one designed for it.** `Sandbox::builder("...").create()` "boots a microVM as a child process. **No infrastructure required**" — no daemon, no control plane, embeddable in your own process. For BYO bare metal behind NAT this is the natural building block, but you supply scheduling, multi-tenancy, quotas and networking yourself.
7. None — by design.

---

## Implications for "BYO bare metal behind NAT"

Plain yes/no across the field: **self-hostable on your own bare metal** — Microsandbox (fully, no control plane), Beam/beta9 (AGPL, incl. Hetzner), Northflank (BYOC incl. on-prem/bare metal, commercial terms), Daytona BYOC (your runners, **their** control plane, Enterprise-gated, and the OSS repo is abandoned as of June 2026). **Cloud-coupled self-host only** — E2B (Terraform, GCP/AWS + mandatory Cloudflare domain; no bare-metal path documented). **No self-host at all** — Modal, Fly, Cloudflare, AWS, Vercel, Together/CodeSandbox, Morph, Koyeb, Blaxel, Runloop, Depot.

The recurring architectural pattern for NAT-behind runners is Daytona's: **runner nodes dial out to a hosted control plane**, with region-local proxy + snapshot-manager services co-located with the runners so snapshot blobs never traverse the NAT boundary. The only published design in this set that solves the topology.

Two more design-relevant transfers: Fly's honesty that **suspend/resume is best-effort, not durable** ("always design for both resume and cold start paths") is the correct contract to copy; and Vercel's published **control-plane rate quotas** (dynamic vCPU/min ramp, 20 deletions/s) show what a multi-tenant sandbox API actually has to rate-limit.

---

## Load-bearing sources

1. https://modal.com/blog/mem-snapshots — 2025-01-28 — gVisor runsc + CRIU-style snapshots; PyTorch 5s→1.05s p50, SD 13s→3.5s.
2. https://modal.com/docs/guide/sandbox-snapshots — accessed 2026-08-13 — Sandbox memory snapshots are **alpha**, same-instance-type restore only.
3. https://modal.com/pricing — accessed 2026-08-13 — $0.0000131/core/s; sandbox tier 3x higher.
4. https://fly.io/docs/reference/suspend-resume/ — accessed 2026-08-13 — resume "few hundred ms", ≤2 GB RAM, storage-only billing, resume not guaranteed.
5. https://fly.io/docs/about/pricing/ — accessed 2026-08-13 — 1 GB rootfs stopped 30 days = $0.15.
6. https://fly.io/docs/reference/architecture/ — accessed 2026-08-13 — Firecracker, 8–32 core hosts.
7. https://docs.e2b.dev/sandbox/persistence — accessed 2026-08-13 — resume ~1s, pause ~4s/GiB, indefinite retention.
8. https://github.com/e2b-dev/infra (+ /blob/main/self-host.md) — accessed 2026-08-13 — Apache-2.0, Firecracker/Nomad/Consul, GCP+AWS-only self-host.
9. https://vercel.com/docs/sandbox and /docs/sandbox/pricing — last_updated 2026-08-04 — Firecracker explicit; $0.128/CPU-h + $0.0212/GB-h; iad1 only; persistence by default.
10. https://developers.cloudflare.com/containers/platform-details/ — accessed 2026-08-13 — "own VM" (VMM undisclosed), 1–3s cold start, ephemeral disk, snapshots "coming soon".
11. https://blog.cloudflare.com/cloudflare-containers-coming-2025/ — 2025-04-11 — Durable-Object-as-sidecar control plane, placement near request.
12. https://docs.aws.amazon.com/lambda/latest/dg/snapstart.html — accessed 2026-08-13 — Firecracker snapshot; Java/Python 3.12+/.NET 8+ only; 3-hour min cache + per-restore charge.
13. https://blog.acolyer.org/2020/03/02/firecracker/ — 2020-03-02 — **secondary** summary of NSDI'20: ~3 MB/microVM overhead, 10x prod / 20x tested oversubscription (USENIX PDF 403'd twice).
14. https://www.together.ai/blog/code-sandbox — 2025-05-20 — 500ms snapshot start, 2.7s cold P95, 511ms resume P95, $0.0446/vCPU-h.
15. https://cloud.morph.so/docs/blog/developers — 2024-11-19 — Infinibranch <250ms, "instantaneous" branching; **no methodology, hypervisor undisclosed**.
16. https://www.daytona.io/docs/en/bring-your-own-compute/ + raw README — accessed 2026-08-13 — BYOC runners via Helm; **"no longer maintained… as of June 2026"**.
17. https://www.beam.cloud/blog/how-to-self-host-code-sandbox — 2026-06-16 — gVisor+runc; beta9 AGPL-3.0; BYOC incl. Hetzner.
18. https://northflank.com/blog/how-to-spin-up-a-secure-code-sandbox-and-microvm-in-seconds-with-northflank-firecracker-gvisor-kata-clh — 2025-07-10 — Kata/CLH + Firecracker + gVisor per workload; BYOC incl. bare metal; 100k-in-24s is a **vendor-cited** benchmark.
19. https://github.com/microsandbox/microsandbox — accessed 2026-08-13 — Apache-2.0 (not MIT), libkrun, <100ms boot on M1, no infrastructure required.
20. https://depot.dev/blog/now-available-the-depot-sandbox-sdk — 2026-06-23 — real microVMs, private beta, no snapshots yet.

**Caveats:** E2B's famous ~150ms figure and Runloop's <2s-for-10GB figure could not be confirmed on primary sources — both [UNVERIFIED]. Daytona's license is genuinely unresolved. The Firecracker density numbers are from a secondary summary (USENIX blocked fetches). Cloudflare Containers', Morph's, Blaxel's, Runloop's and Depot's underlying VMMs are undisclosed by their own docs. Fargate's isolation is contested between AWS's own 2020 sources.
