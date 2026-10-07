# Why this host cannot run Firecracker

Recorded so nobody repeats the investigation. `/dev/kvm` **cannot be created
here** — not for want of privilege, but because the kernel's KVM subsystem
never initialized.

## Evidence

```
$ uname -m                      aarch64
$ systemd-detect-virt           kvm          # we are ourselves a KVM guest
$ lscpu | grep 'Model name'     Neoverse-N1
$ grep Features /proc/cpuinfo
  fp asimd evtstrm aes pmull sha1 sha2 crc32 atomics fphp asimdhp cpuid
  asimdrdm lrcpc dcpop asimddp ssbs
$ modinfo kvm | head -2
  name: kvm
  filename: (builtin)                        # not a module; nothing to load
$ sudo modprobe kvm; echo $?    0            # no-op, it is builtin
$ grep kvm /proc/misc                        # EMPTY -- misc minor 232 absent
$ sudo mknod /tmp/kvmprobe c 10 232
$ python3 -c "import os; os.open('/tmp/kvmprobe', os.O_RDWR)"
  OSError: [Errno 19] No such device
```

`/proc/misc` is the decisive one: KVM registers misc minor 232 **only if
`kvm_arch_init` succeeded**. It is absent, and opening a hand-made node returns
`ENODEV`. The device node is not the thing that is missing.

## Root cause

arm64 KVM requires the kernel to run at **EL2**. This kernel runs at EL1
because it is a guest. Getting KVM inside a guest needs **nested
virtualization**, which on arm64 requires **ARMv8.4-NV** hardware. Neoverse-N1
is ARMv8.2 and its feature list has no `nv`. Passwordless `sudo` is available
and changes nothing: this is a hardware and privilege-level limit, not a
permissions one.

## What was done instead

Creating `/dev/kvm` by hand would be worse than useless: Firecracker would fail
at a *different and more confusing* point, and the driver would look "tested".

Instead the test double was made faithful. `fakeVMM`
(`internal/vmm/firecracker/driver_test.go`) previously answered `204` to every
`PUT`/`PATCH` regardless of state, so a driver that drove the API in an illegal
order passed exactly like one that did it right — which is why a whole class of
driver bugs was invisible. It now enforces the Firecracker API state machine:

- instance states `Not started` -> `Running` <-> `Paused`
- `InstanceStart` requires a boot source and a root drive, only from `Not started`
- `PATCH /vm` `Paused` requires `Running`; `Resumed` requires `Paused`
- **`PUT /snapshot/create` requires the microVM to be paused** — a 400 otherwise
- `PUT /snapshot/load` only from `Not started`
- configuration endpoints are rejected after boot

Switching the fake over immediately reproduced four real driver defects that
had been reported by code reading but never demonstrated. See `.audit/PASS-18.md`.

## What still needs real hardware

Contract conformance is not the same as working. Still unverified here, and
requiring a KVM host:

- that a restored guest actually resumes and runs
- real snapshot/restore latency against the p50 <= 300ms target (`bench/phase0`)
- UFFD memory restore
- jailer chroot/uid behaviour under a real jailer binary
- vsock data path against a real Firecracker vsock device

Neither `firecracker` nor `jailer` is installed on this host either
(`/usr/local/bin/firecracker` does not exist), so even the process-spawn path is
exercised only through `/bin/true`.
