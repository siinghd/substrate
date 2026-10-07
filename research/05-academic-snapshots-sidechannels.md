# Academic SOTA: microVM snapshot/restore, memory pooling, density, cross-tenant side channels — as of 2026-08-13

*Research report 5 of 6 for the Substrate architecture plan. Produced 2026-08-13.*

---

## 1. Aquifer (arXiv:2606.24079, submitted 2026-06-23)

"Aquifer: Hierarchical Memory Pooling with CXL and RDMA for MicroVM Snapshots" — Junliang Hu, Huaicheng Li, Ming-Chang Yang. https://arxiv.org/abs/2606.24079

**Snapshot composition (the headline characterization).** 9 workloads from FunctionBench + SeBS, each microVM given 1.5 GiB (Azure's default minimum), snapshot taken after cold boot, then 16 invocations replayed with `userfaultfd` write-protection to classify pages:
- **82.8% of all pages are zero** on average; range **46.9% (`recognition`) → 90.7% (`pyaes`)**.
- Of the remaining non-zero pages, **72.7% are cold** (never touched in the 16-invocation window); range 60.2–86.0%.
- **5.4% of total pages are dirtied**; **0.05% are read-only**. **Hot working set = 5.5% of total pages**, "with highly fragmented address layout."
- Fleet implication: a sampled Azure region has 20,809 serverless apps → ~**30 TiB** of full-size snapshots; eliminating zero pages shrinks this to **~5.3 TiB**; with ≥10 server pods per cluster, each CXL pool need only hold **~130 GiB**.
- Motivation stat: memory stranding wastes **25–35% of installed DRAM** in production clusters. Azure trace: **80% of instances receive ≤16 invocations per 10-min keep-alive window**.

**Restore latency — important caveat.** The paper does **not** publish a clean CXL-vs-RDMA-vs-local-NVMe restore latency table. All baselines were *re-adapted to run over the same RDMA pool* "ensuring that performance differences reflect algorithmic design choices rather than storage media." Local NVMe is not a baseline. What it gives (chameleon, 32 concurrent restores, Fig. 6c — time in `uffd.copy()` as page-install proxy):

| Config | UFFD copy total |
|---|---|
| Firecracker (RDMA demand-page) | 121 ms |
| FcTiered | 121 ms |
| REAP (adapted to RDMA) | 72 ms |
| FaaSnap (adapted to RDMA) | 64 ms |
| **Aquifer (CXL hot pre-install)** | **0.65 ms** |

i.e. Firecracker's page-install cost is **187× higher** than Aquifer's. Aquifer = 2.12× lower total invocation time than Firecracker, 1.19× lower than FaaSnap on chameleon.

**End-to-end:** **2.2× geomean speedup over Firecracker**; **1.3× over FaaSnap**; **1.1× over REAP** (the abstract says "1.1× over the next best alternative"; §1 says REAP is next-best, contributions bullet cites 1.3× over FaaSnap — both appear). Per-workload vs FaaSnap: 1.00× (pyaes) → 1.57× (json). Sole loss: **ffmpeg, where REAP beats Aquifer** (ffmpeg writes intermediate files to tmpfs then frees them, creating many zero pages in the working set that REAP prefetches wholesale).

**Test conditions / limits.** CloudLab Clemson r6525: 2× AMD EPYC Milan 7543 @2.8 GHz, 256 GiB DDR4-3200, 100 Gb/s ConnectX-6 Dx. **CXL is emulated** via a remote NUMA node exposed as a `virtio-pmem`/DAX device, with `clflushopt` on every borrow to emulate a non-coherent CXL 2.0 MHD. RDMA pool backed by the pool master's local DRAM over SR-IOV VFs on one NIC. Firecracker v1.15 unmodified; guest boots with `init_on_free=1` so snapshots contain no stale data from lazily sanitized pages. Concurrency swept 1→32; 32 is a harness limit, not a system limit.

**Snapshot-format implications (what an architect should take):** (a) never store zero pages — ~83% of the image is waste; (b) split the format by *hotness*, not by file offset, and pre-install the hot ~5% synchronously before resume while demand-paging cold pages asynchronously; (c) a compact non-file-backed format **precludes `mmap`-based page serving** and forces a copy-based install path (`uffd.copy()` / `uffd.zeropage()`); (d) hot-page fragmentation means the format needs an explicit index, not contiguous ranges; (e) CXL 2.0 MHDs give no inter-host coherence (only CXL 3.0 specifies it, and full HW coherence is argued economically unrealistic), so cross-host snapshot sharing needs a software ownership protocol.

---

## 2. Nanvix (arXiv:2604.11669, submitted 2026-04-13)

Segarra, Penna, Saurez, Goiri, Pietzuch, Lu, Fonseca (Imperial/Microsoft). https://arxiv.org/abs/2604.11669

**Snapshot reuse safety across tenants — verbatim:** *"For a snapshot to be safely re-used across tenants it must include neither tenant code nor system state that may have been influenced by the execution of tenant code like the GC or JIT compiler state in a managed language runtime such as Python, Java, or .NET."* Also: *"the guest OS must reinitialize the entropy pool and other sources of randomness after a restore."* Notes current practice: **AWS Lambda SnapStart uses per-function snapshots; Ant FaaS uses per-tenant snapshots.** Nanvix's escape hatch is that its *system VM* contains no tenant code and has a generic image, so it can be restored from **one generic snapshot shared across all tenants**.

**Isolation constraint stated as a design premise — verbatim:** *"The hypervisor cannot allocate applications from different tenants in sibling cores that share micro-architectural state nor employ memory sharing techniques like KSM due to the risk of side-channel attacks."*

**Density / latency numbers:** user VM start p50 **7.26 ms** / p99 **11.37 ms** vs Firecracker-snapshot p50 **69.45 ms** / p99 **83.77 ms** (9.56× / 7.36×); standalone user VM p50 3.553 ms. **30–50% lower memory footprint → 30–50% more sandboxes per GiB** than snapshot-based baselines. Peak throughput **862 RPS vs 43.1 RPS** for Firecracker/Cloud Hypervisor. **20–100× fewer servers** — test condition: *"replay the first minute of the first region of the Huawei trace from 2025"* (chosen because it carries per-invocation tenant IDs). Hardware setup disables hyperthreading and frequency scaling.

---

## 3. PASS — USENIX ATC '24

Pang, Zhang, Liu, Cheng, Xu, Zhou, "Expeditious High-Concurrency MicroVM SnapStart in Persistent Memory with an Augmented Hypervisor," ATC'24, pp. 985–998. https://www.usenix.org/conference/atc24/presentation/pang (PDF 403'd; numbers from the USENIX/ACM abstract text, not the PDF body). [PARTIALLY UNVERIFIED — abstract-level only]

- **Problem:** frequent page faults during on-demand paging in tiered memory. Specifically a **semantic gap**: the PMEM filesystem and the microVM keep separate address-mapping tables for the snapshot memory file, so a microVM's on-demand paging triggers **expensive DAX faults**.
- **Technique:** a **PMEM-aware augmented hypervisor in user space** that closes that gap and restores memory directly from byte-addressable PMEM rather than prefetching a profiled working set into DRAM (its critique of FaaSnap: high caching overhead and I/O demand).
- **Numbers:** up to **72% lower SnapStart execution time vs Firecracker on a PMEM filesystem**; **47% vs FaaSnap**; **2× the maximum concurrency** of both; cost-effectiveness **2.2× / 1.6×** better than Firecracker / FaaSnap.
- Practical caveat for 2026 planning: Optane PMEM is EOL, so the PMEM substrate is now mostly a proxy for CXL-attached byte-addressable memory (the Aquifer thesis).

---

## 4. The restore-acceleration line

**Catalyzer (ASPLOS'20)** — sandbox fork (`sfork`) + on-demand restore from a checkpoint image; deployed at Ant Group; reports **sub-millisecond** startup. [UNVERIFIED at primary source in this pass — ASPLOS PDF not fetched.]

**REAP (ASPLOS'21, "Benchmarking, Analysis, and Optimization of Serverless Function Snapshots", arXiv:2101.09355)** — records the stable **guest-physical working set** across invocations and eagerly prefetches it from disk on restore; **3.7× lower cold-start latency** vs vanilla Firecracker snapshot-restore. Synchronous prefetch → minimal major faults but a fixed up-front delay. https://arxiv.org/abs/2101.09355

**FaaSnap (EuroSys'22)** — better working-set estimation + **asynchronous** prefetch + a **layered memory-mapping** scheme that converts major faults on zero-initialized pages into minor faults. Per Aquifer's re-implementation, FaaSnap prefetches only non-zero working-set pages, cutting RDMA reads vs REAP.

**Newer in this line (2024–2026):**
- **Spice, "Taming Serverless Cold Starts Through OS Co-Design"** (arXiv:2509.14292, Sept 2025). Argues the bottleneck is **OS memory primitives, not storage speed**, and that *minor* faults — which neither REAP nor FaaSnap fix — dominate. Restores kernel state without replay, adds primitives for restoring memory mappings. **14.9× vs state-of-the-art process-based systems, 10.6× vs VM-based**, claiming near-warm performance on cold restores *from disk*. https://arxiv.org/abs/2509.14292
- **Aquifer (2026)** — see §1; the disaggregated-memory branch.
- **Snapipeline** (ACM 10.1145/3698038.3698513) pipelines snapshot decompression with function execution; **Sabre** uses Intel IAA to accelerate snapshot (de)compression — both cited as related work by Aquifer.
- **"Restoring Uniqueness in MicroVM Snapshots"** (arXiv:2102.12892, Brooker, Catangiu, Danilov, Graf, MacCarthaigh, Sandu, Feb 2021) — the AWS-authored statement of the *correctness/security* half of snapshot reuse: restored clones must regain uniqueness for UUIDs, secrets, nonces, and high-value memory must be scrubbed. Proposes **`MADV_WIPEONSUSPEND`** and **`SysGenId`**. https://arxiv.org/abs/2102.12892 — the paper to cite for "why you cannot just fan out one snapshot."

**On-demand container loading in AWS Lambda (ATC'23, Brooker et al., arXiv:2305.13162)** — the production numbers:
- **512 KiB chunks**, flattened image, SHA-256 of plaintext → AES-CTR key (**convergent encryption**, Farsite-style), deterministic all-zero IV; manifest AES-GCM'd under a per-customer KMS key; manifest < **3 MiB for a 16 GiB image (0.02% overhead)**.
- **Dedup:** ~**80% of newly uploaded functions produce zero unique chunks** (pure CI/CD re-uploads). Of the remaining 20%, **mean 4.3% unique chunks, median 2.5%**. Dedup reduces storage **as much as 23×**, and deduplicating the trivially-identical 80% saves **another 5×**.
- **Tiered cache hit rates over one week in a large AZ:** on-worker L1 median **67%**, in-AZ L2 median **99.9%** (p10 low **99.4%**), backing store L3 **0.06%**. Chunk source split: **67% L1 / 32% L2 / 0.06% S3**.
- **Latency:** in-AZ cache hit median **550 µs**, p99.9 **3.7 ms**; S3 origin fetch median **36 ms**, p99.9 **175 ms**. One deployment measured client-side median **500 µs**, p99.9 **4 ms**.
- **Erasure coding 4-of-5** in production: 25% storage overhead, 25% more requests, in exchange for tail-latency collapse.
- **Security engineering to copy:** a **rotating salt** in convergent key derivation, varying by time, chunk popularity, and infrastructure placement (AZ/DC), to bound blast radius and hot-spotting; no compression before encryption, explicitly to avoid a **compression side channel**; generational GC with `active → retired → expired (alarm on access) → deleted` roots.

---

## 5. KSM / memory-dedup side channels

- Classic line: dedup-timing attacks reveal co-resident page contents via COW write-latency. Lindemann & Fischer (ACM SAC'18) detected **sshd/apache2 on Linux and IE6/Firefox on Windows XP** in a co-resident KVM guest purely from KSM write timing. https://svs.informatik.uni-hamburg.de/publications/2018/2018-04-10-Lindemann-Memory-Deduplication-Side-Channel.pdf
- Recent (2023): "Exploiting Memory Page Management in KSM for Remote Memory Deduplication Attack" (Springer, 10.1007/978-981-99-8024-6_19) — exploits KSM's **max-shared-pages cap** to make the attack work *remotely*, not just from a co-resident VM.
- Recent defense (2024): **Jens Lindemann, "Faking deduplication to prevent timing side-channel attacks on memory deduplication," ACSAC 2024, pp. 380–392.** https://svs.informatik.uni-hamburg.de/publications/2024/Lindemann_ACSAC2024_FakeDD.pdf — mitigates by making non-deduplicated writes *look* deduplicated, closing the timing distinguisher.
- **Current guidance is unambiguous: do not enable KSM across tenants.** Firecracker's production doc says to disable KSM "to mitigate side channel issues that rely on page deduplication": https://github.com/firecracker-microvm/firecracker/blob/main/docs/prod-host-setup.md. Nanvix (2026) restates it as a hard architectural constraint. Practical read: dedup savings must come from *format-level* dedup on immutable, provider-authored content (Lambda's chunk store, Aquifer's zero-page elision, Nanvix's generic system-VM snapshot) — never from host-level opportunistic KSM across tenant boundaries.

---

## 6. Microarchitectural side channels 2024–2026

- **Training Solo** (VUSec, IEEE S&P 2025) — three classes of **self-training Spectre-v2** attacks that bypass *even perfect* domain isolation without crossing a privilege boundary, re-enabling Spectre-v2 within the victim domain; **up to 17 KB/s** kernel-memory leak on recent Intel CPUs. Explicit cloud framing: a guest VM leaking from the hypervisor, and thereby from other tenants. https://www.vusec.net/projects/training-solo/ ; paper https://download.vusec.net/papers/trainingsolo_sp25.pdf
- **Branch Privilege Injection (BPI)** (ETH Zurich COMSEC, May 2025) — injects branch predictions tagged with elevated privilege from user mode, **defeating eIBRS and IBPB** guarantees; Intel shipped microcode. Coverage: https://www.theregister.com/security/2025/05/13/intel-data-leaking-spectre-defenses-scared-off-once-again/
- **VMScape (CVE-2025-40300, ETH Zurich COMSEC, Sept 2025)** — the most architecturally significant for a microVM platform: the **first practical guest-to-host Spectre-BTI against an *unmodified* hypervisor in a default cloud config** (KVM/QEMU). Affects **all AMD Zen 1–5** and Intel Coffee Lake. **32 B/s on Zen 4; disk-encryption keys extracted in ~18 min.** Linux mitigation = **IBPB on VM exit**; reported overhead **~1% on compute-bound, up to ~51% on I/O-heavy** workloads — a direct tax on I/O-chatty microVM fleets. https://comsec.ethz.ch/research/microarch/vmscape-exposing-and-exploiting-incomplete-branch-predictor-isolation-in-cloud-environments/
- **Januscape (CVE-2026-53359, disclosed/patched July 2026)** — not a side channel but the current top KVM risk: a **16-year-old use-after-free in KVM x86 shadow MMU** (`kvm_mmu_get_child_sp()` reused shadow pages on GFN match alone → role confusion → host memory corruption). **First KVM guest-to-host escape working on both Intel and AMD.** Found via Google kvmCTF ($250K). Fixed kernels shipped 2026-07-04: 7.1.3, 6.18.38, 6.12.95, 6.6.144, 6.1.177, 5.15.211, 5.10.260. A companion earlier fix, **CVE-2026-46113**, covers a different hole in the same MMU code — **you need both**. https://thehackernews.com/2026/07/16-year-old-linux-kvm-flaw-lets-guest.html ; https://corgea.com/research/cve-2026-53359-januscape-linux-kvm-vm-escape
- **SMT / co-tenancy policy in practice.** Firecracker's production host doc: *"SMT is frequently a precondition for speculation issues utilized in side channel attacks such as Spectre variants, MDS… disable SMT in production scenarios that require tenant separation."* Same doc mandates KPTI, L1TF, SSBD mitigations, Rowhammer-resistant DRAM, and no plaintext swap. https://github.com/firecracker-microvm/firecracker/blob/main/docs/prod-host-setup.md — the closest thing to an authoritative AWS statement found; **no public AWS document states Lambda's exact hyperthread-scheduling policy** (searched Brooker's "Seven Years of Firecracker" 2025 post, Lambda tenant-isolation docs, re:Post — none address it). Treat "Lambda never co-schedules different tenants on sibling hyperthreads" as [UNVERIFIED] in public sources; the *recommendation* to disable SMT is verified.
- **Google:** GKE exposes SMT configurability explicitly framed as mitigating MDS-class "threads sharing core state" attacks, and since GKE 1.25.5-gke.2500 / 1.26.0-gke.2500 **gVisor is configured to use Linux core scheduling** to mitigate side channels. https://docs.cloud.google.com/kubernetes-engine/docs/how-to/configure-smt ; https://docs.cloud.google.com/kubernetes-engine/docs/concepts/sandbox-pods — i.e. the mainstream alternative to SMT-off is **core scheduling with the tenant as the cookie**, which keeps SMT throughput while forbidding cross-tenant sibling co-residency.

---

## 7. Ballooning / overcommit for microVM fleets (2024–2026)

- **HotMem** (arXiv:2411.12893, Nov 2024; Lagkas Nikolos, Alverti, Psomadakis, Goumas, Koziris, NTUA). Diagnoses why the standard knobs fail at serverless churn: **virtio-balloon** has long inflation latencies because it triggers guest-OS allocations that delay reclamation; **virtio-mem** still needs *multiple seconds* to resize a VM after tens of function instances are recycled, because process footprints interleave across memory blocks so almost every block is partially occupied and requires page migration + offlining before unplug. HotMem adds a new guest zone (`ZONE_HotMem`) partitioning guest memory into fixed-size chunks, one per function instance, so unplug can drop **entirely empty** partitions instantly. Results: **order-of-magnitude faster reclamation than vanilla virtio-mem, 7× higher reclamation throughput** under real traces; removes the **>100% latency spikes** vanilla mechanisms cause on co-located instances during unplug. https://arxiv.org/html/2411.12893v1
- **Free page reporting** (virtio-balloon extension) is the passive complement — guest reports free pages so the host can `MADV_DONTNEED` them, enabling total-guest-RAM > physical-RAM. Firecracker's ballooning doc covers the overcommit interaction (`vm.overcommit_memory=1`). https://github.com/firecracker-microvm/firecracker/blob/main/docs/ballooning.md
- Cloud Hypervisor documents the balloon / virtio-mem / free-page-reporting trade space. https://www.cloudhypervisor.org/blog/memory-management-techniques/
- **DAMON/DAMOS-based reclaim for microVM fleets: no 2024–2026 paper found.** [UNVERIFIED gap.] Nearest adjacency is Aquifer's hotness classification via `userfaultfd` rather than DAMON.

---

## Load-bearing sources

1. Aquifer, arXiv:2606.24079 (2026-06-23) — https://arxiv.org/abs/2606.24079 (numbers read from the PDF body, pp. 2–5, 10–12)
2. Nanvix, arXiv:2604.11669 (2026-04-13) — https://arxiv.org/abs/2604.11669
3. PASS, USENIX ATC'24 (July 2024) — https://www.usenix.org/conference/atc24/presentation/pang
4. Spice, arXiv:2509.14292 (Sept 2025) — https://arxiv.org/abs/2509.14292
5. REAP, arXiv:2101.09355 (ASPLOS'21) — https://arxiv.org/abs/2101.09355
6. Restoring Uniqueness in MicroVM Snapshots, arXiv:2102.12892 (2021-02-04) — https://arxiv.org/abs/2102.12892
7. On-demand Container Loading in AWS Lambda, arXiv:2305.13162 / ATC'23 best paper — https://arxiv.org/pdf/2305.13162
8. Lindemann, "Faking deduplication…", ACSAC 2024 — https://svs.informatik.uni-hamburg.de/publications/2024/Lindemann_ACSAC2024_FakeDD.pdf
9. Lindemann & Fischer, memory-dedup side channel, SAC'18 — https://svs.informatik.uni-hamburg.de/publications/2018/2018-04-10-Lindemann-Memory-Deduplication-Side-Channel.pdf
10. Training Solo, IEEE S&P 2025 — https://www.vusec.net/projects/training-solo/ and https://download.vusec.net/papers/trainingsolo_sp25.pdf
11. VMScape (CVE-2025-40300), ETH Zurich COMSEC, Sept 2025 — https://comsec.ethz.ch/research/microarch/vmscape-exposing-and-exploiting-incomplete-branch-predictor-isolation-in-cloud-environments/
12. Januscape (CVE-2026-53359), July 2026 — https://thehackernews.com/2026/07/16-year-old-linux-kvm-flaw-lets-guest.html
13. Firecracker production host setup (SMT off, KSM off) — https://github.com/firecracker-microvm/firecracker/blob/main/docs/prod-host-setup.md
14. GKE SMT configuration + gVisor core scheduling — https://docs.cloud.google.com/kubernetes-engine/docs/how-to/configure-smt and https://docs.cloud.google.com/kubernetes-engine/docs/concepts/sandbox-pods
15. HotMem, arXiv:2411.12893 (Nov 2024) — https://arxiv.org/html/2411.12893v1

**Caveats.** (a) Aquifer's CXL is *emulated* on a remote NUMA node; there is no local-NVMe baseline and no direct CXL/RDMA/NVMe latency table — the gap is only observable indirectly via the 0.65 ms vs 64–121 ms `uffd.copy()` figure. (b) PASS numbers are abstract-level only; usenix.org and dl.acm.org both returned 403. (c) Catalyzer's sub-ms claim is not primary-source verified here. (d) Nanvix's 20–100× is a *simulation over one minute of one region* of a 2025 Huawei trace, not a deployed measurement. (e) No public AWS statement found on Lambda's specific hyperthread co-tenancy policy.
