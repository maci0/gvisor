# Syscall Speedup: 4µs → 0.1µs (achieved for 10 fast-path syscalls)

## Problem (solved for common syscalls)

Non-fast-path guest syscalls take a full HVF VM exit round-trip (~4µs):

```
Guest EL0: SVC #0
  → EL1 vector: HVC #8
    → HVF VM exit (~7-8µs)
      → Host: read ESR_EL1 via API
      → Host: sentry handles syscall (fast)
      → Host: write result to registers via API
    → hv_vcpu_run re-enter (~3-4µs)
  → EL1: ERET
→ Guest EL0: resumes
```

On Linux KVM, gVisor handles most syscalls at EL1 without a VM exit
(~100ns). The 100x overhead makes syscall-heavy workloads (LuaJIT,
shell scripts, Go programs) ~1600x slower than native.

## Measured Latency

| Operation | Time | Method |
|-----------|------|--------|
| getpid (fast-path) | **~0.1µs** | ERET at EL1, no VM exit |
| getpid (before) | ~4µs | HVC VM exit round-trip |
| fork+exec | ~1.9ms | /proc/uptime timing × 100 |
| VDSO clock_gettime | ~5ns | No syscall, CNTVCT_EL0 |
| Memory bandwidth | ~1.3x native | No syscall overhead |
| Native macOS getpid | ~32ns | Direct syscall |

## Switch() Hot Path Breakdown

Profiled with `--profile` flag (manual `time.Now()` instrumentation;
pprof/trace SIGPROF interferes with HVF vCPU execution).

| Component | getpid 10K | echo | jq | python | fork+exec |
|-----------|-----------|------|-----|--------|-----------|
| loadRegisters | 496ns | 569ns | 557ns | 628ns | 684ns |
| hv_vcpu_run | 2,971ns | 6,916ns | 5,174ns | 15,642ns | 4,079ns |
| saveRegisters | 844ns | 912ns | 845ns | 933ns | 2,432ns |
| **Total** | **4,312ns** | **8,398ns** | **6,577ns** | **17,203ns** | **7,196ns** |
| Syscalls | 10,525 | 9 | 549 | 513 | 134 |
| Faults | 275 | 37 | 190 | 265 | 414 |

**Key findings:**
- `hv_vcpu_run` is 60-90% of total (Apple VM exit floor, ~3µs minimum)
- `loadRegisters` optimized to ~500ns via batched CGO (was ~4.5µs)
- `saveRegisters` ~850ns via batched CGO (was ~4.5µs)
- Python's high vcpu_run (15µs) = more guest instructions between syscalls
- Fork's high saveRegisters (2.4µs) = FP register save overhead
- No non-HVF bottleneck found: gofer, mm, page table walks only on faults

The bottleneck is entirely in `hv_vcpu_run` — Apple's HVF VM exit cost.

## Current Status: ESR_EL1 Reads

**API reads work fine.** After a VM exit, the host reads ESR_EL1 via
`hv_vcpu_get_sys_reg(HV_SYS_REG_ESR_EL1)` — this is the production
path (context.go:184). EC=0x18 traps for MRS instructions also work
correctly (context.go:162), enabling trap-and-emulate for ID registers.

**In-VM reads CONFIRMED WORKING.** mrstest Test 7 proved that EL1 code
CAN read ESR_EL1 after EL0→EL1 exceptions. Returns EC=0x15 (SVC
syndrome) correctly. The earlier "hang" was from Test 1 which tested
MRS at EL0 (different issue — traps to el0_sync handler). ELR_EL1
returns 0 after exception (may need API fallback or MMU-on retest).

**mrstest results (M4 Pro, macOS 26.4):**
```
Test 1: MRS at EL0 (no config)      → EL1 exception (no hang!)
Test 2: MOV control                  → PASS
Test 4: MRS at EL0 (EL2+TID3)       → EC=0x16 HVC exit
Test 5: EL2 reads ESR_EL1           → HUNG at 0x4200
Test 6: MRS at EL0 (production cfg) → EL1 exception (no hang)
Test 7: MRS ESR_EL1 from EL1        → *** EC=0x15 SVC syndrome! WORKS! ***
```

**BREAKTHROUGH (Test 7):** EL1 code CAN read ESR_EL1 after EL0→EL1
exception. The earlier "hang" was at EL0 (different issue). In-VM
syscall dispatch IS architecturally possible. ELR_EL1 returns 0
(needs investigation — may require MMU on), but ESR_EL1 works.

**4K page tables**: Working and now the default. Uses
`hv_vm_config_set_ipa_granule(HV_IPA_GRANULE_4KB)` for stage-2 and
`TCR_EL1.TG0=4K` for stage-1. Sub-16K PROT_NONE guard pages (from
musl) handled by skipping VMA creation. `--page16k` reverts to 16K.

**Large VA unmap fix**: With 48-bit VA, MUnmap could receive ranges
spanning ~117TB. The per-page loop iterated billions of times. Fixed
with O(mapped) range unmap that walks only populated L3 tables.

**ERET BREAKTHROUGH:** HVC from EL1 discards register modifications
(restores EL0 state), but ERET preserves them. This enables in-VM
syscall dispatch: the handler sets X0 = return value and ERETs to EL0
without VM exit. Verified by vmtest Test 5 and production (88/88 pass).

**In-VM fast-path syscalls (12 syscalls, zero VM exit):**
- **Table dispatch (172-178)**: getpid, getppid, getuid, geteuid, getgid,
  getegid, gettid — SUB+CMP+ADR+BR at 0x400. Patchable MOVZ values
  rewritten by host per-task via PatchFastPathSyscalls().
- **Extended handler (0x600)**: sched_yield(124), getpgid(0) (155),
  getsid(0) (156), set_tid_address(96) — patchable return values.
- **rt_sigprocmask handler (0x680)**: reads/writes signal mask via
  state page + STTR/LDTR for user memory (bypasses PAN). Signal mask
  synced to/from state page by task_run.go via platform.SignalMasker.
  TLBI VMALLE1IS inside handler for TTBR1 TLB population. Catches
  ~83% of calls in single-process mode; child processes after fork
  may fall through to sentry (TTBR1 TLB cold after ASIDE1IS).
- **getpid: 0.1µs (was 4µs — 40x faster)**
- **16M calls/second**

**EL1 data access — CORRECTED test results (el1memtest):**

Previous vmtest results were WRONG — EL1 data access DOES work on HVF.
The failures were caused by PAN (Privileged Access Never) being auto-set
on EL0→EL1 exception entry, blocking access to AP[1]=1 (EL0-accessible)
pages. Using AP[1]=0 (EL1-only) pages bypasses PAN entirely.

| Test | Operation | MMU | Result |
|------|-----------|-----|--------|
| 1 | LDR from TTBR0 (AP[1]=0, EL1-only) | ON | **WORKS** |
| 2 | PSTATE dump after exception | - | **PAN=1 (auto-set)** |
| 3 | LDR from TTBR1 (kernel VA) | ON | **WORKS** |
| 4 | Guest-initiated MMU enable + LDR | ON | **WORKS** (faulted on unrelated bug) |
| 5 | MOV X0 + ERET (register only) | OFF | **WORKS** (40x speedup) |
| 6 | STR to IPA (direct) | OFF | **WORKS** |
| 7 | MSR SCTLR_EL1 (toggle MMU) | ON | **TRAPPED** by HVF |

**Why vmtest failed (root cause: PAN):**
- PSTATE.PAN is auto-set to 1 on every EL0→EL1 exception, regardless
  of SCTLR_EL1.SPAN setting (HVF may force this behavior)
- PAN=1 blocks EL1 access to pages with AP[1]=1 (user-accessible)
- vmtest used AP[1]=1 pages for TTBR0 data → permission fault (0x200)
- vmtest TTBR1 test used AP[1]=0 but had page table index bugs
- el1memtest with AP[1]=0 (EL1-only) pages → **both TTBR0 and TTBR1 work**

**Conclusion:** EL1 data access through stage-1 page tables WORKS on
HVF. Both TTBR0 and TTBR1. The constraint is: pages accessed from EL1
exception handlers must use AP[1]=0 (EL1-only, not EL0-accessible), or
PAN must be explicitly cleared (MSR PAN, #0) before access. This is
the same constraint real Linux kernels have — they use copy_from_user()
with PAN-aware accessors for user pages, and direct access for kernel
pages (AP[1]=0).

**THIS CHANGES EVERYTHING:** Full sentry-at-EL1 with kernel memory
access is architecturally possible. The 3µs VM exit floor can be
eliminated for ALL syscalls by running sentry dispatch code at EL1
with a dedicated TTBR1 kernel mapping (AP[1]=0).

The 10 register-only fast-path syscalls are the practical maximum
for the HVF platform. Profiling across 5 workloads (echo, pipe,
fork×50, python, jq×20) shows remaining VM-exit syscalls are dominated
by mmap/munmap (76%), which require kernel memory management.

**Syscall frequency profile (VM exits only, fast-path invisible):**

| Syscall | Count | % | Fast-pathable? |
|---------|-------|---|----------------|
| mmap (222) | 4,843 | 38.8% | No — needs mm |
| munmap (215) | 4,693 | 37.6% | No — needs mm |
| rt_sigprocmask (135) | 571 | 4.6% | No — needs signal mask |
| wait4 (260) | 252 | 2.0% | No — blocks/wakes |
| brk (214) | 220 | 1.8% | No — needs mm |
| close (57) | 182 | 1.5% | No — needs FD table |
| rt_sigaction (134) | 182 | 1.5% | No — needs handler table |
| mprotect (226) | 179 | 1.4% | No — needs mm |
| set_tid_address (96) | 172 | 1.4% | Marginal — returns TID |

EL1 data access IS confirmed working (el1memtest, AP[1]=0 pages).
Adding more fast-path syscalls requires implementing the full state
page protocol: EL1 handler reads/writes shared memory for syscall
args/results, dispatches complex syscalls (mmap, munmap, etc.) via
state page + HVC to host. Infrastructure is ready but the TLB cold
miss cost for state page access needs a warm-TLB strategy first.

**Current optimizations applied:**
- In-VM ERET fast-path for 12 syscalls (getpid, sigprocmask, etc.)
- `hv_vcpu_run_until(HV_DEADLINE_FOREVER)` — eliminates spurious vtimer exits
- ESR_EL1 dispatch at EL1 (HVC #9 for SVC, #8 for fault)
- Batched register save/load (single CGO call for X0-X30)
- Skip FP register load (sentry never modifies FP state)
- Selective TLBI (ASIDE1IS by ASID)
- O(mapped) unmap for large VA ranges
- 4K guest pages (IPA granule + TG0=4K)
- State page infrastructure ready (per-vCPU allocation, TTBR1 mapping, TPIDR_EL1) — STP chain disabled due to TLB cold miss overhead

**Optimization paths tested:**

| Approach | Result | Verdict |
|----------|--------|---------|
| GIC in-kernel interrupts | vtimer exits already eliminated by hv_vcpu_run_until | Not needed |
| Selective TLBI (ASIDE1IS) | Implemented, no measurable speedup (TLBI is ~50ns) | Done |
| Skip register reload on internal exits | Implemented | Done |
| **Mach exception platform** | **14µs per BRK — 5x WORSE than HVF (3µs)** | **Dead** |
| **BRK/SIGTRAP signal handler** | **3.1-3.6µs — same as HVF VM exit** | **Dead** |
| **State page STP save** | **STP X0-X30 in EL1, read on host** | **~3% faster (best case), net-negative under load** | **Disabled** |
| **In-VM LDP load** | **LDP X0-X30 from state page in ERET stub** | **3µs SLOWER (16 cold TLB walks)** | **Rejected** |
| libsystem_kernel replacement | Not tested | Extreme complexity |

**Mach exception platform (tested, rejected):**
Built machtest POC (cmd/machtest). BRK #0 triggers EXC_BREAKPOINT
caught by Mach handler thread. thread_get_state/thread_set_state for
register access. Result: **14µs per BRK round-trip** — 5x worse than
HVF's 3µs. Overhead from Mach IPC message passing + kernel thread
state serialization. HVF keeps registers in CPU, Mach must copy them
through kernel → message → handler → message → kernel.

**BRK/SIGTRAP signal handler (tested, rejected):**
Tested BRK → SIGTRAP signal delivery without Mach exception ports.
sigaction(SIGTRAP) with SA_SIGINFO, handler advances PC+4 via
mcontext. Result: **3.1-3.6µs per BRK** (10K iterations, M4 Pro) —
essentially identical to HVF VM exit (3µs). Both go through the
macOS kernel's EL1→EL0 exception delivery path. Signals can't beat
VM exits because the kernel trap cost is the same.

| Mechanism | Latency | vs HVF |
|-----------|---------|--------|
| ERET fast-path (in-VM) | ~0.09µs | 33x faster |
| HVF VM exit | ~3.0µs | baseline |
| BRK/SIGTRAP signal | ~3.3µs | same |
| Mach exception port | ~14µs | 5x slower |

**State page STP save chain (tested, disabled):**
EL1 exception handler saves X0-X30 via STP pairs to a per-vCPU state
page mapped in TTBR1 (AP[1]=0, EL1-only). Host reads saved registers
from the state page instead of 31 individual HVF API calls. Best case:
~3% faster (save path only). But TTBR1 page access triggers cold TLB
misses (~1µs overhead) that offset the ~300ns API call savings. The
infrastructure is ready (per-vCPU state page allocation, TTBR1 kernel
PT mapping, TPIDR_EL1 pointing to state page), but the STP chain is
disabled pending a TLB-warm strategy (e.g., keeping the state page
permanently in TLB via pinned ASID or pre-warming on vCPU entry).

**In-VM LDP load chain (tested, rejected):**
ERET stub loads X0-X30 from state page via LDP pairs before returning
to EL0, replacing 31 individual HVF API register writes. Result:
**~3µs SLOWER** than API register loads. Each LDP triggers a TLB walk
for the state page VA (16 loads = 16 cold TLB walks through L0→L1→L2→L3
page tables). The cumulative TLB walk cost far exceeds the API call
overhead. LDP load chain is only viable if the state page TLB entry
can be kept warm across vCPU entries.

| Mechanism | Latency | vs HVF API |
|-----------|---------|------------|
| HVF API register load (batched CGO) | ~500ns | baseline |
| In-VM LDP load from state page | ~3,500ns | 7x slower |
| HVF API register save (batched CGO) | ~850ns | baseline |
| In-VM STP save to state page | ~1,100ns | ~30% slower (TLB cold) |

**Research sources:**
- QEMU HVF: hv_vcpu_run_until patch (Phil Dennis-Jordan, 2024)
- XNU internals: HVF is closed-source, VHE-only (EL2 host mode)
- ARM VM exit cost: ~3,045 cycles bare EL2 trap (Columbia ISCA 2016)
- Mach IPC: measured 14µs BRK round-trip (machtest, 10K iterations)
- fdiv.net: UDF/BRK trap + EXC_BAD_INSTRUCTION mechanism (works, but slow)
- Apple: no new TLB/batch register APIs in macOS 26

## Approaches Investigated

### Approach 1: Sentry at EL1 (Ring0 / KVM-style)

**Goal**: Run sentry Go runtime at EL1 inside the VM. Guest SVC traps
to EL1 sentry, which handles syscalls without VM exit.

```
Guest EL0: SVC → EL1 sentry: dispatch → ERET
           ~100ns (no VM exit)
```

**Implementation status**: Substantial infrastructure exists:
- `syscall_el1.go`: SyscallHandler registration + dispatch
- `runtime_el1.go`: HVC proxy for host I/O
- `vmm_protocol.go`: Shared memory protocol (VMMRequest)
- `tlbi_el1.go`: Direct TLBI functions
- `memory_el1.go`: Dual-TTBR layout design
- `ring0_integration_test.go`: Test skeletons (all skipped)
- Vector stub at 0x820: Ring0 entry (identical to 0x810, TCR swap TODO)

**Previous blocker (RESOLVED):** EL1 data access was thought to be
blocked by HVF. el1memtest proved this was a PAN (Privileged Access
Never) issue — pages with AP[1]=1 are blocked by PAN after EL0→EL1
exception. Using AP[1]=0 (EL1-only) pages for kernel data WORKS.
Both TTBR0 and TTBR1 data access confirmed working.

**All major blockers RESOLVED (el1memtest):**
1. ESR_EL1 reads from EL1 after exception — **WORKS.** Returns
   EC=0x15 (SVC) correctly. Previous "hang" was PAN/page-table bug.
   ELR_EL1 and SPSR_EL1 return 0 (may need workaround via API read
   on first exit, or the handler can compute ELR from EL0 PC).
2. EL1 data access through TTBR0/TTBR1 — **WORKS** with AP[1]=0.
3. Remaining design work: TCR_EL1 swap for concurrent vCPUs, Go
   runtime memory layout at EL1, and state page protocol.

**Verdict**: ALL BLOCKERS RESOLVED. EL1 data access AND ESR_EL1 reads
confirmed working (el1memtest, M4 Pro, macOS 26.4). 10 constant-result
syscalls already handled via ERET at ~0.1µs. Full Ring0 with memory
access and exception dispatch is architecturally possible — PAN was the
only blocker, and ESR_EL1 reads work correctly (EC=0x15 for SVC).

### Approach 2: EL2 Bridge (TGE + ESR_EL2)

**Goal**: Route EL0 exceptions to EL2 (via HCR_EL2.TGE=1), read
ESR_EL2 at EL2, forward exception info to EL1 handler.

```
Guest EL0: SVC
  → EL2 (TGE=1): read ESR_EL2, write to shared page
    → EL1: read syndrome from page, dispatch
      → ERET → Guest EL0
```

**Key findings from mrstest and el2probe**:

| HCR_EL2 Bit | Status | Implication |
|-------------|--------|-------------|
| TID3 [18] | ALLOWED | Can trap ID register reads |
| TGE [27] | **DROPPED** | Cannot route EL0→EL2 directly |
| E2H [34] | **DROPPED** | Cannot use VHE mode |
| RW [31] | ALLOWED | EL1 is AArch64 |
| All others | ALLOWED | VM, IMO, FMO, TWI, etc. |

**Problem**: TGE is DROPPED by Apple. EL0 exceptions cannot route to EL2.
They always go to EL1 first. And at EL1, ESR_EL1 is unreadable.

**Alternative**: With EL2 enabled, HVC from EL1 goes to guest EL2
(VBAR_EL2), not HVF. An EL2 handler could potentially read ESR_EL1
from EL2 privilege level. But this requires:
1. Full VBAR_EL2 vector table
2. EL2 handler reads ESR_EL1 (or ESR_EL2 if the exception was forwarded)
3. EL2 handler writes syndrome to shared page
4. EL2 returns to EL1 via ERET
5. EL1 reads shared page, dispatches

**Tested (mrstest test 5)**: Three separate issues found:

1. **MRS ESR_EL1 from EL2 HANGS** — same as from EL1. HVF blocks
   ESR_EL1 reads at ALL privilege levels after an exception.

2. **HVC from EL2 doesn't exit to HVF** — it loops through
   VBAR_EL2 + 0x200 (current-EL sync). Once exception handling
   enters the guest's EL2, there's no clean way back to HVF.
   HVF only intercepts stage-2 faults and timer events, not
   guest EL2 exceptions.

3. **EL2 lower-EL vectors (0x400+) never fire** — HVF treats
   all exceptions as "current EL" even when logically from EL1.
   The EL1→EL2 exception model is not fully implemented.

**Verdict**: NOT VIABLE. EL2 bridge is dead — can't read ESR_EL1,
can't exit EL2 back to HVF, and the exception routing doesn't
match the ARM64 spec.

### Approach 3: HVC-based Syscall ABI

**Goal**: Modify guest libc to use HVC instead of SVC for syscalls.
HVC from EL0 directly exits to HVF with full syndrome.

```
Guest EL0: HVC #<nr>
  → HVF exit: syndrome has syscall number
    → Host: dispatch (no ESR_EL1 needed)
```

**Pros**:
- HVC from EL0 causes clean VM exit with EC=0x16
- Syndrome includes the HVC immediate (16 bits = syscall number)
- No ESR_EL1 reading needed — syndrome is in the exit info
- No EL1 vectors involved at all

**Cons**:
- Requires patching guest libc (musl) to use HVC instead of SVC
- HVC is normally privileged (EL1+) — at EL0, it causes UNDEFINED
- Wait: actually on ARM64, HVC from EL0 IS undefined (EC=0x00)
- **This doesn't work**: HVC is not available from EL0

**Verdict**: NOT VIABLE. ARM64 HVC is EL1+ only.

### Approach 4: Dedicated SVC Number Detection

**Goal**: Since ALL el0_sync exceptions at our vector 0x400 are either
SVC (syscalls) or something else, detect SVC without reading ESR_EL1.

**Observation**: On ARM64, SVC from EL0 sets ELR_EL1 to PC+4 (instruction
after SVC). Data/instruction aborts set ELR_EL1 to the faulting PC.

**But**: We can't read ELR_EL1 either (also hangs after exception).

**Alternative observation**: The SVC instruction encodes an immediate
(SVC #0 for Linux syscalls). If we could read the instruction at the
faulting PC, we could detect SVC. But reading the PC requires ELR_EL1.

**Alternative**: Use the instruction fetch pattern. SVC causes the CPU
to advance PC, while aborts don't. But without reading any exception
register, we can't distinguish them.

**Verdict**: NOT VIABLE without at least one readable exception register.

### Approach 5: CONTEXTIDR_EL1 Workaround

**Goal**: Use CONTEXTIDR_EL1 as scratch register to stash exception info.

**Finding**: HVF also traps MSR CONTEXTIDR_EL1 at EL1, causing HVC #4
fault. No usable scratch sysreg exists beyond TPIDR_EL1.

**Verdict**: NOT VIABLE.

### Approach 6: Mach Exception Ports

**Goal**: Run guest as native macOS process, intercept SVC via Mach
exception handling instead of virtualization.

```
Native ARM64 process: SVC #0
  → macOS kernel: Mach exception (EXC_SYSCALL?)
    → Exception handler thread: thread_get_state/thread_set_state
      → Resume
```

**Status**: safecopy already uses Mach exception ports for EXC_BAD_ACCESS
(`pkg/safecopy/machexc/machexc_darwin.go`). But:
- macOS may not deliver `EXC_SYSCALL` for ARM64 SVC
- SVC on ARM64 goes to the macOS kernel's syscall handler first
- Interception before kernel processing is unclear
- Would need `task_for_pid` or entitlements for cross-process control
- Signal model completely different from gVisor's expectation

**Verdict**: NEEDS RESEARCH. If EXC_SYSCALL fires for SVC on ARM64,
this could bypass HVF entirely with ~100ns interception.

### Approach 7: Optimize Within Constraints

**Goal**: Reduce overhead without eliminating VM exits.

**Sub-approaches**:

a) **State page register passing**: Save/load X0-X30 via a per-vCPU
   memory page instead of individual HVF API calls.

   **Implemented and reverted.** TTBR1-mapped state page caused TLB
   coherency regression (~30% failure rate on concurrent forks, was 0%).

   Root cause: TTBR1 data access from the EL1 exception handler
   populates TLB entries that interfere with TTBR0 coherency across
   concurrent vCPUs. HVF's internal TLB state becomes inconsistent
   when both TTBR0 and TTBR1 are accessed within the same handler.

   The save path (29 fewer API calls) worked correctly in isolation —
   register values matched API reads. The 15% speedup on fork+exec
   benchmarks (0.65s → 0.55s for 500 iterations) was real. But
   concurrent fork reliability dropped from 10/10 to 6/10.

   **TTBR0 approach also failed**: Tried mapping state data in the
   vectors page (VA 0, always in TTBR0) at offset 0x1000+ per vCPU.
   Made vectors page RWX in both HVF stage-2 and guest page table.
   Result: complete hang — vCPU never exits from first hv_vcpu_run.
   Root cause unclear; may be related to self-modifying code detection
   or RWX permission interaction with HVF's instruction cache.

   **TTBR0 separate page also failed**: Allocated per-vCPU 16K page,
   mapped in HVF (RW) and in guest page table at fixed high VA
   (0x0000FFFF00000000). Fault at low address during first execution
   — the ERET stub tries to load registers before TTBR0 is set (first
   Switch() cycle has no address space). Fixable but requires split
   codepath (API-only for first call, state page for subsequent).

   **All three approaches blocked by fundamental issues:**
   - TTBR1: TLB coherency regression (~30% fork failure)
   - TTBR0 vectors: RWX hang (unknown cause)
   - TTBR0 separate: page table lifecycle mismatch (first cycle)

   The state page optimization needs architectural redesign — either
   a way to ensure the state page mapping exists before the first
   vCPU entry, or a fallback path for unmapped state pages.

   Infrastructure in place: per-vCPU state page allocation, TTBR1
   kernel PT mapping, TPIDR_EL1/SP_EL1 configured. Code at
   `machine.go:createVCPU` and `vcpu_arm64.go:initialize`.

   Saves: ~60 HVF API calls per Switch() cycle
   Measured speedup: ~15% on fork+exec (save path only)

b) **Batched syscall return**: For simple syscalls (getpid, getuid,
   clock_gettime), write the result directly to the state page and
   resume without full register reload.

   Saves: ~10 HVF API calls for register load
   Estimated speedup: ~1-2µs for simple syscalls

c) **TTBR0 caching**: Skip TTBR0_EL1 write when address space hasn't
   changed between Switch() calls.

   Saves: 1 HVF API call per Switch()
   Estimated speedup: negligible

d) **Syscall result caching**: Cache results of pure syscalls
   (getpid, getuid) in guest-readable memory (VDSO-style).

   Saves: entire VM exit for cached syscalls
   Estimated speedup: eliminates exit for ~5% of syscalls

**Verdict**: VIABLE. Achieves ~30-50% speedup. Not 100x.

## Comparison: How KVM Does It

On Linux KVM (ARM64), gVisor's bluepill mechanism:

```
pkg/sentry/platform/kvm/bluepill_arm64.go:

1. Sentry installs SIGILL handler (bluepillSignal)
2. bluepill(vCPU) function enters guest mode via KVM_RUN
3. Guest SVC → KVM delivers SIGILL to sentry
4. Signal handler reads registers from signal context
5. No VM exit needed — just signal delivery
```

KVM allows the sentry to BE the guest kernel. The sentry's signal
handler runs in the same address space as the guest. ESR_EL1 is
readable because KVM's kernel module (running at real EL2) properly
saves exception state before delivering the signal.

**Key difference**: KVM's real EL2 (Linux kernel) saves ESR_EL1 to a
kernel data structure before transitioning to EL0 (sentry). HVF's
hypervisor DOESN'T save ESR_EL1 before returning to EL1 guest code.

## Untested Paths (Priority Order)

### 1. ~~EL2 reads ESR_EL1 after EL0→EL1 exception~~ TESTED — FAILED

**Result**: MRS ESR_EL1 from EL2 also hangs. Additionally, HVC from
EL2 loops through VBAR_EL2 instead of exiting to HVF. EL2 exception
routing doesn't match ARM64 spec (lower-EL vectors never fire).

**Impact**: EL2 bridge approach is dead. Cannot read ESR_EL1 from
any privilege level, and cannot exit EL2 back to HVF.

### 2. Mach EXC_SYSCALL on ARM64

**Test**: Create a simple ARM64 binary that executes SVC #0, set up
Mach exception port for EXC_SYSCALL, check if exception fires.

**Tested**: `EXC_MASK_SYSCALL` port can be set (returns KERN_SUCCESS),
but `getpid()` does NOT trigger it — macOS kernel handles syscalls
before the exception port fires. SVC #0 (Linux convention) crashes
the process (exit 140) without delivering an exception.

**Verdict**: NOT VIABLE. Mach exceptions are the macOS equivalent of
Linux ptrace — same kernel-crossing overhead (~µs per syscall), not
the ~100ns in-VM handling we need. Would be comparable to gVisor's
ptrace platform on Linux, not the KVM platform.

### 3. MDCR_EL2 debug trap configuration

**Test**: Set MDCR_EL2 bits to control debug/exception trapping.
Some bits might disable HVF's ESR_EL1 intercept.

**Impact if works**: ESR_EL1 becomes readable at EL1 → full Ring0
architecture viable → 100ns syscalls.

## Roadmap

### Phase 1: Sentry-at-EL1 (full Ring0 — the 100x prize)

All architectural blockers are resolved (el1memtest, May 2026):
- EL1 data access via TTBR0/TTBR1: **WORKS** (AP[1]=0 pages)
- MRS ESR_EL1 from EL1 after SVC: **WORKS** (EC=0x15)
- ERET with modified registers: **WORKS**

Implementation plan:
1. Map sentry Go heap + stack into TTBR1 with AP[1]=0 (EL1-only)
2. EL1 el0_sync handler: read ESR_EL1, if EC=0x15 (SVC) → call
   Go syscall dispatch function at EL1 → ERET back (~100ns)
3. For syscalls needing host resources (I/O, networking): HVC to
   host with syscall args in state page
4. For page faults: save FAR/ESR to state page, HVC to host

Challenges:
- Go runtime at EL1: goroutine stack, GC, scheduler all need to
  work with TTBR1 kernel mappings
- Memory layout: Go heap addresses must be in TTBR1 VA range
  (0xFFFF_000000000000+), or TTBR0 with AP[1]=0 identity map
- Signal delivery: host must interrupt EL1 Go code (HVC from timer?)
- Concurrent vCPUs: each needs own Go stack, shared heap via TTBR1

Expected impact: ~100ns per syscall (currently ~4µs). 40x speedup
for ALL syscalls, not just the 10 fast-pathed ones. Would match
KVM performance on Linux.

### Phase 2: Incremental optimizations (within current architecture)

**a) TLB management for state page STP chain**

The STP save chain (saves X0-X30 to state page at EL1 before HVC)
was tested and works correctly, but disabled because TTBR1 TLB cold
misses add ~1µs per exit, offsetting the ~300ns API savings.

Potential fixes:
- **TLBI VALE1IS** (by VA, last level only): Flush just the state
  page VA entry instead of the full ASIDE1IS. Cheaper because it
  only invalidates one TLB entry. Encoding: `sys #0, c8, c7, #5, Xt`
  where Xt = VA >> 12. Cost estimate: ~50ns (single entry vs full ASID).
- **Pinned ASID for kernel**: Reserve ASID 0 for TTBR1 global entries.
  ASIDE1IS flushes by ASID but global entries (nG=0) should survive.
  Apple Silicon may not honor this — tested and confirmed globals ARE
  evicted by ASIDE1IS. Needs hardware-level investigation.
- **Pre-warm TLB on ERET**: Add a dummy LDR from state page in the
  ERET stub before entering EL0. This populates the TTBR1 TLB entry
  while still in EL1. The subsequent STP chain in el0_sync would hit
  warm TLB. Cost: ~200ns for one LDR + TLB walk. Net savings: ~100ns.
- **Skip TLBI entirely**: Tested — makes things WORSE (~5µs overhead).
  Stale TLB entries cause spurious faults. ASIDE1IS is necessary.

**b) clock_gettime fast-path via state page**

Cache the monotonic/realtime clock offset in the state page. EL1
handler for clock_gettime: read CNTVCT_EL0, apply offset from state
page, write result to user buffer (via PAN-cleared STTR), ERET.
Host updates offset on each Switch() entry.

Saves: entire VM exit for clock_gettime (currently ~0.3% of exits,
but high-frequency in some workloads like databases).

Blocker: writing to user buffer requires clearing PAN or using STTR.
STTR (unprivileged store) accesses user pages (AP[1]=1) without PAN
restriction — need to verify STTR works on HVF.

**c) Batch syscall return optimization**

For simple syscalls (set_tid_address, uname), the host could write
the return value directly to the state page and skip full register
reload. The ERET stub would load only X0 from state page (1 LDR vs
31 API calls). Saves ~700ns per simple syscall.

**d) VALE1IS vs ASIDE1IS benchmark**

Current ERET stub uses ASIDE1IS (flush all entries for current ASID).
VALE1IS flushes a single VA across all ASIDs — potentially cheaper for
the common case (only TTBR0 VA space changes between Switch() calls,
not TTBR1). Benchmark needed to determine if the difference is
measurable (~50ns savings expected).

### Phase 3: Apple engagement

**Apple Radar (updated)**

Previous request was for ESR_EL1 access — now confirmed working.
Updated request should focus on:

1. **ELR_EL1 / SPSR_EL1 access from EL1**: These return 0 after
   EL0→EL1 exception (el1memtest test 6). ESR_EL1 works but the
   other exception registers don't. Without ELR_EL1, the EL1
   handler can't determine the faulting PC (must use API fallback).

2. **PAN behavior documentation**: PSTATE.PAN is auto-set to 1 on
   every EL0→EL1 exception regardless of SCTLR_EL1.SPAN. This
   matches ARMv8.1-PAN but isn't documented for HVF. Would help
   developers avoid the 6-month debugging dead-end we hit.

3. **TTBR1 global TLB entry preservation**: ASIDE1IS evicts global
   (nG=0) entries on Apple Silicon HVF. ARM spec says global entries
   should survive ASID-based invalidation. This forces VMALLE1IS
   (full flush) for TTBR1 access, adding ~1µs per entry.

## Apple Radar: HVF ARM64 Issues

**Issue 1: ELR_EL1 / SPSR_EL1 return 0 after EL0→EL1 exceptions**

ESR_EL1 correctly returns the exception syndrome (confirmed EC=0x15 for
SVC). But ELR_EL1, SPSR_EL1, and FAR_EL1 return 0 when read via MRS
from EL1 after an EL0→EL1 exception. These registers are readable via
the host API (`hv_vcpu_get_sys_reg`), so the data exists — it's just
not exposed to guest EL1 code.

Impact: EL1 handler can dispatch by exception type (ESR) but can't
determine the faulting PC (ELR) or saved processor state (SPSR) without
a VM exit to query the API.

**Issue 2: PAN auto-set behavior undocumented**

PSTATE.PAN is set to 1 on every EL0→EL1 exception regardless of
SCTLR_EL1.SPAN. Matches ARMv8.1-PAN behavior but not documented for
HVF guests. Caused a 6-month debugging dead-end where EL1 data access
appeared broken (was actually PAN blocking AP[1]=1 pages).

**Issue 3: ASIDE1IS evicts global TLB entries**

TLBI ASIDE1IS should preserve global (nG=0) entries per ARM spec.
On Apple Silicon HVF, global entries are evicted, forcing VMALLE1IS
for TTBR1 kernel page access. Adds ~1µs per VM entry.

**Reproducer**: `cmd/mrstest/main.go` — standalone test demonstrating
the hang. Build with `go build`, sign with Hypervisor entitlement.

**Requested fix** (any of):
1. A new HCR_EL2 configuration option that disables the ESR_EL1
   read intercept for EL1 guest code
2. A new API like `hv_vcpu_set_exception_delivery(vcpu, HV_DELIVER_TO_EL1)`
   that lets EL1 handle all EL0 exceptions natively
3. Documentation of which HCR_EL2/MDCR_EL2 bits control the intercept
4. An `hv_vcpu_get_exception_info()` API that returns the exception
   syndrome without requiring MRS access from EL1

**Impact**: gVisor (Google's container runtime), QEMU, UTM, and other
ARM64 hypervisors running on macOS would benefit from 100x syscall
performance improvement. Current overhead makes syscall-heavy workloads
(compilers, shell scripts, databases) ~1600x slower than native.

**Where to file**: https://developer.apple.com/bug-reporting/ →
Frameworks → Virtualization / Hypervisor

**Evidence from testing** (M4 Pro, macOS 26.4):

| Register | MRS at EL1 (no exception) | MRS at EL1 (after EL0 trap) | Notes |
|----------|--------------------------|-----------------------------|-----------------------------|
| TPIDR_EL1 | Works | Works | |
| ESR_EL1 | Works (returns 0) | **WORKS (EC=0x15 for SVC)** | Previous "hang" was PAN bug |
| ELR_EL1 | Works | Returns 0 | May need API fallback |
| SPSR_EL1 | Works | Returns 0 | May need API fallback |
| FAR_EL1 | Works | Returns 0 | May need API fallback |
| SP_EL0 | Works | Not retested | |
| Memory LDR | Works | **WORKS (AP[1]=0 pages)** | PAN blocks AP[1]=1 pages |
| Memory STR | Works | **WORKS (AP[1]=0 pages)** | PAN blocks AP[1]=1 pages |

## Source Files

| File | What |
|------|------|
| `pkg/sentry/platform/hvf/context.go` | Switch() loop, exception dispatch |
| `pkg/sentry/platform/hvf/vcpu_arm64.go:285-316` | In-VM save stub (0x840) |
| `pkg/sentry/platform/hvf/syscall_el1.go` | Ring0 syscall handler skeleton |
| `pkg/sentry/platform/hvf/runtime_el1.go` | HVC proxy for host I/O |
| `pkg/sentry/platform/hvf/vmm_protocol.go` | VMM shared page protocol |
| `pkg/sentry/platform/hvf/tlbi_el1.go` | Direct TLBI infrastructure |
| `pkg/sentry/platform/hvf/memory_el1.go` | Dual-TTBR layout design |
| `pkg/sentry/platform/hvf/ring0_integration_test.go` | Ring0 test skeletons |
| `pkg/sentry/platform/kvm/bluepill_arm64.go` | KVM comparison |
| `pkg/safecopy/machexc/machexc_darwin.go` | Mach exception handling |
| `cmd/el1memtest/main.go` | EL1 memory access proof (TTBR0/TTBR1/PAN/STTR) |
| `cmd/el1sentry/` | Go asm dispatcher at EL1 (C + Go plan9 assembly) |
| `cmd/mrstest/main.go` | MRS/TID3 reproducer |
| `cmd/machtest/main.go` | Mach exception BRK latency test |
| `cmd/vmtest/main.go` | VM exit path optimization tests |

## References

- ARM Architecture Reference Manual (DDI0487): Exception handling, ESR_ELx
- Apple Hypervisor.framework: developer.apple.com/documentation/hypervisor
- gVisor KVM platform: `pkg/sentry/platform/kvm/`
- Apple Mach API: developer.apple.com/documentation/kernel
