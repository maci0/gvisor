// Copyright 2024 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build darwin && arm64

package hvf

/*
#include <Hypervisor/Hypervisor.h>
#include <dispatch/dispatch.h>
#include <dlfcn.h>

// vcpuRunUntilForever calls hv_vcpu_run_until(vcpu, FOREVER) via dlsym
// to avoid Bazel deployment target issues. Falls back to hv_vcpu_run.
static hv_return_t vcpuRunUntilForever(hv_vcpu_t vcpu) {
    typedef hv_return_t (*run_until_fn)(hv_vcpu_t, uint64_t);
    static run_until_fn fn = NULL;
    static dispatch_once_t once;
    dispatch_once(&once, ^{
        fn = (run_until_fn)dlsym(RTLD_DEFAULT, "hv_vcpu_run_until");
    });
    if (fn) {
        return fn(vcpu, ~(uint64_t)0); // HV_DEADLINE_FOREVER = ~0ULL
    }
    return hv_vcpu_run(vcpu); // fallback
}

static inline void memoryBarrier(void) {
    __asm__ __volatile__("dsb ish" ::: "memory");
}

// clearExclusiveMonitor clears any stale exclusive reservation on
// the current core, preventing unpredictable STXR behavior from
// a leftover LDXR reservation of a previous vCPU run.
static inline void clearExclusiveMonitor(void) {
    __asm__ __volatile__("clrex" ::: "memory");
}
*/
import "C"

import (
	"encoding/binary"
	"fmt"
	"os"
	"sync/atomic"
	"time"

	"gvisor.dev/gvisor/pkg/abi/linux"
	pkgcontext "gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/sentry/arch"
	"gvisor.dev/gvisor/pkg/sentry/platform"
	"gvisor.dev/gvisor/pkg/sentry/platform/interrupt"
)

// Performance counters for Switch() hot path.
var (
	statSwitchCount atomic.Int64
	statLoadRegNs   atomic.Int64
	statVcpuRunNs   atomic.Int64
	statSaveRegNs   atomic.Int64
	statFaultCount  atomic.Int64
	statSyscallNr   [512]atomic.Int64
)

// DumpStats writes performance stats to a file.
func DumpStats(path string) {
	n := statSwitchCount.Load()
	if n == 0 {
		return
	}
	f, err := os.Create(path)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "=== HVF Performance Stats (syscall exits only) ===\n")
	fmt.Fprintf(f, "Syscall exits:     %d\n", n)
	fmt.Fprintf(f, "Fault exits:       %d\n", statFaultCount.Load())
	fmt.Fprintf(f, "Avg loadRegisters: %d ns\n", statLoadRegNs.Load()/n)
	fmt.Fprintf(f, "Avg hv_vcpu_run:   %d ns\n", statVcpuRunNs.Load()/n)
	fmt.Fprintf(f, "Avg saveRegisters: %d ns\n", statSaveRegNs.Load()/n)
	fmt.Fprintf(f, "Avg total Switch:  %d ns\n", (statLoadRegNs.Load()+statVcpuRunNs.Load()+statSaveRegNs.Load())/n)
	fmt.Fprintf(f, "\n=== Syscall Frequency (VM exits only, fast-path handled in-VM) ===\n")
	type sc struct {
		nr    int
		count int64
	}
	var all []sc
	for i := range statSyscallNr {
		if c := statSyscallNr[i].Load(); c > 0 {
			all = append(all, sc{i, c})
		}
	}
	for i := 0; i < len(all); i++ {
		for j := i + 1; j < len(all); j++ {
			if all[j].count > all[i].count {
				all[i], all[j] = all[j], all[i]
			}
		}
	}
	for _, s := range all {
		pct := float64(s.count) * 100 / float64(n)
		fmt.Fprintf(f, "  nr=%-4d  %8d  (%5.1f%%)\n", s.nr, s.count, pct)
	}
}

// abortAccessType returns the access type for a data or instruction abort
// based on the exception class and ISS (Instruction Specific Syndrome).
func abortAccessType(ec, iss uint64) hostarch.AccessType {
	if ec == 0x20 || ec == 0x21 { // Instruction abort
		return hostarch.Execute
	}
	if iss&0x40 != 0 { // WnR bit (Write not Read)
		return hostarch.Write
	}
	return hostarch.Read
}

// hvfContext implements platform.Context and platform.SignalMasker.
type hvfContext struct {
	machine     *machine
	info        linux.SignalInfo
	interrupt   interrupt.Forwarder
	sigMask     uint64 // cached signal mask for in-VM sigprocmask
	sigDirty    bool   // true if EL1 handler modified the mask
	lastVCPU    *vCPU  // last vCPU used (for state page access)
}

// SetCachedSignalMask implements platform.SignalMasker.
func (c *hvfContext) SetCachedSignalMask(mask uint64) {
	c.sigMask = mask
}

// GetCachedSignalMask implements platform.SignalMasker.
func (c *hvfContext) GetCachedSignalMask() (uint64, bool) {
	if !c.sigDirty {
		return 0, false
	}
	c.sigDirty = false
	return c.sigMask, true
}

// Switch runs guest code until a syscall or fault occurs.
func (c *hvfContext) Switch(
	_ pkgcontext.Context,
	mm platform.MemoryManager,
	ac *arch.Context64,
	_ int32,
) (*linux.SignalInfo, hostarch.AccessType, error) {
	// Get the current task's address space.
	var as *addressSpace
	if mm != nil {
		if a, ok := mm.AddressSpace().(*addressSpace); ok {
			as = a
		}
	}

	// Acquire a vCPU.
	vcpu := c.machine.Get()

	// Enable interrupt forwarding to this vCPU.
	if !c.interrupt.Enable(vcpu) {
		c.machine.Put(vcpu)
		return nil, hostarch.NoAccess, platform.ErrContextInterrupt
	}

	// returnAndRelease releases vCPU resources. No memory sync needed:
	// per-MM page tables handle address space isolation, and MemoryFile
	// pages are mapped directly via the IPA allocator.
	returnAndRelease := func(si *linux.SignalInfo, at hostarch.AccessType, err error) (*linux.SignalInfo, hostarch.AccessType, error) {
		// Read back signal mask if EL1 handler modified it.
		sp := (*[16384]byte)(vcpu.statePageHost)
		if binary.LittleEndian.Uint64(sp[spOffsetSigDirty:]) != 0 {
			c.sigMask = binary.LittleEndian.Uint64(sp[spOffsetSigMask:])
			c.sigDirty = true
		}
		c.interrupt.Disable()
		c.machine.Put(vcpu)
		return si, at, err
	}

	// Reset FP loaded flag — vCPU may be reused for a different task.
	vcpu.fpLoaded = false
	c.lastVCPU = vcpu

	// Write signal mask to state page for in-VM sigprocmask handler.
	binary.LittleEndian.PutUint64(
		(*[16384]byte)(vcpu.statePageHost)[spOffsetSigMask:], c.sigMask)
	binary.LittleEndian.PutUint64(
		(*[16384]byte)(vcpu.statePageHost)[spOffsetSigDirty:], 0)

	unknownExits := 0
	vtimerExits := 0
	skipAll := false
	for {
		t0 := time.Now()
		vcpu.loadRegisters(ac, skipAll)
		skipAll = false
		t1 := time.Now()

		C.clearExclusiveMonitor()
		C.memoryBarrier()

		if as != nil && as.pt != nil {
			// ASID rotation: each Switch() gets a unique ASID to avoid
			// TLB aliasing. TLBI is needed every re-entry because the
			// sentry may have modified page tables between exits (e.g.,
			// mapping new pages after a fault).
			vcpu.asidCounter++
			asid := uint64(vcpu.asidCounter & 0xFFFF)
			if asid == 0 {
				asid = 1
				vcpu.asidCounter = 1
				vcpu.asidWrapped = true
			}
			ttbr := as.pt.ttbr0() | (asid << 48)
			vcpu.setSysReg(C.HV_SYS_REG_TTBR0_EL1, ttbr)
		}

		ret := C.vcpuRunUntilForever(vcpu.vcpuID)
		t2 := time.Now()

		C.memoryBarrier()

		if ret != C.HV_SUCCESS {
			return returnAndRelease(nil, hostarch.NoAccess, fmt.Errorf("hv_vcpu_run failed: %d", ret))
		}

		exitReason := vcpu.getExitReason()

		if exitReason != exitReasonVtimerActivated {
			vtimerExits = 0
		}

		switch exitReason {
		case exitReasonException:
			syndrome := vcpu.getExceptionSyndrome()
			ec := (syndrome >> 26) & 0x3f

			log.Debugf("HVF exit: ec=%#x syndrome=%#x", ec, syndrome)

			// EC=0x18: MSR/MRS trap from TID3 (direct EL2 trap, no HVC).
			if ec == 0x18 {
				vcpu.saveFP = true
				vcpu.saveRegisters(ac)
				iss := syndrome & 0x1ffffff
				if emulateSysreg(ac, iss) {
					ac.Regs.Pc = vcpu.getReg(C.HV_REG_PC) + 4
					continue
				}
				c.info = linux.SignalInfo{}
				c.info.Signo = int32(linux.SIGILL)
				return returnAndRelease(&c.info, hostarch.NoAccess, platform.ErrContextSignal)
			}

			if ec == 0x16 { // HVC from AArch64
				hvcImm := syndrome & 0xffff

				// HVC #9: SVC exit from el0_sync handler.
				// GP regs saved to state page by STP chain.
				// Skip FP save — syscall dispatch doesn't touch FP.
				if hvcImm == 9 {
					vcpu.gpInStatePage = true
					vcpu.saveFP = false
					vcpu.saveRegisters(ac)
					t3 := time.Now()
					statSwitchCount.Add(1)
					statLoadRegNs.Add(t1.Sub(t0).Nanoseconds())
					statVcpuRunNs.Add(t2.Sub(t1).Nanoseconds())
					statSaveRegNs.Add(t3.Sub(t2).Nanoseconds())
					if nr := ac.Regs.Regs[8]; nr < 512 {
						statSyscallNr[nr].Add(1)
					}
					log.Debugf("HVF syscall (hvc#9): nr=%d x0=%#x x1=%#x x2=%#x pc=%#x",
						ac.Regs.Regs[8], ac.Regs.Regs[0], ac.Regs.Regs[1], ac.Regs.Regs[2], ac.Regs.Pc)
					return returnAndRelease(nil, hostarch.NoAccess, nil)
				}

				// HVC #0/#8: fault or other exception. The el0_sync
				// handler saved the original ESR_EL1 in X18 before HVC
				// (HVC overwrites ESR_EL1 with HVC syndrome).
				if hvcImm == 0 || hvcImm == 8 {
					statFaultCount.Add(1)
					// Read ESR before saveRegisters to determine FP save need.
					var esrEL1 uint64
					if hvcImm == 8 {
						esrEL1 = vcpu.getReg(C.HV_REG_X18)
					} else {
						esrEL1 = vcpu.getSysReg(C.HV_SYS_REG_ESR_EL1)
					}
					origEC := (esrEL1 >> 26) & 0x3f
					vcpu.saveFP = origEC != 0x15 // skip FP for SVC fallback
					vcpu.saveRegisters(ac)

					if origEC == 0x15 { // SVC from AArch64 (fallback)
						statSwitchCount.Add(1)
						if nr := ac.Regs.Regs[8]; nr < 512 {
							statSyscallNr[nr].Add(1)
						}
						log.Debugf("HVF syscall (hvc#%d fallback): nr=%d pc=%#x",
							hvcImm, ac.Regs.Regs[8], ac.Regs.Pc)
						return returnAndRelease(nil, hostarch.NoAccess, nil)
					}

					// Data/instruction abort: EC=0x24/0x20 (lower EL, guest at EL0) or
					// EC=0x25/0x21 (current EL, e.g. fault in EL1 stub).
					if origEC == 0x24 || origEC == 0x20 || origEC == 0x25 || origEC == 0x21 {
						far := vcpu.getSysReg(C.HV_SYS_REG_FAR_EL1)
						log.Debugf("HVF fault (hvc#%d): EC=%#x FAR=%#x ISS=%#x PC=%#x",
							hvcImm, origEC, far, esrEL1&0x1ffffff, vcpu.getSysReg(C.HV_SYS_REG_ELR_EL1))
						c.info = linux.SignalInfo{}
						c.info.Signo = int32(linux.SIGSEGV)
						c.info.SetAddr(far)
						return returnAndRelease(&c.info, abortAccessType(origEC, esrEL1&0x1ffffff), platform.ErrContextSignal)
					}

					// EC=0x18: MSR/MRS/System instruction trap. Emulate
					// ID register reads like Linux does for EL0 userspace.
					if origEC == 0x18 {
						iss := esrEL1 & 0x1ffffff
						if emulateSysreg(ac, iss) {
							// ELR_EL1 points to the trapping MRS instruction.
							// Advance past it (4 bytes) to resume at the next instruction.
							ac.Regs.Pc = vcpu.getSysReg(C.HV_SYS_REG_ELR_EL1) + 4
							continue
						}
					}

					// Other exception — deliver SIGILL.
					log.Warningf("HVF: unhandled exception (hvc#%d): origEC=%#x ESR_EL1=%#x ELR_EL1=%#x",
						hvcImm, origEC, esrEL1, vcpu.getSysReg(C.HV_SYS_REG_ELR_EL1))
					c.info = linux.SignalInfo{}
					c.info.Signo = int32(linux.SIGILL)
					return returnAndRelease(&c.info, hostarch.NoAccess, platform.ErrContextSignal)
				}

				if hvcImm == 4 {
					// Current-EL sync fault (vector 0x200). X18=ESR, X17=FAR.
					esrVal := vcpu.getReg(C.HV_REG_X18)
					farVal := vcpu.getReg(C.HV_REG_X17)
					pc := vcpu.getReg(C.HV_REG_PC)
					ec4 := (esrVal >> 26) & 0x3f
					dfsc := esrVal & 0x3f
					log.Warningf("HVF: current-EL fault: ESR=%#x (EC=%#x DFSC=%#x level=%d) FAR=%#x PC=%#x",
						esrVal, ec4, dfsc, dfsc&3, farVal, pc)
				}
				vcpu.saveFP = true
				vcpu.saveRegisters(ac)
				log.Warningf("HVF: unexpected HVC #%d, syndrome=%#x", hvcImm, syndrome)
				c.info = linux.SignalInfo{}
				c.info.Signo = int32(linux.SIGILL)
				return returnAndRelease(&c.info, hostarch.NoAccess, platform.ErrContextSignal)
			}

			if ec == 0x24 || ec == 0x25 || ec == 0x20 || ec == 0x21 { // Direct data/instruction abort
				vcpu.saveFP = true
				vcpu.saveRegisters(ac)
				ac.Regs.Pc = vcpu.getReg(C.HV_REG_PC)
				far := vcpu.getFaultAddress()
				c.info = linux.SignalInfo{}
				c.info.Signo = int32(linux.SIGSEGV)
				c.info.SetAddr(far)
				return returnAndRelease(&c.info, abortAccessType(ec, syndrome&0x1ffffff), platform.ErrContextSignal)
			}

			vcpu.saveFP = true
			vcpu.saveRegisters(ac)
			pc := vcpu.getReg(C.HV_REG_PC)
			ac.Regs.Pc = pc
			c.info = linux.SignalInfo{}
			c.info.Signo = int32(linux.SIGILL)
			c.info.SetAddr(pc)
			log.Debugf("HVF: unhandled exception ec=%#x at PC=%#x, delivering SIGILL", ec, pc)
			return returnAndRelease(&c.info, hostarch.NoAccess, platform.ErrContextSignal)

		case exitReasonVtimerActivated:
			// Virtual timer fired — unmask and re-enter guest.
			// If the compare value is stuck in the past, the timer will
			// keep firing; after 10K consecutive exits, leave it masked.
			vtimerExits++
			if vtimerExits > 10000 {
				C.hv_vcpu_set_vtimer_mask(vcpu.vcpuID, C.bool(true))
			} else {
				C.hv_vcpu_set_vtimer_mask(vcpu.vcpuID, C.bool(false))
			}
			skipAll = true // no register changes on vtimer
			continue

		case exitReasonCanceled:
			pc := vcpu.getReg(C.HV_REG_PC)
			log.Debugf("HVF canceled: PC=%#x", pc)
			return returnAndRelease(nil, hostarch.NoAccess, platform.ErrContextInterrupt)

		default:
			unknownExits++
			if unknownExits > 1000 {
				return returnAndRelease(nil, hostarch.NoAccess,
					fmt.Errorf("HVF: too many unknown exit reasons (%d), last=%d", unknownExits, exitReason))
			}
			log.Debugf("HVF: unknown exit reason %d, retrying (%d)", exitReason, unknownExits)
			skipAll = true
			continue
		}
	}
}

// Interrupt implements platform.Context.Interrupt.
func (c *hvfContext) Interrupt() {
	c.interrupt.NotifyInterrupt()
}

// Preempt implements platform.Context.Preempt.
func (c *hvfContext) Preempt() {}

// Release implements platform.Context.Release.
func (c *hvfContext) Release() {}

// PullFullState implements platform.Context.PullFullState.
func (c *hvfContext) PullFullState(_ platform.AddressSpace, _ *arch.Context64) error {
	return nil
}

// FullStateChanged implements platform.Context.FullStateChanged.
// Called when the sentry modifies registers beyond the syscall return value
// (e.g., signal delivery, clone, execve, rt_sigreturn).
func (c *hvfContext) FullStateChanged() {}

// PrepareSleep implements platform.Context.PrepareSleep.
func (*hvfContext) PrepareSleep() {}

// PrepareUninterruptibleSleep implements platform.Context.PrepareUninterruptibleSleep.
func (*hvfContext) PrepareUninterruptibleSleep() {}

// PrepareStop implements platform.Context.PrepareStop.
func (*hvfContext) PrepareStop() {}

// PrepareExecve implements platform.Context.PrepareExecve.
func (*hvfContext) PrepareExecve() {}

// PrepareExit implements platform.Context.PrepareExit.
func (*hvfContext) PrepareExit() {}

// LastCPUNumber implements platform.Context.LastCPUNumber.
func (*hvfContext) LastCPUNumber() int32 { return 0 }

// emulateSysreg handles trapped MSR/MRS instructions (EC=0x18).
// The ISS encodes: direction, Op0, Op2, Op1, CRn, CRm, Rt.
// Like Linux, we emulate ID register reads for EL0 userspace.
func emulateSysreg(ac *arch.Context64, iss uint64) bool {
	// ISS encoding for EC=0x18 (MSR/MRS/System instruction trap):
	// [19:17] = Op2
	// [16:14] = Op1
	// [13:10] = CRn
	// [9:5]   = Rt (destination register)
	// [4:1]   = CRm
	// [0]     = direction: 0=MSR(write), 1=MRS(read)
	dir := iss & 1
	crm := (iss >> 1) & 0xf
	rt := (iss >> 5) & 0x1f
	crn := (iss >> 10) & 0xf
	op1 := (iss >> 14) & 0x7
	op2 := (iss >> 17) & 0x7

	if dir == 0 {
		return false // MSR (write) — not emulated
	}

	// Build a key from op1:crn:crm:op2 for lookup.
	key := (op1 << 12) | (crn << 8) | (crm << 4) | op2

	var val uint64
	switch key {
	// ID_AA64MMFR0_EL1: op1=0, CRn=0, CRm=7, op2=0 → key=0x0070
	case 0x0070:
		val = 0x101122 // Apple Silicon: 16-bit ASID, 40-bit PA, TGran16

	// ID_AA64MMFR1_EL1: op1=0, CRn=0, CRm=7, op2=1 → key=0x0071
	case 0x0071:
		val = 0x0

	// ID_AA64MMFR2_EL1: op1=0, CRn=0, CRm=7, op2=2 → key=0x0072
	case 0x0072:
		val = 0x0

	// ID_AA64PFR0_EL1: op1=0, CRn=0, CRm=4, op2=0 → key=0x0040
	case 0x0040:
		// EL0[3:0]=1 (AArch64), EL1[7:4]=1 (AArch64),
		// FP[19:16]=0 (implemented), AdvSIMD[23:20]=0 (implemented).
		val = 0x0011

	// ID_AA64PFR1_EL1: op1=0, CRn=0, CRm=4, op2=1 → key=0x0041
	case 0x0041:
		val = 0x0

	// ID_AA64ISAR0_EL1: op1=0, CRn=0, CRm=6, op2=0 → key=0x0060
	case 0x0060:
		// Match Apple M4 Pro features (must agree with /proc/cpuinfo):
		// AES[7:4]=2 (PMULL), SHA1[11:8]=1, SHA2[15:12]=2 (SHA512),
		// CRC32[19:16]=1, Atomic[23:20]=2 (LSE), RDM[31:28]=1,
		// SHA3[35:32]=1, DP[47:44]=1, FHM[51:48]=1.
		val = 0x0001100110212120

	// ID_AA64ISAR1_EL1: op1=0, CRn=0, CRm=6, op2=1 → key=0x0061
	case 0x0061:
		// DPB[3:0]=1, JSCVT[15:12]=1, FCMA[19:16]=1, LRCPC[23:20]=2.
		val = 0x0000000000211001

	// ID_AA64DFR0_EL1: op1=0, CRn=0, CRm=5, op2=0 → key=0x0050
	case 0x0050:
		val = 0x0

	// MIDR_EL1: op1=0, CRn=0, CRm=0, op2=0 → key=0x0000
	case 0x0000:
		// Implementer=0x61 (Apple), Variant=1, Part=0x022, Rev=0
		val = 0x611f0220

	// MPIDR_EL1: op1=0, CRn=0, CRm=0, op2=5 → key=0x0005
	case 0x0005:
		val = 0x80000000

	// CTR_EL0: op1=3, CRn=0, CRm=0, op2=1 → key=0x3001
	case 0x3001:
		val = 0x80038003 // 64-byte cache line

	// DCZID_EL0: op1=3, CRn=0, CRm=0, op2=7 → key=0x3007
	case 0x3007:
		val = 0x4 // 64-byte DC ZVA block

	// CLIDR_EL1: op1=1, CRn=0, CRm=0, op2=1 → key=0x1001
	case 0x1001:
		val = 0x0 // no caches described

	// CSSELR_EL1: op1=2, CRn=0, CRm=0, op2=0 → key=0x2000
	case 0x2000:
		val = 0x0

	default:
		log.Debugf("emulateSysreg: unhandled MRS key=%#x (op1=%d CRn=%d CRm=%d op2=%d) rt=x%d",
			key, op1, crn, crm, op2, rt)
		return false
	}

	if rt < 31 {
		ac.Regs.Regs[rt] = val
	}
	log.Debugf("emulateSysreg: key=%#x val=%#x → x%d", key, val, rt)
	return true
}

