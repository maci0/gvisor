//go:build darwin && arm64

// Command el1memtest exhaustively tests EL1 data access through stage-1
// page tables on Apple Hypervisor.framework. Linux kernels running under
// QEMU/UTM/libkrun on HVF do EL1 memory access freely — this test
// determines why our earlier vmtest failed and what configuration is needed.
//
// Tests:
//   1. EL1 LDR through TTBR0 with AP[1]=0 (EL1-only), PAN cleared
//   2. EL1 LDR through TTBR0 with AP[1]=1 (EL0+EL1), PAN cleared
//   3. EL1 LDR through TTBR1, PAN cleared
//   4. EL1 LDR through TTBR0, guest sets up own page tables via MSR
//   5. Dump PSTATE.PAN after exception entry
//
// Build: go build -o el1memtest ./cmd/el1memtest
// Sign:  codesign -s - --entitlements cmd/sentrydarwin/entitlements.plist -f el1memtest
package main

/*
#cgo LDFLAGS: -framework Hypervisor -framework CoreFoundation
#include <Hypervisor/Hypervisor.h>
#include <CoreFoundation/CoreFoundation.h>
#include <stdlib.h>
#include <string.h>
*/
import "C"

import (
	"encoding/binary"
	"fmt"
	"runtime"
	"sync"
	"time"
	"unsafe"
)

const pageSize = 16384

func main() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	fmt.Println("=== EL1 Memory Access Tests ===")
	fmt.Println("If Linux kernels work on HVF, EL1 data access MUST work.")
	fmt.Println("These tests systematically find the correct configuration.")
	fmt.Println()

	test1_EL1_TTBR0_EL1Only()
	test2_EL1_TTBR0_PAN_check()
	test3_EL1_TTBR1_simple()
	test6_ESR_EL1_retest()
	test7_full_EL1_handler()
	test8_STTR_LDTR()
}

func alloc(n int) unsafe.Pointer {
	var p unsafe.Pointer
	C.posix_memalign(&p, C.size_t(pageSize), C.size_t(n))
	C.memset(p, 0, C.size_t(n))
	return p
}

func runWithTimeout(vcpu C.hv_vcpu_t, sec int) (hung bool, ret C.hv_return_t) {
	var mu sync.Mutex
	go func() {
		time.Sleep(time.Duration(sec) * time.Second)
		mu.Lock()
		hung = true
		mu.Unlock()
		C.hv_vcpus_exit(&vcpu, 1)
	}()
	ret = C.hv_vcpu_run(vcpu)
	mu.Lock()
	defer mu.Unlock()
	return
}

// test1: EL1 LDR through TTBR0 with AP[1]=0 (EL1-only pages).
// PAN can't trigger because AP[1]=0 means not EL0-accessible.
// If this faults, the issue is NOT PAN — it's something else entirely.
func test1_EL1_TTBR0_EL1Only() {
	fmt.Println("--- Test 1: EL1 LDR via TTBR0, AP[1]=0 (EL1-only), no PAN possible ---")

	config := C.hv_vm_config_create()
	defer C.CFRelease(C.CFTypeRef(unsafe.Pointer(config)))
	C.hv_vm_config_set_ipa_size(config, 40)
	if ret := C.hv_vm_create(config); ret != C.HV_SUCCESS {
		fmt.Printf("  hv_vm_create: %d\n", ret)
		return
	}
	defer C.hv_vm_destroy()

	var vcpu C.hv_vcpu_t
	var exit *C.hv_vcpu_exit_t
	C.hv_vcpu_create(&vcpu, &exit, nil)
	defer C.hv_vcpu_destroy(vcpu)

	// Allocate: vectors, user code, data page, 4 page table levels
	vecMem := alloc(pageSize)
	userMem := alloc(pageSize)
	dataMem := alloc(pageSize)
	ptL0 := alloc(pageSize)
	ptL1 := alloc(pageSize)
	ptL2 := alloc(pageSize)
	ptL3 := alloc(pageSize)
	defer func() {
		for _, p := range []unsafe.Pointer{vecMem, userMem, dataMem, ptL0, ptL1, ptL2, ptL3} {
			C.free(p)
		}
	}()

	// IPA layout (16K aligned):
	// 0x00000 vectors (RX)
	// 0x04000 user code (RX)
	// 0x08000 data page (RW) — holds magic value
	// 0x10000 L0 page table
	// 0x14000 L1
	// 0x18000 L2
	// 0x1C000 L3
	type m struct {
		p   unsafe.Pointer
		ipa uint64
		f   C.hv_memory_flags_t
	}
	maps := []m{
		{vecMem, 0x00000, C.HV_MEMORY_READ | C.HV_MEMORY_EXEC},
		{userMem, 0x04000, C.HV_MEMORY_READ | C.HV_MEMORY_EXEC},
		{dataMem, 0x08000, C.HV_MEMORY_READ | C.HV_MEMORY_WRITE},
		{ptL0, 0x10000, C.HV_MEMORY_READ | C.HV_MEMORY_WRITE},
		{ptL1, 0x14000, C.HV_MEMORY_READ | C.HV_MEMORY_WRITE},
		{ptL2, 0x18000, C.HV_MEMORY_READ | C.HV_MEMORY_WRITE},
		{ptL3, 0x1C000, C.HV_MEMORY_READ | C.HV_MEMORY_WRITE},
	}
	for _, m := range maps {
		if ret := C.hv_vm_map(m.p, C.hv_ipa_t(m.ipa), C.size_t(pageSize), m.f); ret != C.HV_SUCCESS {
			fmt.Printf("  hv_vm_map IPA=%#x: %d\n", m.ipa, ret)
			return
		}
	}
	defer func() {
		for _, m := range maps {
			C.hv_vm_unmap(C.hv_ipa_t(m.ipa), C.size_t(pageSize))
		}
	}()

	// Write magic to data page
	magic := uint64(0xDEADBEEF12345678)
	binary.LittleEndian.PutUint64((*[pageSize]byte)(dataMem)[:], magic)

	// Build TTBR0 page table (16K granule, T0SZ=16 → 48-bit VA)
	// Identity mapping: VA = IPA
	// All entries: AF=1, SH=ISH, AttrIndx=0 (Normal WB), AP[1]=0 (EL1-only)
	l0 := (*[2048]uint64)(ptL0)
	l1 := (*[2048]uint64)(ptL1)
	l2 := (*[2048]uint64)(ptL2)
	l3 := (*[2048]uint64)(ptL3)

	tableDesc := uint64(0x3) // valid + table
	l0[0] = 0x14000 | tableDesc
	l1[0] = 0x18000 | tableDesc
	l2[0] = 0x1C000 | tableDesc

	// L3 page descriptors:
	// Bits: [47:14]=OA, [11]=nG, [10]=AF, [9:8]=SH, [7]=AP[2],
	//       [6]=AP[1], [4:2]=AttrIndx, [1:0]=0b11
	// AP[1]=0: EL1 only (NO EL0 access) → PAN cannot trigger
	// AP[2]=0: read-write
	// AF=1, SH=ISH(3), AttrIndx=0
	mkPage := func(ipa uint64, el0 bool) uint64 {
		d := ipa | (1 << 10) | (3 << 8) | 0x3 // AF, ISH, valid page
		if el0 {
			d |= (1 << 6) // AP[1]=1: EL0+EL1
		}
		return d
	}

	l3[0] = mkPage(0x00000, false) // vectors — EL1-only
	l3[1] = mkPage(0x04000, true)  // user code — EL0+EL1 (for EL0 fetch)
	l3[2] = mkPage(0x08000, false) // data — EL1-only (THE KEY: no PAN)

	// EL0 sync handler at VBAR+0x400:
	//   MSR PAN, #0       ← explicitly clear PAN just in case
	//   LDR X0, [X18]     ← load from data page VA (X18 = 0x8000)
	//   HVC #9             ← exit to host
	vectors := (*[pageSize]byte)(vecMem)
	// Default: HVC #0 at each vector
	for i := 0; i < 16; i++ {
		binary.LittleEndian.PutUint32(vectors[i*128:], 0xd4000002|uint32(i)<<5)
	}

	off := 0x400
	put := func(instr uint32) {
		binary.LittleEndian.PutUint32(vectors[off:], instr)
		off += 4
	}
	put(0xd500409f) // MSR PAN, #0 (clear PAN)
	put(0xd5033fdf) // ISB
	put(0xf9400240) // LDR X0, [X18]
	put(0xd4000122) // HVC #9

	// ERET at 0x800
	binary.LittleEndian.PutUint32(vectors[0x800:], 0xd69f03e0)

	// User code at VA 0x4000: SVC #0
	binary.LittleEndian.PutUint32((*[pageSize]byte)(userMem)[:], 0xd4000001)

	// System registers
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_VBAR_EL1, 0)
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_CPACR_EL1, 3<<20)

	// MAIR: Attr[0]=0xFF (Normal WB), like Linux
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_MAIR_EL1, 0xFF)

	// SCTLR: MMU on, caches on, SPAN=1 (don't auto-set PAN), no WXN
	// Use exact Linux default: 0x30D00985 but simplified
	sctlr := uint64(0x30D00985) // M, C, I, SPAN=1
	sctlr |= (1 << 0)           // M: MMU enable
	sctlr |= (1 << 2)           // C: data cache
	sctlr |= (1 << 12)          // I: instruction cache
	sctlr |= (1 << 23)          // SPAN: don't auto-set PAN
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_SCTLR_EL1, C.uint64_t(sctlr))

	// TCR: T0SZ=16, TG0=16K, IPS=40bit, SH0=ISH, ORGN0=WB, IRGN0=WB
	tcr := uint64(16) |       // T0SZ
		(0x1 << 8) |       // IRGN0: WB WA
		(0x1 << 10) |      // ORGN0: WB WA
		(0x3 << 12) |      // SH0: Inner Shareable
		(0x2 << 14) |      // TG0: 16KB
		(uint64(16) << 16) | // T1SZ
		(0x1 << 24) |      // ORGN1
		(0x1 << 26) |      // IRGN1
		(0x3 << 28) |      // SH1
		(uint64(0x1) << 30) | // TG1: 16KB
		(uint64(0x2) << 32) | // IPS: 40-bit
		(uint64(1) << 36)     // AS: 16-bit ASID
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_TCR_EL1, C.uint64_t(tcr))

	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_TTBR0_EL1, 0x10000) // L0 at IPA 0x10000
	C.hv_vcpu_set_reg(vcpu, C.HV_REG_X18, 0x8000)                // data page VA

	// Start: ERET from EL1 → EL0 at user code (VA 0x4000)
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_ELR_EL1, 0x4000)
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_SPSR_EL1, 0) // EL0t
	C.hv_vcpu_set_reg(vcpu, C.HV_REG_PC, 0x800)            // ERET stub
	C.hv_vcpu_set_reg(vcpu, C.HV_REG_CPSR, 0x3c4)          // EL1h, PAN=0

	hung, ret := runWithTimeout(vcpu, 3)
	if ret != C.HV_SUCCESS {
		fmt.Printf("  hv_vcpu_run: %d\n", ret)
		return
	}
	if hung {
		var pc C.uint64_t
		C.hv_vcpu_get_reg(vcpu, C.HV_REG_PC, &pc)
		var esr C.uint64_t
		C.hv_vcpu_get_sys_reg(vcpu, C.HV_SYS_REG_ESR_EL1, &esr)
		var far C.uint64_t
		C.hv_vcpu_get_sys_reg(vcpu, C.HV_SYS_REG_FAR_EL1, &far)
		fmt.Printf("  HUNG: PC=%#x ESR_EL1=%#x FAR_EL1=%#x\n", uint64(pc), uint64(esr), uint64(far))
		ec := (uint64(esr) >> 26) & 0x3f
		dfsc := uint64(esr) & 0x3f
		fmt.Printf("  EC=%#x DFSC=%#x\n", ec, dfsc)
		if uint64(pc) == 0x200 || (uint64(pc) >= 0x200 && uint64(pc) < 0x280) {
			fmt.Println("  → Current-EL sync fault. EL1 data access FAULTS.")
			if ec == 0x25 {
				fmt.Printf("  → Data abort at current EL. DFSC=%#x\n", dfsc)
				switch dfsc & 0x3c {
				case 0x04:
					fmt.Println("  → Translation fault (unmapped page)")
				case 0x08:
					fmt.Println("  → Access flag fault")
				case 0x0c:
					fmt.Println("  → Permission fault (PAN or AP)")
				}
				fmt.Printf("  → Level: %d\n", dfsc&0x3)
			}
		}
		return
	}

	var x0 C.uint64_t
	C.hv_vcpu_get_reg(vcpu, C.HV_REG_X0, &x0)
	syndrome := uint64(exit.exception.syndrome)
	ec := (syndrome >> 26) & 0x3f
	fmt.Printf("  X0=%#x (want %#x) EC=%#x hvcImm=%d\n",
		uint64(x0), magic, ec, syndrome&0xffff)
	if uint64(x0) == magic {
		fmt.Println("  *** WORKS! EL1 can read memory through TTBR0 stage-1! ***")
	} else {
		fmt.Printf("  FAILED: got %#x\n", uint64(x0))
	}
	fmt.Println()
}

// test2: Check what PAN/CPSR looks like after exception entry
func test2_EL1_TTBR0_PAN_check() {
	fmt.Println("--- Test 2: Dump PSTATE after EL0→EL1 exception ---")

	config := C.hv_vm_config_create()
	defer C.CFRelease(C.CFTypeRef(unsafe.Pointer(config)))
	C.hv_vm_config_set_ipa_size(config, 40)
	C.hv_vm_create(config)
	defer C.hv_vm_destroy()

	var vcpu C.hv_vcpu_t
	var exit *C.hv_vcpu_exit_t
	C.hv_vcpu_create(&vcpu, &exit, nil)
	defer C.hv_vcpu_destroy(vcpu)

	vecMem := alloc(pageSize)
	userMem := alloc(pageSize)
	defer C.free(vecMem)
	defer C.free(userMem)

	C.hv_vm_map(vecMem, 0, C.size_t(pageSize), C.HV_MEMORY_READ|C.HV_MEMORY_EXEC)
	C.hv_vm_map(userMem, C.hv_ipa_t(pageSize), C.size_t(pageSize), C.HV_MEMORY_READ|C.HV_MEMORY_EXEC)
	defer C.hv_vm_unmap(0, C.size_t(pageSize))
	defer C.hv_vm_unmap(C.hv_ipa_t(pageSize), C.size_t(pageSize))

	vectors := (*[pageSize]byte)(vecMem)
	for i := 0; i < 16; i++ {
		binary.LittleEndian.PutUint32(vectors[i*128:], 0xd4000002|uint32(i)<<5)
	}

	// el0_sync: read CPSR into X0 via MRS CurrentEL and MRS DAIF, then HVC #9
	off := 0x400
	put := func(instr uint32) {
		binary.LittleEndian.PutUint32(vectors[off:], instr)
		off += 4
	}
	// MRS X0, NZCV (just to get something)
	// Actually, read PAN: MRS X0, PAN → S3_0_C4_C2_3
	// Encoding: 1101_0101_0011_1000_0100_0010_0110_0000
	put(0xd5384260) // MRS X0, S3_0_C4_C2_3 (PAN)
	put(0xd4000122) // HVC #9

	binary.LittleEndian.PutUint32(vectors[0x800:], 0xd69f03e0) // ERET

	binary.LittleEndian.PutUint32((*[pageSize]byte)(userMem)[:], 0xd4000001) // SVC

	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_VBAR_EL1, 0)
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_CPACR_EL1, 3<<20)
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_SCTLR_EL1, 0) // MMU OFF (simple)
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_ELR_EL1, C.uint64_t(pageSize))
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_SPSR_EL1, 0)
	C.hv_vcpu_set_reg(vcpu, C.HV_REG_PC, 0x800)
	C.hv_vcpu_set_reg(vcpu, C.HV_REG_CPSR, 0x3c4) // EL1h, PAN=0

	hung, _ := runWithTimeout(vcpu, 3)
	if hung {
		fmt.Println("  HUNG")
		return
	}

	var x0, cpsr C.uint64_t
	C.hv_vcpu_get_reg(vcpu, C.HV_REG_X0, &x0)
	C.hv_vcpu_get_reg(vcpu, C.HV_REG_CPSR, &cpsr)
	fmt.Printf("  PAN register (MRS): %#x\n", uint64(x0))
	fmt.Printf("  CPSR at exit: %#x\n", uint64(cpsr))
	fmt.Printf("  CPSR.PAN (bit 22): %d\n", (uint64(cpsr)>>22)&1)
	fmt.Println()
}

// test3: TTBR1 access. Simple: one L1 block descriptor (1GB) instead of
// full 4-level walk. Fewer things to get wrong.
func test3_EL1_TTBR1_simple() {
	fmt.Println("--- Test 3: EL1 LDR via TTBR1, 1GB block mapping ---")

	config := C.hv_vm_config_create()
	defer C.CFRelease(C.CFTypeRef(unsafe.Pointer(config)))
	C.hv_vm_config_set_ipa_size(config, 40)
	C.hv_vm_create(config)
	defer C.hv_vm_destroy()

	var vcpu C.hv_vcpu_t
	var exit *C.hv_vcpu_exit_t
	C.hv_vcpu_create(&vcpu, &exit, nil)
	defer C.hv_vcpu_destroy(vcpu)

	vecMem := alloc(pageSize)
	userMem := alloc(pageSize)
	dataMem := alloc(pageSize)
	ptL0 := alloc(pageSize)     // TTBR0
	ptL1t0 := alloc(pageSize)   // TTBR0 L1
	ptL2t0 := alloc(pageSize)   // TTBR0 L2
	ptL3t0 := alloc(pageSize)   // TTBR0 L3
	kptL0 := alloc(pageSize)    // TTBR1 L0
	kptL1 := alloc(pageSize)    // TTBR1 L1
	kptL2 := alloc(pageSize)    // TTBR1 L2
	kptL3 := alloc(pageSize)    // TTBR1 L3
	allPtrs := []unsafe.Pointer{vecMem, userMem, dataMem, ptL0, ptL1t0, ptL2t0, ptL3t0, kptL0, kptL1, kptL2, kptL3}
	defer func() {
		for _, p := range allPtrs {
			C.free(p)
		}
	}()

	type mp struct {
		p   unsafe.Pointer
		ipa uint64
		f   C.hv_memory_flags_t
	}
	maps := []mp{
		{vecMem, 0x00000, C.HV_MEMORY_READ | C.HV_MEMORY_EXEC},
		{userMem, 0x04000, C.HV_MEMORY_READ | C.HV_MEMORY_EXEC},
		{dataMem, 0x08000, C.HV_MEMORY_READ | C.HV_MEMORY_WRITE},
		{ptL0, 0x10000, C.HV_MEMORY_READ | C.HV_MEMORY_WRITE},
		{ptL1t0, 0x14000, C.HV_MEMORY_READ | C.HV_MEMORY_WRITE},
		{ptL2t0, 0x18000, C.HV_MEMORY_READ | C.HV_MEMORY_WRITE},
		{ptL3t0, 0x1C000, C.HV_MEMORY_READ | C.HV_MEMORY_WRITE},
		{kptL0, 0x20000, C.HV_MEMORY_READ | C.HV_MEMORY_WRITE},
		{kptL1, 0x24000, C.HV_MEMORY_READ | C.HV_MEMORY_WRITE},
		{kptL2, 0x28000, C.HV_MEMORY_READ | C.HV_MEMORY_WRITE},
		{kptL3, 0x2C000, C.HV_MEMORY_READ | C.HV_MEMORY_WRITE},
	}
	for _, m := range maps {
		if ret := C.hv_vm_map(m.p, C.hv_ipa_t(m.ipa), C.size_t(pageSize), m.f); ret != C.HV_SUCCESS {
			fmt.Printf("  hv_vm_map IPA=%#x: %d\n", m.ipa, ret)
			return
		}
	}
	defer func() {
		for _, m := range maps {
			C.hv_vm_unmap(C.hv_ipa_t(m.ipa), C.size_t(pageSize))
		}
	}()

	magic := uint64(0xAAAABBBBCCCCDDDD)
	binary.LittleEndian.PutUint64((*[pageSize]byte)(dataMem)[:], magic)

	tableDesc := uint64(0x3)
	mkPage := func(ipa uint64, el0 bool) uint64 {
		d := ipa | (1 << 10) | (3 << 8) | 0x3
		if el0 {
			d |= (1 << 6)
		}
		return d
	}

	// TTBR0: identity map VA=IPA
	(*[2048]uint64)(ptL0)[0] = 0x14000 | tableDesc
	(*[2048]uint64)(ptL1t0)[0] = 0x18000 | tableDesc
	(*[2048]uint64)(ptL2t0)[0] = 0x1C000 | tableDesc
	(*[2048]uint64)(ptL3t0)[0] = mkPage(0x00000, false) // vectors
	(*[2048]uint64)(ptL3t0)[1] = mkPage(0x04000, true)  // user code (EL0 needs access)
	(*[2048]uint64)(ptL3t0)[2] = mkPage(0x08000, false) // data

	// TTBR1: map kernel VA 0xFFFF000000008000 → IPA 0x8000 (data)
	// With T1SZ=16, VA space is 0xFFFF000000000000..0xFFFFFFFFFFFFFFFF
	// 16K granule: L0 has 2 entries (bit 47)
	// VA 0xFFFF000000008000:
	//   bit[47] = 0 → L0[0]
	//   bits[46:36] = 0 → L1[0]
	//   bits[35:25] = 0 → L2[0]
	//   bits[24:14] = 0x8000>>14 = 0x2 → L3[2]... wait
	// Actually: 0x8000 = 32768. bits[24:14] = 32768 >> 14 = 2. L3[2].

	kernelVA := uint64(0xFFFF000000008000)
	fmt.Printf("  Kernel VA: %#x\n", kernelVA)
	// L0 index: bit[47] of stripped VA
	stripped := kernelVA & ((1 << 48) - 1) // 0x0000000000008000
	l0idx := (stripped >> 47) & 1
	l1idx := (stripped >> 36) & 0x7FF
	l2idx := (stripped >> 25) & 0x7FF
	l3idx := (stripped >> 14) & 0x7FF
	fmt.Printf("  Indices: L0=%d L1=%d L2=%d L3=%d\n", l0idx, l1idx, l2idx, l3idx)

	(*[2048]uint64)(kptL0)[l0idx] = 0x24000 | tableDesc
	(*[2048]uint64)(kptL1)[l1idx] = 0x28000 | tableDesc
	(*[2048]uint64)(kptL2)[l2idx] = 0x2C000 | tableDesc
	(*[2048]uint64)(kptL3)[l3idx] = mkPage(0x08000, false) // EL1-only

	// Vectors
	vectors := (*[pageSize]byte)(vecMem)
	for i := 0; i < 16; i++ {
		binary.LittleEndian.PutUint32(vectors[i*128:], 0xd4000002|uint32(i)<<5)
	}

	off := 0x400
	put := func(instr uint32) {
		binary.LittleEndian.PutUint32(vectors[off:], instr)
		off += 4
	}
	put(0xd500409f) // MSR PAN, #0
	put(0xd5033fdf) // ISB
	put(0xf9400240) // LDR X0, [X18]  ← X18 holds kernelVA
	put(0xd4000122) // HVC #9

	binary.LittleEndian.PutUint32(vectors[0x800:], 0xd69f03e0)

	binary.LittleEndian.PutUint32((*[pageSize]byte)(userMem)[:], 0xd4000001) // SVC

	sctlr := uint64(0x30D00985)
	tcr := uint64(16) | (0x1 << 8) | (0x1 << 10) | (0x3 << 12) | (0x2 << 14) |
		(uint64(16) << 16) | (0x1 << 24) | (0x1 << 26) | (0x3 << 28) |
		(uint64(0x1) << 30) | (uint64(0x2) << 32) | (uint64(1) << 36)

	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_VBAR_EL1, 0)
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_CPACR_EL1, 3<<20)
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_MAIR_EL1, 0xFF)
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_SCTLR_EL1, C.uint64_t(sctlr))
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_TCR_EL1, C.uint64_t(tcr))
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_TTBR0_EL1, 0x10000)
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_TTBR1_EL1, 0x20000)
	C.hv_vcpu_set_reg(vcpu, C.HV_REG_X18, C.uint64_t(kernelVA))
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_ELR_EL1, 0x4000)
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_SPSR_EL1, 0)
	C.hv_vcpu_set_reg(vcpu, C.HV_REG_PC, 0x800)
	C.hv_vcpu_set_reg(vcpu, C.HV_REG_CPSR, 0x3c4)

	hung, _ := runWithTimeout(vcpu, 3)
	if hung {
		var pc, esr, far C.uint64_t
		C.hv_vcpu_get_reg(vcpu, C.HV_REG_PC, &pc)
		C.hv_vcpu_get_sys_reg(vcpu, C.HV_SYS_REG_ESR_EL1, &esr)
		C.hv_vcpu_get_sys_reg(vcpu, C.HV_SYS_REG_FAR_EL1, &far)
		fmt.Printf("  HUNG: PC=%#x ESR=%#x FAR=%#x\n", uint64(pc), uint64(esr), uint64(far))
		ec := (uint64(esr) >> 26) & 0x3f
		dfsc := uint64(esr) & 0x3f
		fmt.Printf("  EC=%#x DFSC=%#x level=%d\n", ec, dfsc, dfsc&3)
		if dfsc&0x3c == 0x04 {
			fmt.Println("  → Translation fault")
		} else if dfsc&0x3c == 0x0c {
			fmt.Println("  → Permission fault")
		}
		fmt.Println()
		return
	}

	var x0 C.uint64_t
	C.hv_vcpu_get_reg(vcpu, C.HV_REG_X0, &x0)
	fmt.Printf("  X0=%#x (want %#x)\n", uint64(x0), magic)
	if uint64(x0) == magic {
		fmt.Println("  *** WORKS! EL1 TTBR1 data access through stage-1! ***")
	}
	fmt.Println()
}

// test6: Re-test ESR_EL1 read from EL1 after EL0→EL1 exception.
// Uses same proven setup as test1 (TTBR0, AP[1]=0, EL1-only pages).
// The el0_sync handler reads ESR_EL1 into X0 and exits via HVC.
// If X0 contains EC=0x15 (SVC), ESR_EL1 reads work from EL1.
func test6_ESR_EL1_retest() {
	fmt.Println("--- Test 6: MRS ESR_EL1 from EL1 after EL0→EL1 SVC ---")

	config := C.hv_vm_config_create()
	defer C.CFRelease(C.CFTypeRef(unsafe.Pointer(config)))
	C.hv_vm_config_set_ipa_size(config, 40)
	C.hv_vm_create(config)
	defer C.hv_vm_destroy()

	var vcpu C.hv_vcpu_t
	var exit *C.hv_vcpu_exit_t
	C.hv_vcpu_create(&vcpu, &exit, nil)
	defer C.hv_vcpu_destroy(vcpu)

	vecMem := alloc(pageSize)
	userMem := alloc(pageSize)
	ptL0 := alloc(pageSize)
	ptL1 := alloc(pageSize)
	ptL2 := alloc(pageSize)
	ptL3 := alloc(pageSize)
	allPtrs := []unsafe.Pointer{vecMem, userMem, ptL0, ptL1, ptL2, ptL3}
	defer func() { for _, p := range allPtrs { C.free(p) } }()

	type mp struct {
		p   unsafe.Pointer
		ipa uint64
		f   C.hv_memory_flags_t
	}
	maps := []mp{
		{vecMem, 0x00000, C.HV_MEMORY_READ | C.HV_MEMORY_EXEC},
		{userMem, 0x04000, C.HV_MEMORY_READ | C.HV_MEMORY_EXEC},
		{ptL0, 0x10000, C.HV_MEMORY_READ | C.HV_MEMORY_WRITE},
		{ptL1, 0x14000, C.HV_MEMORY_READ | C.HV_MEMORY_WRITE},
		{ptL2, 0x18000, C.HV_MEMORY_READ | C.HV_MEMORY_WRITE},
		{ptL3, 0x1C000, C.HV_MEMORY_READ | C.HV_MEMORY_WRITE},
	}
	for _, m := range maps {
		C.hv_vm_map(m.p, C.hv_ipa_t(m.ipa), C.size_t(pageSize), m.f)
	}
	defer func() {
		for _, m := range maps {
			C.hv_vm_unmap(C.hv_ipa_t(m.ipa), C.size_t(pageSize))
		}
	}()

	// Page tables: identity map
	tableDesc := uint64(0x3)
	mkPage := func(ipa uint64, el0 bool) uint64 {
		d := ipa | (1 << 10) | (3 << 8) | 0x3
		if el0 {
			d |= (1 << 6)
		}
		return d
	}
	(*[2048]uint64)(ptL0)[0] = 0x14000 | tableDesc
	(*[2048]uint64)(ptL1)[0] = 0x18000 | tableDesc
	(*[2048]uint64)(ptL2)[0] = 0x1C000 | tableDesc
	(*[2048]uint64)(ptL3)[0] = mkPage(0x00000, false) // vectors EL1-only
	(*[2048]uint64)(ptL3)[1] = mkPage(0x04000, true)  // user EL0

	// el0_sync at 0x400: MRS X0, ESR_EL1; HVC #9
	vectors := (*[pageSize]byte)(vecMem)
	for i := 0; i < 16; i++ {
		binary.LittleEndian.PutUint32(vectors[i*128:], 0xd4000002|uint32(i)<<5)
	}
	off := 0x400
	put := func(instr uint32) { binary.LittleEndian.PutUint32(vectors[off:], instr); off += 4 }
	put(0xd5385200) // MRS X0, ESR_EL1       ← THE KEY TEST
	put(0xd5384101) // MRS X1, ELR_EL1       ← also test ELR
	put(0xd5384002) // MRS X2, SPSR_EL1      ← and SPSR
	put(0xd5386003) // MRS X3, FAR_EL1       ← and FAR
	put(0xd4000122) // HVC #9

	binary.LittleEndian.PutUint32(vectors[0x800:], 0xd69f03e0) // ERET

	// User code: SVC #0
	binary.LittleEndian.PutUint32((*[pageSize]byte)(userMem)[:], 0xd4000001)

	sctlr := uint64(0x30D00985)
	tcr := uint64(16) | (0x1 << 8) | (0x1 << 10) | (0x3 << 12) | (0x2 << 14) |
		(uint64(16) << 16) | (0x1 << 24) | (0x1 << 26) | (0x3 << 28) |
		(uint64(0x1) << 30) | (uint64(0x2) << 32) | (uint64(1) << 36)

	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_VBAR_EL1, 0)
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_CPACR_EL1, 3<<20)
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_MAIR_EL1, 0xFF)
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_SCTLR_EL1, C.uint64_t(sctlr))
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_TCR_EL1, C.uint64_t(tcr))
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_TTBR0_EL1, 0x10000)
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_ELR_EL1, 0x4000)
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_SPSR_EL1, 0)
	C.hv_vcpu_set_reg(vcpu, C.HV_REG_PC, 0x800)
	C.hv_vcpu_set_reg(vcpu, C.HV_REG_CPSR, 0x3c4)

	hung, _ := runWithTimeout(vcpu, 5)
	if hung {
		var pc C.uint64_t
		C.hv_vcpu_get_reg(vcpu, C.HV_REG_PC, &pc)
		fmt.Printf("  HUNG at PC=%#x\n", uint64(pc))
		if uint64(pc) >= 0x400 && uint64(pc) <= 0x414 {
			fmt.Println("  → MRS ESR_EL1 hangs (confirmed — HVF traps this)")
		}
		fmt.Println()
		return
	}

	var x0, x1, x2, x3 C.uint64_t
	C.hv_vcpu_get_reg(vcpu, C.HV_REG_X0, &x0)
	C.hv_vcpu_get_reg(vcpu, C.HV_REG_X1, &x1)
	C.hv_vcpu_get_reg(vcpu, C.HV_REG_X2, &x2)
	C.hv_vcpu_get_reg(vcpu, C.HV_REG_X3, &x3)

	esr := uint64(x0)
	ec := (esr >> 26) & 0x3f
	fmt.Printf("  ESR_EL1 = %#x (EC=%#x)\n", esr, ec)
	fmt.Printf("  ELR_EL1 = %#x (expected ~0x4000)\n", uint64(x1))
	fmt.Printf("  SPSR_EL1 = %#x\n", uint64(x2))
	fmt.Printf("  FAR_EL1 = %#x\n", uint64(x3))

	if ec == 0x15 {
		fmt.Println("  *** ESR_EL1 WORKS! EC=0x15 = SVC from AArch64 ***")
		fmt.Println("  Full in-VM syscall dispatch is possible!")
	} else if esr == 0 {
		fmt.Println("  ESR_EL1 returned 0 (register not populated or cleared)")
	} else {
		fmt.Printf("  Unexpected EC=%#x\n", ec)
	}
	fmt.Println()
}

// test7: STP chain — save X0-X30 to TTBR1 state page from EL1.
// EL0 sets X0-X7 to known values, does SVC.
// EL1 handler: MRS X16, TPIDR_EL1; STP chain; HVC #9.
// Host reads state page and verifies values match.
func test7_full_EL1_handler() {
	fmt.Println("--- Test 7: STP chain save X0-X30 to TTBR1 state page ---")

	config := C.hv_vm_config_create()
	defer C.CFRelease(C.CFTypeRef(unsafe.Pointer(config)))
	C.hv_vm_config_set_ipa_size(config, 40)
	C.hv_vm_create(config)
	defer C.hv_vm_destroy()

	var vcpu C.hv_vcpu_t
	var exit *C.hv_vcpu_exit_t
	C.hv_vcpu_create(&vcpu, &exit, nil)
	defer C.hv_vcpu_destroy(vcpu)

	vecMem := alloc(pageSize)
	userMem := alloc(pageSize)
	stateMem := alloc(pageSize)
	ptL0 := alloc(pageSize)
	ptL1 := alloc(pageSize)
	ptL2 := alloc(pageSize)
	ptL3 := alloc(pageSize)
	kptL0 := alloc(pageSize)
	kptL1 := alloc(pageSize)
	kptL2 := alloc(pageSize)
	kptL3 := alloc(pageSize)
	allPtrs := []unsafe.Pointer{vecMem, userMem, stateMem, ptL0, ptL1, ptL2, ptL3, kptL0, kptL1, kptL2, kptL3}
	defer func() {
		for _, p := range allPtrs {
			C.free(p)
		}
	}()

	type mp struct {
		p   unsafe.Pointer
		ipa uint64
		f   C.hv_memory_flags_t
	}
	maps := []mp{
		{vecMem, 0x00000, C.HV_MEMORY_READ | C.HV_MEMORY_EXEC},
		{userMem, 0x04000, C.HV_MEMORY_READ | C.HV_MEMORY_EXEC},
		{stateMem, 0x08000, C.HV_MEMORY_READ | C.HV_MEMORY_WRITE},
		{ptL0, 0x10000, C.HV_MEMORY_READ | C.HV_MEMORY_WRITE},
		{ptL1, 0x14000, C.HV_MEMORY_READ | C.HV_MEMORY_WRITE},
		{ptL2, 0x18000, C.HV_MEMORY_READ | C.HV_MEMORY_WRITE},
		{ptL3, 0x1C000, C.HV_MEMORY_READ | C.HV_MEMORY_WRITE},
		{kptL0, 0x20000, C.HV_MEMORY_READ | C.HV_MEMORY_WRITE},
		{kptL1, 0x24000, C.HV_MEMORY_READ | C.HV_MEMORY_WRITE},
		{kptL2, 0x28000, C.HV_MEMORY_READ | C.HV_MEMORY_WRITE},
		{kptL3, 0x2C000, C.HV_MEMORY_READ | C.HV_MEMORY_WRITE},
	}
	for _, m := range maps {
		C.hv_vm_map(m.p, C.hv_ipa_t(m.ipa), C.size_t(pageSize), m.f)
	}
	defer func() {
		for _, m := range maps {
			C.hv_vm_unmap(C.hv_ipa_t(m.ipa), C.size_t(pageSize))
		}
	}()

	// TTBR0: identity map
	tableDesc := uint64(0x3)
	mkPage := func(ipa uint64, el0 bool) uint64 {
		d := ipa | (1 << 10) | (3 << 8) | 0x3
		if el0 {
			d |= (1 << 6)
		}
		return d
	}
	(*[2048]uint64)(ptL0)[0] = 0x14000 | tableDesc
	(*[2048]uint64)(ptL1)[0] = 0x18000 | tableDesc
	(*[2048]uint64)(ptL2)[0] = 0x1C000 | tableDesc
	(*[2048]uint64)(ptL3)[0] = mkPage(0x00000, false) // vectors
	(*[2048]uint64)(ptL3)[1] = mkPage(0x04000, true)  // user code
	(*[2048]uint64)(ptL3)[2] = mkPage(0x08000, false) // state page (EL1-only)

	// TTBR1: map kernel VA 0xFFFF000000008000 → IPA 0x8000 (state page)
	kernelVA := uint64(0xFFFF000000008000)
	stripped := kernelVA & ((1 << 48) - 1)
	l0idx := (stripped >> 47) & 1
	l1idx := (stripped >> 36) & 0x7FF
	l2idx := (stripped >> 25) & 0x7FF
	l3idx := (stripped >> 14) & 0x7FF
	(*[2048]uint64)(kptL0)[l0idx] = 0x24000 | tableDesc
	(*[2048]uint64)(kptL1)[l1idx] = 0x28000 | tableDesc
	(*[2048]uint64)(kptL2)[l2idx] = 0x2C000 | tableDesc
	(*[2048]uint64)(kptL3)[l3idx] = mkPage(0x08000, false) // state (EL1-only)

	// Vectors: el0_sync at 0x400
	vectors := (*[pageSize]byte)(vecMem)
	for i := 0; i < 16; i++ {
		binary.LittleEndian.PutUint32(vectors[i*128:], 0xd4000002|uint32(i)<<5)
	}
	// 0x200: fault handler (diagnostic)
	binary.LittleEndian.PutUint32(vectors[0x200:], 0xd5385212) // MRS X18, ESR_EL1
	binary.LittleEndian.PutUint32(vectors[0x204:], 0xd5386011) // MRS X17, FAR_EL1
	binary.LittleEndian.PutUint32(vectors[0x208:], 0xd4000082) // HVC #4

	// el0_sync at 0x400: STP chain to state page, then HVC #9
	// ARM64 STP Xt1, Xt2, [Xn, #imm7*8]:
	//   31 29 | 28 27 | 26 | 25 22 | 21       15 | 14  10 | 9  5 | 4  0
	//   x  0  | 1  0  |  1 | 0  0  | imm7        | Rt2    | Rn   | Rt1
	//   = 0xA9000000 | (imm7<<15) | (Rt2<<10) | (Rn<<5) | Rt1
	off := 0x400
	put := func(instr uint32) {
		binary.LittleEndian.PutUint32(vectors[off:], instr)
		off += 4
	}
	stpEnc := func(rt1, rt2, rn, byteOff int) uint32 {
		imm7 := (byteOff / 8) & 0x7F
		return 0xA9000000 | uint32(imm7<<15) | uint32(rt2<<10) | uint32(rn<<5) | uint32(rt1)
	}
	strEnc := func(rt, rn, byteOff int) uint32 {
		imm12 := byteOff / 8
		return 0xF9000000 | uint32(imm12<<10) | uint32(rn<<5) | uint32(rt)
	}

	put(0xd538d090) // MRS X16, TPIDR_EL1 (state page kernel VA)
	// Save X0-X15 (8 STP pairs, X16 used as base)
	put(stpEnc(0, 1, 16, 0x00))
	put(stpEnc(2, 3, 16, 0x10))
	put(stpEnc(4, 5, 16, 0x20))
	put(stpEnc(6, 7, 16, 0x30))
	put(stpEnc(8, 9, 16, 0x40))
	put(stpEnc(10, 11, 16, 0x50))
	put(stpEnc(12, 13, 16, 0x60))
	put(stpEnc(14, 15, 16, 0x70))
	// X16 clobbered (base reg). X17 available.
	// Save X18-X29 (6 STP pairs)
	put(stpEnc(18, 19, 16, 0x90))
	put(stpEnc(20, 21, 16, 0xA0))
	put(stpEnc(22, 23, 16, 0xB0))
	put(stpEnc(24, 25, 16, 0xC0))
	put(stpEnc(26, 27, 16, 0xD0))
	put(stpEnc(28, 29, 16, 0xE0))
	// Save X30
	put(strEnc(30, 16, 0xF0))
	// HVC #9 to exit
	put(0xd4000122) // HVC #9
	fmt.Printf("  Handler: %d instructions, ends at offset %#x\n", (off-0x400)/4, off)

	// ERET at 0x800
	binary.LittleEndian.PutUint32(vectors[0x800:], 0xd69f03e0)

	// User code: set X0-X7 to known values, then SVC
	user := (*[pageSize]byte)(userMem)
	uoff := 0
	uput := func(instr uint32) {
		binary.LittleEndian.PutUint32(user[uoff:], instr)
		uoff += 4
	}
	// MOVZ Xd, #imm16 = 0xD2800000 | (imm16<<5) | Rd
	for i := 0; i < 8; i++ {
		val := uint32(0x1000 + i)
		uput(0xD2800000 | (val << 5) | uint32(i))
	}
	uput(0xd4000001) // SVC #0

	// System registers
	sctlr := uint64(0x30D00985)
	tcr := uint64(16) | (0x1 << 8) | (0x1 << 10) | (0x3 << 12) | (0x2 << 14) |
		(uint64(16) << 16) | (0x1 << 24) | (0x1 << 26) | (0x3 << 28) |
		(uint64(0x1) << 30) | (uint64(0x2) << 32) | (uint64(1) << 36)

	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_VBAR_EL1, 0)
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_CPACR_EL1, 3<<20)
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_MAIR_EL1, 0xFF)
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_SCTLR_EL1, C.uint64_t(sctlr))
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_TCR_EL1, C.uint64_t(tcr))
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_TTBR0_EL1, 0x10000)
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_TTBR1_EL1, 0x20000)
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_TPIDR_EL1, C.uint64_t(kernelVA))
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_ELR_EL1, 0x4000)
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_SPSR_EL1, 0)
	C.hv_vcpu_set_reg(vcpu, C.HV_REG_PC, 0x800) // ERET → EL0
	C.hv_vcpu_set_reg(vcpu, C.HV_REG_CPSR, 0x3c4)

	hung, _ := runWithTimeout(vcpu, 5)
	if hung {
		var pc C.uint64_t
		C.hv_vcpu_get_reg(vcpu, C.HV_REG_PC, &pc)
		fmt.Printf("  HUNG at PC=%#x\n", uint64(pc))
		if uint64(pc) >= 0x200 && uint64(pc) < 0x280 {
			var x18, x17 C.uint64_t
			C.hv_vcpu_get_reg(vcpu, C.HV_REG_X18, &x18)
			C.hv_vcpu_get_reg(vcpu, C.HV_REG_X17, &x17)
			esr := uint64(x18)
			far := uint64(x17)
			fmt.Printf("  Fault: ESR=%#x (EC=%#x DFSC=%#x) FAR=%#x\n",
				esr, (esr>>26)&0x3f, esr&0x3f, far)
		}
		fmt.Println()
		return
	}

	// Verify: read state page from host side
	sp := (*[pageSize]byte)(stateMem)
	fmt.Println("  State page contents (X0-X7):")
	allMatch := true
	for i := 0; i < 8; i++ {
		val := binary.LittleEndian.Uint64(sp[i*8:])
		expected := uint64(0x1000 + i)
		match := val == expected
		if !match {
			allMatch = false
		}
		marker := "  "
		if !match {
			marker = "!!"
		}
		fmt.Printf("  %s X%d: got=%#x want=%#x\n", marker, i, val, expected)
	}
	// Also check X30
	x30 := binary.LittleEndian.Uint64(sp[0xF0:])
	fmt.Printf("     X30: %#x\n", x30)

	if allMatch {
		fmt.Println("  *** STP CHAIN WORKS! All registers saved to TTBR1 state page! ***")
	} else {
		fmt.Println("  FAILED: some registers not saved correctly")
	}
	fmt.Println()
}

// test8: STTR/LDTR — unprivileged store/load from EL1.
// These do EL0-privilege access, bypassing PAN. If they work on HVF,
// EL1 handlers can read/write user memory (needed for rt_sigprocmask,
// clock_gettime result, uname, etc.).
//
// Flow: EL0 sets X0 = user buffer VA, SVC.
// EL1 handler: STTR X17(=#0xBEEF), [X0]; LDTR X0, [X0]; HVC #9.
// Host checks X0 = 0xBEEF (read back via LDTR).
func test8_STTR_LDTR() {
	fmt.Println("--- Test 8: STTR/LDTR (unprivileged store/load from EL1) ---")

	config := C.hv_vm_config_create()
	defer C.CFRelease(C.CFTypeRef(unsafe.Pointer(config)))
	C.hv_vm_config_set_ipa_size(config, 40)
	C.hv_vm_create(config)
	defer C.hv_vm_destroy()

	var vcpu C.hv_vcpu_t
	var exit *C.hv_vcpu_exit_t
	C.hv_vcpu_create(&vcpu, &exit, nil)
	defer C.hv_vcpu_destroy(vcpu)

	vecMem := alloc(pageSize)
	userMem := alloc(pageSize)
	bufMem := alloc(pageSize)  // user-accessible buffer
	ptL0 := alloc(pageSize)
	ptL1 := alloc(pageSize)
	ptL2 := alloc(pageSize)
	ptL3 := alloc(pageSize)
	allPtrs := []unsafe.Pointer{vecMem, userMem, bufMem, ptL0, ptL1, ptL2, ptL3}
	defer func() { for _, p := range allPtrs { C.free(p) } }()

	type mp struct {
		p   unsafe.Pointer
		ipa uint64
		f   C.hv_memory_flags_t
	}
	maps := []mp{
		{vecMem, 0x00000, C.HV_MEMORY_READ | C.HV_MEMORY_EXEC},
		{userMem, 0x04000, C.HV_MEMORY_READ | C.HV_MEMORY_EXEC},
		{bufMem, 0x08000, C.HV_MEMORY_READ | C.HV_MEMORY_WRITE},
		{ptL0, 0x10000, C.HV_MEMORY_READ | C.HV_MEMORY_WRITE},
		{ptL1, 0x14000, C.HV_MEMORY_READ | C.HV_MEMORY_WRITE},
		{ptL2, 0x18000, C.HV_MEMORY_READ | C.HV_MEMORY_WRITE},
		{ptL3, 0x1C000, C.HV_MEMORY_READ | C.HV_MEMORY_WRITE},
	}
	for _, m := range maps {
		C.hv_vm_map(m.p, C.hv_ipa_t(m.ipa), C.size_t(pageSize), m.f)
	}
	defer func() {
		for _, m := range maps {
			C.hv_vm_unmap(C.hv_ipa_t(m.ipa), C.size_t(pageSize))
		}
	}()

	// TTBR0: identity map
	tableDesc := uint64(0x3)
	mkPage := func(ipa uint64, el0 bool) uint64 {
		d := ipa | (1 << 10) | (3 << 8) | 0x3
		if el0 { d |= (1 << 6) }
		return d
	}
	(*[2048]uint64)(ptL0)[0] = 0x14000 | tableDesc
	(*[2048]uint64)(ptL1)[0] = 0x18000 | tableDesc
	(*[2048]uint64)(ptL2)[0] = 0x1C000 | tableDesc
	(*[2048]uint64)(ptL3)[0] = mkPage(0x00000, false) // vectors: EL1-only
	(*[2048]uint64)(ptL3)[1] = mkPage(0x04000, true)  // user code: EL0+EL1
	(*[2048]uint64)(ptL3)[2] = mkPage(0x08000, true)  // buffer: EL0+EL1 (for STTR)

	// Vectors
	vectors := (*[pageSize]byte)(vecMem)
	for i := 0; i < 16; i++ {
		binary.LittleEndian.PutUint32(vectors[i*128:], 0xd4000002|uint32(i)<<5)
	}
	// 0x200: fault diagnostic
	binary.LittleEndian.PutUint32(vectors[0x200:], 0xd5385212) // MRS X18, ESR_EL1
	binary.LittleEndian.PutUint32(vectors[0x204:], 0xd5386011) // MRS X17, FAR_EL1
	binary.LittleEndian.PutUint32(vectors[0x208:], 0xd4000082) // HVC #4

	// el0_sync at 0x400:
	//   MOV X17, #0xBEEF
	//   STTR X17, [X0]        ; unprivileged store to user buffer
	//   LDTR X0, [X0]         ; unprivileged load back
	//   HVC #9
	off := 0x400
	put := func(instr uint32) {
		binary.LittleEndian.PutUint32(vectors[off:], instr)
		off += 4
	}
	put(0xd2817DD1) // MOV X17, #0xBEEF (0xBEEF << 1 = 0x17DDE... wait)

	// MOVZ X17, #0xBEEF: 0xD2800000 | (0xBEEF << 5) | 17
	beef := uint32(0xBEEF)
	put(0xD2800000 | (beef << 5) | 17) // MOVZ X17, #0xBEEF

	// STTR Xt, [Xn, #0]: 0xB8000800 | (Xn << 5) | Xt
	// STTR X17, [X0]: 0xB8000800 | (0 << 5) | 17 = 0xB8000811
	put(0xB8000811) // STTR X17, [X0, #0]

	// LDTR Xt, [Xn, #0]: 0xB8400800 | (Xn << 5) | Xt
	// LDTR X0, [X0]: 0xB8400800 | (0 << 5) | 0 = 0xB8400800
	put(0xB8400800) // LDTR X0, [X0, #0]

	put(0xd4000122) // HVC #9

	// Wait — STTR/LDTR are 32-bit (W register) by default.
	// Need 64-bit: STTR Xt uses 0xF8000800 for 64-bit.
	// Let me fix the encoding.
	off = 0x400
	put(0xD2800000 | (beef << 5) | 17) // MOVZ X17, #0xBEEF
	// 64-bit STTR: 0xF8000800 | (imm9<<12) | (Rn<<5) | Rt
	// STTR X17, [X0, #0]: 0xF8000800 | (0<<12) | (0<<5) | 17
	put(0xF8000811) // STTR X17, [X0, #0]  (64-bit)
	// 64-bit LDTR: 0xF8400800 | (imm9<<12) | (Rn<<5) | Rt
	// LDTR X0, [X0, #0]: 0xF8400800 | (0<<12) | (0<<5) | 0
	put(0xF8400800) // LDTR X0, [X0, #0]  (64-bit)
	put(0xd4000122) // HVC #9

	binary.LittleEndian.PutUint32(vectors[0x800:], 0xd69f03e0) // ERET

	// User code: MOV X0, #bufVA; SVC
	bufVA := uint32(0x8000)
	user := (*[pageSize]byte)(userMem)
	binary.LittleEndian.PutUint32(user[0:], 0xD2800000|(bufVA<<5)|0) // MOVZ X0, #0x8000
	binary.LittleEndian.PutUint32(user[4:], 0xd4000001) // SVC #0

	sctlr := uint64(0x30D00985)
	tcr := uint64(16) | (0x1 << 8) | (0x1 << 10) | (0x3 << 12) | (0x2 << 14) |
		(uint64(16) << 16) | (0x1 << 24) | (0x1 << 26) | (0x3 << 28) |
		(uint64(0x1) << 30) | (uint64(0x2) << 32) | (uint64(1) << 36)

	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_VBAR_EL1, 0)
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_CPACR_EL1, 3<<20)
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_MAIR_EL1, 0xFF)
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_SCTLR_EL1, C.uint64_t(sctlr))
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_TCR_EL1, C.uint64_t(tcr))
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_TTBR0_EL1, 0x10000)
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_ELR_EL1, 0x4000)
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_SPSR_EL1, 0)
	C.hv_vcpu_set_reg(vcpu, C.HV_REG_PC, 0x800) // ERET → EL0
	C.hv_vcpu_set_reg(vcpu, C.HV_REG_CPSR, 0x3c4)

	hung, _ := runWithTimeout(vcpu, 5)
	if hung {
		var pc C.uint64_t
		C.hv_vcpu_get_reg(vcpu, C.HV_REG_PC, &pc)
		fmt.Printf("  HUNG at PC=%#x\n", uint64(pc))
		if uint64(pc) >= 0x200 && uint64(pc) < 0x280 {
			var x18, x17 C.uint64_t
			C.hv_vcpu_get_reg(vcpu, C.HV_REG_X18, &x18)
			C.hv_vcpu_get_reg(vcpu, C.HV_REG_X17, &x17)
			esr := uint64(x18)
			far := uint64(x17)
			fmt.Printf("  Fault: ESR=%#x (EC=%#x DFSC=%#x) FAR=%#x\n",
				esr, (esr>>26)&0x3f, esr&0x3f, far)
		}
		fmt.Println()
		return
	}

	var x0 C.uint64_t
	C.hv_vcpu_get_reg(vcpu, C.HV_REG_X0, &x0)

	// Also check host-side buffer
	hostVal := binary.LittleEndian.Uint64((*[pageSize]byte)(bufMem)[:])
	fmt.Printf("  X0=%#x (want 0xBEEF) host=%#x\n", uint64(x0), hostVal)

	if uint64(x0) == 0xBEEF {
		fmt.Println("  *** STTR+LDTR WORK! EL1 can access user memory! ***")
		fmt.Println("  This enables in-VM rt_sigprocmask, clock_gettime, uname!")
	} else if hostVal == 0xBEEF {
		fmt.Println("  STTR wrote correctly but LDTR returned wrong value")
	} else {
		fmt.Println("  FAILED: STTR/LDTR not working")
	}
	fmt.Println()
}
