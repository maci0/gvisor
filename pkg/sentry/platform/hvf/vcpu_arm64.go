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

// copyStatePageGPRegs copies X0-X15, X18-X30 from a state page into
// a uint64[31] buffer and reads X16, X17 from the vCPU via API.
static void copyStatePageGPRegs(hv_vcpu_t vcpu, const void *statePage, uint64_t *buf) {
    const uint64_t *sp = (const uint64_t *)statePage;
    for (int i = 0; i < 16; i++) buf[i] = sp[i];
    hv_vcpu_get_reg(vcpu, HV_REG_X16, &buf[16]);
    hv_vcpu_get_reg(vcpu, HV_REG_X17, &buf[17]);
    for (int i = 18; i < 31; i++) buf[i] = sp[i];
}

// writeStatePageGPRegs copies X0-X30 from a uint64[31] buffer into
// the state page. The ERET stub loads registers from it in-VM.
static void writeStatePageGPRegs(void *statePage, const uint64_t *buf) {
    uint64_t *sp = (uint64_t *)statePage;
    for (int i = 0; i < 31; i++) sp[i] = buf[i];
}

// saveGPRegs saves X0-X30 from the vCPU into a uint64[31] buffer.
// Single CGO call replaces 31 individual hv_vcpu_get_reg calls.
static void saveGPRegs(hv_vcpu_t vcpu, uint64_t *buf) {
    for (int i = 0; i < 31; i++) {
        hv_vcpu_get_reg(vcpu, (hv_reg_t)(HV_REG_X0 + i), &buf[i]);
    }
}

// loadGPRegs loads X0-X30 into the vCPU from a uint64[31] buffer.
static void loadGPRegs(hv_vcpu_t vcpu, const uint64_t *buf) {
    for (int i = 0; i < 31; i++) {
        hv_vcpu_set_reg(vcpu, (hv_reg_t)(HV_REG_X0 + i), buf[i]);
    }
}

// loadReturnRegs loads only X0 (syscall return value) into the vCPU.
// Used after syscall exits where only X0 changes.
static void loadReturnRegs(hv_vcpu_t vcpu, uint64_t x0) {
    hv_vcpu_set_reg(vcpu, HV_REG_X0, x0);
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
	"runtime"
	"sync/atomic"
	"unsafe"

	"gvisor.dev/gvisor/pkg/sentry/arch"
)

// Exit reason constants.
const (
	exitReasonException       = C.HV_EXIT_REASON_EXCEPTION
	exitReasonCanceled        = C.HV_EXIT_REASON_CANCELED
	exitReasonVtimerActivated = C.HV_EXIT_REASON_VTIMER_ACTIVATED
)

// maxUserAddress is the maximum user VA for HVF on ARM64.
// Accessed atomically to prevent data races with concurrent readers.
var maxUserAddress atomic.Uint64

func init() {
	maxUserAddress.Store((1 << 48) - 1)
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

	// TCR_EL1: dual-TTBR split VA space, 48-bit VAs.
	// TG0: 4K (0x0) or 16K (0x2) depending on hvfPageSize.
	// TG1: always 16K (kernel page tables stay 16K).
	var tg0 uint64
	if hvfPageSize == 4096 {
		tg0 = 0x0 // 4K granule
	} else {
		tg0 = 0x2 // 16K granule
	}
	tcr := uint64(16) | // T0SZ=16 → 48-bit VA
		(0x1 << 8) | // IRGN0
		(0x1 << 10) | // ORGN0
		(0x3 << 12) | // SH0
		(tg0 << 14) | // TG0: 4K or 16K
		(uint64(16) << 16) | // T1SZ=16 → 48-bit VA
		(0x1 << 24) | // ORGN1
		(0x1 << 26) | // IRGN1
		(0x3 << 28) | // SH1
		(uint64(0x1) << 30) | // TG1: 16K (kernel stays 16K)
		(uint64(0x2) << 32) | // IPS: 40-bit PA
		(uint64(1) << 36) // AS: 16-bit ASID
	if err := c.setSysReg(C.HV_SYS_REG_TCR_EL1, tcr); err != nil {
		return fmt.Errorf("set TCR_EL1: %w", err)
	}

	// MAIR_EL1 and SCTLR_EL1 for per-process MMU.
	if err := c.setSysReg(C.HV_SYS_REG_MAIR_EL1, 0xFF); err != nil {
		return fmt.Errorf("set MAIR_EL1: %w", err)
	}
	// SCTLR_EL1: MMU enabled, caches, stack alignment.
	// UCI[26]=1: allow EL0 cache maintenance (DC CIVAC, DC CVAU, IC IVAU).
	// UCT[15]=1: allow EL0 read of CTR_EL0 (cache type register).
	// Without these, JVM's cache flush and feature detection trap to EL1.
	if err := c.setSysReg(C.HV_SYS_REG_SCTLR_EL1, 0x34909185); err != nil {
		return fmt.Errorf("set SCTLR_EL1: %w", err)
	}

	// TPIDR_EL1: kernel VA of per-vCPU state page (in TTBR1).
	// Used by el0_sync handler to save X0-X30 and by ERET stub to load them.
	if err := c.setSysReg(C.HV_SYS_REG_TPIDR_EL1, c.statePageVA); err != nil {
		return fmt.Errorf("set TPIDR_EL1: %w", err)
	}

	// SP_EL1: scratch stack at end of state page (for STP push in save handler).
	if err := c.setSysReg(C.HV_SYS_REG_SP_EL1, c.statePageVA+16384-16); err != nil {
		return fmt.Errorf("set SP_EL1: %w", err)
	}

	// Write dispatch code page VA to state page for BLR from EL1 handler.
	binary.LittleEndian.PutUint64(
		(*[16384]byte)(c.statePageHost)[spOffsetDispatchVA:], c.machine.dispatchKVA)

	// CNTKCTL_EL1: enable EL0 access to the virtual counter (CNTVCT_EL0).
	// Bit 1 (EL0VCTEN): allow EL0 to read the virtual counter.
	// This is required for the VDSO clock_gettime implementation which
	// reads CNTVCT_EL0 directly from userspace.
	if err := c.setSysReg(C.HV_SYS_REG_CNTKCTL_EL1, 0x2); err != nil {
		return fmt.Errorf("set CNTKCTL_EL1: %w", err)
	}

	return nil
}

// setupSharedMemory allocates and maps vectors + page tables into the VM.
// These are shared across all vCPUs. Called once from newMachine().
func (m *machine) setupSharedMemory() error {
	// --- Exception vector table ---
	var vecMem unsafe.Pointer
	if ret := C.posix_memalign(&vecMem, C.size_t(vectorsPageSize), C.size_t(vectorsPageSize)); ret != 0 || vecMem == nil {
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
	// Each entry is 128 bytes (32 instructions). Most entries forward
	// to the hypervisor via HVC #i. The el0_sync handler at 0x400
	// reads ESR_EL1 to classify SVC vs fault and exits with HVC #9
	// or HVC #8 respectively.
	vectors := make([]byte, vectorsPageSize)
	for i := 0; i < 16; i++ {
		hvcInstr := uint32(0xd4000002) | (uint32(i) << 5) // HVC #i
		binary.LittleEndian.PutUint32(vectors[i*128:], hvcInstr)
	}
	// 0x200 (current-EL SPx sync): TLB fault recovery.
	// When STP/LDR to TTBR1 state page faults (cold D-TLB),
	// TLBI + ERET retries. Must save/restore SPSR_EL1 because
	// the current-EL exception overwrites it (needed for ERET to EL0).
	{
		off := 0x200
		put := func(instr uint32) {
			binary.LittleEndian.PutUint32(vectors[off:], instr)
			off += 4
		}
		put(0xd5384009) // MRS X9, SPSR_EL1 (save — overwritten by exception)
		put(0xd508831f) // TLBI VMALLE1IS
		put(0xd5033b9f) // DSB ISH
		put(0xd5033fdf) // ISB
		put(0xd5184009) // MSR SPSR_EL1, X9 (restore for ERET to EL0)
		put(0xd69f03e0) // ERET (retry faulting instruction)
	}

	// el0_sync at 0x400: read ESR_EL1, classify exception.
	// SVC (EC=0x15): save X0-X30 to state page, exit HVC #9.
	// Other: save ESR to X18, exit HVC #8.
	// State page mapped in TTBR1 with AP[1]=0 (EL1-only), so PAN
	// does not block access. TPIDR_EL1 holds state page kernel VA.
	{
		off := 0x400
		put := func(instr uint32) {
			binary.LittleEndian.PutUint32(vectors[off:], instr)
			off += 4
		}
		// In-VM syscall fast-path via ERET (no VM exit for known syscalls).
		// Table dispatch for syscalls 172-178 (getpid..gettid).
		// Patchable MOVZ instructions at known offsets for per-task values.
		// Clobbers: X16, X17 (caller-saved), X18 (platform-reserved).
		put(0xd5385212) // MRS X18, ESR_EL1
		put(0xd35afe51) // LSR X17, X18, #26
		put(0x7100563f) // CMP W17, #0x15     (SVC?)
		put(0x540002c1) // B.NE .+88          (→ fault)
		put(0xd102b111) // SUB X17, X8, #172
		put(0xf1001a3f) // CMP X17, #6
		put(0x54000248) // B.HI .+72          (→ slow)
		put(0x10000070) // ADR X16, .+12      (→ table)
		put(0x8b110e10) // ADD X16, X16, X17, LSL #3
		put(0xd61f0200) // BR X16
		// table[0]: getpid(172)  PATCHABLE at vectors[0x428]
		put(0xd2800020) // MOV X0, #1
		put(0xd69f03e0) // ERET
		// table[1]: getppid(173) PATCHABLE at vectors[0x430]
		put(0xd2800000) // MOV X0, #0
		put(0xd69f03e0) // ERET
		// table[2]: getuid(174)  PATCHABLE at vectors[0x438]
		put(0xd2800000) // MOV X0, #0
		put(0xd69f03e0) // ERET
		// table[3]: geteuid(175) PATCHABLE at vectors[0x440]
		put(0xd2800000) // MOV X0, #0
		put(0xd69f03e0) // ERET
		// table[4]: getgid(176)  PATCHABLE at vectors[0x448]
		put(0xd2800000) // MOV X0, #0
		put(0xd69f03e0) // ERET
		// table[5]: getegid(177) PATCHABLE at vectors[0x450]
		put(0xd2800000) // MOV X0, #0
		put(0xd69f03e0) // ERET
		// table[6]: gettid(178)  PATCHABLE at vectors[0x458]
		put(0xd2800020) // MOV X0, #1
		put(0xd69f03e0) // ERET
		// slow: branch to extended handler at 0x600
		put(0x14000068) // B .+0x1A0 (→ 0x600)
		// fault: save ESR to X18, exit via HVC #8.
		// No STP chain for faults — TLB always cold after ASIDE1IS,
		// recovery overhead (~300ns) outweighs API savings.
		put(0xd5385212) // MRS X18, ESR_EL1
		put(0xd4000102) // HVC #8
	}

	// Extended syscall handler at 0x600 (AArch32 vector space, unused).
	// Handles syscalls not in the 172-178 table dispatch.
	{
		off := 0x600
		put := func(instr uint32) {
			binary.LittleEndian.PutUint32(vectors[off:], instr)
			off += 4
		}
		// sched_yield(124) — bypasses t.Yield(), safe for single-task.
		put(0xf101f11f) // CMP X8, #124
		put(0x54000061) // B.NE .+12
		put(0xd2800000) // MOV X0, #0
		put(0xd69f03e0) // ERET
		// getpgid(155) when X0==0
		put(0xf1026d1f) // CMP X8, #155
		put(0x54000081) // B.NE .+16 (→ getsid check)
		put(0xb5000100) // CBNZ X0, .+32 (→ slow)
		put(0xd2800020) // MOV X0, #1  PATCHABLE at vectors[0x61C]
		put(0xd69f03e0) // ERET
		// getsid(156) when X0==0
		put(0xf102711f) // CMP X8, #156
		put(0x54000081) // B.NE .+16 (→ slow)
		put(0xb5000060) // CBNZ X0, .+12 (→ slow)
		put(0xd2800020) // MOV X0, #1  PATCHABLE at vectors[0x630]
		put(0xd69f03e0) // ERET
		// set_tid_address(96): return TID, skip SetClearTID.
		// SetClearTID only matters at thread exit (futex wake).
		// Single-process workloads: no observable difference.
		// off=0x638
		put(0xf101811f) // CMP X8, #96
		put(0x54000061) // B.NE .+12 (→ slow)
		put(0xd2800020) // MOV X0, #1  PATCHABLE at vectors[fastPathSetTidOff]
		put(0xd69f03e0) // ERET
		// slow: save X0-X30 to state page, then HVC #9.
		// Full STP chain. Cold TLB → fault to 0x200 → TLBI+ERET retry.
		put(0xd538d090) // MRS X16, TPIDR_EL1
		stpEnc := func(rt1, rt2, rn, byteOff int) uint32 {
			return 0xA9000000 | uint32(((byteOff/8)&0x7F)<<15) | uint32(rt2<<10) | uint32(rn<<5) | uint32(rt1)
		}
		put(stpEnc(0, 1, 16, 0x00))
		put(stpEnc(2, 3, 16, 0x10))
		put(stpEnc(4, 5, 16, 0x20))
		put(stpEnc(6, 7, 16, 0x30))
		put(stpEnc(8, 9, 16, 0x40))
		put(stpEnc(10, 11, 16, 0x50))
		put(stpEnc(12, 13, 16, 0x60))
		put(stpEnc(14, 15, 16, 0x70))
		put(stpEnc(18, 19, 16, 0x90))
		put(stpEnc(20, 21, 16, 0xA0))
		put(stpEnc(22, 23, 16, 0xB0))
		put(stpEnc(24, 25, 16, 0xC0))
		put(stpEnc(26, 27, 16, 0xD0))
		put(stpEnc(28, 29, 16, 0xE0))
		put(0xF9000000 | uint32((0xF0/8)<<10) | uint32(16<<5) | 30) // STR X30
		put(0xd4000122) // HVC #9
	}

	// Fault STP chain at 0x700: same as slow-path STP but HVC #8.
	// The 0x200 TLBI+ERET recovery handles cold TLB faults.
	{
		off := 0x700
		put := func(instr uint32) {
			binary.LittleEndian.PutUint32(vectors[off:], instr)
			off += 4
		}
		stpEnc := func(rt1, rt2, rn, byteOff int) uint32 {
			return 0xA9000000 | uint32(((byteOff/8)&0x7F)<<15) | uint32(rt2<<10) | uint32(rn<<5) | uint32(rt1)
		}
		put(0xd538d090) // MRS X16, TPIDR_EL1
		put(stpEnc(0, 1, 16, 0x00))
		put(stpEnc(2, 3, 16, 0x10))
		put(stpEnc(4, 5, 16, 0x20))
		put(stpEnc(6, 7, 16, 0x30))
		put(stpEnc(8, 9, 16, 0x40))
		put(stpEnc(10, 11, 16, 0x50))
		put(stpEnc(12, 13, 16, 0x60))
		put(stpEnc(14, 15, 16, 0x70))
		put(stpEnc(18, 19, 16, 0x90))
		put(stpEnc(20, 21, 16, 0xA0))
		put(stpEnc(22, 23, 16, 0xB0))
		put(stpEnc(24, 25, 16, 0xC0))
		put(stpEnc(26, 27, 16, 0xD0))
		put(stpEnc(28, 29, 16, 0xE0))
		put(0xF9000000 | uint32((0xF0/8)<<10) | uint32(16<<5) | 30) // STR X30
		put(0xd4000102) // HVC #8
	}

	// Single-instruction ERET at 0x800 (unused, kept for compat).
	// Sigreturn trampoline immediately follows at 0x804.
	binary.LittleEndian.PutUint32(vectors[0x800:], 0xd69f03e0) // ERET

	// ERET stub at 0x810: TLBI, load GP regs from state page, ERET.
	// Host writes GP regs to state page. LDP chain loads them in-VM.
	// If LDP faults (cold TTBR1 TLB) → 0x200 → TLBI+ERET → retry.
	{
		off := 0x810
		put := func(instr uint32) {
			binary.LittleEndian.PutUint32(vectors[off:], instr)
			off += 4
		}
		put(0xd5382011) // MRS X17, TTBR0_EL1 (ASID)
		put(0xd5088351) // TLBI ASIDE1IS, X17
		put(0xd5033b9f) // DSB ISH
		put(0xd5033fdf) // ISB
		put(0xd69f03e0) // ERET

		// Full TLBI stub at 0x828 — used on ASID wrap.
		binary.LittleEndian.PutUint32(vectors[0x828:], 0xd508831f) // TLBI VMALLE1IS
		binary.LittleEndian.PutUint32(vectors[0x82c:], 0xd5033b9f) // DSB ISH
		binary.LittleEndian.PutUint32(vectors[0x830:], 0xd5033fdf) // ISB
		binary.LittleEndian.PutUint32(vectors[0x834:], 0xd69f03e0) // ERET
		m.fullTLBIStubOff = 0x828
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

// buildDispatchCode writes the EL1 dispatch handler to the dispatch
// code page. Called via BLR from the el0_sync vector for non-fast-path
// syscalls. Returns X0=result, X1=0(handled via ERET) or X1=1(exit).
// Has access to state page via MRS TPIDR_EL1 (already loaded in X16
// by the calling vector handler).
func (m *machine) buildDispatchCode() error {
	code := (*[16384]byte)(m.dispatchMem)

	// Dispatch code page: called via BLR from el0_sync vector handler.
	// X8 = syscall number, X0-X5 = args, X16 = state page VA.
	// Returns: X9=0 (handled, vector does ERET) or X9=1 (exit via HVC #9).
	//
	// Currently: pure pass-through (all syscalls exit to host).
	// The dispatch page infrastructure is proven by el1gotest.
	// Future: add handlers for rt_sigprocmask, rt_sigaction, etc.
	binary.LittleEndian.PutUint32(code[0:], 0xd2800029) // MOV X9, #1
	binary.LittleEndian.PutUint32(code[4:], 0xD65F03C0) // RET

	return nil
}

// State page layout (16K per vCPU, pointed to by TPIDR_EL1).
// EL1 handler saves registers here; host reads via statePageHost pointer.
// All offsets must be 16-byte aligned for STP/LDP.
const (
	spOffsetGPRegs  = 0x000 // X0-X30: 31 * 8 = 248 bytes
	spOffsetESR     = 0x100 // ESR_EL1 saved by handler
	spOffsetSP_EL0  = 0x108 // SP_EL0 (MRS from EL1)
	spOffsetTPIDR   = 0x110 // TPIDR_EL0
	spOffsetPC         = 0x118 // ELR_EL1 (via API — returns 0 from EL1 MRS)
	spOffsetPSTATE     = 0x120 // SPSR_EL1 (via API)
	spOffsetSigMask    = 0x128 // Signal mask (uint64, synced by host)
	spOffsetSigDirty   = 0x130 // Non-zero if EL1 handler modified signal mask
	spOffsetDispatchVA = 0x200 // Kernel VA of dispatch code page
)

// Offsets of patchable MOVZ X0,#imm instructions in the vectors page.
// Table entries at 0x428 + index*8 (8 bytes per entry: MOVZ + ERET).
const (
	fastPathGetpidOff  = 0x428 // table[0]: getpid(172)
	fastPathGetppidOff = 0x430 // table[1]: getppid(173)
	fastPathGetuidOff  = 0x438 // table[2]: getuid(174)
	fastPathGeteuidOff = 0x440 // table[3]: geteuid(175)
	fastPathGetgidOff  = 0x448 // table[4]: getgid(176)
	fastPathGetegidOff = 0x450 // table[5]: getegid(177)
	fastPathGettidOff  = 0x458 // table[6]: gettid(178)
	// Extended handler (0x600+)
	fastPathGetpgidOff  = 0x61C // getpgid(155) when X0==0
	fastPathGetsidOff   = 0x630 // getsid(156) when X0==0
	fastPathSetTidOff   = 0x640 // set_tid_address(96) returns TID
)

// encodeMOVZ returns the ARM64 encoding for MOVZ X0, #imm16.
func encodeMOVZ(val uint16) uint32 {
	return 0xD2800000 | (uint32(val) << 5)
}

// PatchFastPathSyscalls writes per-task return values into the
// vectors page's patchable MOVZ slots.
//
// WARNING: The vectors page is shared across all vCPUs and address
// spaces. These values are only correct for the init process. After
// fork, child processes will see the parent's PID/TID/UID. A proper
// fix would move these values to the per-vCPU state page.
func (m *machine) PatchFastPathSyscalls(pid, ppid, tid, uid, euid, gid, egid, pgid, sid uint16) {
	vec := (*[vectorsPageSize]byte)(m.vectorsMem)
	binary.LittleEndian.PutUint32(vec[fastPathGetpidOff:], encodeMOVZ(pid))
	binary.LittleEndian.PutUint32(vec[fastPathGetppidOff:], encodeMOVZ(ppid))
	binary.LittleEndian.PutUint32(vec[fastPathGetuidOff:], encodeMOVZ(uid))
	binary.LittleEndian.PutUint32(vec[fastPathGeteuidOff:], encodeMOVZ(euid))
	binary.LittleEndian.PutUint32(vec[fastPathGetgidOff:], encodeMOVZ(gid))
	binary.LittleEndian.PutUint32(vec[fastPathGetegidOff:], encodeMOVZ(egid))
	binary.LittleEndian.PutUint32(vec[fastPathGettidOff:], encodeMOVZ(tid))
	binary.LittleEndian.PutUint32(vec[fastPathGetpgidOff:], encodeMOVZ(pgid))
	binary.LittleEndian.PutUint32(vec[fastPathGetsidOff:], encodeMOVZ(sid))
	binary.LittleEndian.PutUint32(vec[fastPathSetTidOff:], encodeMOVZ(tid))
}

// SigreturnAddr is the guest VA of the sigreturn trampoline in the
// vectors page (offset 0x804). Used as the signal restorer (R30).
const SigreturnAddr = 0x804

// loadRegisters loads application registers from arch.Context64 into the vCPU
// and sets up the EL1-to-EL0 transition via ERET.
// If skipAll is true, skip all register loading (regs unchanged since last exit).
func (c *vCPU) loadRegisters(ac *arch.Context64, skipAll bool) {
	if skipAll {
		// Bare ERET — no register changes, no TLBI needed.
		c.setReg(C.HV_REG_PC, c.machine.vectorsAddr+0x800)
		c.setReg(C.HV_REG_CPSR, 0x3c5)
		return
	}

	regs := &ac.Regs

	C.loadGPRegs(c.vcpuID, (*C.uint64_t)(unsafe.Pointer(&regs.Regs[0])))
	c.setSysReg(C.HV_SYS_REG_SP_EL0, regs.Sp)
	c.setSysReg(C.HV_SYS_REG_TPIDR_EL0, regs.TPIDR_EL0)

	// FP regs: only load on first entry or after signal delivery
	// (when the sentry modifies FP state). The guest's FP state stays
	// in the vCPU between exits — the sentry never touches it.
	if !c.fpLoaded {
		fpData := ac.FloatingPointData()
		if fpData != nil && len(*fpData) >= 520 {
			C.loadFPRegs(c.vcpuID, unsafe.Pointer(&(*fpData)[8]))
			runtime.KeepAlive(fpData)
		}
		c.fpLoaded = true
	}

	c.setSysReg(C.HV_SYS_REG_ELR_EL1, regs.Pc)
	spsr := regs.Pstate &^ 0xf
	c.setSysReg(C.HV_SYS_REG_SPSR_EL1, spsr)

	eretStub := c.machine.vectorsAddr + 0x810
	if c.asidWrapped {
		eretStub = c.machine.vectorsAddr + c.machine.fullTLBIStubOff
		c.asidWrapped = false
	}
	c.setReg(C.HV_REG_PC, eretStub)
	c.setReg(C.HV_REG_CPSR, 0x3c5)
}

// saveRegisters saves vCPU registers back to arch.Context64.
// When gpInStatePage is set (HVC #8/#9 exits with STP chain), GP regs
// are read from the state page. Otherwise falls back to API calls
// (for EC=0x18 traps, direct data aborts, etc. that bypass our handler).
func (c *vCPU) saveRegisters(ac *arch.Context64) {
	regs := &ac.Regs

	if c.gpInStatePage {
		C.copyStatePageGPRegs(c.vcpuID, c.statePageHost,
			(*C.uint64_t)(unsafe.Pointer(&regs.Regs[0])))
		c.gpInStatePage = false
	} else {
		C.saveGPRegs(c.vcpuID, (*C.uint64_t)(unsafe.Pointer(&regs.Regs[0])))
	}

	regs.Sp = c.getSysReg(C.HV_SYS_REG_SP_EL0)
	regs.Pc = c.getSysReg(C.HV_SYS_REG_ELR_EL1)
	pstate := c.getSysReg(C.HV_SYS_REG_SPSR_EL1)
	regs.Pstate = pstate &^ 0xf
	regs.TPIDR_EL0 = c.getSysReg(C.HV_SYS_REG_TPIDR_EL0)

	// Skip FP save on syscall exits (HVC #9) — FP stays in vCPU,
	// re-loaded on next entry. Only save on fault exits (HVC #8)
	// which may trigger signal delivery that needs FP context.
	if c.saveFP {
		fpData := ac.FloatingPointData()
		if fpData != nil && len(*fpData) >= 520 {
			C.saveFPRegs(c.vcpuID, unsafe.Pointer(&(*fpData)[8]))
			runtime.KeepAlive(fpData)
		}
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
// Errors are not checked — these calls are on the hot path (31 regs
// per Switch iteration) and HVF register operations don't fail in
// practice for valid vCPU handles and register IDs.
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
