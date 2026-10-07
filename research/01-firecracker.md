# Firecracker microVM — state as of 2026-08-13

*Research report 1 of 6 for the Substrate architecture plan. Produced 2026-08-13 by a dedicated research pass over primary sources (GitHub releases API, Firecracker docs on `main`, AWS blogs). Every claim carries a URL and date; [UNVERIFIED] marks what could not be traced to a primary source.*

## 1. Releases & feature status

Latest: **v1.16.1, published 2026-07-02** (`api.github.com/repos/firecracker-microvm/firecracker/releases`). Cadence ~every 2–3 months. Dates below are `published_at` from the GitHub Releases API; feature lines from https://raw.githubusercontent.com/firecracker-microvm/firecracker/main/CHANGELOG.md (fetched 2026-08-13).

| Ver | Date | Load-bearing items |
|---|---|---|
| main (Unreleased) | — | **THP option** (not yet in a release), guest kernel 6.18, bzImage support, virtio device reset, vsock busy-spin fix (**+44% host→guest throughput**) |
| v1.16.1 | 2026-07-02 | Reverted jailer `O_NOFOLLOW` (symlinks allowed again); vsock g2h timeout after restore |
| v1.16.0 | 2026-06-04 | **PCI virtio device hotplug/unplug (developer preview)**; vsock UDS path override on restore; serial/log rate limiting; `VIRTIO_NET_F_MTU`; aarch64 `rng-seed` FDT node; host kernel 6.18. Fixes: virtio-rng entropy capped 64 KiB, VMGenID HID alignment, guest UART stuck after snapshot, kvm-clock restore clock jump, PCI init-sequence validation (**CVE-2026-5747**) |
| v1.15.1 / v1.14.4 | 2026-04-07 | security backports of the above |
| v1.15.0 | 2026-03-09 | **VMClock device** ("snapshot safety features"); Intel Granite Rapids. Costs a GSI → max **92 devices aarch64 / 17 x86** |
| v1.14.0 | 2025-12-17 | **virtio-pmem**; **virtio-mem memory hotplug**; **balloon free page reporting (GA) + free page hinting (dev preview)**; `/serial` endpoint |
| v1.13.0 | 2025-08-28 | **Snapshots moved developer-preview → generally available** (incremental/diff stay preview); **PCI support behind `--enable-pci`**; diff snapshots without dirty-page tracking via `mincore(2)`; deprecated `enable_diff_snapshots`; dropped Skylake |
| v1.12.0 | 2025-05-07 | **PVH boot mode**; Intel AMX; TAP rename on restore; Sapphire Rapids + Graviton4 |
| v1.11.0 | 2025-03-18 | "replaced seccompiler implementation with libseccomp"; AMD Genoa |
| v1.10.0 | 2024-11-07 | `VIRTIO_NET_F_RX_MRGBUF`; net RX via `readv`; GDB debugging; dropped guest 4.14 |
| v1.9.0 | (date [UNVERIFIED], ~2024) | **VMGenID for ARM**; guest 6.1 official |
| v1.8.0 | 2024-07-10 | **ACPI for x86_64**; **VMGenID device x86_64**; deprecated MPTable boot |
| v1.7.0 | (~Jan 2024, [UNVERIFIED]) | **2M hugetlbfs backing (dev preview)**; snapshot format versioning v1.0.0 |
| v1.6.0 | (~Nov 2023, [UNVERIFIED]) | **vhost-user-blk (dev preview)**; `snapshot-editor` |

Per-feature status:
- **Snapshots**: full = GA since v1.13.0. **Diff snapshots still developer preview** — docs/snapshotting/snapshot-support.md: "Diff snapshot support is in developer preview", team is "diving deep into how the feature can be combined with `guest_memfd` support in Firecracker."
- **UFFD restore**: supported and documented (`backend_type: "Uffd"`), docs/snapshotting/handling-page-faults-on-snapshot-resume.md.
- **io_uring**: block `Async` engine is **still developer preview**, requires host kernel ≥5.10.51; docs/api_requests/block-io-engine.md claims read workloads "1.5x-3x in overall efficiency (IOPS per CPU load) and up to 30x in total IOPS" (NVMe, data not in page cache), writes +20–45% IOPS at higher CPU, ~110 ms device-creation penalty.
- **Huge pages**: 2M hugetlbfs; guest memory must be a multiple of 2 MB; **hugepage snapshots can only be restored via UFFD**; needs a pre-allocated 2M pool or you risk SIGBUS; balloon cannot reclaim hugepage backing; dirty-page tracking negates the benefit (docs/hugepages.md).
- **Ballooning**: GA; `deflate_on_oom`, stats polling (needs guest ≥6.12); free page reporting GA, **hinting preview with a documented memory-corruption race** ("the guest is able to reclaim memory before Firecracker even receives the range to free… potentially corrupting memory"; mitigate with UFFD WRITEPROTECT) — docs/ballooning.md.
- **PVH**: used when the kernel has `XEN_ELFNOTE_PHYS32_ENTRY`; **not default for Linux** (needs `CONFIG_PVH=y`); default on FreeBSD ≥14.0. No boot-time delta published (docs/pvh.md).
- **vhost-user**: block only, still developer preview since v1.6.0. **No vhost-user-net / vhost-net** found in changelog or docs listing.
- **Network**: virtio-net only (mergeable RX bufs, `readv`, MTU advertisement); PCI transport optional since v1.13.0 and hotplug preview in v1.16.0.
- **Secret hiding**: not in any release. Lives on branch `feature/secret-hiding`; active PRs through **2026-08-12** (#6108 "SH: port patches to AL 6.18"; #6106 "skip secret freedom with huge pages… not compatible"). "Secret freedom/hiding" = **direct-map removal for `guest_memfd`** — kernel series by Patrick Roy (AWS), v4 posted **2025-02-21**, v7 Sept 2025, v11 March 2026, **not merged in anything seen** [merge status beyond v11 UNVERIFIED]. Rationale: unmapping guest memory from the host direct map blocks Spectre-style transient reads.

## 2. Snapshot restore latency (numbers + conditions)

- **Firecracker itself publishes no headline restore-latency figure.** snapshot-support.md gives only a warning: "High snapshot restoration latency when cgroups V1 are in use" → use cgroups v2. Verified by reading the doc.
- **AWS Lambda SnapStart** (https://aws.amazon.com/blogs/compute/under-the-hood-how-aws-lambda-snapstart-optimizes-function-startup-latency/, **2025-08-19**, Kulkarni & Heinz): snapshots split into **512 KB chunks**; **L1 (worker-node) cache ≈ 1 ms per 512 KB chunk**; **L2 (distributed) single-digit ms per chunk**; **S3 up to hundreds of ms per chunk**. Loading is lazy + prefetched: Lambda records chunk access patterns and "proactively retrieves and loads this 'working set' of chunks before they are needed." Snapshots immutable, encrypted, one copy per AZ. End-to-end startup "as low as sub-second."
- **E2B, first-party** (https://docs.e2b.dev/sandbox/persistence, undated page, fetched 2026-08-13): "Pausing a sandbox takes approximately **4 seconds per 1 GiB of RAM**"; "Resuming a sandbox takes approximately **1 second**." This **contradicts** widely repeated third-party "5–30 ms E2B resume" claims (blaxel.ai, zenml.io) — treat those as [UNVERIFIED/wrong].
- **CodeSandbox** [UNVERIFIED — both pages return HTTP 403 to WebFetch; numbers via search snippets]: "How we clone a running VM in 2 seconds" (2022, **possibly stale**) — `create_snapshot` writes memory at **~1 s/GB**, so a 12 GB VM = 12 s. "Cloning microVMs by sharing memory through userfaultfd" (**2023-05-09**, possibly stale) — fork "within **1.5 s**, regardless of how much memory," via UFFD + CoW on a **forked Firecracker**.
- **Hocus** (https://hocus.dev/blog/qemu-vs-firecracker/, ~2023–24, **possibly stale**) replaced Firecracker with QEMU for long-lived dev workspaces: no dynamic RAM management, no GPU, poor multi-disk IO. Relevant negative signal for a long-lived-sandbox product.
- Modal: **not covered** — Firecracker use not confirmed; no claim made.

## 3. Boot time and memory overhead

From https://raw.githubusercontent.com/firecracker-microvm/firecracker/main/SPECIFICATION.md (fetched 2026-08-13, still the live claim):
- "It takes `<= 125 ms` to go from receiving the Firecracker InstanceStart API call to the start of the Linux guest user-space `/sbin/init` process" — **conditions: serial console disabled, minimal kernel and root filesystem.**
- VMM thread memory overhead **`<= 5 MiB`** for a **1 vCPU / 128 MiB** microVM; excludes MMDS data store; workload-dependent (many vsock connections can exceed it).
- Also: guest compute >95% of bare metal; network up to 14.5 Gbps at ≤80% host CPU / 25 Gbps at 100%, +~0.06 ms latency; storage up to 1 GiB/s at ≤70% host CPU.
- **Stale test bed**: SPECIFICATION cites **M5D.metal and M6G.metal, hyperthreading disabled** — two+ generations behind the README's current tested-platform table (m5n.metal … m8g.metal-48xl; Cascade Lake/Ice Lake/Sapphire Rapids/Granite Rapids, Milan/Genoa, Graviton2–4; hosts AL2 5.10, AL2023 6.1/6.18; guests Ubuntu 24.04 + 5.10/6.1). So the numbers are *current claims* on *old hardware*.
- Best documented boot-time lever: **2M hugetlbfs cuts boot time "by up to 50% as measured by Firecracker's boot time performance tests"** (docs/hugepages.md).
- The often-quoted "150 microVMs/second/host" is from the NSDI'20 paper, **not** SPECIFICATION.md — [UNVERIFIED here, and possibly stale].

## 4. Snapshot safety across tenants

- snapshot-support.md, blunt: "Without a strong mechanism that enables users to guarantee that unique things stay unique across snapshot restores, we consider **resuming execution from the same state more than once insecure**." Risk set: "Unique identifiers, random numbers and random number seeds, the guest OS entropy pool, as well as cryptographic tokens." In a multi-resume scenario both VMs are insecure "as soon as microVM A resumes execution." **Sanctioned pattern: boot → snapshot → terminate original → restore exactly once.** A multi-tenant fork-from-golden-snapshot design is explicitly outside Firecracker's supported security model.
- **VMGenID**: "Firecracker will always enable the device"; 16-byte ID regenerated on resume; **Linux ≥5.18 (ACPI) / ≥6.10 (DeviceTree)** reseeds the CSPRNG. Caveats: a **race window between resume and reseed**, and "State other than the guest kernel entropy pool… will still be replicated."
- docs/snapshotting/random-for-clones.md user obligations: pre-snapshot delete `/var/lib/systemd/random-seed` (and optionally bind-mount `/proc/sys/kernel/random/boot_id`); on kernels <5.18 issue `RNDADDENTROPY` + `RNDRESEEDCRNG` post-resume; attach **virtio-rng**; userspace PRNGs (OpenSSL, numpy) keep their own pools and are **your** problem.
- **arXiv 2102.12892, "Restoring Uniqueness in MicroVM Snapshots"**, Brooker, Catangiu, Danilov, Graf, MacCarthaigh, Sandu — **2021-02-04 (possibly stale)**. Proposes two interfaces: **`MADV_WIPEONSUSPEND`** (wipe high-value memory, e.g. crypto secrets, across snapshot) and **`SysGenId`** (a VMGenID-comparable clone-detection mechanism).
  - **Shipped**: the SysGenId/VMGenID line landed as the **VMGenID device (x86_64, v1.8.0, 2024-07-10; ARM, v1.9.0)**, plus **VMClock (v1.15.0, 2026-03-09)** and the **aarch64 rng-seed FDT node (v1.16.0)**.
  - **Not shipped**: **no evidence of `MADV_WIPEONSUSPEND`** in the Firecracker changelog or docs. The memory-secrecy half of the paper appears superseded by the `guest_memfd` / secret-hiding branch work, which is still unmerged. [UNVERIFIED whether SysGenId was formally rejected upstream in favour of the vmgenid driver.]

## 5. "ZeroBoot" — it exists, with caveats

- **zeroboot**, Adam Miribyan, Apache-2.0. Show HN **2026-03-17**, 311 pts / 72 comments (https://news.ycombinator.com/item?id=47412569; hn.algolia.com objectID 47412569). Repos: github.com/adammiribyan/zeroboot, mirror github.com/zerobootdev/zeroboot.
- **Claims (README, fetched 2026-08-13):** spawn **p50 0.79 ms**, **p99 1.74 ms**; **~265 KB memory per sandbox**; **fork + exec Python ~8 ms**; **1000 concurrent forks in 815 ms**. Comparison column: E2B ~150 ms, microsandbox ~200 ms, Daytona ~27 ms.
- **Mechanism:** `Firecracker snapshot ──► mmap(MAP_PRIVATE) ──► KVM VM + restored CPU state`. Firecracker is used **once**, as the snapshot *producer*; at fork time a **custom KVM loader** maps the snapshot memory `MAP_PRIVATE` (kernel provides CoW) and restores vCPU state. **Firecracker is not in the hot path.** Author reports ~93% of pages stay shared across forks; per-fork RSS grows to 3–27 MB with workload.
- **Does it hold up?** Mechanism is plausible and consistent with the CoW/UFFD prior art. But: **no test hardware or host conditions disclosed anywhere** → the 0.79 ms is not reproducible as stated. The number measures VM-create + CPU-state restore, **not workload readiness** (their own fork+exec Python is ~8 ms). README p99 1.74 ms vs author's HN comment "P99 1.3 ms under 1000 concurrent" — unreconciled discrepancy.
- **Author-stated limits:** forks share CSPRNG state (kernel reseeded via `RNDADDENTROPY`, userspace PRNGs not); single vCPU; **no networking**, serial I/O only; template update = full re-snapshot ~15 s; "working prototype… not production-hardened."
- **HN critique:** cperciva flagged identical RNG state across forks (author: on the roadmap); ASLR replication also raised. This is **exactly** the failure mode Firecracker's own docs (§4) call insecure — a fork-from-one-snapshot multi-tenant design is the unsupported case.
- **Other sub-10 ms claims 2025–26:** a dev.to post claims 28 ms spawns via Firecracker snapshot + CoW copy [UNVERIFIED, hobbyist]. **Warning:** several SEO/AI-content sites (pandastack.ai, techbytes.app) assert "Lambda SnapStart p50 3.2 ms / p99 8.7 ms" — traceable to **no AWS primary source**; the real AWS post gives per-chunk figures only. Do not use those numbers.

## 6. Jailer and seccomp

**Jailer** (docs/jailer.md): chroot at `<chroot_base>/<exec_file_name>/<id>/root`, **copying the Firecracker binary in** so instances don't share text pages; mount namespace via `unshare()` + `pivot_root()`; optional network namespace via `--netns`; optional new PID namespace via `--new-pid-ns` (Firecracker becomes `init(1)`); **cgroups v1 or v2** via `--cgroup-version` (creates subfolders, writes pid + limits); drops privileges via supplied **uid/gid** before `exec`; `setrlimit()` on file size and FD count (**default 2048 FDs**); mknods `/dev/net/tun` and `/dev/kvm`. **The jailer doc does not describe seccomp or capability dropping** — seccomp is installed by Firecracker itself. Note v1.14.1/v1.13.2 (2026-01-20) added symlink restrictions to the jailer binary copy; v1.16.1 partially reverted `O_NOFOLLOW`.

**Seccomp** (docs/seccomp.md, docs/seccompiler.md): **on by default**, **allowlist** ("the bare minimum set of system calls and parameters that Firecracker needs"), installed **per thread** (VMM, API, vCPU) right before running guest code. `--seccomp-filter <path>` for custom filters (**discouraged**: "filter misconfiguration can result in abruptly terminating the process or disabling the seccomp security boundary altogether"); `--no-seccomp` is dev-only. Toolchain is **seccompiler-bin**, compiling per-arch JSON in `resources/seccomp` into serialized BPF embedded at build (or load) time. The v1.11.0 changelog line "replaced seccompiler implementation with libseccomp" refers to seccompiler's **internal backend**; docs/seccompiler.md (current, main) **never mentions libseccomp** and the JSON→BPF interface is unchanged. Filters are not enumerated in docs.

## Most load-bearing sources

1. https://api.github.com/repos/firecracker-microvm/firecracker/releases — authoritative version/date pairs; **v1.16.1 = 2026-07-02**.
2. https://raw.githubusercontent.com/firecracker-microvm/firecracker/main/CHANGELOG.md (fetched 2026-08-13) — full feature history; THP still `[Unreleased]`.
3. https://api.github.com/repos/firecracker-microvm/firecracker/releases/tags/v1.13.0 (2025-08-28) — snapshots GA, diff stays preview, `--enable-pci`.
4. .../main/docs/snapshotting/snapshot-support.md — diff-snapshot preview + `guest_memfd`; "resuming… more than once insecure"; cgroups v1 latency warning.
5. .../main/docs/snapshotting/random-for-clones.md — VMGenID, kernel version gates, pre/post-snapshot user obligations.
6. .../main/SPECIFICATION.md — the live `<=125 ms` and `<=5 MiB` claims + their conditions and (stale) M5D/M6G test bed.
7. https://aws.amazon.com/blogs/compute/under-the-hood-how-aws-lambda-snapstart-optimizes-function-startup-latency/ (2025-08-19) — only rigorous first-party restore-latency data: 512 KB chunks, L1 ~1 ms, L2 single-digit ms, S3 hundreds of ms, working-set prefetch.
8. https://docs.e2b.dev/sandbox/persistence (fetched 2026-08-13) — E2B first-party: pause ~4 s/GiB, **resume ~1 s** — refutes third-party "5–30 ms."
9. https://arxiv.org/abs/2102.12892 (2021-02-04, possibly stale) — `MADV_WIPEONSUSPEND` + `SysGenId`; only the latter has shipped.
10. https://github.com/adammiribyan/zeroboot + https://news.ycombinator.com/item?id=47412569 (2026-03-17) — ZeroBoot 0.79 ms p50 / 265 KB, custom KVM loader not Firecracker at runtime, no hardware disclosed, shared CSPRNG.
11. .../main/docs/jailer.md and .../main/docs/seccomp.md + seccompiler.md — isolation and syscall posture.
12. https://hocus.dev/blog/qemu-vs-firecracker/ (possibly stale) — documented reasons a long-lived-workspace product abandoned Firecracker.

**Caveats:** dates for v1.6.0/v1.7.0/v1.9.0 unverified. CodeSandbox pages 403 to WebFetch; their numbers come from search snippets and are 2022/2023 (possibly stale). Secret-hiding kernel merge status beyond v11 (2026-03) unverified. SEO-farm "SnapStart p50 3.2 ms" figures are untraceable — do not use. Modal not investigated in this pass.
