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
#include <mach/mach_time.h>


static inline void memoryBarrier(void) {
    __asm__ __volatile__("dsb ish" ::: "memory");
}

// clearExclusiveMonitor clears any stale exclusive reservation on
// the current core. This prevents a stale LDXR reservation from a
// previous vCPU run from causing STXR to spuriously succeed.
static inline void clearExclusiveMonitor(void) {
    __asm__ __volatile__("clrex" ::: "memory");
}
*/
import "C"

import (
	"fmt"

	"gvisor.dev/gvisor/pkg/abi/linux"
	pkgcontext "gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/sentry/arch"
	"gvisor.dev/gvisor/pkg/sentry/platform"
	"gvisor.dev/gvisor/pkg/sentry/platform/interrupt"
)

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

// hvfContext implements platform.Context.
type hvfContext struct {
	// machine is the parent machine, and is immutable.
	machine *machine

	// info is the cached SignalInfo for this context.
	info linux.SignalInfo

	// interrupt is the interrupt forwarder.
	interrupt interrupt.Forwarder
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
		c.interrupt.Disable()
		c.machine.Put(vcpu)
		return si, at, err
	}

	for {
		// Load guest registers from arch.Context64.
		vcpu.loadRegisters(ac)

		C.clearExclusiveMonitor()
		C.memoryBarrier()

		if as != nil && as.pt != nil {
			vcpu.asidCounter++
			asid := uint64(vcpu.asidCounter & 0xFFFF)
			if asid == 0 {
				asid = 1
				vcpu.asidCounter = 1
			}
			ttbr := as.pt.ttbr0() | (asid << 48)
			vcpu.setSysReg(C.HV_SYS_REG_TTBR0_EL1, ttbr)
		}

		ret := C.hv_vcpu_run(vcpu.vcpuID)

		C.memoryBarrier()

		if ret != C.HV_SUCCESS {
			return returnAndRelease(nil, hostarch.NoAccess, fmt.Errorf("hv_vcpu_run failed: %d", ret))
		}

		// Save guest registers back.
		vcpu.saveRegisters(ac)

		// Check exit reason.
		exitReason := vcpu.getExitReason()

		switch exitReason {
		case exitReasonException:
			syndrome := vcpu.getExceptionSyndrome()
			ec := (syndrome >> 26) & 0x3f

			log.Debugf("HVF exit: ec=%#x syndrome=%#x", ec, syndrome)
			if ec == 0x16 { // HVC from AArch64
				// HVC exit from our EL1 exception vector handler.
				// Extract the HVC immediate from ESR bits [15:0] to
				// determine which vector entry triggered this exit.
				hvcImm := syndrome & 0xffff

				// HVC #0: current-EL sync, HVC #8: lower-EL sync (el0_sync).
				if hvcImm == 0 || hvcImm == 8 {
					// Check ESR_EL1 to determine the original exception.
					esrEL1 := vcpu.getSysReg(C.HV_SYS_REG_ESR_EL1)
					origEC := (esrEL1 >> 26) & 0x3f

					if origEC == 0x15 { // SVC from AArch64
						// Syscall! saveRegisters set Pc from ELR_EL1
						// (which points after the SVC - ARM auto-advances).
						log.Debugf("HVF syscall (hvc#%d): nr=%d x0=%#x x1=%#x x2=%#x pc=%#x",
							hvcImm, ac.Regs.Regs[8], ac.Regs.Regs[0], ac.Regs.Regs[1], ac.Regs.Regs[2], ac.Regs.Pc)
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

					// Other exception — deliver SIGILL.
					log.Warningf("HVF: unhandled exception (hvc#%d): origEC=%#x ESR_EL1=%#x ELR_EL1=%#x",
						hvcImm, origEC, esrEL1, vcpu.getSysReg(C.HV_SYS_REG_ELR_EL1))
					c.info = linux.SignalInfo{}
					c.info.Signo = int32(linux.SIGILL)
					return returnAndRelease(&c.info, hostarch.NoAccess, platform.ErrContextSignal)
				}

				// Other HVC immediates (1-7: current-EL IRQ/FIQ/SError,
				// 9-15: lower-EL IRQ/FIQ/SError and AArch32 vectors).
				log.Warningf("HVF: unexpected HVC #%d, syndrome=%#x", hvcImm, syndrome)
				c.info = linux.SignalInfo{}
				c.info.Signo = int32(linux.SIGILL)
				return returnAndRelease(&c.info, hostarch.NoAccess, platform.ErrContextSignal)
			}

			if ec == 0x24 || ec == 0x25 || ec == 0x20 || ec == 0x21 { // Direct data/instruction abort
				// Stage-2 fault: guest accessed unmapped IPA.
				ac.Regs.Pc = vcpu.getReg(C.HV_REG_PC)
				far := vcpu.getFaultAddress()
				c.info = linux.SignalInfo{}
				c.info.Signo = int32(linux.SIGSEGV)
				c.info.SetAddr(far)
				return returnAndRelease(&c.info, abortAccessType(ec, syndrome&0x1ffffff), platform.ErrContextSignal)
			}

			// Unknown/unhandled exception (e.g., EC=0x19 SVE trap,
			// EC=0x00 undefined instruction). Deliver SIGILL so the
			// guest's signal handler can handle it (e.g., OpenSSL
			// CPUID probing uses SIGILL to detect unsupported instructions).
			// Read the actual PC from HV_REG_PC since the exception
			// was taken directly by the hypervisor (not via EL1 vectors).
			ac.Regs.Pc = vcpu.getReg(C.HV_REG_PC)
			c.info = linux.SignalInfo{}
			c.info.Signo = int32(linux.SIGILL)
			c.info.SetAddr(ac.Regs.Pc)
			log.Debugf("HVF: unhandled exception ec=%#x at PC=%#x, delivering SIGILL", ec, ac.Regs.Pc)
			return returnAndRelease(&c.info, hostarch.NoAccess, platform.ErrContextSignal)

		case exitReasonVtimerActivated:
			// Virtual timer fired — re-arm and continue. This wakes
			// the vCPU from WFE/WFI used by musl spinlocks and OpenSSL.
			C.hv_vcpu_set_vtimer_mask(vcpu.vcpuID, C.bool(true))
			C.hv_vcpu_set_vtimer_mask(vcpu.vcpuID, C.bool(false))
			continue

		case exitReasonCanceled:
			pc := vcpu.getReg(C.HV_REG_PC)
			log.Debugf("HVF canceled: PC=%#x", pc)
			return returnAndRelease(nil, hostarch.NoAccess, platform.ErrContextInterrupt)

		default:
			// HV_EXIT_REASON_UNKNOWN (3) can occur during heavy workloads.
			// Log and retry — the vCPU state is still valid.
			log.Debugf("HVF: unknown exit reason %d, retrying", exitReason)
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
