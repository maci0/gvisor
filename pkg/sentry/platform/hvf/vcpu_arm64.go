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

// memoryBarrier issues a full data synchronization barrier (DSB ISH)
// to ensure all memory accesses are visible across cores before/after
// vCPU execution.
static inline void memoryBarrier(void) {
    __asm__ __volatile__("dsb ish" ::: "memory");
}

// saveFPRegs saves all 32 SIMD/FP Q registers plus FPCR/FPSR from
// the vCPU into the given buffer. The buffer layout matches Linux's
// fpsimd_context: fpsr(4) + fpcr(4) + vregs[32*16](512) = 520 bytes.
// The caller must pass a pointer to the fpsr field (offset 8 in
// the full fpsimd_context which has an 8-byte header).
static void saveFPRegs(hv_vcpu_t vcpu, void *buf) {
    uint32_t *ctrl = (uint32_t *)buf;
    uint8_t *vregs = (uint8_t *)buf + 8; // after fpsr + fpcr

    // FPSR and FPCR via the regular register API.
    uint64_t fpsr, fpcr;
    hv_vcpu_get_reg(vcpu, HV_REG_FPSR, &fpsr);
    hv_vcpu_get_reg(vcpu, HV_REG_FPCR, &fpcr);
    ctrl[0] = (uint32_t)fpsr;
    ctrl[1] = (uint32_t)fpcr;

    // Q0-Q31 (128-bit each).
    for (int i = 0; i < 32; i++) {
        hv_simd_fp_uchar16_t val;
        hv_vcpu_get_simd_fp_reg(vcpu, (hv_simd_fp_reg_t)(HV_SIMD_FP_REG_Q0 + i), &val);
        memcpy(vregs + i * 16, &val, 16);
    }
}

// loadFPRegs loads all 32 SIMD/FP Q registers plus FPCR/FPSR into
// the vCPU from the given buffer (same layout as saveFPRegs).
static void loadFPRegs(hv_vcpu_t vcpu, const void *buf) {
    const uint32_t *ctrl = (const uint32_t *)buf;
    const uint8_t *vregs = (const uint8_t *)buf + 8;

    hv_vcpu_set_reg(vcpu, HV_REG_FPSR, (uint64_t)ctrl[0]);
    hv_vcpu_set_reg(vcpu, HV_REG_FPCR, (uint64_t)ctrl[1]);

    for (int i = 0; i < 32; i++) {
        hv_simd_fp_uchar16_t val;
        memcpy(&val, vregs + i * 16, 16);
        hv_vcpu_set_simd_fp_reg(vcpu, (hv_simd_fp_reg_t)(HV_SIMD_FP_REG_Q0 + i), val);
    }
}
*/
import "C"

import (
	"encoding/binary"
	"fmt"
	"unsafe"

	"gvisor.dev/gvisor/pkg/sentry/arch"
)

// Exit reason constants.
const (
	exitReasonException       = C.HV_EXIT_REASON_EXCEPTION
	exitReasonCanceled        = C.HV_EXIT_REASON_CANCELED
	exitReasonVtimerActivated = C.HV_EXIT_REASON_VTIMER_ACTIVATED
)

// ARM64 PSTATE masks.
const nzcvMask = 0xf0000000 // NZCV condition flags [31:28]

// Maximum user address for HVF on ARM64. Identity-mapped page tables
// cover 64GB (36-bit VA with 16K granule, T0SZ=28). Subtract space
// for vectors page (IPA 0) and page table page (IPA 16K).
const maxUserAddress = (1 << 36) - 1

// hvRegs maps general-purpose register indices to HVF register IDs.
var hvRegs = [31]C.hv_reg_t{
	C.HV_REG_X0,
	C.HV_REG_X1,
	C.HV_REG_X2,
	C.HV_REG_X3,
	C.HV_REG_X4,
	C.HV_REG_X5,
	C.HV_REG_X6,
	C.HV_REG_X7,
	C.HV_REG_X8,
	C.HV_REG_X9,
	C.HV_REG_X10,
	C.HV_REG_X11,
	C.HV_REG_X12,
	C.HV_REG_X13,
	C.HV_REG_X14,
	C.HV_REG_X15,
	C.HV_REG_X16,
	C.HV_REG_X17,
	C.HV_REG_X18,
	C.HV_REG_X19,
	C.HV_REG_X20,
	C.HV_REG_X21,
	C.HV_REG_X22,
	C.HV_REG_X23,
	C.HV_REG_X24,
	C.HV_REG_X25,
	C.HV_REG_X26,
	C.HV_REG_X27,
	C.HV_REG_X28,
	C.HV_REG_X29,
	C.HV_REG_X30,
}

// vectorsPageSize is the size of the exception vector page. Must be at least
// one page. On macOS ARM64, pages are 16K.
const vectorsPageSize = 16384

// initialize sets up the vCPU with exception vectors for EL0 guest execution.
// The approach:
//  1. Build an exception vector table at EL1 that forwards all exceptions
//     to the hypervisor via HVC.
//  2. Guest code runs at EL0. SVC traps to EL1, our handler does HVC to exit.
//  3. The hypervisor reads ESR_EL1 to determine the original exception type.
func (c *vCPU) initialize() error {
	// Enable floating point and SIMD access at EL0/EL1.
	if err := c.setSysReg(C.HV_SYS_REG_CPACR_EL1, 3<<20); err != nil {
		return fmt.Errorf("set CPACR_EL1: %w", err)
	}

	// Mask the virtual timer to prevent spurious timer interrupts.
	C.hv_vcpu_set_vtimer_mask(c.vcpuID, C.bool(true))

	// Point this vCPU to the shared exception vectors.
	if err := c.setSysReg(C.HV_SYS_REG_VBAR_EL1, c.machine.vectorsAddr); err != nil {
		return fmt.Errorf("set VBAR_EL1: %w", err)
	}

	// TTBR0_EL1 is set dynamically in Switch() for per-process page tables.
	// Initialize to 0 (no valid mappings until Switch sets it).
	if err := c.setSysReg(C.HV_SYS_REG_TTBR0_EL1, 0); err != nil {
		return fmt.Errorf("set TTBR0_EL1: %w", err)
	}

	// TTBR1_EL1: shared kernel page table for upper-half VAs (sentry memory).
	if err := c.setSysReg(C.HV_SYS_REG_TTBR1_EL1, c.machine.kernelPT.ttbr1()); err != nil {
		return fmt.Errorf("set TTBR1_EL1: %w", err)
	}

	// TCR_EL1: dual-TTBR split VA space with 16K granule, 36-bit VAs.
	//
	// TTBR0 (lower-half, guest app at EL0):
	//   T0SZ=28   [5:0]    36-bit VA
	//   IRGN0=1   [9:8]    Write-Back, Write-Allocate
	//   ORGN0=1   [11:10]  Write-Back, Write-Allocate
	//   SH0=3     [13:12]  Inner Shareable
	//   TG0=2     [15:14]  16K granule
	//
	// TTBR1 (upper-half, sentry kernel):
	//   T1SZ=28   [21:16]  36-bit VA
	//   EPD1=0    [23]     Enable TTBR1 walks
	//   ORGN1=1   [25:24]  Write-Back, Write-Allocate
	//   IRGN1=1   [27:26]  Write-Back, Write-Allocate
	//   SH1=3     [29:28]  Inner Shareable
	//   TG1=1     [31:30]  16K granule (TG1 encoding: 01=16K)
	//
	// Shared:
	//   IPS=2     [34:32]  40-bit PA
	//   AS=1      [36]     16-bit ASID
	tcr := uint64(28) | // T0SZ=28
		(0x1 << 8) | // IRGN0
		(0x1 << 10) | // ORGN0
		(0x3 << 12) | // SH0
		(0x2 << 14) | // TG0: 16K
		(uint64(28) << 16) | // T1SZ=28
		(0x1 << 24) | // ORGN1
		(0x1 << 26) | // IRGN1
		(0x3 << 28) | // SH1
		(uint64(0x1) << 30) | // TG1: 16K (01)
		(uint64(0x2) << 32) | // IPS: 40-bit PA
		(uint64(1) << 36) // AS: 16-bit ASID
	if err := c.setSysReg(C.HV_SYS_REG_TCR_EL1, tcr); err != nil {
		return fmt.Errorf("set TCR_EL1: %w", err)
	}

	// MAIR_EL1 and SCTLR_EL1 for per-process MMU.
	if err := c.setSysReg(C.HV_SYS_REG_MAIR_EL1, 0xFF); err != nil {
		return fmt.Errorf("set MAIR_EL1: %w", err)
	}
	if err := c.setSysReg(C.HV_SYS_REG_SCTLR_EL1, 0x30901185); err != nil {
		return fmt.Errorf("set SCTLR_EL1: %w", err)
	}

	return nil
}

// setupSharedMemory allocates and maps vectors + page tables into the VM.
// These are shared across all vCPUs. Called once from newMachine().
func (m *machine) setupSharedMemory() error {
	// --- Exception vector table ---
	var vecMem unsafe.Pointer
	C.posix_memalign(&vecMem, C.size_t(vectorsPageSize), C.size_t(vectorsPageSize))
	if vecMem == nil {
		return fmt.Errorf("failed to allocate vector page")
	}
	C.memset(vecMem, 0, C.size_t(vectorsPageSize))

	// ARM64 exception vector table layout (VBAR_EL1-relative offsets):
	//
	// Current EL, SP_EL0:
	//   0x000: Synchronous  → HVC #0
	//   0x080: IRQ          → HVC #1
	//   0x100: FIQ          → HVC #2
	//   0x180: SError       → HVC #3
	//
	// Current EL, SP_ELx:
	//   0x200: Synchronous  → HVC #4
	//   0x280: IRQ          → HVC #5
	//   0x300: FIQ          → HVC #6
	//   0x380: SError       → HVC #7
	//
	// Lower EL, AArch64 (EL0 traps — sentry-as-ring0):
	//   0x400: Synchronous  → HVC #8  (el0_sync: SVC syscalls, faults)
	//   0x480: IRQ          → HVC #9
	//   0x500: FIQ          → HVC #10
	//   0x580: SError       → HVC #11
	//
	// Lower EL, AArch32 (unused, but must be present):
	//   0x600: Synchronous  → HVC #12
	//   0x680: IRQ          → HVC #13
	//   0x700: FIQ          → HVC #14
	//   0x780: SError       → HVC #15
	//
	// Each entry is 128 bytes (32 instructions). For Phase 1, all entries
	// forward to the hypervisor via HVC #i so the sentry can handle them.
	// The HVC immediate encodes the vector index, allowing context.go to
	// distinguish current-EL exceptions (HVC #0-#7) from lower-EL
	// exceptions (HVC #8-#15). In Phase 2, the el0_sync handler at 0x400
	// will process SVC syscalls entirely at EL1 without HVC exit.
	vectors := make([]byte, vectorsPageSize)
	for i := 0; i < 16; i++ {
		// el0_sync (i=8, offset 0x400) uses HVC #8 like other vectors.
		hvcInstr := uint32(0xd4000002) | (uint32(i) << 5) // HVC #i
		binary.LittleEndian.PutUint32(vectors[i*128:], hvcInstr)
	}
	binary.LittleEndian.PutUint32(vectors[0x800:], 0xd69f03e0) // ERET stub

	// TLB flush + ERET stub at offset 0x810.
	binary.LittleEndian.PutUint32(vectors[0x810:], 0xd508831f) // TLBI VMALLE1IS
	binary.LittleEndian.PutUint32(vectors[0x814:], 0xd5033b9f) // DSB ISH
	binary.LittleEndian.PutUint32(vectors[0x818:], 0xd5033fdf) // ISB
	binary.LittleEndian.PutUint32(vectors[0x81c:], 0xd69f03e0) // ERET

	// Ring0 entry stub at offset 0x820. Used by ring0 mode Switch()
	// to transition from EL1 (sentry) to EL0 (guest).
	// X16 = guest TTBR0, X17 = guest TCR (T0SZ=28).
	// ELR_EL1, SPSR_EL1, SP_EL0 already set by loadRegisters().
	//
	// Executes from TTBR1 (upper-half VA) so TCR/TTBR0 changes
	// don't affect instruction fetch. Includes TLBI for direct
	// TLB flush at EL1 — eliminates reliance on ASID rotation alone.
	binary.LittleEndian.PutUint32(vectors[0x820:], 0xd5182051) // MSR TCR_EL1, X17
	binary.LittleEndian.PutUint32(vectors[0x824:], 0xd5182010) // MSR TTBR0_EL1, X16
	binary.LittleEndian.PutUint32(vectors[0x828:], 0xd508831f) // TLBI VMALLE1IS
	binary.LittleEndian.PutUint32(vectors[0x82c:], 0xd5033b9f) // DSB ISH
	binary.LittleEndian.PutUint32(vectors[0x830:], 0xd5033fdf) // ISB
	binary.LittleEndian.PutUint32(vectors[0x834:], 0xd69f03e0) // ERET

	// In-VM el0_sync handler at offset 0x840. Saves ESR_EL1 and
	// FAR_EL1 to the per-vCPU state page (TPIDR_EL1), then HVC exits.
	// The host reads ESR/FAR from the state page (one memory read)
	// instead of per-register HVF API calls.
	//
	// Clobbers: X9 (caller-saved scratch, not a syscall arg).
	// Preserves: X0-X8, X18 (saved/restored via CONTEXTIDR_EL1).
	{
		off := 0x840
		put := func(instr uint32) {
			binary.LittleEndian.PutUint32(vectors[off:], instr)
			off += 4
		}
		// Save ESR_EL1 and FAR_EL1 to per-vCPU state slot.
		// TPIDR_EL1 = VMMSharedPageIPA + vCPU_id*64.
		// State page mapped at VA=IPA=0x80000 in every guest PT.
		// Save guest GPRs X0-X17 to per-vCPU state page.
		// X18 clobbered (platform-reserved, used as state pointer).
		// Exception sysregs (ESR/ELR/SPSR/FAR/SP_EL0) NOT readable
		// at EL1 after EL0→EL1 exception — HVF traps them. Host reads
		// those via HVF API after HVC exit.
		// Save X0-X8 (syscall args + number) to per-vCPU state page.
		// Only X18 clobbered (platform-reserved, OK to lose).
		// X9-X17, X19-X30 untouched — host reads them via API.
		put(0xd538d092) // MRS X18, TPIDR_EL1 (state slot ptr)
		put(0xa9000640) // STP X0, X1, [X18, #0]
		put(0xa9010e42) // STP X2, X3, [X18, #16]
		put(0xa9021644) // STP X4, X5, [X18, #32]
		put(0xa9031e46) // STP X6, X7, [X18, #48]
		put(0xf9002248) // STR X8, [X18, #64]
		put(0xd5033b9f) // DSB ISH
		put(0xd4000102) // HVC #8
	}

	// Sigreturn trampoline at offset 0x804. Used as the signal
	// restorer (R30) when SA_RESTORER is not set. Executes:
	//   MOV X8, #139    // SYS_RT_SIGRETURN
	//   SVC #0
	binary.LittleEndian.PutUint32(vectors[0x804:], 0xd2801168) // MOV X8, #139
	binary.LittleEndian.PutUint32(vectors[0x808:], 0xd4000001) // SVC #0

	C.memcpy(vecMem, unsafe.Pointer(&vectors[0]), C.size_t(len(vectors)))

	m.vectorsAddr = 0
	m.vectorsMem = vecMem
	ret := C.hv_vm_map(vecMem, C.hv_ipa_t(m.vectorsAddr), C.size_t(vectorsPageSize),
		C.HV_MEMORY_READ|C.HV_MEMORY_EXEC)
	if ret != C.HV_SUCCESS {
		C.free(vecMem)
		return fmt.Errorf("hv_vm_map vectors failed: %d", ret)
	}

	// Per-process page tables are allocated in NewAddressSpace()
	// via the ptPageAllocator. No global page table needed.

	return nil
}

// SigreturnAddr is the guest VA of the sigreturn trampoline in the
// vectors page (offset 0x804). Used as the signal restorer (R30).
const SigreturnAddr = 0x804

// loadRegisters loads application registers from arch.Context64 into the vCPU
// and sets up the EL1-to-EL0 transition via ERET.
func (c *vCPU) loadRegisters(ac *arch.Context64) {
	regs := &ac.Regs

	// General purpose registers X0-X30.
	for i := 0; i < 31; i++ {
		c.setReg(hvRegs[i], regs.Regs[i])
	}

	// Stack pointer: SP_EL0 since guest runs at EL0t.
	c.setSysReg(C.HV_SYS_REG_SP_EL0, regs.Sp)

	// TLS register.
	c.setSysReg(C.HV_SYS_REG_TPIDR_EL0, regs.TPIDR_EL0)

	// Floating point / SIMD registers.
	fpData := ac.FloatingPointData()
	if fpData != nil && len(*fpData) >= 520 {
		// FP state layout: [0:8]=head, [8:520]=fpsr+fpcr+vregs.
		C.loadFPRegs(c.vcpuID, unsafe.Pointer(&(*fpData)[8]))
	}

	// Guest runs at EL0 (sentry-as-ring0 model).
	// SPSR_EL1 determines what ERET returns to: EL0t (mode=0x0), no DAIF.
	// Preserve NZCV flags from the saved Pstate.
	c.setSysReg(C.HV_SYS_REG_ELR_EL1, regs.Pc)
	spsr := regs.Pstate & nzcvMask // EL0t (mode=0), DAIF clear
	c.setSysReg(C.HV_SYS_REG_SPSR_EL1, spsr)
	c.setReg(C.HV_REG_PC, c.machine.vectorsAddr+0x810) // TLB flush + ERET stub
	c.setReg(C.HV_REG_CPSR, 0x3c5)                     // EL1h, DAIF masked (stub runs at EL1)
}

// saveRegisters saves vCPU registers back to arch.Context64.
// After an EL0 exception, the guest's PC and PSTATE are in ELR_EL1
// and SPSR_EL1 (set by the CPU when taking the exception to EL1).
func (c *vCPU) saveRegisters(ac *arch.Context64) {
	regs := &ac.Regs

	for i := 0; i < 31; i++ {
		regs.Regs[i] = c.getReg(hvRegs[i])
	}

	// Stack pointer (SP_EL0 since guest runs at EL0t).
	regs.Sp = c.getSysReg(C.HV_SYS_REG_SP_EL0)

	// Guest PC and PSTATE from exception registers.
	regs.Pc = c.getSysReg(C.HV_SYS_REG_ELR_EL1)
	// SPSR_EL1 holds the guest's PSTATE at the time of the exception.
	// Guest runs at EL0t, so mode bits are already 0x0.
	// Clear DAIF bits (0x3c0) to normalize — EL0 never has DAIF masked.
	pstate := c.getSysReg(C.HV_SYS_REG_SPSR_EL1)
	// Clear mode bits (EL1h→EL0t) and DAIF mask bits.
	const modeDaifMask = 0x3cf // mode[3:0] | DAIF[9:6]
	regs.Pstate = pstate &^ modeDaifMask

	// TLS register.
	regs.TPIDR_EL0 = c.getSysReg(C.HV_SYS_REG_TPIDR_EL0)

	// Floating point / SIMD registers.
	fpData := ac.FloatingPointData()
	if fpData != nil && len(*fpData) >= 520 {
		C.saveFPRegs(c.vcpuID, unsafe.Pointer(&(*fpData)[8]))
	}
}

// getExitReason returns the exit reason from the vCPU exit info.
func (c *vCPU) getExitReason() C.uint32_t {
	return C.uint32_t(c.exit.reason)
}

// getExceptionSyndrome returns the exception syndrome (ESR) from the
// vCPU exit information. This is valid when the exit reason is
// HV_EXIT_REASON_EXCEPTION.
func (c *vCPU) getExceptionSyndrome() uint64 {
	return uint64(c.exit.exception.syndrome)
}

// getFaultAddress returns the faulting virtual address from the vCPU
// exit information. This is valid for data/instruction abort exceptions.
func (c *vCPU) getFaultAddress() uint64 {
	return uint64(c.exit.exception.virtual_address)
}

// setReg sets a general-purpose or special register on the vCPU.
func (c *vCPU) setReg(reg C.hv_reg_t, val uint64) {
	C.hv_vcpu_set_reg(c.vcpuID, reg, C.uint64_t(val))
}

// getReg gets a general-purpose or special register from the vCPU.
func (c *vCPU) getReg(reg C.hv_reg_t) uint64 {
	var val C.uint64_t
	C.hv_vcpu_get_reg(c.vcpuID, reg, &val)
	return uint64(val)
}

// setSysReg sets a system register on the vCPU.
func (c *vCPU) setSysReg(reg C.hv_sys_reg_t, val uint64) error {
	ret := C.hv_vcpu_set_sys_reg(c.vcpuID, reg, C.uint64_t(val))
	if ret != C.HV_SUCCESS {
		return fmt.Errorf("hv_vcpu_set_sys_reg(%d) failed: %d", reg, ret)
	}
	return nil
}

// getSysReg gets a system register from the vCPU.
func (c *vCPU) getSysReg(reg C.hv_sys_reg_t) uint64 {
	var val C.uint64_t
	C.hv_vcpu_get_sys_reg(c.vcpuID, reg, &val)
	return uint64(val)
}
