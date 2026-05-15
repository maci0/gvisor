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

// Command vmtest is a standalone reproducer for HVF VM exit path
// optimizations. Tests whether the EL1 exception handler can access
// memory via TTBR1 (state page), TTBR0 (vectors page), or direct
// IPA (MMU off) to cache syscall results or save/load registers.
//
// Build and run:
//
//	go build -o vmtest ./cmd/vmtest
//	codesign -s - --entitlements cmd/sentrydarwin/entitlements.plist -f vmtest
//	./vmtest
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
	"os"
	"runtime"
	"sync"
	"time"
	"unsafe"
)

const pageSize = 16384

func main() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	fmt.Println("=== HVF VM Exit Path Optimization Tests ===")
	fmt.Println()

	// Test 1: TTBR1 read from EL1 handler (MMU on)
	fmt.Println("--- Test 1: TTBR1 LDP from EL1 handler (MMU on) ---")
	testTTBR1Read()
	fmt.Println()

	// Test 2: TTBR1 write from EL1 handler (MMU on)
	fmt.Println("--- Test 2: TTBR1 STP from EL1 handler (MMU on) ---")
	testTTBR1Write()
	fmt.Println()

	// Test 3: Direct IPA read from EL1 handler (MMU off)
	fmt.Println("--- Test 3: LDP from IPA (MMU off) ---")
	testDirectIPARead()
	fmt.Println()

	// Test 4: TTBR0 read from EL1 handler (vectors page)
	fmt.Println("--- Test 4: TTBR0 LDP from EL1 (vectors page data) ---")
	testTTBR0Read()
	fmt.Println()

	// Test 5: ERET with MOV X0 (no memory access, just register + ERET)
	fmt.Println("--- Test 5: EL1 MOV X0 + ERET (no memory access) ---")
	testEretModifyX0()
	fmt.Println()

	// Test 6: EL1 writes to guest memory (STR to TTBR0 VA)
	fmt.Println("--- Test 6: EL1 STR to guest buffer (MMU off) ---")
	testEL1Write()
	fmt.Println()

	// Test 7: MMU toggle — disable MMU, access data at IPA, re-enable
	fmt.Println("--- Test 7: MMU toggle from EL1 (disable, STR, re-enable) ---")
	testMMUToggle()
	fmt.Println()

	// Test 8: Direct STR to TTBR0 VA from EL1 (MMU on, no toggle)
	fmt.Println("--- Test 8: EL1 STR to guest VA (MMU on, TTBR0) ---")
	testEL1WriteMMUOn()
	fmt.Println()
}

func runWithTimeout(vcpu C.hv_vcpu_t) (bool, C.hv_return_t) {
	var hung bool
	var mu sync.Mutex
	go func() {
		time.Sleep(3 * time.Second)
		mu.Lock()
		hung = true
		mu.Unlock()
		C.hv_vcpus_exit(&vcpu, 1)
	}()
	ret := C.hv_vcpu_run(vcpu)
	mu.Lock()
	defer mu.Unlock()
	return hung, ret
}

func createVM() {
	config := C.hv_vm_config_create()
	defer C.CFRelease(C.CFTypeRef(unsafe.Pointer(config)))
	C.hv_vm_config_set_ipa_size(config, 40)
	ret := C.hv_vm_create(config)
	if ret != C.HV_SUCCESS {
		fmt.Printf("hv_vm_create: %d\n", ret)
		os.Exit(1)
	}
}

func destroyVM() {
	C.hv_vm_destroy()
}

// testTTBR1Read: can the EL1 handler read from a TTBR1-mapped page?
//
// Setup: MMU on, TTBR1 maps a "state page" at a kernel VA.
// EL0 code does SVC → el0_sync handler reads from state page via
// TPIDR_EL1 → loads value into X0 → HVC #8.
// Host checks: did X0 get the expected value?
func testTTBR1Read() {
	createVM()
	defer destroyVM()

	var vcpu C.hv_vcpu_t
	var exit *C.hv_vcpu_exit_t
	C.hv_vcpu_create(&vcpu, &exit, nil)
	defer C.hv_vcpu_destroy(vcpu)

	// Allocate pages: vectors, user code, stack, state page, L0-L3 page tables
	var vecMem, userMem, stackMem, stateMem unsafe.Pointer
	var ptL0, ptL1, ptL2, ptL3 unsafe.Pointer     // TTBR0 page tables
	var kptL0, kptL1, kptL2, kptL3 unsafe.Pointer // TTBR1 page tables

	allocs := []*unsafe.Pointer{
		&vecMem, &userMem, &stackMem, &stateMem,
		&ptL0, &ptL1, &ptL2, &ptL3,
		&kptL0, &kptL1, &kptL2, &kptL3,
	}
	for _, p := range allocs {
		C.posix_memalign(p, C.size_t(pageSize), C.size_t(pageSize))
		C.memset(*p, 0, C.size_t(pageSize))
	}
	defer func() {
		for _, p := range allocs {
			C.free(*p)
		}
	}()

	// IPA layout:
	// 0x00000: vectors (RX)
	// 0x04000: user code (RX)
	// 0x08000: stack (RW)
	// 0x0C000: state page (RW)
	// 0x10000: TTBR0 L0
	// 0x14000: TTBR0 L1
	// 0x18000: TTBR0 L2
	// 0x1C000: TTBR0 L3
	// 0x20000: TTBR1 L0
	// 0x24000: TTBR1 L1
	// 0x28000: TTBR1 L2
	// 0x2C000: TTBR1 L3
	type mapping struct {
		mem   unsafe.Pointer
		ipa   uint64
		flags C.hv_memory_flags_t
	}
	mappings := []mapping{
		{vecMem, 0x00000, C.HV_MEMORY_READ | C.HV_MEMORY_EXEC},
		{userMem, 0x04000, C.HV_MEMORY_READ | C.HV_MEMORY_EXEC},
		{stackMem, 0x08000, C.HV_MEMORY_READ | C.HV_MEMORY_WRITE},
		{stateMem, 0x0C000, C.HV_MEMORY_READ | C.HV_MEMORY_WRITE},
		{ptL0, 0x10000, C.HV_MEMORY_READ | C.HV_MEMORY_WRITE},
		{ptL1, 0x14000, C.HV_MEMORY_READ | C.HV_MEMORY_WRITE},
		{ptL2, 0x18000, C.HV_MEMORY_READ | C.HV_MEMORY_WRITE},
		{ptL3, 0x1C000, C.HV_MEMORY_READ | C.HV_MEMORY_WRITE},
		{kptL0, 0x20000, C.HV_MEMORY_READ | C.HV_MEMORY_WRITE},
		{kptL1, 0x24000, C.HV_MEMORY_READ | C.HV_MEMORY_WRITE},
		{kptL2, 0x28000, C.HV_MEMORY_READ | C.HV_MEMORY_WRITE},
		{kptL3, 0x2C000, C.HV_MEMORY_READ | C.HV_MEMORY_WRITE},
	}
	for _, m := range mappings {
		ret := C.hv_vm_map(m.mem, C.hv_ipa_t(m.ipa), C.size_t(pageSize), m.flags)
		if ret != C.HV_SUCCESS {
			fmt.Printf("  hv_vm_map IPA=%#x: %d\n", m.ipa, ret)
			return
		}
	}
	defer func() {
		for _, m := range mappings {
			C.hv_vm_unmap(C.hv_ipa_t(m.ipa), C.size_t(pageSize))
		}
	}()

	// Write magic value to state page
	magic := uint64(0xDEADBEEF12345678)
	binary.LittleEndian.PutUint64((*[pageSize]byte)(stateMem)[:], magic)

	// Build TTBR0 page table (16K granule, 48-bit VA)
	// L0[0] → L1, L1[0] → L2, L2[0] → L3
	// L3[0] → vectors (0x00000), L3[1] → user (0x04000),
	// L3[2] → stack (0x08000), L3[3] → state (0x0C000)
	validTable := uint64(0x3) // valid + table
	l0 := (*[2048]uint64)(ptL0)
	l1 := (*[2048]uint64)(ptL1)
	l2 := (*[2048]uint64)(ptL2)
	l3 := (*[2048]uint64)(ptL3)
	l0[0] = 0x14000 | validTable // L1
	l1[0] = 0x18000 | validTable // L2
	l2[0] = 0x1C000 | validTable // L3
	// L3 entries: AF=1, SH=ISH, nG=1, AP[1]=1 (EL0), valid+page
	apEL0 := uint64(1 << 6) // AP[1]=1 for EL0 access
	ngBit := uint64(1 << 11)
	afBit := uint64(1 << 10)
	shBits := uint64(3 << 8)
	pageDesc := afBit | shBits | ngBit | apEL0 | 0x3
	l3[0] = 0x00000 | pageDesc // vectors at VA 0
	l3[1] = 0x04000 | pageDesc // user code at VA 0x4000
	l3[2] = 0x08000 | pageDesc | (1 << 7) // stack: AP[2]=0 (RW) — wait, need to clear AP[2]
	// Actually AP[2]=0 means RW, AP[2]=1 means RO. Default is 0 (RW).
	l3[2] = 0x08000 | afBit | shBits | ngBit | apEL0 | 0x3 // stack RW
	l3[3] = 0x0C000 | afBit | shBits | ngBit | apEL0 | 0x3 // state RW

	// Build TTBR1 page table (16K granule, 48-bit VA for upper half)
	// kernelVABase = 0xFFFFFFF000000000
	// L0 index for this VA: bit 47 = 1 → index 1
	// L1 index: bits [46:36] of 0xFFFFFFF000000000 = all 1s = 2047
	// L2 index: bits [35:25] of 0xFFFFFFF000000000 = 0x780 = 1920
	// L3 index: 0 for first page
	kl0 := (*[2048]uint64)(kptL0)
	kl1 := (*[2048]uint64)(kptL1)
	kl2 := (*[2048]uint64)(kptL2)
	kl3 := (*[2048]uint64)(kptL3)
	kl0[1] = 0x24000 | validTable    // L0[1] → L1
	kl1[2047] = 0x28000 | validTable // L1[2047] → L2
	kl2[0] = 0x2C000 | validTable    // L2[0] → L3
	// L3[0]: state page at kernel VA, global (no nG), EL1-only (no AP[1])
	kl3[0] = 0x0C000 | afBit | shBits | 0x3 // global, EL1-only, valid+page

	kernelVA := uint64(0xFFFFFFF000000000)

	// Build vectors: el0_sync at 0x400 reads state page via TPIDR_EL1
	vectors := (*[pageSize]byte)(vecMem)

	// Default handlers: HVC #i
	for i := 0; i < 16; i++ {
		hvc := uint32(0xd4000002) | (uint32(i) << 5)
		binary.LittleEndian.PutUint32(vectors[i*128:], hvc)
	}

	// el0_sync at 0x400:
	//   MRS X18, TPIDR_EL1  → state page VA
	//   LDR X0, [X18]       → load magic value
	//   HVC #8               → exit with value in X0
	off := 0x400
	putInstr := func(instr uint32) {
		binary.LittleEndian.PutUint32(vectors[off:], instr)
		off += 4
	}
	putInstr(0xd538d092) // MRS X18, TPIDR_EL1
	putInstr(0xf9400240) // LDR X0, [X18]
	putInstr(0xd4000102) // HVC #8

	// ERET at 0x800
	binary.LittleEndian.PutUint32(vectors[0x800:], 0xd69f03e0) // ERET

	// User code at IPA 0x4000: SVC #0
	userCode := (*[pageSize]byte)(userMem)
	binary.LittleEndian.PutUint32(userCode[0:], 0xd4000001) // SVC #0

	// Configure vCPU
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_VBAR_EL1, 0)
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_CPACR_EL1, 3<<20)
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_MAIR_EL1, 0xFF)
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_SCTLR_EL1, 0x34909185) // MMU on (production value)

	// TCR: T0SZ=16, T1SZ=16, TG0=16K, TG1=16K, IPS=40-bit, AS=16-bit
	tcr := uint64(16) | (0x1 << 8) | (0x1 << 10) | (0x3 << 12) | (0x2 << 14) |
		(uint64(16) << 16) | (0x1 << 24) | (0x1 << 26) | (0x3 << 28) |
		(uint64(0x1) << 30) | (uint64(0x2) << 32) | (uint64(1) << 36)
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_TCR_EL1, C.uint64_t(tcr))

	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_TTBR0_EL1, C.uint64_t(0x10000)) // L0 IPA
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_TTBR1_EL1, C.uint64_t(0x20000)) // kernel L0 IPA
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_TPIDR_EL1, C.uint64_t(kernelVA)) // state page VA
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_SP_EL1, C.uint64_t(0x08000+pageSize-16))
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_SP_EL0, C.uint64_t(0x08000+pageSize-16))

	// Start at EL1h → ERET → EL0 at user code (VA 0x4000)
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_ELR_EL1, C.uint64_t(0x4000))
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_SPSR_EL1, 0) // EL0t
	C.hv_vcpu_set_reg(vcpu, C.HV_REG_PC, C.uint64_t(0x800)) // ERET
	C.hv_vcpu_set_reg(vcpu, C.HV_REG_CPSR, 0x3c5) // EL1h

	fmt.Println("  Flow: EL1 ERET → EL0 SVC → el0_sync: LDR X0,[TPIDR_EL1] → HVC #8")

	wasHung, ret := runWithTimeout(vcpu)
	if ret != C.HV_SUCCESS {
		fmt.Printf("  hv_vcpu_run: %d\n", ret)
		return
	}

	if wasHung {
		var pc C.uint64_t
		C.hv_vcpu_get_reg(vcpu, C.HV_REG_PC, &pc)
		fmt.Printf("  HUNG: PC=0x%x\n", uint64(pc))
		if uint64(pc) >= 0x400 && uint64(pc) < 0x410 {
			fmt.Println("  VERDICT: TTBR1 LDP from EL1 handler HANGS")
		} else {
			fmt.Printf("  VERDICT: Hung at unexpected location\n")
		}
		return
	}

	var x0 C.uint64_t
	C.hv_vcpu_get_reg(vcpu, C.HV_REG_X0, &x0)
	fmt.Printf("  X0 = 0x%x (expected 0x%x)\n", uint64(x0), magic)

	syndrome := uint64(exit.exception.syndrome)
	ec := (syndrome >> 26) & 0x3f
	fmt.Printf("  Exit: EC=0x%x, syndrome=0x%x\n", ec, syndrome)

	if uint64(x0) == magic {
		fmt.Println("  *** VERDICT: TTBR1 LDP from EL1 handler WORKS! ***")
	} else if uint64(x0) == 0 {
		fmt.Println("  VERDICT: LDP returned 0 (page not mapped or TLB miss)")
	} else {
		fmt.Printf("  VERDICT: LDP returned unexpected value 0x%x\n", uint64(x0))
	}
}

// Placeholder for other tests
func testTTBR1Write() {
	fmt.Println("  (TODO: implement STP to TTBR1 state page)")
}

// testDirectIPARead: with MMU off, IPA=PA. LDR from state page IPA directly.
func testDirectIPARead() {
	createVM()
	defer destroyVM()

	var vcpu C.hv_vcpu_t
	var exit *C.hv_vcpu_exit_t
	C.hv_vcpu_create(&vcpu, &exit, nil)
	defer C.hv_vcpu_destroy(vcpu)

	var vecMem, userMem, stackMem, stateMem unsafe.Pointer
	allocs := []*unsafe.Pointer{&vecMem, &userMem, &stackMem, &stateMem}
	for _, p := range allocs {
		C.posix_memalign(p, C.size_t(pageSize), C.size_t(pageSize))
		C.memset(*p, 0, C.size_t(pageSize))
	}
	defer func() { for _, p := range allocs { C.free(*p) } }()

	// Map at sequential IPAs
	C.hv_vm_map(vecMem, 0, C.size_t(pageSize), C.HV_MEMORY_READ|C.HV_MEMORY_EXEC)
	C.hv_vm_map(userMem, C.hv_ipa_t(pageSize), C.size_t(pageSize), C.HV_MEMORY_READ|C.HV_MEMORY_EXEC)
	C.hv_vm_map(stackMem, C.hv_ipa_t(2*pageSize), C.size_t(pageSize), C.HV_MEMORY_READ|C.HV_MEMORY_WRITE)
	C.hv_vm_map(stateMem, C.hv_ipa_t(3*pageSize), C.size_t(pageSize), C.HV_MEMORY_READ|C.HV_MEMORY_WRITE)
	defer func() {
		for i := uint64(0); i < 4; i++ {
			C.hv_vm_unmap(C.hv_ipa_t(i*uint64(pageSize)), C.size_t(pageSize))
		}
	}()

	// Write magic to state page
	magic := uint64(0xCAFEBABE42424242)
	binary.LittleEndian.PutUint64((*[pageSize]byte)(stateMem)[:], magic)

	// Vectors: el0_sync at 0x400 loads from state page via TPIDR_EL1
	vectors := (*[pageSize]byte)(vecMem)
	for i := 0; i < 16; i++ {
		binary.LittleEndian.PutUint32(vectors[i*128:], uint32(0xd4000002)|(uint32(i)<<5))
	}
	off := 0x400
	put := func(instr uint32) { binary.LittleEndian.PutUint32(vectors[off:], instr); off += 4 }
	put(0xd538d092) // MRS X18, TPIDR_EL1 (holds state page IPA directly)
	put(0xf9400240) // LDR X0, [X18]
	put(0xd4000102) // HVC #8

	binary.LittleEndian.PutUint32(vectors[0x800:], 0xd69f03e0) // ERET

	// User code at IPA pageSize: SVC #0
	binary.LittleEndian.PutUint32((*[pageSize]byte)(userMem)[:], 0xd4000001)

	stateIPA := uint64(3 * pageSize)

	// Configure: MMU OFF, TPIDR_EL1 = state page IPA (= PA with MMU off)
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_VBAR_EL1, 0)
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_CPACR_EL1, 3<<20)
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_SCTLR_EL1, 0) // MMU OFF
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_TPIDR_EL1, C.uint64_t(stateIPA))
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_SP_EL1, C.uint64_t(2*uint64(pageSize)+uint64(pageSize)-16))
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_SP_EL0, C.uint64_t(2*uint64(pageSize)+uint64(pageSize)-16))
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_ELR_EL1, C.uint64_t(pageSize)) // user code
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_SPSR_EL1, 0) // EL0t
	C.hv_vcpu_set_reg(vcpu, C.HV_REG_PC, 0x800) // ERET
	C.hv_vcpu_set_reg(vcpu, C.HV_REG_CPSR, 0x3c5)

	fmt.Println("  Flow: EL1 ERET → EL0 SVC → el0_sync: LDR X0,[IPA] (MMU off) → HVC #8")

	wasHung, ret := runWithTimeout(vcpu)
	if ret != C.HV_SUCCESS {
		fmt.Printf("  hv_vcpu_run: %d\n", ret)
		return
	}

	if wasHung {
		var pc C.uint64_t
		C.hv_vcpu_get_reg(vcpu, C.HV_REG_PC, &pc)
		fmt.Printf("  HUNG: PC=0x%x\n", uint64(pc))
		return
	}

	var x0 C.uint64_t
	C.hv_vcpu_get_reg(vcpu, C.HV_REG_X0, &x0)
	fmt.Printf("  X0 = 0x%x (expected 0x%x)\n", uint64(x0), magic)
	if uint64(x0) == magic {
		fmt.Println("  *** VERDICT: LDR from IPA (MMU off) WORKS! ***")
	} else {
		fmt.Printf("  VERDICT: unexpected value\n")
	}
}

// testTTBR0Read: read data from vectors page (mapped in TTBR0) from EL1 handler.
// testEretModifyX0: Can the EL1 handler set X0 and ERET back to EL0?
// Uses MMU off (proven working for data access). Two SVCs:
// 1st SVC: handler sets X0=#0x42, X1=#1, ERETs
// 2nd SVC: handler checks X1!=0, exits via HVC #8
// Host reads X0 — if 0x42, ERET preserved registers.
func testEretModifyX0() {
	createVM()
	defer destroyVM()

	var vcpu C.hv_vcpu_t
	var exit *C.hv_vcpu_exit_t
	C.hv_vcpu_create(&vcpu, &exit, nil)
	defer C.hv_vcpu_destroy(vcpu)

	var vecMem, userMem, stackMem unsafe.Pointer
	allocs := []*unsafe.Pointer{&vecMem, &userMem, &stackMem}
	for _, p := range allocs {
		C.posix_memalign(p, C.size_t(pageSize), C.size_t(pageSize))
		C.memset(*p, 0, C.size_t(pageSize))
	}
	defer func() { for _, p := range allocs { C.free(*p) } }()

	C.hv_vm_map(vecMem, 0, C.size_t(pageSize), C.HV_MEMORY_READ|C.HV_MEMORY_EXEC)
	C.hv_vm_map(userMem, C.hv_ipa_t(pageSize), C.size_t(pageSize), C.HV_MEMORY_READ|C.HV_MEMORY_EXEC)
	C.hv_vm_map(stackMem, C.hv_ipa_t(2*pageSize), C.size_t(pageSize), C.HV_MEMORY_READ|C.HV_MEMORY_WRITE)
	defer func() {
		for i := uint64(0); i < 3; i++ {
			C.hv_vm_unmap(C.hv_ipa_t(i*uint64(pageSize)), C.size_t(pageSize))
		}
	}()

	// Build vectors
	vectors := (*[pageSize]byte)(vecMem)
	for i := 0; i < 16; i++ {
		binary.LittleEndian.PutUint32(vectors[i*128:], uint32(0xd4000002)|(uint32(i)<<5))
	}

	// el0_sync at 0x400:
	//   CBZ X1, .+8    → first call (X1=0): jump to MOV
	//   HVC #8          → second call (X1=1): exit
	//   MOV X0, #0x42
	//   MOV X1, #1
	//   ERET             → return to EL0 with X0=0x42
	off := 0x400
	put := func(instr uint32) { binary.LittleEndian.PutUint32(vectors[off:], instr); off += 4 }
	put(0xb4000041) // CBZ X1, .+8
	put(0xd4000102) // HVC #8 (exit on 2nd call)
	put(0xd2800840) // MOV X0, #0x42
	put(0xd2800021) // MOV X1, #1
	put(0xd69f03e0) // ERET

	// ERET stub at 0x800
	binary.LittleEndian.PutUint32(vectors[0x800:], 0xd69f03e0)

	// User code: SVC; SVC (two calls)
	user := (*[pageSize]byte)(userMem)
	binary.LittleEndian.PutUint32(user[0:], 0xd4000001) // SVC #0
	binary.LittleEndian.PutUint32(user[4:], 0xd4000001) // SVC #0

	// Configure: MMU OFF
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_VBAR_EL1, 0)
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_CPACR_EL1, 3<<20)
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_SCTLR_EL1, 0) // MMU OFF
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_SP_EL1, C.uint64_t(2*uint64(pageSize)+uint64(pageSize)-16))
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_SP_EL0, C.uint64_t(2*uint64(pageSize)+uint64(pageSize)-16))
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_ELR_EL1, C.uint64_t(pageSize)) // user code
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_SPSR_EL1, 0) // EL0t
	C.hv_vcpu_set_reg(vcpu, C.HV_REG_PC, 0x800) // ERET → EL0
	C.hv_vcpu_set_reg(vcpu, C.HV_REG_CPSR, 0x3c5) // EL1h

	fmt.Println("  Flow: EL0 SVC → EL1: MOV X0,#0x42; ERET → EL0 SVC → EL1: HVC #8")

	wasHung, ret := runWithTimeout(vcpu)
	if ret != C.HV_SUCCESS {
		fmt.Printf("  hv_vcpu_run: %d\n", ret)
		return
	}
	if wasHung {
		var pc C.uint64_t
		C.hv_vcpu_get_reg(vcpu, C.HV_REG_PC, &pc)
		fmt.Printf("  HUNG: PC=0x%x\n", uint64(pc))

		// Also read ELR_EL1 via API
		var elr C.uint64_t
		C.hv_vcpu_get_sys_reg(vcpu, C.HV_SYS_REG_ELR_EL1, &elr)
		fmt.Printf("  ELR_EL1 (API)=0x%x (expected userIPA+4=0x%x)\n", uint64(elr), pageSize+4)
		return
	}

	var x0, x1 C.uint64_t
	C.hv_vcpu_get_reg(vcpu, C.HV_REG_X0, &x0)
	C.hv_vcpu_get_reg(vcpu, C.HV_REG_X1, &x1)
	syndrome := uint64(exit.exception.syndrome)
	fmt.Printf("  X0=0x%x X1=0x%x syndrome=0x%x\n", uint64(x0), uint64(x1), syndrome)

	if uint64(x0) == 0x42 && uint64(x1) == 1 {
		fmt.Println("  *** VERDICT: ERET preserves modified registers! ***")
		fmt.Println("  In-VM syscall dispatch IS possible via ERET!")
	} else {
		fmt.Println("  VERDICT: registers not preserved")
	}
}

// testEL1Write: Can the EL1 handler write to guest memory?
// Uses MMU off. EL0 passes buffer address in X0.
// Handler writes magic to [X0], sets X0=0, ERETs.
// EL0 reads back the buffer and checks via second SVC.
func testEL1Write() {
	createVM()
	defer destroyVM()

	var vcpu C.hv_vcpu_t
	var exit *C.hv_vcpu_exit_t
	C.hv_vcpu_create(&vcpu, &exit, nil)
	defer C.hv_vcpu_destroy(vcpu)

	var vecMem, userMem, stackMem, dataMem unsafe.Pointer
	allocs := []*unsafe.Pointer{&vecMem, &userMem, &stackMem, &dataMem}
	for _, p := range allocs {
		C.posix_memalign(p, C.size_t(pageSize), C.size_t(pageSize))
		C.memset(*p, 0, C.size_t(pageSize))
	}
	defer func() { for _, p := range allocs { C.free(*p) } }()

	C.hv_vm_map(vecMem, 0, C.size_t(pageSize), C.HV_MEMORY_READ|C.HV_MEMORY_EXEC)
	C.hv_vm_map(userMem, C.hv_ipa_t(pageSize), C.size_t(pageSize), C.HV_MEMORY_READ|C.HV_MEMORY_EXEC)
	C.hv_vm_map(stackMem, C.hv_ipa_t(2*pageSize), C.size_t(pageSize), C.HV_MEMORY_READ|C.HV_MEMORY_WRITE)
	C.hv_vm_map(dataMem, C.hv_ipa_t(3*pageSize), C.size_t(pageSize), C.HV_MEMORY_READ|C.HV_MEMORY_WRITE)
	defer func() {
		for i := uint64(0); i < 4; i++ {
			C.hv_vm_unmap(C.hv_ipa_t(i*uint64(pageSize)), C.size_t(pageSize))
		}
	}()

	dataIPA := uint64(3 * pageSize)

	// Vectors
	vectors := (*[pageSize]byte)(vecMem)
	for i := 0; i < 16; i++ {
		binary.LittleEndian.PutUint32(vectors[i*128:], uint32(0xd4000002)|(uint32(i)<<5))
	}

	// el0_sync: First call: STR magic to [X0], MOV X0=#0, X1=#1, ERET
	//           Second call: check X1, HVC #8 (exit)
	off := 0x400
	put := func(instr uint32) { binary.LittleEndian.PutUint32(vectors[off:], instr); off += 4 }
	put(0xb4000041) // CBZ X1, .+8 (first call)
	put(0xd4000102) // HVC #8 (second call: exit)
	// Write magic to [X0]
	put(0xd2800012) // MOV X18, #0 (will be patched below)
	put(0xf2a7dcf2) // MOVK X18, #0x3EE7, LSL #16
	put(0xf2dfd112) // MOVK X18, #0xFE88, LSL #32
	put(0xf2f5f9f2) // MOVK X18, #0xAFCF, LSL #48
	// Actually let me use a simpler immediate
	// Just write 0x42 as the test value
	off = 0x400 + 8 // reset after CBZ+HVC
	put(0xd2800852) // MOV X18, #0x42
	put(0xf9000012) // STR X18, [X0]
	put(0xd2800000) // MOV X0, #0 (success)
	put(0xd2800021) // MOV X1, #1
	put(0xd69f03e0) // ERET

	binary.LittleEndian.PutUint32(vectors[0x800:], 0xd69f03e0) // ERET

	// User code: MOV X0, #dataIPA; SVC; LDR X2,[X0=dataIPA] (read back); SVC
	user := (*[pageSize]byte)(userMem)
	// MOV X0, #(dataIPA & 0xFFFF)
	binary.LittleEndian.PutUint32(user[0:], 0xd2800000|uint32((dataIPA&0xFFFF)<<5)) // MOV X0, #dataIPA
	binary.LittleEndian.PutUint32(user[4:], 0xd4000001) // SVC #0 (first: handler writes to [X0])
	// After ERET, X0=0 (handler set it). Reload data address.
	binary.LittleEndian.PutUint32(user[8:], 0xd2800000|uint32((dataIPA&0xFFFF)<<5)) // MOV X0, #dataIPA
	binary.LittleEndian.PutUint32(user[12:], 0xf9400002) // LDR X2, [X0] (read written value)
	binary.LittleEndian.PutUint32(user[16:], 0xaa0203e0) // MOV X0, X2 (result in X0)
	binary.LittleEndian.PutUint32(user[20:], 0xd4000001) // SVC #0 (second: exit via HVC)

	// Configure: MMU OFF
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_VBAR_EL1, 0)
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_CPACR_EL1, 3<<20)
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_SCTLR_EL1, 0) // MMU OFF
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_SP_EL1, C.uint64_t(2*uint64(pageSize)+uint64(pageSize)-16))
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_SP_EL0, C.uint64_t(2*uint64(pageSize)+uint64(pageSize)-16))
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_ELR_EL1, C.uint64_t(pageSize))
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_SPSR_EL1, 0)
	C.hv_vcpu_set_reg(vcpu, C.HV_REG_PC, 0x800)
	C.hv_vcpu_set_reg(vcpu, C.HV_REG_CPSR, 0x3c5)

	fmt.Println("  Flow: EL0 → SVC → EL1: STR #0x42 to [X0] → ERET → EL0 reads back → SVC → HVC")

	wasHung, ret := runWithTimeout(vcpu)
	if ret != C.HV_SUCCESS {
		fmt.Printf("  hv_vcpu_run: %d\n", ret)
		return
	}
	if wasHung {
		var pc C.uint64_t
		C.hv_vcpu_get_reg(vcpu, C.HV_REG_PC, &pc)
		fmt.Printf("  HUNG: PC=0x%x\n", uint64(pc))
		return
	}

	var x0, x2 C.uint64_t
	C.hv_vcpu_get_reg(vcpu, C.HV_REG_X0, &x0)
	C.hv_vcpu_get_reg(vcpu, C.HV_REG_X2, &x2)
	fmt.Printf("  X0=0x%x X2=0x%x\n", uint64(x0), uint64(x2))

	// Also check the host-side data page
	hostVal := binary.LittleEndian.Uint64((*[pageSize]byte)(dataMem)[:])
	fmt.Printf("  Host reads data page: 0x%x\n", hostVal)

	if uint64(x0) == 0x42 || hostVal == 0x42 {
		fmt.Println("  *** VERDICT: EL1 STR to guest memory WORKS! ***")
	} else {
		fmt.Println("  VERDICT: write not visible")
	}
}

// testMMUToggle: Can EL1 handler toggle MMU off, write to IPA, toggle back on?
// This enables memory-writing syscalls (clock_gettime, uname, getcpu).
// testEL1WriteMMUOn: Direct STR to guest VA from EL1 with MMU on.
// EL0 passes buffer VA in X0. Handler does STR #0x42, [X0]; ERET.
// Tests if stage-1 data WRITES work from EL1 (reads return 0).
func testEL1WriteMMUOn() {
	createVM()
	defer destroyVM()

	var vcpu C.hv_vcpu_t
	var exit *C.hv_vcpu_exit_t
	C.hv_vcpu_create(&vcpu, &exit, nil)
	defer C.hv_vcpu_destroy(vcpu)

	var vecMem, userMem, stackMem, dataMem unsafe.Pointer
	var ptL0, ptL1, ptL2, ptL3 unsafe.Pointer

	allocs := []*unsafe.Pointer{&vecMem, &userMem, &stackMem, &dataMem, &ptL0, &ptL1, &ptL2, &ptL3}
	for _, p := range allocs {
		C.posix_memalign(p, C.size_t(pageSize), C.size_t(pageSize))
		C.memset(*p, 0, C.size_t(pageSize))
	}
	defer func() { for _, p := range allocs { C.free(*p) } }()

	// IPA layout: vec=0, user=4000, stack=8000, data=C000, pt=10000+
	C.hv_vm_map(vecMem, 0, C.size_t(pageSize), C.HV_MEMORY_READ|C.HV_MEMORY_EXEC)
	C.hv_vm_map(userMem, C.hv_ipa_t(pageSize), C.size_t(pageSize), C.HV_MEMORY_READ|C.HV_MEMORY_EXEC)
	C.hv_vm_map(stackMem, C.hv_ipa_t(2*pageSize), C.size_t(pageSize), C.HV_MEMORY_READ|C.HV_MEMORY_WRITE)
	C.hv_vm_map(dataMem, C.hv_ipa_t(3*pageSize), C.size_t(pageSize), C.HV_MEMORY_READ|C.HV_MEMORY_WRITE)
	C.hv_vm_map(ptL0, C.hv_ipa_t(4*pageSize), C.size_t(pageSize), C.HV_MEMORY_READ|C.HV_MEMORY_WRITE)
	C.hv_vm_map(ptL1, C.hv_ipa_t(5*pageSize), C.size_t(pageSize), C.HV_MEMORY_READ|C.HV_MEMORY_WRITE)
	C.hv_vm_map(ptL2, C.hv_ipa_t(6*pageSize), C.size_t(pageSize), C.HV_MEMORY_READ|C.HV_MEMORY_WRITE)
	C.hv_vm_map(ptL3, C.hv_ipa_t(7*pageSize), C.size_t(pageSize), C.HV_MEMORY_READ|C.HV_MEMORY_WRITE)
	defer func() {
		for i := uint64(0); i < 8; i++ {
			C.hv_vm_unmap(C.hv_ipa_t(i*uint64(pageSize)), C.size_t(pageSize))
		}
	}()

	// Page table: VA=IPA identity mapping
	validTable := uint64(0x3)
	afBit := uint64(1 << 10)
	shBits := uint64(3 << 8)
	apEL0 := uint64(1 << 6) // AP[1]=1: EL0+EL1 access
	pageDesc := afBit | shBits | apEL0 | 0x3

	l0 := (*[2048]uint64)(ptL0)
	l1 := (*[2048]uint64)(ptL1)
	l2 := (*[2048]uint64)(ptL2)
	l3 := (*[2048]uint64)(ptL3)
	l0[0] = uint64(5*pageSize) | validTable
	l1[0] = uint64(6*pageSize) | validTable
	l2[0] = uint64(7*pageSize) | validTable
	l3[0] = 0 | pageDesc
	l3[1] = uint64(pageSize) | pageDesc
	l3[2] = uint64(2*pageSize) | pageDesc
	l3[3] = uint64(3*pageSize) | pageDesc // data page: RW for EL0+EL1

	dataVA := uint64(3 * pageSize) // 0xC000

	// Vectors
	vectors := (*[pageSize]byte)(vecMem)
	for i := 0; i < 16; i++ {
		binary.LittleEndian.PutUint32(vectors[i*128:], uint32(0xd4000002)|(uint32(i)<<5))
	}

	// el0_sync at 0x400:
	//   CBZ X1, .+8; HVC #8  (2nd call: exit)
	//   MOV X18, #0x42; STR X18, [X0]  (write to guest buffer VA)
	//   MOV X0, #0; MOV X1, #1; ERET
	off := 0x400
	put := func(instr uint32) { binary.LittleEndian.PutUint32(vectors[off:], instr); off += 4 }
	put(0xb4000041) // CBZ X1, .+8
	put(0xd4000102) // HVC #8
	put(0xd2800852) // MOV X18, #0x42
	put(0xf9000012) // STR X18, [X0]  ← THE KEY TEST
	put(0xd2800000) // MOV X0, #0
	put(0xd2800021) // MOV X1, #1
	put(0xd69f03e0) // ERET

	binary.LittleEndian.PutUint32(vectors[0x800:], 0xd69f03e0) // ERET

	// User code: MOV X0, #dataVA; SVC; (ERET) LDR X2,[dataVA]; MOV X0,X2; SVC
	user := (*[pageSize]byte)(userMem)
	binary.LittleEndian.PutUint32(user[0:], 0xd2800000|uint32((dataVA&0xFFFF)<<5)) // MOV X0, #dataVA
	binary.LittleEndian.PutUint32(user[4:], 0xd4000001) // SVC (1st: handler writes)
	binary.LittleEndian.PutUint32(user[8:], 0xd2800000|uint32((dataVA&0xFFFF)<<5)) // MOV X0, #dataVA
	binary.LittleEndian.PutUint32(user[12:], 0xf9400002) // LDR X2, [X0]
	binary.LittleEndian.PutUint32(user[16:], 0xaa0203e0) // MOV X0, X2
	binary.LittleEndian.PutUint32(user[20:], 0xd4000001) // SVC (2nd: exit)

	// Configure: MMU ON
	tcr := uint64(16) | (0x1 << 8) | (0x1 << 10) | (0x3 << 12) | (0x2 << 14) |
		(uint64(16) << 16) | (0x1 << 24) | (0x1 << 26) | (0x3 << 28) |
		(uint64(0x1) << 30) | (uint64(0x2) << 32) | (uint64(1) << 36)
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_VBAR_EL1, 0)
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_CPACR_EL1, 3<<20)
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_MAIR_EL1, 0xFF)
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_SCTLR_EL1, 0x34909185) // MMU ON
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_TCR_EL1, C.uint64_t(tcr))
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_TTBR0_EL1, C.uint64_t(4*uint64(pageSize)))
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_SP_EL1, C.uint64_t(2*uint64(pageSize)+uint64(pageSize)-16))
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_SP_EL0, C.uint64_t(2*uint64(pageSize)+uint64(pageSize)-16))
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_ELR_EL1, C.uint64_t(pageSize))
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_SPSR_EL1, 0)
	C.hv_vcpu_set_reg(vcpu, C.HV_REG_PC, 0x800)
	C.hv_vcpu_set_reg(vcpu, C.HV_REG_CPSR, 0x3c5)

	fmt.Println("  Flow: EL0 SVC → EL1: STR #0x42,[X0=VA] (MMU on) → ERET → EL0 reads back")

	wasHung, ret := runWithTimeout(vcpu)
	if ret != C.HV_SUCCESS {
		fmt.Printf("  hv_vcpu_run: %d\n", ret)
		return
	}
	if wasHung {
		var pc C.uint64_t
		C.hv_vcpu_get_reg(vcpu, C.HV_REG_PC, &pc)
		fmt.Printf("  HUNG: PC=0x%x\n", uint64(pc))
		if uint64(pc) >= 0x200 && uint64(pc) < 0x280 {
			fmt.Println("  STR from EL1 caused current-EL fault (permission or translation)")
		} else if uint64(pc) >= 0x400 && uint64(pc) < 0x480 {
			fmt.Println("  Stuck in el0_sync handler")
		}
		return
	}

	var x0, x2 C.uint64_t
	C.hv_vcpu_get_reg(vcpu, C.HV_REG_X0, &x0)
	C.hv_vcpu_get_reg(vcpu, C.HV_REG_X2, &x2)
	hostVal := binary.LittleEndian.Uint64((*[pageSize]byte)(dataMem)[:])
	fmt.Printf("  X0=0x%x X2=0x%x host=0x%x\n", uint64(x0), uint64(x2), hostVal)

	if hostVal == 0x42 {
		fmt.Println("  *** VERDICT: EL1 STR to guest VA with MMU ON WORKS! ***")
		if uint64(x0) == 0x42 {
			fmt.Println("  EL0 read-back also works!")
		} else {
			fmt.Println("  EL0 read-back returned different value (cache coherency?)")
		}
	} else if uint64(x0) == 0x42 {
		fmt.Println("  VERDICT: EL0 reads 0x42 but host sees 0 (write in guest cache only)")
	} else {
		fmt.Println("  VERDICT: write not visible anywhere")
	}
}

func testMMUToggle() {
	createVM()
	defer destroyVM()

	var vcpu C.hv_vcpu_t
	var exit *C.hv_vcpu_exit_t
	C.hv_vcpu_create(&vcpu, &exit, nil)
	defer C.hv_vcpu_destroy(vcpu)

	var vecMem, userMem, stackMem, dataMem unsafe.Pointer
	var ptL0, ptL1, ptL2, ptL3 unsafe.Pointer

	allocs := []*unsafe.Pointer{&vecMem, &userMem, &stackMem, &dataMem, &ptL0, &ptL1, &ptL2, &ptL3}
	for _, p := range allocs {
		C.posix_memalign(p, C.size_t(pageSize), C.size_t(pageSize))
		C.memset(*p, 0, C.size_t(pageSize))
	}
	defer func() { for _, p := range allocs { C.free(*p) } }()

	C.hv_vm_map(vecMem, 0, C.size_t(pageSize), C.HV_MEMORY_READ|C.HV_MEMORY_WRITE|C.HV_MEMORY_EXEC)
	C.hv_vm_map(userMem, C.hv_ipa_t(pageSize), C.size_t(pageSize), C.HV_MEMORY_READ|C.HV_MEMORY_EXEC)
	C.hv_vm_map(stackMem, C.hv_ipa_t(2*pageSize), C.size_t(pageSize), C.HV_MEMORY_READ|C.HV_MEMORY_WRITE)
	C.hv_vm_map(dataMem, C.hv_ipa_t(3*pageSize), C.size_t(pageSize), C.HV_MEMORY_READ|C.HV_MEMORY_WRITE)
	C.hv_vm_map(ptL0, C.hv_ipa_t(4*pageSize), C.size_t(pageSize), C.HV_MEMORY_READ|C.HV_MEMORY_WRITE)
	C.hv_vm_map(ptL1, C.hv_ipa_t(5*pageSize), C.size_t(pageSize), C.HV_MEMORY_READ|C.HV_MEMORY_WRITE)
	C.hv_vm_map(ptL2, C.hv_ipa_t(6*pageSize), C.size_t(pageSize), C.HV_MEMORY_READ|C.HV_MEMORY_WRITE)
	C.hv_vm_map(ptL3, C.hv_ipa_t(7*pageSize), C.size_t(pageSize), C.HV_MEMORY_READ|C.HV_MEMORY_WRITE)
	defer func() {
		for i := uint64(0); i < 8; i++ {
			C.hv_vm_unmap(C.hv_ipa_t(i*uint64(pageSize)), C.size_t(pageSize))
		}
	}()

	dataIPA := uint64(3 * pageSize)

	// TTBR0 page table: VA 0 → IPA 0 (vectors), etc.
	validTable := uint64(0x3)
	afBit := uint64(1 << 10)
	shBits := uint64(3 << 8)
	apEL0 := uint64(1 << 6)
	pageDesc := afBit | shBits | apEL0 | 0x3

	l0 := (*[2048]uint64)(ptL0)
	l1 := (*[2048]uint64)(ptL1)
	l2 := (*[2048]uint64)(ptL2)
	l3 := (*[2048]uint64)(ptL3)
	l0[0] = uint64(5*pageSize) | validTable
	l1[0] = uint64(6*pageSize) | validTable
	l2[0] = uint64(7*pageSize) | validTable
	l3[0] = 0 | pageDesc                       // VA 0 → IPA 0
	l3[1] = uint64(pageSize) | pageDesc         // VA 0x4000 → user
	l3[2] = uint64(2*pageSize) | pageDesc       // VA 0x8000 → stack
	l3[3] = uint64(3*pageSize) | pageDesc       // VA 0xC000 → data

	// Vectors with MMU toggle handler
	vectors := (*[pageSize]byte)(vecMem)
	for i := 0; i < 16; i++ {
		binary.LittleEndian.PutUint32(vectors[i*128:], uint32(0xd4000002)|(uint32(i)<<5))
	}

	// el0_sync at 0x400:
	//   CBZ X1, .+8; HVC #8  (second call exits)
	//   MRS X17, SCTLR_EL1; BIC X16,X17,#1; MSR SCTLR_EL1,X16; ISB
	//   MOV X18, #0x42; STR X18, [X0]  (X0 = IPA of data page)
	//   MSR SCTLR_EL1, X17; ISB
	//   MOV X0, #0; MOV X1, #1; ERET
	off := 0x400
	put := func(instr uint32) { binary.LittleEndian.PutUint32(vectors[off:], instr); off += 4 }
	put(0xb4000041) // CBZ X1, .+8
	put(0xd4000102) // HVC #8 (exit on 2nd call)
	put(0xd5381011) // MRS X17, SCTLR_EL1
	put(0x927ffa30) // AND X16, X17, #~1 (clear M bit)
	put(0xd5181010) // MSR SCTLR_EL1, X16
	put(0xd5033fdf) // ISB
	put(0xd2800852) // MOV X18, #0x42
	put(0xf9000012) // STR X18, [X0] (write to IPA with MMU off)
	put(0xd5181011) // MSR SCTLR_EL1, X17 (re-enable MMU)
	put(0xd5033fdf) // ISB
	put(0xd2800000) // MOV X0, #0
	put(0xd2800021) // MOV X1, #1
	put(0xd69f03e0) // ERET

	binary.LittleEndian.PutUint32(vectors[0x800:], 0xd69f03e0) // ERET

	// User code: MOV X0, #dataIPA; SVC; (after ERET) MOV X0, #dataVA; LDR X2,[X0]; MOV X0,X2; SVC
	user := (*[pageSize]byte)(userMem)
	binary.LittleEndian.PutUint32(user[0:], 0xd2800000|uint32((dataIPA&0xFFFF)<<5)) // MOV X0, #dataIPA
	binary.LittleEndian.PutUint32(user[4:], 0xd4000001) // SVC #0 (handler writes with MMU off)
	// After ERET: read back from data VA (0xC000 = 3*16K)
	dataVA := uint64(3 * pageSize) // same as IPA in our simple mapping
	binary.LittleEndian.PutUint32(user[8:], 0xd2800000|uint32((dataVA&0xFFFF)<<5)) // MOV X0, #dataVA
	binary.LittleEndian.PutUint32(user[12:], 0xf9400002) // LDR X2, [X0]
	binary.LittleEndian.PutUint32(user[16:], 0xaa0203e0) // MOV X0, X2
	binary.LittleEndian.PutUint32(user[20:], 0xd4000001) // SVC #0 (exit)

	// Configure: MMU ON, TTBR0 set
	tcr := uint64(16) | (0x1 << 8) | (0x1 << 10) | (0x3 << 12) | (0x2 << 14) |
		(uint64(16) << 16) | (0x1 << 24) | (0x1 << 26) | (0x3 << 28) |
		(uint64(0x1) << 30) | (uint64(0x2) << 32) | (uint64(1) << 36)
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_VBAR_EL1, 0)
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_CPACR_EL1, 3<<20)
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_MAIR_EL1, 0xFF)
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_SCTLR_EL1, 0x34909185) // MMU ON
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_TCR_EL1, C.uint64_t(tcr))
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_TTBR0_EL1, C.uint64_t(4*uint64(pageSize)))
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_SP_EL1, C.uint64_t(2*uint64(pageSize)+uint64(pageSize)-16))
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_SP_EL0, C.uint64_t(2*uint64(pageSize)+uint64(pageSize)-16))
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_ELR_EL1, C.uint64_t(pageSize)) // user VA
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_SPSR_EL1, 0)
	C.hv_vcpu_set_reg(vcpu, C.HV_REG_PC, 0x800)
	C.hv_vcpu_set_reg(vcpu, C.HV_REG_CPSR, 0x3c5)

	fmt.Println("  Flow: EL0 SVC → EL1: disable MMU, STR, enable MMU, ERET → EL0 reads back")

	wasHung, ret := runWithTimeout(vcpu)
	if ret != C.HV_SUCCESS {
		fmt.Printf("  hv_vcpu_run: %d\n", ret)
		return
	}
	if wasHung {
		var pc C.uint64_t
		C.hv_vcpu_get_reg(vcpu, C.HV_REG_PC, &pc)
		fmt.Printf("  HUNG: PC=0x%x\n", uint64(pc))
		if uint64(pc) >= 0x200 && uint64(pc) < 0x280 {
			fmt.Println("  MSR SCTLR_EL1 from EL1 caused fault (HVF traps it)")
		}
		return
	}

	var x0 C.uint64_t
	C.hv_vcpu_get_reg(vcpu, C.HV_REG_X0, &x0)
	hostVal := binary.LittleEndian.Uint64((*[pageSize]byte)(dataMem)[:])
	fmt.Printf("  X0=0x%x host=0x%x\n", uint64(x0), hostVal)

	if uint64(x0) == 0x42 || hostVal == 0x42 {
		fmt.Println("  *** VERDICT: MMU toggle + STR from EL1 WORKS! ***")
		fmt.Println("  Can now fast-path syscalls that write to user memory!")
	} else {
		fmt.Println("  VERDICT: write not visible or MMU toggle failed")
	}
}

func testTTBR0Read() {
	createVM()
	defer destroyVM()

	var vcpu C.hv_vcpu_t
	var exit *C.hv_vcpu_exit_t
	C.hv_vcpu_create(&vcpu, &exit, nil)
	defer C.hv_vcpu_destroy(vcpu)

	var vecMem, userMem, stackMem unsafe.Pointer
	var ptL0, ptL1, ptL2, ptL3 unsafe.Pointer

	allocs := []*unsafe.Pointer{&vecMem, &userMem, &stackMem, &ptL0, &ptL1, &ptL2, &ptL3}
	for _, p := range allocs {
		C.posix_memalign(p, C.size_t(pageSize), C.size_t(pageSize))
		C.memset(*p, 0, C.size_t(pageSize))
	}
	defer func() { for _, p := range allocs { C.free(*p) } }()

	C.hv_vm_map(vecMem, 0, C.size_t(pageSize), C.HV_MEMORY_READ|C.HV_MEMORY_WRITE|C.HV_MEMORY_EXEC)
	C.hv_vm_map(userMem, C.hv_ipa_t(pageSize), C.size_t(pageSize), C.HV_MEMORY_READ|C.HV_MEMORY_WRITE|C.HV_MEMORY_EXEC)
	C.hv_vm_map(stackMem, C.hv_ipa_t(2*pageSize), C.size_t(pageSize), C.HV_MEMORY_READ|C.HV_MEMORY_WRITE)
	C.hv_vm_map(ptL0, C.hv_ipa_t(3*pageSize), C.size_t(pageSize), C.HV_MEMORY_READ|C.HV_MEMORY_WRITE)
	C.hv_vm_map(ptL1, C.hv_ipa_t(4*pageSize), C.size_t(pageSize), C.HV_MEMORY_READ|C.HV_MEMORY_WRITE)
	C.hv_vm_map(ptL2, C.hv_ipa_t(5*pageSize), C.size_t(pageSize), C.HV_MEMORY_READ|C.HV_MEMORY_WRITE)
	C.hv_vm_map(ptL3, C.hv_ipa_t(6*pageSize), C.size_t(pageSize), C.HV_MEMORY_READ|C.HV_MEMORY_WRITE)
	defer func() {
		for i := uint64(0); i < 7; i++ {
			C.hv_vm_unmap(C.hv_ipa_t(i*uint64(pageSize)), C.size_t(pageSize))
		}
	}()

	// Write magic at vectors page offset 0xF00 (within same 16K page)
	magic := uint64(0x1122334455667788)
	binary.LittleEndian.PutUint64((*[pageSize]byte)(vecMem)[0xF00:], magic)

	// Build TTBR0 page table: map VA 0 → IPA 0 (vectors), VA 0x4000 → user, VA 0x8000 → stack
	validTable := uint64(0x3)
	apEL0 := uint64(1 << 6)
	afBit := uint64(1 << 10)
	shBits := uint64(3 << 8)
	ngBit := uint64(1 << 11)
	pageDesc := afBit | shBits | ngBit | apEL0 | 0x3

	l0 := (*[2048]uint64)(ptL0)
	l1 := (*[2048]uint64)(ptL1)
	l2 := (*[2048]uint64)(ptL2)
	l3 := (*[2048]uint64)(ptL3)
	l0[0] = uint64(4*pageSize) | validTable
	l1[0] = uint64(5*pageSize) | validTable
	l2[0] = uint64(6*pageSize) | validTable
	// Map vectors as global (no nG) so EL1 can access via TTBR0
	globalPageDesc := afBit | shBits | apEL0 | 0x3 // no nG bit
	l3[0] = 0 | globalPageDesc                // VA 0 → IPA 0 (vectors, global)
	l3[1] = uint64(pageSize) | pageDesc         // VA 0x4000 → user
	l3[2] = uint64(2*pageSize) | pageDesc       // VA 0x8000 → stack

	// Vectors: el0_sync loads from VA 0xF00 (same page as vectors)
	vectors := (*[pageSize]byte)(vecMem)
	for i := 0; i < 16; i++ {
		binary.LittleEndian.PutUint32(vectors[i*128:], uint32(0xd4000002)|(uint32(i)<<5))
	}
	off := 0x400
	put := func(instr uint32) { binary.LittleEndian.PutUint32(vectors[off:], instr); off += 4 }
	// First call: MOV X0, #0x42; ERET (return to EL0)
	// Second call: HVC #8 (exit to host, X0 should be 0x42)
	// Use X1 as call counter: CBZ X1,.+12 (first call); HVC #8 (second)
	put(0xb4000041) // CBZ X1, .+8 (skip to MOV if X1==0)
	put(0xd4000102) // HVC #8 (second call: exit with X0)
	put(0xd2800840) // MOV X0, #0x42
	put(0xd2800021) // MOV X1, #1
	put(0xd69f03e0) // ERET

	binary.LittleEndian.PutUint32(vectors[0x800:], 0xd69f03e0) // ERET

	// User code: SVC #0 (first call triggers ERET with X0=#0x42)
	// Then SVC #0 again (second call exits via HVC #8, host reads X0)
	binary.LittleEndian.PutUint32((*[pageSize]byte)(userMem)[0:], 0xd4000001) // SVC #0 (first)
	binary.LittleEndian.PutUint32((*[pageSize]byte)(userMem)[4:], 0xd4000001) // SVC #0 (second)

	// Configure
	tcr := uint64(16) | (0x1 << 8) | (0x1 << 10) | (0x3 << 12) | (0x2 << 14) |
		(uint64(16) << 16) | (0x1 << 24) | (0x1 << 26) | (0x3 << 28) |
		(uint64(0x1) << 30) | (uint64(0x2) << 32) | (uint64(1) << 36)

	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_VBAR_EL1, 0)
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_CPACR_EL1, 3<<20)
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_MAIR_EL1, 0xFF)
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_SCTLR_EL1, 0x30901185)
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_TCR_EL1, C.uint64_t(tcr))
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_TTBR0_EL1, C.uint64_t(3*uint64(pageSize))) // L0 at IPA 3*16K=0xC000
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_SP_EL1, C.uint64_t(2*uint64(pageSize)+uint64(pageSize)-16))
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_SP_EL0, C.uint64_t(2*uint64(pageSize)+uint64(pageSize)-16))
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_ELR_EL1, C.uint64_t(pageSize))
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_SPSR_EL1, 0)
	C.hv_vcpu_set_reg(vcpu, C.HV_REG_PC, 0x800)
	C.hv_vcpu_set_reg(vcpu, C.HV_REG_CPSR, 0x3c5)

	expected := uint64(0x42)
	fmt.Println("  Flow: EL1 ERET → EL0 SVC → el0_sync: MOV X0,#0x42 → HVC #8")

	wasHung, ret := runWithTimeout(vcpu)
	if ret != C.HV_SUCCESS {
		fmt.Printf("  hv_vcpu_run: %d\n", ret)
		return
	}

	if wasHung {
		var pc C.uint64_t
		C.hv_vcpu_get_reg(vcpu, C.HV_REG_PC, &pc)
		fmt.Printf("  HUNG: PC=0x%x\n", uint64(pc))
		return
	}

	var x0, x18 C.uint64_t
	C.hv_vcpu_get_reg(vcpu, C.HV_REG_X0, &x0)
	C.hv_vcpu_get_reg(vcpu, C.HV_REG_X18, &x18)
	var pc C.uint64_t
	C.hv_vcpu_get_reg(vcpu, C.HV_REG_PC, &pc)
	syndrome := uint64(exit.exception.syndrome)
	ec := (syndrome >> 26) & 0x3f
	fmt.Printf("  X0=0x%x X18=0x%x PC=0x%x EC=0x%x\n", uint64(x0), uint64(x18), uint64(pc), ec)
	fmt.Printf("  Exit syndrome=0x%x hvcImm=%d\n", syndrome, syndrome&0xffff)
	if uint64(x0) == expected {
		fmt.Println("  *** VERDICT: Handler executed correctly! ***")
	} else {
		fmt.Println("  VERDICT: handler did not set X0 as expected")
	}
}
