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
#include <stdlib.h>
#include <string.h>
#include <sys/mman.h>

static inline void memBarrier(void) {
	__asm__ __volatile__("dsb ish" ::: "memory");
}

// copyPageRaw copies a 16K page from src to dst without cgo pointer check.
static inline void copyPageRaw(void *dst, uint64_t src, size_t size) {
	memcpy(dst, (void *)src, size);
}

// vmMapRaw maps memory into the VM without Go's cgo pointer check.
// Used for demand-paging where the host address may be in Go's
// data/heap/stack segments.
static inline hv_return_t vmMapRaw(uint64_t hostAddr, uint64_t ipa,
                                    size_t size, hv_memory_flags_t flags) {
	return hv_vm_map((void *)hostAddr, (hv_ipa_t)ipa, size, flags);
}

// readR28C reads the ARM64 R28 register (Go's g pointer).
static inline uint64_t readR28C(void) {
	uint64_t val;
	__asm__ __volatile__("mov %0, x28" : "=r"(val));
	return val;
}
*/
import "C"

import (
	"fmt"
	"unsafe"

	"gvisor.dev/gvisor/pkg/log"
)

// bluepillMapHostPage maps a single host page into the VM's IPA space
// and the kernel page table (TTBR1). The host page is mapped at the
// same offset from kernelVABase, creating a predictable VA layout.
//
// hostAddr must be page-aligned.
// Returns the kernel VA that maps to this host page.
func (m *machine) bluepillMapHostPage(hostAddr uintptr) (kernelVA uint64, err error) {
	// Align to page boundary.
	pageHost := hostAddr &^ (hvfPageSize - 1)

	// Get or create IPA mapping for this host page.
	ipa, err := m.ipaAlloc.mapPage(pageHost, hvfPageSize)
	if err != nil {
		return 0, fmt.Errorf("bluepill mapPage host=%#x: %w", pageHost, err)
	}

	// Map into kernel page table at a VA derived from the host address.
	// Use a simple scheme: kernelVA = kernelVABase + (hostAddr % kernelVASize).
	// Since the kernel VA space is 64GB (36-bit), this wraps for very large
	// host addresses, but Go process memory is typically well within range.
	offset := uint64(pageHost) & ((1 << 36) - 1)
	kva := kernelVABase + offset

	if err := m.kernelPT.mapPage(kva, ipa, true); err != nil {
		return 0, fmt.Errorf("bluepill kernel mapPage va=%#x ipa=%#x: %w", kva, ipa, err)
	}

	return kva, nil
}

// bluepillMapRange maps a contiguous range of host memory into the
// VM's kernel address space (TTBR1). Returns the kernel VA base.
func (m *machine) bluepillMapRange(hostAddr uintptr, size uint64) (kernelVA uint64, err error) {
	baseHost := hostAddr &^ (hvfPageSize - 1)
	alignedSize := ((size + uint64(hostAddr-uintptr(baseHost))) + hvfPageSize - 1) &^ (hvfPageSize - 1)

	var baseKVA uint64
	for off := uint64(0); off < alignedSize; off += hvfPageSize {
		kva, err := m.bluepillMapHostPage(uintptr(baseHost) + uintptr(off))
		if err != nil {
			return 0, err
		}
		if off == 0 {
			baseKVA = kva
		}
	}

	// Add the sub-page offset.
	baseKVA += uint64(hostAddr - uintptr(baseHost))
	return baseKVA, nil
}

// bluepillTest runs a minimal test of EL1 execution inside the VM.
// It maps a small code stub into TTBR1, enters the vCPU at EL1,
// and verifies that HVC #0 exits cleanly.
//
// This is the proof-of-concept for sentry-as-ring0: if we can run
// ANY code at EL1 inside the VM, we can eventually run the entire
// sentry.
func (m *machine) bluepillTest(vcpu *vCPU) error {
	// Allocate a page for EL1 code: just HVC #0 + infinite loop.
	var codeMem unsafe.Pointer
	C.posix_memalign(&codeMem, C.size_t(hvfPageSize), C.size_t(hvfPageSize))
	if codeMem == nil {
		return fmt.Errorf("failed to allocate code page")
	}
	defer C.free(codeMem)
	C.memset(codeMem, 0, C.size_t(hvfPageSize))

	// Write test code: HVC #0 (exit to hypervisor).
	code := unsafe.Slice((*byte)(codeMem), hvfPageSize)
	// MOV X0, #0x42 (store marker value)
	code[0] = 0x40
	code[1] = 0x08
	code[2] = 0x80
	code[3] = 0xd2 // MOVZ X0, #0x42
	// HVC #0 (exit)
	code[4] = 0x02
	code[5] = 0x00
	code[6] = 0x00
	code[7] = 0xd4 // HVC #0

	// Map code into VM IPA space.
	codeIPA, err := m.ipaAlloc.mapPage(uintptr(codeMem), hvfPageSize)
	if err != nil {
		return fmt.Errorf("map code page: %w", err)
	}

	// Map code into kernel page table (TTBR1).
	codeKVA := uint64(kernelVABase) // Use first page of kernel VA space.
	if err := m.kernelPT.mapPage(codeKVA, codeIPA, false); err != nil {
		return fmt.Errorf("kernel map code: %w", err)
	}

	// Allocate a stack for EL1 execution.
	var stackMem unsafe.Pointer
	C.posix_memalign(&stackMem, C.size_t(hvfPageSize), C.size_t(hvfPageSize))
	if stackMem == nil {
		return fmt.Errorf("failed to allocate stack page")
	}
	defer C.free(stackMem)
	C.memset(stackMem, 0, C.size_t(hvfPageSize))

	stackIPA, err := m.ipaAlloc.mapPage(uintptr(stackMem), hvfPageSize)
	if err != nil {
		return fmt.Errorf("map stack page: %w", err)
	}

	stackKVA := uint64(kernelVABase) + hvfPageSize
	if err := m.kernelPT.mapPage(stackKVA, stackIPA, true); err != nil {
		return fmt.Errorf("kernel map stack: %w", err)
	}

	C.memBarrier()

	// Set vCPU to run at EL1 with our test code.
	// PC = kernel VA of code page.
	vcpu.setReg(C.HV_REG_PC, codeKVA)
	// CPSR = EL1h (0x3c5) with DAIF masked.
	vcpu.setReg(C.HV_REG_CPSR, 0x3c5)
	// SP = top of stack page (stack grows down).
	vcpu.setSysReg(C.HV_SYS_REG_SP_EL1, stackKVA+hvfPageSize-16)

	log.Infof("bluepill test: PC=%#x CPSR=%#x SP=%#x", codeKVA, 0x3c5, stackKVA+hvfPageSize-16)

	// Run the vCPU.
	ret := C.hv_vcpu_run(vcpu.vcpuID)
	if ret != C.HV_SUCCESS {
		return fmt.Errorf("hv_vcpu_run failed: %d", ret)
	}

	C.memBarrier()

	// Check exit reason.
	exitReason := vcpu.getExitReason()
	syndrome := vcpu.getExceptionSyndrome()
	ec := (syndrome >> 26) & 0x3f

	log.Infof("bluepill test: exit=%d ec=%#x syndrome=%#x", exitReason, ec, syndrome)

	if exitReason != exitReasonException {
		return fmt.Errorf("unexpected exit reason: %d (want exception)", exitReason)
	}

	if ec != 0x16 { // HVC from AArch64
		return fmt.Errorf("unexpected EC: %#x (want 0x16 HVC)", ec)
	}

	// Verify marker value in X0.
	x0 := vcpu.getReg(C.HV_REG_X0)
	if x0 != 0x42 {
		return fmt.Errorf("X0=%#x, want 0x42", x0)
	}

	log.Infof("bluepill test: SUCCESS — ran code at EL1, X0=%#x", x0)
	return nil
}

// bluepillTestMemory runs an EL1 test that reads and writes memory
// through TTBR1. The code stub writes a magic value to a data page,
// then HVC exits. The host verifies the data page was modified.
func (m *machine) bluepillTestMemory(vcpu *vCPU) error {
	// Allocate code page.
	var codeMem unsafe.Pointer
	C.posix_memalign(&codeMem, C.size_t(hvfPageSize), C.size_t(hvfPageSize))
	if codeMem == nil {
		return fmt.Errorf("failed to allocate code page")
	}
	defer C.free(codeMem)
	C.memset(codeMem, 0, C.size_t(hvfPageSize))

	// Allocate data page (will be written by EL1 code).
	var dataMem unsafe.Pointer
	C.posix_memalign(&dataMem, C.size_t(hvfPageSize), C.size_t(hvfPageSize))
	if dataMem == nil {
		return fmt.Errorf("failed to allocate data page")
	}
	defer C.free(dataMem)
	C.memset(dataMem, 0, C.size_t(hvfPageSize))

	// Allocate stack page.
	var stackMem unsafe.Pointer
	C.posix_memalign(&stackMem, C.size_t(hvfPageSize), C.size_t(hvfPageSize))
	if stackMem == nil {
		return fmt.Errorf("failed to allocate stack page")
	}
	defer C.free(stackMem)
	C.memset(stackMem, 0, C.size_t(hvfPageSize))

	// Map all three pages into VM IPA space.
	codeIPA, err := m.ipaAlloc.mapPage(uintptr(codeMem), hvfPageSize)
	if err != nil {
		return fmt.Errorf("map code: %w", err)
	}
	dataIPA, err := m.ipaAlloc.mapPage(uintptr(dataMem), hvfPageSize)
	if err != nil {
		return fmt.Errorf("map data: %w", err)
	}
	stackIPA, err := m.ipaAlloc.mapPage(uintptr(stackMem), hvfPageSize)
	if err != nil {
		return fmt.Errorf("map stack: %w", err)
	}

	// Map into kernel page table at consecutive upper-half VAs.
	codeKVA := uint64(kernelVABase)
	dataKVA := uint64(kernelVABase) + hvfPageSize
	stackKVA := uint64(kernelVABase) + 2*hvfPageSize

	if err := m.kernelPT.mapPage(codeKVA, codeIPA, false); err != nil {
		return fmt.Errorf("kernel map code: %w", err)
	}
	if err := m.kernelPT.mapPage(dataKVA, dataIPA, true); err != nil {
		return fmt.Errorf("kernel map data: %w", err)
	}
	if err := m.kernelPT.mapPage(stackKVA, stackIPA, true); err != nil {
		return fmt.Errorf("kernel map stack: %w", err)
	}

	// Write test code that:
	//   1. Loads data page address into X1
	//   2. Writes magic value 0xDEADBEEF to [X1]
	//   3. Writes 0xCAFEBABE to [X1+8]
	//   4. Reads [X1] back into X0
	//   5. HVC #0 exits
	code := unsafe.Slice((*byte)(codeMem), hvfPageSize)
	i := 0
	// MOVZ X1, #(dataKVA & 0xFFFF)
	putInstr(code, &i, 0xd2800001|((uint32(dataKVA)&0xFFFF)<<5))
	// MOVK X1, #((dataKVA>>16) & 0xFFFF), LSL #16
	putInstr(code, &i, 0xf2a00001|((uint32(dataKVA>>16)&0xFFFF)<<5))
	// MOVK X1, #((dataKVA>>32) & 0xFFFF), LSL #32
	putInstr(code, &i, 0xf2c00001|((uint32(dataKVA>>32)&0xFFFF)<<5))
	// MOVK X1, #((dataKVA>>48) & 0xFFFF), LSL #48
	putInstr(code, &i, 0xf2e00001|((uint32(dataKVA>>48)&0xFFFF)<<5))
	// MOVZ X2, #0xBEEF
	putInstr(code, &i, 0xd2800002|((0xBEEF)<<5))
	// MOVK X2, #0xDEAD, LSL #16
	putInstr(code, &i, 0xf2a00002|((0xDEAD)<<5))
	// STR X2, [X1]
	putInstr(code, &i, 0xf9000022)
	// MOVZ X3, #0xBABE
	putInstr(code, &i, 0xd2800003|((0xBABE)<<5))
	// MOVK X3, #0xCAFE, LSL #16
	putInstr(code, &i, 0xf2a00003|((0xCAFE)<<5))
	// STR X3, [X1, #8]
	putInstr(code, &i, 0xf9000423)
	// LDR X0, [X1] — read back
	putInstr(code, &i, 0xf9400020)
	// HVC #0
	putInstr(code, &i, 0xd4000002)

	C.memBarrier()

	// Enter EL1.
	vcpu.setReg(C.HV_REG_PC, codeKVA)
	vcpu.setReg(C.HV_REG_CPSR, 0x3c5)
	vcpu.setSysReg(C.HV_SYS_REG_SP_EL1, stackKVA+hvfPageSize-16)

	ret := C.hv_vcpu_run(vcpu.vcpuID)
	if ret != C.HV_SUCCESS {
		return fmt.Errorf("hv_vcpu_run: %d", ret)
	}
	C.memBarrier()

	// Check exit.
	exitReason := vcpu.getExitReason()
	syndrome := vcpu.getExceptionSyndrome()
	ec := (syndrome >> 26) & 0x3f
	if exitReason != exitReasonException || ec != 0x16 {
		return fmt.Errorf("unexpected exit: reason=%d ec=%#x", exitReason, ec)
	}

	// Verify X0 has the value we stored (read-back test).
	x0 := vcpu.getReg(C.HV_REG_X0)
	if x0 != 0xDEADBEEF {
		return fmt.Errorf("X0=%#x, want 0xDEADBEEF (EL1 memory read failed)", x0)
	}

	// Verify host can see the writes (memory coherency).
	dataSlice := unsafe.Slice((*uint64)(dataMem), 2)
	if dataSlice[0] != 0xDEADBEEF {
		return fmt.Errorf("data[0]=%#x, want 0xDEADBEEF (host can't see EL1 write)", dataSlice[0])
	}
	if dataSlice[1] != 0xCAFEBABE {
		return fmt.Errorf("data[1]=%#x, want 0xCAFEBABE", dataSlice[1])
	}

	log.Infof("bluepill memory test: SUCCESS — EL1 read/write through TTBR1 works, data[0]=%#x data[1]=%#x", dataSlice[0], dataSlice[1])
	return nil
}

// Bluepill Architecture Note — TTBR0 Swapping
//
// macOS ARM64 uses 45+ bit virtual addresses for Go stacks/heap.
// TTBR1 with T1SZ=28 only provides 36-bit upper-half VAs, which
// is too small to map sentry memory at its host VAs.
//
// Solution (same as KVM's ring0): use TTBR0 for BOTH sentry and
// guest, swapping on EL0↔EL1 transitions:
//
//   EL1 (sentry): TTBR0 = sentry page tables (host VAs)
//   EL0 (guest):  TTBR0 = guest page tables (guest VAs)
//
// This requires wider VAs. TCR_EL1.T0SZ must be reduced to support
// the full macOS VA range (~48-bit). This is a future task — for now,
// TTBR1 tests use manually allocated memory at low addresses.
//
// To run actual Go functions at EL1, we need either:
//   1. Reduce T0SZ to 16 (48-bit VA) and use TTBR0 for sentry at EL1
//   2. Use a VA remapping scheme (complex, fragile)
//   3. Run a separate sentry binary compiled for low addresses

// bluepillTestMMUOff tests EL1 execution with MMU disabled.
// With MMU off, VA = IPA. If we map host memory at IPA = host_VA
// via hv_vm_map, the sentry sees the same addresses as on the host.
// This avoids the need for stage-1 page tables entirely.
func (m *machine) bluepillTestMMUOff(vcpu *vCPU) error {
	// Allocate a code page.
	var codeMem unsafe.Pointer
	C.posix_memalign(&codeMem, C.size_t(hvfPageSize), C.size_t(hvfPageSize))
	if codeMem == nil {
		return fmt.Errorf("failed to allocate code page")
	}
	defer C.free(codeMem)
	C.memset(codeMem, 0, C.size_t(hvfPageSize))

	// Write test code: MOV X0, #0xBB; HVC #0.
	code := unsafe.Slice((*byte)(codeMem), hvfPageSize)
	i := 0
	putInstr(code, &i, 0xd2801760) // MOVZ X0, #0xBB
	putInstr(code, &i, 0xd4000002) // HVC #0

	// Map code at IPA = host VA of codeMem. With MMU off, the CPU
	// will use the PC value directly as IPA.
	codeHostVA := uintptr(codeMem)
	codeIPA := uint64(codeHostVA) &^ (hvfPageSize - 1)

	C.vmMapRaw(C.uint64_t(codeIPA), C.uint64_t(codeIPA), C.size_t(hvfPageSize),
		C.HV_MEMORY_READ|C.HV_MEMORY_EXEC)

	// Allocate a stack page and map at IPA = host VA.
	var stackMem unsafe.Pointer
	C.posix_memalign(&stackMem, C.size_t(hvfPageSize), C.size_t(hvfPageSize))
	if stackMem == nil {
		return fmt.Errorf("failed to allocate stack page")
	}
	defer C.free(stackMem)
	C.memset(stackMem, 0, C.size_t(hvfPageSize))

	stackHostVA := uintptr(stackMem)
	stackIPA := uint64(stackHostVA) &^ (hvfPageSize - 1)
	C.vmMapRaw(C.uint64_t(stackIPA), C.uint64_t(stackIPA), C.size_t(hvfPageSize),
		C.HV_MEMORY_READ|C.HV_MEMORY_WRITE)

	// Disable MMU: SCTLR_EL1 with M=0 (bit 0 clear).
	// Current SCTLR_EL1 is 0x30901185. Clear bit 0 → 0x30901184.
	if err := vcpu.setSysReg(C.HV_SYS_REG_SCTLR_EL1, 0x30901184); err != nil {
		return fmt.Errorf("set SCTLR_EL1 (MMU off): %w", err)
	}

	// Enter at EL1 with PC = host VA of code (which = IPA with MMU off).
	vcpu.setReg(C.HV_REG_PC, uint64(codeHostVA))
	vcpu.setReg(C.HV_REG_CPSR, 0x3c5) // EL1h, DAIF masked
	vcpu.setSysReg(C.HV_SYS_REG_SP_EL1, uint64(stackHostVA)+hvfPageSize-16)

	log.Infof("bluepill MMU-off: PC=%#x SP=%#x (IPA = host VA)", codeHostVA, uint64(stackHostVA)+hvfPageSize-16)

	C.memBarrier()
	runRet := C.hv_vcpu_run(vcpu.vcpuID)
	C.memBarrier()

	// Re-enable MMU for subsequent tests.
	vcpu.setSysReg(C.HV_SYS_REG_SCTLR_EL1, 0x30901185)

	if runRet != C.HV_SUCCESS {
		return fmt.Errorf("hv_vcpu_run: %d", runRet)
	}

	exitReason := vcpu.getExitReason()
	syndrome := vcpu.getExceptionSyndrome()
	ec := (syndrome >> 26) & 0x3f

	log.Infof("bluepill MMU-off: exit=%d ec=%#x syndrome=%#x", exitReason, ec, syndrome)

	if exitReason != exitReasonException || ec != 0x16 {
		return fmt.Errorf("unexpected exit: reason=%d ec=%#x (want HVC)", exitReason, ec)
	}

	x0 := vcpu.getReg(C.HV_REG_X0)
	if x0 != 0xBB {
		return fmt.Errorf("X0=%#x, want 0xBB", x0)
	}

	log.Infof("bluepill MMU-off: SUCCESS — EL1 with MMU off, VA=IPA=host_VA, X0=%#x", x0)
	return nil
}

// bluepillRunMMUOff runs code at EL1 with MMU disabled and demand-
// paging. When the vCPU faults on an unmapped IPA (stage-2 fault),
// the host page is mapped via hv_vm_map and execution resumes.
//
// pc and sp are host VAs. With MMU off, they're used directly as IPAs.
func (m *machine) bluepillRunMMUOff(vcpu *vCPU, pc, sp uint64) (x0 uint64, err error) {
	// Disable MMU.
	if err := vcpu.setSysReg(C.HV_SYS_REG_SCTLR_EL1, 0x30901184); err != nil {
		return 0, fmt.Errorf("SCTLR_EL1 MMU off: %w", err)
	}

	vcpu.setReg(C.HV_REG_PC, pc)
	vcpu.setReg(C.HV_REG_CPSR, 0x3c5)
	vcpu.setSysReg(C.HV_SYS_REG_SP_EL1, sp)

	maxFaults := 10000
	for faults := 0; faults < maxFaults; faults++ {
		C.memBarrier()
		ret := C.hv_vcpu_run(vcpu.vcpuID)
		C.memBarrier()

		if ret != C.HV_SUCCESS {
			return 0, fmt.Errorf("hv_vcpu_run: %d", ret)
		}

		exitReason := vcpu.getExitReason()
		syndrome := vcpu.getExceptionSyndrome()
		ec := (syndrome >> 26) & 0x3f

		if exitReason != exitReasonException {
			vcpu.setSysReg(C.HV_SYS_REG_SCTLR_EL1, 0x30901185)
			return 0, fmt.Errorf("non-exception exit: reason=%d", exitReason)
		}

		switch {
		case ec == 0x16: // HVC from AArch64
			hvcImm := syndrome & 0xffff
			if hvcImm == 0 {
				// Intentional exit (HVC #0).
				vcpu.setSysReg(C.HV_SYS_REG_SCTLR_EL1, 0x30901185)
				return vcpu.getReg(C.HV_REG_X0), nil
			}
			// HVC from exception vector — EL1 exception relayed via vectors.
			esrEL1 := vcpu.getSysReg(C.HV_SYS_REG_ESR_EL1)
			origEC := (esrEL1 >> 26) & 0x3f
			far := vcpu.getSysReg(C.HV_SYS_REG_FAR_EL1)
			elr := vcpu.getSysReg(C.HV_SYS_REG_ELR_EL1)

			if origEC == 0x24 || origEC == 0x25 { // Data abort at EL1
				log.Debugf("bluepill: EL1 data abort FAR=%#x ELR=%#x (HVC#%d, fault %d)", far, elr, hvcImm, faults)
				if err := m.demandMapHostPage(far, true); err != nil {
					vcpu.setSysReg(C.HV_SYS_REG_SCTLR_EL1, 0x30901185)
					return 0, fmt.Errorf("demand map data FAR=%#x: %w", far, err)
				}
				vcpu.setReg(C.HV_REG_PC, elr) // Resume at faulting instruction
				continue
			}
			if origEC == 0x20 || origEC == 0x21 { // Instruction abort at EL1
				log.Debugf("bluepill: EL1 instr abort ELR=%#x (HVC#%d, fault %d)", elr, hvcImm, faults)
				if err := m.demandMapHostPage(elr, false); err != nil {
					vcpu.setSysReg(C.HV_SYS_REG_SCTLR_EL1, 0x30901185)
					return 0, fmt.Errorf("demand map instr ELR=%#x: %w", elr, err)
				}
				vcpu.setReg(C.HV_REG_PC, elr)
				continue
			}
			vcpu.setSysReg(C.HV_SYS_REG_SCTLR_EL1, 0x30901185)
			return 0, fmt.Errorf("unhandled EL1 exception: origEC=%#x ESR=%#x FAR=%#x ELR=%#x",
				origEC, esrEL1, far, elr)

		case ec == 0x24 || ec == 0x25: // Stage-2 data abort (direct)
			far := vcpu.getFaultAddress()
			log.Debugf("bluepill: stage-2 data abort IPA=%#x (fault %d)", far, faults)
			if err := m.demandMapHostPage(far, true); err != nil {
				vcpu.setSysReg(C.HV_SYS_REG_SCTLR_EL1, 0x30901185)
				return 0, fmt.Errorf("demand map data %#x: %w", far, err)
			}
			continue

		case ec == 0x20 || ec == 0x21: // Stage-2 instruction abort (direct)
			faultPC := vcpu.getReg(C.HV_REG_PC)
			log.Debugf("bluepill: stage-2 instr abort IPA=%#x (fault %d)", faultPC, faults)
			if err := m.demandMapHostPage(faultPC, false); err != nil {
				vcpu.setSysReg(C.HV_SYS_REG_SCTLR_EL1, 0x30901185)
				return 0, fmt.Errorf("demand map instr %#x: %w", faultPC, err)
			}
			continue

		default:
			pc := vcpu.getReg(C.HV_REG_PC)
			vcpu.setSysReg(C.HV_SYS_REG_SCTLR_EL1, 0x30901185)
			return 0, fmt.Errorf("unexpected: ec=%#x syndrome=%#x PC=%#x (after %d faults)",
				ec, syndrome, pc, faults)
		}
	}
	vcpu.setSysReg(C.HV_SYS_REG_SCTLR_EL1, 0x30901185)
	return 0, fmt.Errorf("too many faults (%d), possible infinite loop", maxFaults)
}

// demandMapHostPage maps a host page into the VM at IPA = host VA.
// Used for MMU-off mode where VA = IPA.
//
// HVF cannot map code-signed pages (e.g., Go binary text segment)
// directly. For those, we copy the page content to a fresh mmap'd
// page and map that copy at the original IPA.
func (m *machine) demandMapHostPage(ipa uint64, writable bool) error {
	pageIPA := ipa &^ (hvfPageSize - 1)

	flags := C.hv_memory_flags_t(C.HV_MEMORY_READ | C.HV_MEMORY_EXEC)
	if writable {
		flags |= C.HV_MEMORY_WRITE
	}

	// Unmap first in case this IPA was previously mapped (from earlier
	// demand-page or test). hv_vm_unmap errors are ignored (page might
	// not be mapped yet).
	C.hv_vm_unmap(C.hv_ipa_t(pageIPA), C.size_t(hvfPageSize))

	// Try direct mapping first (fast path — works for heap, stack, data).
	ret := C.vmMapRaw(C.uint64_t(pageIPA), C.uint64_t(pageIPA), C.size_t(hvfPageSize), flags)
	if ret == C.HV_SUCCESS {
		return nil
	}

	// Direct mapping failed — likely a code-signed page (Go text segment).
	// Copy page content to a fresh mmap'd page and map that instead.
	copy := C.mmap(nil, C.size_t(hvfPageSize), C.PROT_READ|C.PROT_WRITE,
		C.MAP_ANON|C.MAP_PRIVATE, -1, 0)
	if copy == nil {
		return fmt.Errorf("mmap copy page failed for IPA=%#x", pageIPA)
	}
	C.copyPageRaw(copy, C.uint64_t(pageIPA), C.size_t(hvfPageSize))

	ret = C.hv_vm_map(copy, C.hv_ipa_t(pageIPA), C.size_t(hvfPageSize), flags)
	if ret != C.HV_SUCCESS {
		C.munmap(copy, C.size_t(hvfPageSize))
		return fmt.Errorf("hv_vm_map copy IPA=%#x: %d", pageIPA, ret)
	}
	return nil
}

// getg returns the current goroutine's g pointer from R28 (ARM64).
// On ARM64, Go stores the goroutine pointer in R28.
func getg() uint64 {
	return uint64(C.readR28C())
}

// bluepillRunMMUOn runs code at EL1 with MMU enabled and 4-level
// page tables (T0SZ=16, 48-bit VA). This allows mapping Go's high
// VAs (1.25TB+) to IPAs within the 40-bit range.
func (m *machine) bluepillRunMMUOn(vcpu *vCPU, spt *sentryPageTable, pc, sp uint64) (x0 uint64, err error) {
	// TCR_EL1 for 48-bit VA: T0SZ=16 (was 28), same granule/cacheability.
	tcr48 := uint64(16) | // T0SZ=16 (48-bit VA)
		(0x1 << 8) | // IRGN0
		(0x1 << 10) | // ORGN0
		(0x3 << 12) | // SH0
		(0x2 << 14) | // TG0: 16K
		(uint64(28) << 16) | // T1SZ=28 (keep TTBR1 as-is)
		(0x1 << 24) | // ORGN1
		(0x1 << 26) | // IRGN1
		(0x3 << 28) | // SH1
		(uint64(0x1) << 30) | // TG1: 16K
		(uint64(0x2) << 32) | // IPS: 40-bit PA
		(uint64(1) << 36) // AS: 16-bit ASID

	vcpu.setSysReg(C.HV_SYS_REG_TCR_EL1, tcr48)
	vcpu.setSysReg(C.HV_SYS_REG_TTBR0_EL1, spt.ttbr0())
	// MMU ON (bit 0 = 1).
	vcpu.setSysReg(C.HV_SYS_REG_SCTLR_EL1, 0x30901185)

	vcpu.setReg(C.HV_REG_PC, pc)
	vcpu.setReg(C.HV_REG_CPSR, 0x3c5)
	vcpu.setSysReg(C.HV_SYS_REG_SP_EL1, sp)

	maxFaults := 10000
	for faults := 0; faults < maxFaults; faults++ {
		C.memBarrier()
		ret := C.hv_vcpu_run(vcpu.vcpuID)
		C.memBarrier()

		if ret != C.HV_SUCCESS {
			return 0, fmt.Errorf("hv_vcpu_run: %d", ret)
		}

		exitReason := vcpu.getExitReason()
		syndrome := vcpu.getExceptionSyndrome()
		ec := (syndrome >> 26) & 0x3f

		if exitReason != exitReasonException {
			return 0, fmt.Errorf("non-exception exit: reason=%d", exitReason)
		}

		switch {
		case ec == 0x16: // HVC
			hvcImm := syndrome & 0xffff
			if hvcImm == 0 {
				return vcpu.getReg(C.HV_REG_X0), nil
			}
			// EL1 exception relayed via vectors.
			esrEL1 := vcpu.getSysReg(C.HV_SYS_REG_ESR_EL1)
			origEC := (esrEL1 >> 26) & 0x3f
			far := vcpu.getSysReg(C.HV_SYS_REG_FAR_EL1)
			elr := vcpu.getSysReg(C.HV_SYS_REG_ELR_EL1)

			if origEC == 0x24 || origEC == 0x25 { // Data abort
				log.Debugf("bluepill MMU-on: data abort FAR=%#x ELR=%#x (fault %d)", far, elr, faults)
				if err := spt.demandMapSentryPage(far, true); err != nil {
					return 0, fmt.Errorf("demand map data FAR=%#x: %w", far, err)
				}
				vcpu.setReg(C.HV_REG_PC, elr)
				continue
			}
			if origEC == 0x20 || origEC == 0x21 { // Instruction abort
				log.Debugf("bluepill MMU-on: instr abort ELR=%#x (fault %d)", elr, faults)
				if err := spt.demandMapSentryPage(elr, false); err != nil {
					return 0, fmt.Errorf("demand map instr ELR=%#x: %w", elr, err)
				}
				vcpu.setReg(C.HV_REG_PC, elr)
				continue
			}
			return 0, fmt.Errorf("unhandled EL1 exception: origEC=%#x ESR=%#x FAR=%#x ELR=%#x",
				origEC, esrEL1, far, elr)

		case ec == 0x24 || ec == 0x25: // Stage-2 data abort
			far := vcpu.getFaultAddress()
			log.Debugf("bluepill MMU-on: stage-2 data abort IPA=%#x (fault %d)", far, faults)
			// Stage-2 fault: IPA not mapped in HVF.
			if err := m.demandMapHostPage(far, true); err != nil {
				return 0, fmt.Errorf("demand map s2 data %#x: %w", far, err)
			}
			continue

		case ec == 0x20 || ec == 0x21: // Stage-2 instruction abort
			faultPC := vcpu.getReg(C.HV_REG_PC)
			log.Debugf("bluepill MMU-on: stage-2 instr abort PC=%#x (fault %d)", faultPC, faults)
			ipaPC := faultPC & sptIPAMask
			if err := m.demandMapHostPage(ipaPC, false); err != nil {
				return 0, fmt.Errorf("demand map s2 instr %#x: %w", ipaPC, err)
			}
			continue

		default:
			pc := vcpu.getReg(C.HV_REG_PC)
			return 0, fmt.Errorf("unexpected: ec=%#x syndrome=%#x PC=%#x (after %d faults)",
				ec, syndrome, pc, faults)
		}
	}
	return 0, fmt.Errorf("too many faults (%d)", maxFaults)
}

// putInstr writes a 32-bit ARM64 instruction at the current offset.
func putInstr(code []byte, offset *int, instr uint32) {
	code[*offset] = byte(instr)
	code[*offset+1] = byte(instr >> 8)
	code[*offset+2] = byte(instr >> 16)
	code[*offset+3] = byte(instr >> 24)
	*offset += 4
}
