# Nested Virtualization & Hardware Economics — as of 2026-08-13

*Research report 3 of 6 for the Substrate architecture plan. All prices checked 2026-08-13. AWS instance prices are from **instances.vantage.sh (third-party aggregator)**, Linux on-demand, us-east-1, unless noted. Monthly = hourly × 730.*

## 1. AWS — nested virt is no longer metal-only

**This changed in Feb 2026.** Nested virtualization now works on *virtual* (non-metal) EC2 instances.

- Launch announcement 2026-02-16: C8i, M8i, R8i, all commercial regions, KVM and Hyper-V as L1, **no additional cost** — https://aws.amazon.com/about-aws/whats-new/2026/02/amazon-ec2-nested-virtualization-on-virtual/
- **The docs list has since expanded well beyond the announcement** (this is itself a finding): *"C8i, M8i, R8i, C8id, R8id, M8id, C8i-flex, R8i-flex, M8i-flex, X8i, C7i, R7i, M7i, C7i-flex, M7i-flex, and I7i"* — https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/amazon-ec2-nested-virtualization.html
- **Intel-only. No Graviton, no AMD (no C7a/M7a/R7a, no C8g/M8g).**
- Opt-in per instance: `--cpu-options "NestedVirtualization=enabled"` at launch, or `modify-instance-cpu-options` on a **stopped** instance (same doc).
- AWS's own guidance: *"customers who want to run workloads that require access to hardware virtualization extensions, and are performance sensitive or have strict latency requirements, [should] evaluate bare metal instances."*
- Windows caveats: VSM/Credential Guard auto-disabled; hibernate unsupported; unsupported >192 CPUs.
- Smallest working size reported as `.large`; Firecracker microVMs confirmed booting on nested EC2 — https://dev.classmethod.jp/en/articles/ec2-nested-virtualization-support-non-bare-metal/ (2026-02)
- **Graviton metal**: `/dev/kvm` is exposed on `.metal` only; Firecracker supports aarch64 and runs on Graviton `.metal`. Graviton *virtual* instances have no nested virt. — https://github.com/firecracker-microvm/firecracker/blob/main/docs/getting-started.md

### AWS metal & nested-capable options (us-east-1)

| Instance | vCPU | RAM GiB | OD $/hr | OD $/mo | **$/GB-RAM/mo** | 1yr RI eff. $/hr |
|---|---|---|---|---|---|---|
| m7i.metal-24xl | 96 | 384 | 4.8384 | 3,532 | **9.20** | 3.201 NU / 2.195 AU |
| m7i.metal-48xl | 192 | 768 | 9.6768 | 7,064 | **9.20** | — |
| c7i.metal-48xl | 192 | 384 | 8.568 | 6,255 | **16.29** | 5.668 |
| c7a.metal-48xl (AMD) | 192 | 384 | 9.85 | 7,190 | **18.72** | 6.52 |
| r7i.metal-48xl | 192 | 1536 | 12.70 | 9,271 | **6.04** | 8.40 → $3.99/GB |
| i7i.metal-48xl (3.75TB NVMe) | 192 | 1536 | 18.121 | 13,228 | **8.61** | — |
| m8g.metal-48xl (Graviton4) | 192 | 768 | 8.62 | 6,293 | **8.19** | 5.70 → $5.42/GB |
| m8i.48xlarge (**virtual, nested-capable**) | 192 | 768 | 10.16 | 7,417 | **9.66** | n/a |

m7i.metal-24xl 1yr RI: no-upfront $3.201/hr → **$6.09/GB-mo**; all-upfront $2.195/hr → **$4.17/GB-mo**.

Note: a full-size nested-capable *virtual* m8i.48xlarge ($10.16/hr) costs **more** than m7i.metal-48xl ($9.68/hr) for the same 192/768. Nested virt's economic win is at *small* sizes (m8i.large), not at density.

## 2. Azure

- **Nested virt IS supported on AMD.** Dasv5 (AMD EPYC 7763v Milan / EPYC 9004 Genoa) feature-support table: `Nested Virtualization | Supported`. Sizes D2as_v5 (2/8) → D96as_v5 (96/384). Doc `ms.date` 2026-03-10, updated 2026-07-27 — https://learn.microsoft.com/en-us/azure/virtual-machines/sizes/general-purpose/dasv5-series
- The authoritative test is the **per-size-series "Feature support" table** on learn.microsoft.com, not a global list. Community summaries say Dv4/Dv5, Ev4/Ev5, Fsv2 and parts of M-series are capable; B-series and v2/v3 are not — [UNVERIFIED as a complete list; verify per series page].
- **No published Azure nested-virt performance-penalty number found.** [UNVERIFIED]
- **Bare metal**: Azure has **no general-purpose bare metal VM SKU**. "BareMetal Infrastructure" is a separate, certification-scoped product (SAP HANA, Oracle, Nutanix NC2), 2–24 socket, 1.5–24 TB RAM — https://learn.microsoft.com/en-us/azure/baremetal-infrastructure/ . Azure Dedicated Host is *not* bare metal (still Hyper-V-mediated). For a micro-VM platform on Azure you are running nested inside Hyper-V.

## 3. GCP

- Nested virt is **excluded** on: *"E2 VMs, Memory-optimized VMs, VMs powered by AMD and Arm processors, H4D VMs."* Intel VT-x required. **Only L1 hypervisor supported is Linux KVM — Hyper-V is not.** L2 licensed OSes are BYOL. — https://docs.cloud.google.com/compute/docs/instances/nested-virtualization/overview
- **Documented penalty (vendor primary):** *"Nested VMs might experience a 10% or greater decrease in performance for workloads that are CPU-bound and possibly greater than a 10% decrease for workloads that are input/output bound."* (same page)
- **Contrast worth noting: Azure supports nested virt on AMD; GCP explicitly forbids AMD and Arm.**
- **GCP bare metal is now broad and GA** — the "GCP has no bare metal" assumption is outdated — https://docs.cloud.google.com/compute/docs/instances/bare-metal-instances:
  - C3: `c3-highcpu-192-metal` 192/512, `c3-standard-192-metal` 192/768, `c3-highmem-192-metal` 192/1536 — GA
  - C4 (Granite Rapids): `c4-standard-288-metal` 288/1080, `c4-highmem-288-metal` 288/2232 — GA
  - **C4D (AMD Turin): `c4d-standard-384-metal` 384/1488, `c4d-highmem-384-metal` 384/3072 — GA**
  - **C4A (Arm/Axion): `c4a-standard-96-metal` 96/384, `c4a-highmem-96-metal` 96/768 — GA 2026-05-28** (https://cloud.google.com/blog/products/compute/new-axion-c4a-metal-offers-bare-metal-performance-on-arm)
  - Z3 `z3-highmem-192-highlssd-metal` 192/1536 + 72TB SSD; X4 up to 1920 vCPU / 32 TB
  - Docs explicitly name "run third-party hypervisors" as the use case.
  - **Limits: single NIC only (VMs get 10), NVMe-only disks, no Windows, no live migration** (maintenance terminates the instance — significant for a multi-tenant platform), no vTPM except A4X Max/C4A.
  - GCP bare-metal $/hr: [UNVERIFIED — pricing page not retrieved].

## 4. Nested virtualization performance penalty

Hard published numbers are thin. What is citable:

- **GCP (vendor, current):** ≥10% for CPU-bound, likely worse for I/O-bound (URL above).
- **AWS (vendor, current):** no number; recommends bare metal for perf-sensitive/low-latency (URL above).
- **Mechanism:** an L2 VM exit requires **~4 world switches vs 2** for single-level virtualization.
- **HyperTurtle, USENIX ATC '25** (Ben Zur, Krebs — Technion; Bergman — Huawei Zurich; Silberstein — Technion), July 2025: offloads hot paths of the L1 hypervisor as eBPF into the L0 hypervisor; reports **5× faster EPT fault handling** and **up to 27% faster Kata-container boot**. These are *improvement* figures, not baseline overheads, but they bound how much of nested cost is world-switch/EPT-fault overhead. — https://www.usenix.org/conference/atc25/presentation/zur (PDF returns 403 to automated fetch)
- Ongoing Linux KVM nested-VMX optimization work (Sept 2025 patches) confirms the overhead is still an active problem — Phoronix, "New Linux Patches Enhance Intel Nested Virtualization Performance On Linux."
- **No AWS-specific L0=Nitro / L1=KVM / L2=Firecracker benchmark found** (searched 2026-08-13). [UNVERIFIED] — the single biggest measurement gap for the plan; benchmark in-house (Phase 0).

## 5. Bare-metal rental economics

**Hetzner raised prices three times in 2026.** Standardization + price adjustment effective **15 June 2026, 08:00 CEST**; models renamed to `-1/-2/-3` fixed configs (no more custom RAM/disk), plus `-1-Ltd` cheap-component tiers. Existing rentals grandfathered; new orders pay new prices. — https://docs.hetzner.com/general/infrastructure-and-availability/price-adjustment/ and https://www.hetzner.com/pressroom/standardization-and-price-adjustment-of-our-server-products/

New monthly prices, ex-VAT, Falkenstein/Helsinki (from the Hetzner docs table): **AX42-1 €97.30 · AX102-1 €257.30 · AX102-2 €547.30 · AX162-1 €612.30 · AX162-2 €842.30 · AX162-3 €1,597.30 · EX131-1 €557.30**.
⚠️ **The RAM/disk config behind each `-1/-2/-3` tier is [UNVERIFIED]** — the standardization FAQ did not render, and the base hardware pages are JS-only. AX162 = EPYC 9454P 48c/96t with RAM options 128–512 GB DDR5 ECC reg.; AX102 = Ryzen 9 7950X3D 16c, 128–192 GB. Also note the uniform `.30` price endings — single extraction, worth re-verifying manually.

**Headline economics finding:** AX162 launched (Jan 2024) at *"from €199.00/month + €79 setup"* (hetzner.com/pressroom/new-ax162/). The 2026 entry tier lists at **€612.30** — roughly **3× higher** (config match unverified). Hetzner's historical ~10× cost advantage over AWS metal has narrowed substantially.

**Bandwidth:** Hetzner dedicated = **1 Gbit uplink with unlimited traffic**; 10 Gbit uplink = 20 TB/mo included then **€1/TB**; only *outgoing* traffic counted — https://docs.hetzner.com/robot/general/traffic/
**AWS:** 100 GB/mo free egress aggregated; **$0.09/GB** to internet in us-east-1 (exact 10TB/40TB tier ladder [UNVERIFIED]). → 20 TB/mo egress ≈ **$1,791 on AWS vs €20 (~$23) on Hetzner** — ~78×. For a sandbox platform, egress alone can exceed the compute delta.

### Comparison table

| Provider / server | Cores | RAM GB | $/mo | **$/GB-RAM/mo** | Bandwidth |
|---|---|---|---|---|---|
| AWS m7i.metal-24xl, on-demand | 96 vCPU | 384 | 3,532 | **9.20** | $0.09/GB egress |
| AWS m7i.metal-24xl, 1yr RI no-upfront | 96 | 384 | 2,337 | **6.09** | " |
| AWS m7i.metal-24xl, 1yr RI all-upfront | 96 | 384 | 1,602 | **4.17** | " |
| AWS r7i.metal-48xl, 1yr RI | 192 | 1536 | 6,132 | **3.99** | " |
| Hetzner AX102-1 | 16c/32t | 128 (assumed) | €257.30 ≈ $301 | **~2.35** | 1 Gbit unlimited |
| Hetzner AX162-1 | 48c/96t | 128 (assumed) | €612.30 ≈ $717 | **~5.60** | 1 Gbit unlimited |
| Hetzner AX162-1, if 256 GB | 48c/96t | 256 | €612.30 ≈ $717 | **~2.80** | " |
| Hetzner AX162-3, if 512 GB | 48c/96t | 512 | €1,597.30 ≈ $1,869 | **~3.65** | " |
| Latitude.sh m3.large.x86 (EPYC 7543P) | 32 | 1024 | 938 | **0.92** | 20 TB free out |
| Latitude.sh c3.large.x86 (EPYC 7443P) | 24 | 256 | 496 | **1.94** | 20 TB free out |
| Latitude.sh m4.metal.large (EPYC 9254) | 24 | 384 | 1,482 | **3.86** | 20 TB free out |
| Latitude.sh m4.metal.medium (EPYC 9124) | 16 | 128 | 456 | **3.56** | 20 TB free out |
| OVHcloud Advance-4 | AMD EPYC | 64 | 213 | **3.33** | unmetered |

Latitude.sh: https://www.latitude.sh/pricing (rendered 2026-08-13; hourly also available, e.g. c3.large.x86 $0.68/hr). OVH Advance-4 figure is **third-party (whtop), recorded 2025-09-27** — ovhcloud.com product pages are JS-only and did not render; treat as indicative. Latitude Gen-4 EPYC parts carry **SEV** (relevant for tenant memory encryption).
**EUR→USD 1.17 [ASSUMED, unverified]** — EUR figures shown so the math survives a rate correction.

**Bottom line:** best bare-metal $/GB-RAM (Latitude m3.large.x86, $0.92) is **~10× cheaper than AWS m7i.metal on-demand** and **~4.5× cheaper than 1yr all-upfront RI**. Hetzner post-June-2026 lands ~2.4–6× cheaper than AWS on-demand depending on the unverified config — a much smaller gap than its pre-2026 reputation implies.

## 6. ARM

- **AWS Graviton metal works for KVM**: `/dev/kvm` on `.metal` only; Firecracker officially supports aarch64. m8g.metal-48xl 192/768 at $8.62/hr = **$8.19/GB-mo** on-demand, $5.42 at 1yr RI — cheaper than m7i.metal per GB.
- **No nested virt on Graviton** (virtual instances): AWS docs list is Intel-only; announcement coverage explicitly states Graviton unsupported.
- **GCP**: Arm excluded from nested virt, **but C4A *bare metal* is GA (2026-05-28)** — 96 vCPU / up to 768 GB Axion. The only major-cloud Arm bare metal for this use case.
- **KVM arm64 nested (FEAT_NV2)**: mainline KVM has FEAT_NV2-only NV support (v11 series, Nov 2023; enabled via `kvm-arm.mode=nested`, ~6.16-era usable) — https://lwn.net/Articles/921783/ and https://lists.infradead.org/pipermail/linux-arm-kernel/2023-November/882364.html. **Whether any cloud/rental Arm host actually exposes FEAT_NV2 to guests: not found. [UNVERIFIED]**
- **Hetzner RX line (Ampere Altra)**: RX220 80-core launched at €219/mo (2022 press release). **RX models are absent from the June 2026 price table** — current availability and pricing [UNVERIFIED].
- Whether Hetzner's ToS permit running a nested-KVM multi-tenant service: [UNVERIFIED — not checked].

## Load-bearing sources

1. https://aws.amazon.com/about-aws/whats-new/2026/02/amazon-ec2-nested-virtualization-on-virtual/ — 2026-02-16
2. https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/amazon-ec2-nested-virtualization.html — checked 2026-08-13 (16 supported families)
3. https://docs.cloud.google.com/compute/docs/instances/nested-virtualization/overview — ≥10% penalty, AMD/Arm excluded
4. https://docs.cloud.google.com/compute/docs/instances/bare-metal-instances — C3/C4/C4A/C4D/X4/Z3 metal, GA
5. https://learn.microsoft.com/en-us/azure/virtual-machines/sizes/general-purpose/dasv5-series — AMD nested virt Supported, updated 2026-07-27
6. https://learn.microsoft.com/en-us/azure/baremetal-infrastructure/ — Azure metal is SAP/Oracle/NC2-scoped only
7. https://docs.hetzner.com/general/infrastructure-and-availability/price-adjustment/ — new prices eff. 2026-06-15
8. https://docs.hetzner.com/robot/general/traffic/ — unlimited on 1 Gbit; €1/TB over 20 TB on 10 Gbit
9. https://www.latitude.sh/pricing — rendered 2026-08-13
10. https://www.usenix.org/conference/atc25/presentation/zur — HyperTurtle, ATC'25
11. https://instances.vantage.sh/aws/ec2/ (per-instance pages) — third-party AWS pricing, 2026-08-13
12. https://cloud.google.com/blog/products/compute/new-axion-c4a-metal-offers-bare-metal-performance-on-arm — C4A metal GA 2026-05-28
