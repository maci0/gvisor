# MRS ID Register Trapping on Apple HVF

## The Problem

ARM64 programs read CPU ID registers (e.g., `ID_AA64MMFR0_EL1`,
`ID_AA64ISAR0_EL1`) via `MRS` instructions to detect hardware features.
On Apple's Hypervisor.framework (HVF), these instructions at EL0 trap to
the EL1 exception handler. The handler CAN read `ESR_EL1` (confirmed
May 2026) and use it to dispatch syscalls via ERET without VM exit.

The gVisor HVF platform works around this by:
1. Binary patching MRS→MOV in shadow-copied code pages (static code)
2. Trap-and-emulate via HVC exit + ESR_EL1 read via HVF API (runtime)

Java and .NET remain blocked — not by MRS trapping (which now works) but
by HotSpot's AArch64 assembler assuming 4K page alignment.

## Architecture

### ARM64 trap-and-emulate (how it should work)

```
Guest EL0:  MRS X0, ID_AA64MMFR0_EL1
              ↓ (HCR_EL2.TID3 traps to EL2)
EL2:        Read ESR_EL2 → EC=0x18 (MSR/MRS trap)
            Decode ISS → CRn, CRm, Op1, Op2, Rt
            Write emulated value to X[Rt]
            ERET back to EL0 (PC+4)
```

KVM, Xen, QEMU all use this. No code modification needed.

### What actually happens on HVF (without EL2)

```
Guest EL0:  MRS X0, ID_AA64MMFR0_EL1
              ↓ (UNDEFINED at EL0 → traps to EL1)
EL1 vector: HVC #8 → VM exit
Host:       Read ESR_EL1 via HVF API → EC=0x18
            emulateSysreg() → write value to X[Rt]
            PC += 4, resume guest
```

This works. ESR_EL1 is readable both **from the host via HVF API**
(`hv_vcpu_get_sys_reg`) and **from EL1 code inside the VM** after an
EL0→EL1 exception (confirmed by mrstest Test 7, May 2026). The earlier
"hang" report was incorrect — it tested MRS at EL0, not from EL1.

### ISS encoding for EC=0x18

```
ISS[19:17] = Op2
ISS[16:14] = Op1
ISS[13:10] = CRn
ISS[9:5]   = Rt  (destination register)
ISS[4:1]   = CRm
ISS[0]     = direction: 0=MSR(write), 1=MRS(read)
```

## HCR_EL2 Bit Acceptance

Tested on M4 Pro, macOS 26.4. HCR_EL2 bits are only writable when
the VM has EL2 enabled (`hv_vm_config_set_el2_enabled`):

| Bit | Name | Status | Notes |
|-----|------|--------|-------|
| 0 | VM | ALLOWED | Stage-2 translation |
| 1 | SWIO | ALLOWED | |
| 2 | IMO | ALLOWED | IRQ routing |
| 3 | FMO | ALLOWED | FIQ routing |
| 5 | AMO | ALLOWED | SError routing |
| 13 | TWI | ALLOWED | Trap WFI |
| 14 | TWE | ALLOWED | Trap WFE |
| **18** | **TID3** | **ALLOWED** | Trap ID register reads |
| 19 | TSC | ALLOWED | Trap SMC |
| 26 | TVM | ALLOWED | |
| **27** | **TGE** | **DROPPED** | EL0→EL2 routing blocked |
| 31 | RW | ALLOWED | EL1 is AArch64 |
| **34** | **E2H** | **DROPPED** | VHE blocked |

### Why EL2+TID3 doesn't work in practice

TID3 is accepted, but enabling EL2 changes the HVC routing model:

- **Without EL2**: HVC from EL1 → HVF (VM exit)
- **With EL2**: HVC from EL1 → Guest EL2 (VBAR_EL2), NOT HVF

This breaks the entire exception exit path. The EL1 handler's `HVC #8`
goes to the guest's EL2 vectors instead of causing a VM exit. Would
require a full VBAR_EL2 vector table that re-HVCs to exit HVF.

TID3 also only traps **EL1→EL2** reads, not **EL0→EL2**. EL0 reads of
EL1 registers are already UNDEFINED at EL1 (independent of TID3).

## Current Implementation

### Binary patching (static code)

Shadow-copied executable pages are scanned by `patchIDRegisterReads()`:

```
MRS Xt, <sysreg>:  0xD5300000 | (sysreg << 5) | rt
MOVZ Xt, #imm16:   0xD2800000 | (imm16 << 5) | rt
```

Patched registers:

| Register | Patched value | Purpose |
|----------|---------------|---------|
| ID_AA64MMFR0_EL1 | 0x1122 | ASID=16-bit, PA=40-bit |
| ID_AA64PFR0_EL1 | 0x0011 | EL0+EL1 AArch64, FP+SIMD |
| ID_AA64ISAR0_EL1 | 0x0 | ISA attributes |
| MIDR_EL1 | 0x611f0220 | Apple M4 Pro |
| MPIDR_EL1 | 0x80000000 | Uniprocessor affinity |
| + 6 more | 0x0 | |

### Trap-and-emulate (runtime, all code)

For MRS that escape patching (JIT code, unpatched registers):

1. EL0 MRS → UNDEFINED → EL1 el0_sync vector → HVC #8 → VM exit
2. Host reads ESR_EL1 via HVF API → EC=0x18
3. `emulateSysreg()` decodes ISS, returns emulated value
4. **PC += 4** (critical: was missing, caused infinite loop)
5. Resume guest

### SCTLR_EL1 configuration

```
SCTLR_EL1 = 0x34909185
  UCI [26] = 1  → EL0 cache maintenance (DC CIVAC, DC CVAU, IC IVAU)
  UCT [15] = 1  → EL0 CTR_EL0 read access
  DZE [14] = 1  → EL0 DC ZVA access
```

Without UCI/UCT, every CTR_EL0 read and cache flush traps to EL1,
causing thousands of VM exits per second (observed with Java startup).

## Bugs Found and Fixed

### 1. PC not advanced after sysreg emulation

`context.go` set `PC = ELR_EL1` after emulating MRS, which reloaded
the same instruction. Fixed to `PC = ELR_EL1 + 4`.

**Impact**: Any unpatched MRS caused infinite loop. CTR_EL0 reads
(not in patch list) spun forever. Java hung at startup.

### 2. Missing UCI/UCT in SCTLR_EL1

SCTLR_EL1 had UCI=0 and UCT=0, trapping every EL0 cache maintenance
and CTR_EL0 read. Fixed by setting bits 26 and 15.

**Impact**: Java's `DC CIVAC` (cache flush after JIT compilation)
caused SIGILL. CTR_EL0 reads trapped to emulation (~10k/sec overhead).

### 3. Wrong ID register emulation values

`ID_AA64PFR0_EL1` was 0x1100 (claims EL2+EL3 exist, EL0/EL1 are
AArch32+AArch64). Fixed to 0x0011 (EL0+EL1 AArch64 only).

ISAR0/ISAR1 now return real Apple M4 Pro feature bits matching
`/proc/cpuinfo` (AES, SHA, CRC32, LSE atomics, etc.).

## Java Status: Blocked by 16K Page Size

### The crash

```
Internal Error (assembler_aarch64.hpp:248)
guarantee(val < (1ULL << nbits)) failed: Field too big for insn
```

Occurs during `StubRoutines::initialize()` — before any Java code runs.
Reproducible with `-Xint`, `-XX:+UseSerialGC`, `-XX:-UseCompressedOops`.

### Root cause

HotSpot's AArch64 assembler uses `logical_immediate_encode()` to encode
ARM64 bitmask immediates (for AND/ORR/EOR instructions). Some values
derived from `os::page_size()` (16384) cannot be represented as ARM64
bitmask immediates, causing the encoder to return -1.

The JVM reads page size from `AT_PAGESZ` in the auxiliary vector, which
gVisor sets to `hostarch.PageSize` = 16384 (macOS ARM64 uses 16K pages).

### Why we can't lie about page size

Setting `AT_PAGESZ = 4096` causes musl libc to use 4K alignment for
mmap, but the sentry's VMAs are 16K-aligned. The mismatch causes
SIGSEGV during process startup (mmap returns addresses that appear
valid to musl but fault in the sentry's page table walk).

### Upstream status

This is a known class of JDK bugs on AArch64:
- [JDK-8247766](https://bugs.openjdk.org/browse/JDK-8247766): guarantee(val < (1U << nbits)) failed
- [JDK-8320682](https://bugs.openjdk.org/browse/JDK-8320682): C1 fails with large non-nmethod code heap
- [JDK-8276108](https://bugs.openjdk.org/browse/JDK-8276108): 16K page size support on AArch64

Tested with OpenJDK 17.0.18 and 21.0.10 — both crash identically.

### Possible fixes

1. **Upstream JDK patch**: Fix `logical_immediate_encode()` for 16K-derived values
2. **Build JDK with 16K page support**: JDK-8276108 if it's ever merged
3. **Page size compatibility layer**: Report 4K to `AT_PAGESZ` while internally using 16K alignment — requires fixing the mmap alignment mismatch in the sentry's mm layer

## Test Results

### mrstest output (M4 Pro, macOS 26.4)

```
--- Test 1: MRS ID_AA64MMFR0_EL1 at EL0 ---
  Exit: reason=1, syndrome=0x5a000008 (EC=0x16, ISS=0x8)
  VERDICT: MRS causes EL1 exception (not a hang)

--- Test 2: MOV immediate (patched MRS) ---
  VERDICT: PASS — binary patching works

--- Test 3: HCR_EL2 bits ---
  TID3 [18]: ALLOWED
  TGE  [27]: DROPPED
  E2H  [34]: DROPPED

--- Test 4: MRS with TID3 (EL2-enabled VM) ---
  HUNG: PC=0x200 (trapped to guest EL2, not HVF)
  VERDICT: FAIL — EL2 changes HVC routing
```

## Reproducer

`cmd/mrstest/main.go` — standalone test for all findings above.

```bash
go build -o mrstest ./cmd/mrstest
codesign -s - --entitlements entitlements.plist -f mrstest
./mrstest
```

## Java / JVM Status

**Status: BLOCKED by upstream JDK bug. Tabled.**

### What works (sentry-side fixes applied)

| Issue | Fix | File |
|-------|-----|------|
| MRS infinite loop | PC += 4 after sysreg emulation | `context.go` |
| DC CIVAC SIGILL | SCTLR_EL1.UCI=1 (EL0 cache maintenance) | `vcpu_arm64.go` |
| CTR_EL0 trap storm | SCTLR_EL1.UCT=1 (EL0 counter read) | `vcpu_arm64.go` |
| Wrong PFR0/ISAR0 values | Match Apple M4 Pro features | `context.go`, `ipa_allocator.go` |
| 16K page size in auxv | `hostarch.GuestPageSize=4096` → AT_PAGESZ=4096 | `loader.go`, `hostarch_arm64_darwin.go` |
| Sub-16K guard pages | Skip PROT_NONE < 16K at syscall level | `sys_mmap.go` |

### What blocks Java

```
Internal Error (assembler_aarch64.hpp:24X)
guarantee(val < (1ULL << nbits)) failed: Field too big for insn
```

The HotSpot AArch64 assembler's `logical_immediate_encode()` returns -1
for a bitmask value during VM stub generation. This happens at startup
before any Java code runs. Independent of page size, address layout,
heap placement, GC type, or compilation mode.

**Tested and confirmed broken on:**

| JDK | Version | Result |
|-----|---------|--------|
| Alpine OpenJDK 17 | 17.0.18+8 | Field too big (assembler_aarch64.hpp:248) |
| Alpine OpenJDK 21 | 21.0.10+7 | Field too big (assembler_aarch64.hpp:245) |
| Amazon Corretto 26 | 26.0.1+8 | Field too big (assembler_aarch64.hpp:246) |

All tested with: `-Xint`, `-XX:+UseSerialGC`, `-XX:-UseCompressedOops`,
`-XX:-UseCompressedClassPointers`, `-Xshare:off`. Same crash in all
configurations.

**Root cause**: The bitmask immediate encoding is an ARM64 instruction
format constraint — only specific bit patterns (repeating runs of
consecutive 1-bits) can be encoded as logical immediates. The JVM
computes a value during stub generation that doesn't fit this pattern.
This is NOT caused by our sentry — it's a JVM bug that would reproduce
on any AArch64 system with 16K pages.

**Upstream references:**
- [JDK-8247766](https://bugs.openjdk.org/browse/JDK-8247766): guarantee(val < (1U << nbits)) failed
- [JDK-8335662](https://bugs.openjdk.org/browse/JDK-8335662): C1 "Field too big" with large locals table
- [JDK-8320682](https://bugs.openjdk.org/browse/JDK-8320682): C1 fails with large code heap

**Possible future paths:**
1. Upstream HotSpot fix for 16K-page bitmask values
2. Custom JDK build with patch
3. GraalVM Native Image (bypasses HotSpot assembler entirely)
4. Wait for Apple to ship 4K-page Rosetta translation layer for ARM VMs

## Source Files

| File | What |
|------|------|
| `pkg/sentry/platform/hvf/ipa_allocator.go:280-355` | MRS binary patching |
| `pkg/sentry/platform/hvf/context.go:282-400` | sysreg emulation (EC=0x18) |
| `pkg/sentry/platform/hvf/vcpu_arm64.go:188` | SCTLR_EL1 (UCI/UCT/DZE) |
| `pkg/sentry/platform/hvf/address_space.go:85-121` | Shadow copy decision |
| `pkg/sentry/syscalls/linux/sys_mmap.go` | page4K syscall rounding |
| `pkg/sentry/loader/loader.go:345` | AT_PAGESZ override |
| `cmd/mrstest/main.go` | MRS trapping reproducer |
| `cmd/el2probe/main.go` | EL2 capability probe |

## References

- ARM Architecture Reference Manual (DDI0487), D17.2: System register traps
- Apple Hypervisor.framework: developer.apple.com/documentation/hypervisor
- OpenJDK AArch64 assembler: `src/hotspot/cpu/aarch64/assembler_aarch64.hpp`

## GraalVM Native Image: Working Alternative

GraalVM native-image AOT-compiles Java to native ARM64 binaries,
bypassing HotSpot's JIT assembler entirely. No `logical_immediate_encode`
issues since all instruction encoding happens at build time on the host.

**Tested**: GraalVM CE 23.0.2, native-image built via podman (Oracle
Linux aarch64), dynamically linked against glibc. Runs in gVisor macOS
sentry with glibc compat libs installed in Alpine rootfs.

```
GraalVM Native Image: Hello from gVisor macOS!
Java version: 23.0.2
OS: Linux aarch64
Available processors: 14
Max memory: 1638 MB
10M loop: 2ms (sum=49999995000000)
```

**How to build**: Use `podman` with GraalVM container:
```bash
podman run --rm --platform linux/arm64 \
  -w /work -v ./src:/work \
  --entrypoint bash \
  ghcr.io/graalvm/native-image-community:23 \
  -c 'javac App.java && native-image -o app App'
```

Copy glibc runtime libs from the container into the Alpine rootfs:
```bash
# From GraalVM container: /lib/ld-linux-aarch64.so.1, /lib64/libc.so.6, etc.
```

This is the recommended path for running Java workloads on gVisor macOS
until the HotSpot 16K page size bug is fixed upstream.
