# HVF Platform (macOS)

[TOC]

The HVF platform implements gVisor's `Platform` interface using Apple's
Hypervisor.framework on macOS ARM64 (Apple Silicon). It replaces KVM, enabling
gVisor to run Linux containers on macOS without a Linux VM.

## Overview

![HVF Architecture](hvf-architecture.png "HVF platform architecture on macOS.")

## Comparison with KVM

| Aspect | KVM (Linux) | HVF (macOS) |
|--------|-------------|-------------|
| API | `/dev/kvm` ioctl | Hypervisor.framework C API |
| Guest mode | EL0 (user) + EL1 (kernel) | EL1 only |
| Page tables | Managed by KVM (EPT/NPT) | Stage 1: per-MM in userspace; Stage 2: HVF |
| Syscall trap | SVC from EL0 -> EL1 vectors | SVC from EL1 -> EL1 vectors -> HVC exit |
| Memory mapping | `KVM_SET_USER_MEMORY_REGION` | `hv_vm_map(host, ipa, size, perms)` |
| vCPU binding | Per-thread via ioctl | Per-thread via `hv_vcpu_create` |
| PSTATE | Native EL0/EL1 | Translated: EL1h <-> EL0t |

## Guest Execution

### EL1 Execution and PSTATE Translation

The guest runs entirely at EL1 (supervisor mode). Hypervisor.framework doesn't
support EL0/EL1 separation within the guest. Since gVisor's sentry expects
application code to run at EL0 (user mode), the platform translates PSTATE:

```
 On vCPU exit (save):
   Pstate = SPSR_EL1 & ~0x3CF    // Clear mode (0xF) + DAIF (0x3C0) -> EL0t

 On vCPU entry (load):
   SPSR_EL1 = (Pstate & 0xF0000000) | 0x3C5   // NZCV + EL1h + DAIF masked
```

Without this translation, the Go runtime's `validRegs()` check in signal frame
restoration rejects the saved PSTATE because the DAIF bits (interrupt mask)
are set. This was the root cause of signal delivery failures.

### Exception Vectors

![Exception Flow](exception-flow.png "Exception handling flow from guest to sentry.")

A shared exception vector table is mapped at IPA 0 in every address space.
Each of the 16 vectors contains a single `HVC #N` instruction that exits to
the hypervisor. The sentry reads `ESR_EL1` to determine the original
exception class:

| EC | Exception | Sentry Action |
|----|-----------|---------------|
| 0x15 | SVC (syscall) | Return to sentry for syscall handling |
| 0x24 | Data abort (lower EL) | `HandleUserFault` -> page table update |
| 0x25 | Data abort (current EL) | `HandleUserFault` -> page table update |
| 0x20 | Instruction abort | `HandleUserFault` -> map code page |

A sigreturn trampoline at IPA 0x804 provides `MOV X8, #139; SVC #0` for
signal frame restoration.

## Two-Level Address Translation

![Address Translation](address-translation.png "Two-level address translation: Stage 1 (per-MM) and Stage 2 (HVF).")

**Stage 1** (software, per address space): ARM64 L2/L3 page tables with 16K
granule. Maps guest VA to IPA. Controlled by the platform via `MapFile`.

**Stage 2** (hardware, HVF): Maps IPA to host physical address. Controlled by
the IPA allocator via `hv_vm_map`. Shared across all address spaces.

### Page Table Format

16K granule, T0SZ=28 (36-bit VA, 64GB):

- L2 table: 2048 entries, 32MB per entry, root of each address space
- L3 table: 2048 entries, 16K per entry, allocated on demand
- Descriptors: `IPA | nG | AF | SH | attr | AP[2] | table | valid`

### Copy-on-Write

The AP[2] bit (bit 7) in L3 entries controls write permission:
- AP[2]=0: read-write (normal pages)
- AP[2]=1: read-only (COW pages after fork)

On fork, parent pages become read-only. Write faults trigger COW:
guest faults -> `HandleUserFault` -> `breakCopyOnWriteLocked` -> allocate
new page, copy data, remap writable.

### TLB Invalidation

Non-Global (nG) bit is set on all L3 entries. Each `Switch()` increments an
8-bit ASID in TTBR0_EL1. When the ASID changes, all nG TLB entries from
previous runs become invalid, ensuring new page table entries are visible.

## IPA Space Layout

```
 0x00000 - 0x03FFF   Exception vectors + sigreturn trampoline (16K)
 0x10000 - 0xFFFFF   Page table pages (L2/L3, recycled via free list)
 0x100000+            Data pages (host memory, assigned by IPA allocator)
```

The IPA allocator maps host pages into the HVF VM on first use and caches the
IPA assignment. Multiple address spaces sharing a host page (COW) use the same
IPA but with different Stage 1 permissions (AP[2]).

## vCPU Management

vCPUs are thread-local: each OS thread gets its own HVF vCPU (created lazily
by `machine.Get()`). vCPUs are reused across `Switch()` calls on the same
thread. The Go runtime's `LockOSThread`/`UnlockOSThread` ensures thread
affinity during guest execution.

## Host Networking (utun)

When `--net` is passed, the platform creates a macOS utun device:

```
 macOS host               utun tunnel              gVisor guest
 192.168.110.1  <----  4-byte AF header  ---->  192.168.110.2
                       + raw IP packet           (netstack NIC)
```

Each instance gets a unique /30 subnet: `utunN` -> `192.168.(100+N).0/30`.
The utun endpoint reads/writes raw IP packets with a 4-byte protocol family
header prepended by the kernel.
