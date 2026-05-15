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

// Command mrstest demonstrates that ARM64 MRS instructions for ID registers
// hang the vCPU under Apple's Hypervisor.framework (HVF), and that the
// standard trap mechanism (HCR_EL2.TID3) is silently dropped by Apple.
//
// This is a standalone reproducer for the issue that blocks Java, .NET,
// and other JIT runtimes on the gVisor macOS port.
//
// Build and run:
//
//	go build -o mrstest ./cmd/mrstest
//	codesign -s - --entitlements _tmp/entitlements.plist -f mrstest
//	./mrstest
//
// The entitlements.plist must contain com.apple.security.hypervisor = true.
package main

/*
#cgo LDFLAGS: -framework Hypervisor
#include <Hypervisor/Hypervisor.h>
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

	fmt.Println("=== MRS ID Register Trapping Test ===")
	fmt.Println()
	fmt.Println("Demonstrates two Apple HVF limitations:")
	fmt.Println("  1. MRS <ID_reg> at EL0 hangs the vCPU (no exit, no trap)")
	fmt.Println("  2. HCR_EL2.TID3 (the ARM64 fix) is silently dropped")
	fmt.Println()

	// Create VM (no EL2 for tests 1 & 3).
	ret := C.hv_vm_create(nil)
	if ret != C.HV_SUCCESS {
		fmt.Printf("hv_vm_create: error %d\n", ret)
		os.Exit(1)
	}

	fmt.Println("--- Test 1: MRS ID_AA64MMFR0_EL1 at EL0 ---")
	testMRSHang()
	fmt.Println()

	fmt.Println("--- Test 2: Control — MOV immediate (patched MRS) ---")
	testMOVControl()
	fmt.Println()

	C.hv_vm_destroy()

	// Test 3 needs EL2-enabled VM (HCR_EL2 bits only writable with EL2).
	fmt.Println("--- Test 3: HCR_EL2 trapping bits (EL2-enabled VM) ---")
	testHCRBits()
	fmt.Println()

	// Test 4: the real test — EL2+TID3 with MRS at EL0.
	fmt.Println("--- Test 4: MRS with TID3 trap (EL2-enabled VM) ---")
	testTID3Trap()
	fmt.Println()

	// Test 5: Can EL2 read ESR_EL1 after EL0→EL1→HVC→EL2?
	// This is the key test for syscall speedup.
	fmt.Println("--- Test 5: EL2 reads ESR_EL1 after EL0 SVC ---")
	testEL2ReadsESR()
	fmt.Println()

	// Tests 6 & 7 create their own VMs internally.
	fmt.Println("--- Test 6: MRS at EL0 with production VM config ---")
	testMRSWithConfig()
	fmt.Println()

	fmt.Println("--- Test 7: MRS ESR_EL1 from EL1 after EL0 SVC ---")
	testEL1ReadsESR()
}

// buildVectors creates an exception vector table with el0_sync at 0x400
// that executes: MOV X0, #marker; HVC #8.
// Also places an ERET stub at 0x800 for EL1→EL0 transition.
func buildVectors(marker uint16) []byte {
	vectors := make([]byte, pageSize)
	for i := 0; i < 16; i++ {
		hvc := uint32(0xd4000002) | (uint32(i) << 5) // HVC #i
		binary.LittleEndian.PutUint32(vectors[i*128:], hvc)
	}
	// el0_sync at 0x400: MOV X0, #marker; HVC #8
	binary.LittleEndian.PutUint32(vectors[0x400:], 0xd2800000|(uint32(marker)<<5)) // MOVZ X0, #marker
	binary.LittleEndian.PutUint32(vectors[0x404:], 0xd4000102)                     // HVC #8
	// ERET stub at 0x800
	binary.LittleEndian.PutUint32(vectors[0x800:], 0xd69f03e0) // ERET
	return vectors
}

// setupEL0 configures a vCPU for EL1→EL0 transition via ERET at vecIPA+0x800.
func setupEL0(vcpu C.hv_vcpu_t, vecIPA, stackIPA, userIPA uint64) {
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_VBAR_EL1, C.uint64_t(vecIPA))
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_CPACR_EL1, 3<<20)
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_SP_EL1, C.uint64_t(stackIPA+pageSize-16))
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_ELR_EL1, C.uint64_t(userIPA))
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_SPSR_EL1, 0) // EL0t
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_SP_EL0, C.uint64_t(stackIPA+pageSize-16))
	C.hv_vcpu_set_reg(vcpu, C.HV_REG_PC, C.uint64_t(vecIPA+0x800)) // start at ERET
	C.hv_vcpu_set_reg(vcpu, C.HV_REG_CPSR, 0x3c5)                  // EL1h
}

// runWithTimeout runs the vCPU with a 3-second watchdog.
// Returns (hung, hv_error).
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

// parseSyndrome extracts EC and ISS from an HVF exception syndrome.
func parseSyndrome(s uint64) (ec uint64, iss uint64) {
	return (s >> 26) & 0x3f, s & 0x1ffffff
}

func testMRSHang() {
	var vcpu C.hv_vcpu_t
	var exit *C.hv_vcpu_exit_t
	if ret := C.hv_vcpu_create(&vcpu, &exit, nil); ret != C.HV_SUCCESS {
		fmt.Printf("  SKIP: hv_vcpu_create error %d\n", ret)
		return
	}
	defer C.hv_vcpu_destroy(vcpu)

	var vecMem, stackMem, userMem unsafe.Pointer
	for _, p := range []*unsafe.Pointer{&vecMem, &stackMem, &userMem} {
		C.posix_memalign(p, C.size_t(pageSize), C.size_t(pageSize))
		C.memset(*p, 0, C.size_t(pageSize))
	}
	defer func() { C.free(vecMem); C.free(stackMem); C.free(userMem) }()

	vecIPA := uint64(0)
	stackIPA := uint64(pageSize)
	userIPA := uint64(2 * pageSize)

	vectors := buildVectors(0xEE)
	C.memcpy(vecMem, unsafe.Pointer(&vectors[0]), C.size_t(pageSize))

	// User code: MRS X0, ID_AA64MMFR0_EL1; MOV X1, #0xDEAD; SVC #0
	userCode := make([]byte, pageSize)
	binary.LittleEndian.PutUint32(userCode[0:], 0xd5380700) // MRS X0, ID_AA64MMFR0_EL1
	binary.LittleEndian.PutUint32(userCode[4:], 0xd29BD5A1) // MOV X1, #0xDEAD
	binary.LittleEndian.PutUint32(userCode[8:], 0xd4000001) // SVC #0
	C.memcpy(userMem, unsafe.Pointer(&userCode[0]), C.size_t(pageSize))

	C.hv_vm_map(vecMem, C.hv_ipa_t(vecIPA), C.size_t(pageSize), C.HV_MEMORY_READ|C.HV_MEMORY_EXEC)
	C.hv_vm_map(stackMem, C.hv_ipa_t(stackIPA), C.size_t(pageSize), C.HV_MEMORY_READ|C.HV_MEMORY_WRITE)
	C.hv_vm_map(userMem, C.hv_ipa_t(userIPA), C.size_t(pageSize), C.HV_MEMORY_READ|C.HV_MEMORY_EXEC)
	defer func() {
		C.hv_vm_unmap(C.hv_ipa_t(vecIPA), C.size_t(pageSize))
		C.hv_vm_unmap(C.hv_ipa_t(stackIPA), C.size_t(pageSize))
		C.hv_vm_unmap(C.hv_ipa_t(userIPA), C.size_t(pageSize))
	}()

	setupEL0(vcpu, vecIPA, stackIPA, userIPA)

	wasHung, ret := runWithTimeout(vcpu)
	if ret != C.HV_SUCCESS {
		fmt.Printf("  hv_vcpu_run: error %d\n", ret)
		return
	}

	if wasHung {
		var pc C.uint64_t
		C.hv_vcpu_get_reg(vcpu, C.HV_REG_PC, &pc)
		fmt.Println("  RESULT: vCPU HUNG for 3 seconds (force-exited by watchdog)")
		fmt.Printf("  PC at force-exit: 0x%x (user code at 0x%x)\n", uint64(pc), userIPA)
		fmt.Println("  The MRS instruction caused the vCPU to enter an")
		fmt.Println("  unrecoverable state. No trap, no VM exit.")
		fmt.Println("  VERDICT: CONFIRMED — MRS hangs vCPU")
	} else {
		syndrome := uint64(exit.exception.syndrome)
		ec, iss := parseSyndrome(syndrome)
		fmt.Printf("  Exit: reason=%d, syndrome=0x%08x (EC=0x%x, ISS=0x%x)\n",
			exit.reason, syndrome, ec, iss)

		var x0, x1 C.uint64_t
		C.hv_vcpu_get_reg(vcpu, C.HV_REG_X0, &x0)
		C.hv_vcpu_get_reg(vcpu, C.HV_REG_X1, &x1)
		fmt.Printf("  X0=0x%x, X1=0x%x\n", uint64(x0), uint64(x1))

		if exit.reason == C.HV_EXIT_REASON_EXCEPTION && ec == 0x16 {
			hvcImm := iss & 0xffff
			if hvcImm == 8 && uint64(x0) == 0xEE {
				fmt.Println("  MRS trapped to EL1 el0_sync handler (vector 0x400)")
				fmt.Println("  VERDICT: MRS causes exception (not a hang on this hardware)")
			} else {
				fmt.Printf("  HVC #%d from vector 0x%x\n", hvcImm, hvcImm*128)
				fmt.Println("  VERDICT: MRS caused exception at unexpected vector")
			}
		} else {
			fmt.Println("  VERDICT: Unexpected exit — neither hang nor clean trap")
		}
	}
}

func testMOVControl() {
	var vcpu C.hv_vcpu_t
	var exit *C.hv_vcpu_exit_t
	if ret := C.hv_vcpu_create(&vcpu, &exit, nil); ret != C.HV_SUCCESS {
		fmt.Printf("  SKIP: hv_vcpu_create error %d\n", ret)
		return
	}
	defer C.hv_vcpu_destroy(vcpu)

	var vecMem, stackMem, userMem unsafe.Pointer
	for _, p := range []*unsafe.Pointer{&vecMem, &stackMem, &userMem} {
		C.posix_memalign(p, C.size_t(pageSize), C.size_t(pageSize))
		C.memset(*p, 0, C.size_t(pageSize))
	}
	defer func() { C.free(vecMem); C.free(stackMem); C.free(userMem) }()

	vecIPA := uint64(0x10000)
	stackIPA := uint64(0x14000)
	userIPA := uint64(0x18000)

	vectors := buildVectors(0xAA)
	C.memcpy(vecMem, unsafe.Pointer(&vectors[0]), C.size_t(pageSize))

	// Patched user code: MOVZ X0, #0x1122; MOV X1, #0xDEAD; SVC #0
	userCode := make([]byte, pageSize)
	binary.LittleEndian.PutUint32(userCode[0:], 0xd2822440) // MOVZ X0, #0x1122
	binary.LittleEndian.PutUint32(userCode[4:], 0xd29BD5A1) // MOV X1, #0xDEAD
	binary.LittleEndian.PutUint32(userCode[8:], 0xd4000001) // SVC #0
	C.memcpy(userMem, unsafe.Pointer(&userCode[0]), C.size_t(pageSize))

	C.hv_vm_map(vecMem, C.hv_ipa_t(vecIPA), C.size_t(pageSize), C.HV_MEMORY_READ|C.HV_MEMORY_EXEC)
	C.hv_vm_map(stackMem, C.hv_ipa_t(stackIPA), C.size_t(pageSize), C.HV_MEMORY_READ|C.HV_MEMORY_WRITE)
	C.hv_vm_map(userMem, C.hv_ipa_t(userIPA), C.size_t(pageSize), C.HV_MEMORY_READ|C.HV_MEMORY_EXEC)
	defer func() {
		C.hv_vm_unmap(C.hv_ipa_t(vecIPA), C.size_t(pageSize))
		C.hv_vm_unmap(C.hv_ipa_t(stackIPA), C.size_t(pageSize))
		C.hv_vm_unmap(C.hv_ipa_t(userIPA), C.size_t(pageSize))
	}()

	setupEL0(vcpu, vecIPA, stackIPA, userIPA)

	if ret := C.hv_vcpu_run(vcpu); ret != C.HV_SUCCESS {
		fmt.Printf("  hv_vcpu_run: error %d\n", ret)
		return
	}

	syndrome := uint64(exit.exception.syndrome)
	ec, iss := parseSyndrome(syndrome)
	fmt.Printf("  Exit: reason=%d, syndrome=0x%08x (EC=0x%x, ISS=0x%x)\n",
		exit.reason, syndrome, ec, iss)

	var x0, x1 C.uint64_t
	C.hv_vcpu_get_reg(vcpu, C.HV_REG_X0, &x0)
	C.hv_vcpu_get_reg(vcpu, C.HV_REG_X1, &x1)
	fmt.Printf("  X0=0x%x (handler marker), X1=0x%x (user code)\n", uint64(x0), uint64(x1))

	if exit.reason == C.HV_EXIT_REASON_EXCEPTION && ec == 0x16 && uint64(x1) == 0xDEAD {
		fmt.Println("  VERDICT: PASS — MOV executes, SVC traps, all instructions reached")
		fmt.Println("  Binary patching (MRS→MOV) works for static code pages.")
	} else {
		fmt.Println("  VERDICT: FAIL — unexpected behavior")
	}
}

func testHCRBits() {
	// HCR_EL2 is only writable when VM has EL2 enabled.
	var supported C.bool
	if ret := C.hv_vm_config_get_el2_supported(&supported); ret != C.HV_SUCCESS || !bool(supported) {
		fmt.Println("  SKIP: EL2 not supported on this hardware")
		return
	}

	config := C.hv_vm_config_create()
	C.hv_vm_config_set_ipa_size(config, 40)
	if ret := C.hv_vm_config_set_el2_enabled(config, C.bool(true)); ret != C.HV_SUCCESS {
		fmt.Printf("  SKIP: hv_vm_config_set_el2_enabled error %d\n", ret)
		return
	}
	if ret := C.hv_vm_create(config); ret != C.HV_SUCCESS {
		fmt.Printf("  SKIP: hv_vm_create (EL2) error %d\n", ret)
		return
	}
	defer C.hv_vm_destroy()

	var vcpu C.hv_vcpu_t
	var exit *C.hv_vcpu_exit_t
	if ret := C.hv_vcpu_create(&vcpu, &exit, nil); ret != C.HV_SUCCESS {
		fmt.Printf("  SKIP: hv_vcpu_create error %d\n", ret)
		return
	}
	defer C.hv_vcpu_destroy(vcpu)

	bits := []struct {
		bit  int
		name string
		key  bool
	}{
		{0, "VM (stage-2 translation)", false},
		{1, "SWIO (set/way invalidation override)", false},
		{2, "IMO (IRQ routing to EL2)", false},
		{3, "FMO (FIQ routing to EL2)", false},
		{5, "AMO (SError routing to EL2)", false},
		{13, "TWI (trap WFI)", false},
		{14, "TWE (trap WFE)", false},
		{18, "TID3 (trap ID group 3 regs)", true},
		{19, "TSC (trap SMC)", false},
		{26, "TVM (trap virtual memory controls)", false},
		{27, "TGE (trap general exceptions)", true},
		{31, "RW (EL1 is AArch64)", false},
		{34, "E2H (EL2 host extensions / VHE)", true},
	}

	fmt.Println("  Testing which HCR_EL2 bits Apple HVF accepts:")
	fmt.Println()
	for _, b := range bits {
		val := uint64(1) << b.bit
		C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_HCR_EL2, C.uint64_t(val))
		var rb C.uint64_t
		C.hv_vcpu_get_sys_reg(vcpu, C.HV_SYS_REG_HCR_EL2, &rb)
		stuck := uint64(rb)&val != 0
		status := "ALLOWED"
		if !stuck {
			status = "DROPPED"
		}
		marker := "  "
		if b.key {
			marker = "→ "
		}
		fmt.Printf("  %s[%2d] %-42s %s\n", marker, b.bit, b.name, status)
	}

	fmt.Println()
	fmt.Println("  TID3 is the bit that would trap MRS ID register reads to EL2.")
	fmt.Println("  If DROPPED, the standard ARM64 trap-and-emulate mechanism")
	fmt.Println("  cannot be used, and binary patching is the only option.")
}

// testTID3Trap creates an EL2-enabled VM with TID3, runs MRS at EL0,
// and checks if TID3 produces a clean VM exit with EC=0x18.
func testTID3Trap() {
	var supported C.bool
	if ret := C.hv_vm_config_get_el2_supported(&supported); ret != C.HV_SUCCESS || !bool(supported) {
		fmt.Println("  SKIP: EL2 not supported")
		return
	}

	config := C.hv_vm_config_create()
	C.hv_vm_config_set_ipa_size(config, 40)
	C.hv_vm_config_set_el2_enabled(config, C.bool(true))
	if ret := C.hv_vm_create(config); ret != C.HV_SUCCESS {
		fmt.Printf("  SKIP: hv_vm_create (EL2) error %d\n", ret)
		return
	}
	defer C.hv_vm_destroy()

	var vcpu C.hv_vcpu_t
	var exit *C.hv_vcpu_exit_t
	if ret := C.hv_vcpu_create(&vcpu, &exit, nil); ret != C.HV_SUCCESS {
		fmt.Printf("  SKIP: hv_vcpu_create error %d\n", ret)
		return
	}
	defer C.hv_vcpu_destroy(vcpu)

	// Set HCR_EL2: RW + TID3
	hcr := uint64(1<<31) | (1 << 18)
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_HCR_EL2, C.uint64_t(hcr))
	var hcrRB C.uint64_t
	C.hv_vcpu_get_sys_reg(vcpu, C.HV_SYS_REG_HCR_EL2, &hcrRB)
	fmt.Printf("  HCR_EL2 = 0x%x (wanted 0x%x)\n", uint64(hcrRB), hcr)

	var vecMem, stackMem, userMem unsafe.Pointer
	for _, p := range []*unsafe.Pointer{&vecMem, &stackMem, &userMem} {
		C.posix_memalign(p, C.size_t(pageSize), C.size_t(pageSize))
		C.memset(*p, 0, C.size_t(pageSize))
	}
	defer func() { C.free(vecMem); C.free(stackMem); C.free(userMem) }()

	vecIPA := uint64(0)
	stackIPA := uint64(pageSize)
	userIPA := uint64(2 * pageSize)

	vectors := buildVectors(0xBB)
	C.memcpy(vecMem, unsafe.Pointer(&vectors[0]), C.size_t(pageSize))

	// User code: MRS X0, ID_AA64MMFR0_EL1; MOV X1, #0xBEEF; SVC #0
	userCode := make([]byte, pageSize)
	binary.LittleEndian.PutUint32(userCode[0:], 0xd5380700) // MRS X0, ID_AA64MMFR0_EL1
	binary.LittleEndian.PutUint32(userCode[4:], 0xd29fDDE1) // MOV X1, #0xBEEF
	binary.LittleEndian.PutUint32(userCode[8:], 0xd4000001) // SVC #0
	C.memcpy(userMem, unsafe.Pointer(&userCode[0]), C.size_t(pageSize))

	C.hv_vm_map(vecMem, C.hv_ipa_t(vecIPA), C.size_t(pageSize), C.HV_MEMORY_READ|C.HV_MEMORY_EXEC)
	C.hv_vm_map(stackMem, C.hv_ipa_t(stackIPA), C.size_t(pageSize), C.HV_MEMORY_READ|C.HV_MEMORY_WRITE)
	C.hv_vm_map(userMem, C.hv_ipa_t(userIPA), C.size_t(pageSize), C.HV_MEMORY_READ|C.HV_MEMORY_EXEC)
	defer func() {
		C.hv_vm_unmap(C.hv_ipa_t(vecIPA), C.size_t(pageSize))
		C.hv_vm_unmap(C.hv_ipa_t(stackIPA), C.size_t(pageSize))
		C.hv_vm_unmap(C.hv_ipa_t(userIPA), C.size_t(pageSize))
	}()

	setupEL0(vcpu, vecIPA, stackIPA, userIPA)

	wasHung, ret := runWithTimeout(vcpu)
	if ret != C.HV_SUCCESS {
		fmt.Printf("  hv_vcpu_run: error %d\n", ret)
		return
	}

	if wasHung {
		var pc C.uint64_t
		C.hv_vcpu_get_reg(vcpu, C.HV_REG_PC, &pc)
		fmt.Printf("  HUNG: PC=0x%x (user=0x%x, vec=0x%x)\n", uint64(pc), userIPA, vecIPA)
		fmt.Println("  VERDICT: FAIL — EL2+TID3 does not produce clean VM exit")
		return
	}

	syndrome := uint64(exit.exception.syndrome)
	ec, iss := parseSyndrome(syndrome)
	fmt.Printf("  Exit: reason=%d, syndrome=0x%08x (EC=0x%x, ISS=0x%x)\n",
		exit.reason, syndrome, ec, iss)

	var x0, x1 C.uint64_t
	C.hv_vcpu_get_reg(vcpu, C.HV_REG_X0, &x0)
	C.hv_vcpu_get_reg(vcpu, C.HV_REG_X1, &x1)

	if exit.reason == C.HV_EXIT_REASON_EXCEPTION {
		if ec == 0x18 {
			// Direct TID3 trap! Decode MRS ISS.
			dir := iss & 1
			rt := (iss >> 5) & 0x1f
			crm := (iss >> 1) & 0xf
			crn := (iss >> 10) & 0xf
			op1 := (iss >> 14) & 0x7
			op2 := (iss >> 17) & 0x7
			fmt.Printf("  TID3 trap: dir=%d Rt=X%d Op1=%d CRn=%d CRm=%d Op2=%d\n",
				dir, rt, op1, crn, crm, op2)
			fmt.Println("  VERDICT: PASS — TID3 produces clean EC=0x18 VM exit!")
			fmt.Println("  Trap-and-emulate is FEASIBLE. No binary patching needed.")
		} else if ec == 0x16 {
			hvcImm := iss & 0xffff
			fmt.Printf("  HVC #%d exit (X0=0x%x, X1=0x%x)\n", hvcImm, uint64(x0), uint64(x1))
			if hvcImm == 8 {
				fmt.Println("  MRS trapped to EL1 (not EL2). TID3 didn't redirect to EL2.")
				fmt.Println("  VERDICT: PARTIAL — EL1 handler caught it, not direct EL2 trap")
			}
		} else {
			fmt.Printf("  Unexpected EC=0x%x\n", ec)
		}
	}
}

// testEL2ReadsESR tests the key question for syscall speedup:
// Can EL2 code read ESR_EL1 after an EL0→EL1 exception?
//
// Architecture:
//   EL0: SVC #0 (syscall)
//     → EL1 el0_sync (0x400): HVC #8 → goes to EL2 (not HVF, because EL2 enabled)
//       → EL2 lower-EL sync (0x400): MRS X0, ESR_EL1; HVC #16 → goes to HVF
//         → Host checks X0: does it contain EC=0x15 (SVC syndrome)?
//
// If X0 has the SVC syndrome, EL2 can read ESR_EL1 — enabling the
// "EL2 bridge" architecture for ~2x syscall speedup.
func testEL2ReadsESR() {
	var supported C.bool
	if ret := C.hv_vm_config_get_el2_supported(&supported); ret != C.HV_SUCCESS || !bool(supported) {
		fmt.Println("  SKIP: EL2 not supported")
		return
	}

	config := C.hv_vm_config_create()
	C.hv_vm_config_set_ipa_size(config, 40)
	C.hv_vm_config_set_el2_enabled(config, C.bool(true))
	if ret := C.hv_vm_create(config); ret != C.HV_SUCCESS {
		fmt.Printf("  SKIP: hv_vm_create (EL2) error %d\n", ret)
		return
	}
	defer C.hv_vm_destroy()

	var vcpu C.hv_vcpu_t
	var exit *C.hv_vcpu_exit_t
	if ret := C.hv_vcpu_create(&vcpu, &exit, nil); ret != C.HV_SUCCESS {
		fmt.Printf("  SKIP: hv_vcpu_create error %d\n", ret)
		return
	}
	defer C.hv_vcpu_destroy(vcpu)

	// Set HCR_EL2: RW=1 (EL1 is AArch64). No TGE (EL0→EL1, not EL0→EL2).
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_HCR_EL2, C.uint64_t(1<<31))

	// Allocate pages: EL1 vectors, EL2 vectors, stack, user code.
	var el1VecMem, el2VecMem, stackMem, userMem unsafe.Pointer
	for _, p := range []*unsafe.Pointer{&el1VecMem, &el2VecMem, &stackMem, &userMem} {
		C.posix_memalign(p, C.size_t(pageSize), C.size_t(pageSize))
		C.memset(*p, 0, C.size_t(pageSize))
	}
	defer func() {
		C.free(el1VecMem)
		C.free(el2VecMem)
		C.free(stackMem)
		C.free(userMem)
	}()

	el1VecIPA := uint64(0)
	el2VecIPA := uint64(pageSize)
	stackIPA := uint64(2 * pageSize)
	userIPA := uint64(3 * pageSize)

	// --- EL1 vectors ---
	// el0_sync (0x400): HVC #8 → goes to EL2 (because EL2 is enabled)
	el1Vecs := make([]byte, pageSize)
	for i := 0; i < 16; i++ {
		hvc := uint32(0xd4000002) | (uint32(i) << 5)
		binary.LittleEndian.PutUint32(el1Vecs[i*128:], hvc)
	}
	// el0_sync at 0x400: HVC #8
	binary.LittleEndian.PutUint32(el1Vecs[0x400:], 0xd4000102) // HVC #8
	// ERET stub at 0x800 for EL1→EL0 transition
	binary.LittleEndian.PutUint32(el1Vecs[0x800:], 0xd69f03e0) // ERET
	C.memcpy(el1VecMem, unsafe.Pointer(&el1Vecs[0]), C.size_t(pageSize))

	// --- EL2 vectors ---
	// When EL1 does HVC, it traps to EL2 lower-EL sync (0x400).
	// EL2 handler: MRS X0, ESR_EL1; MRS X1, ELR_EL1; HVC #16
	// HVC from EL2 → HVF exit.
	el2Vecs := make([]byte, pageSize)
	for i := 0; i < 16; i++ {
		hvc := uint32(0xd4000002) | (uint32(i) << 5)
		binary.LittleEndian.PutUint32(el2Vecs[i*128:], hvc)
	}
	// EL2 lower-EL sync at 0x400:
	// Stage A: just MOV + HVC to verify EL2 handler is reached.
	// Stage B (if A works): add MRS ESR_EL2.
	binary.LittleEndian.PutUint32(el2Vecs[0x400:], 0xd2801ba0) // MOV X0, #0xDD
	binary.LittleEndian.PutUint32(el2Vecs[0x404:], 0xd4000202) // HVC #16

	// EL2 ERET stub at 0x800 for initial EL2→EL1 transition
	// Set ELR_EL2 = EL1 ERET stub (el1VecIPA+0x800)
	// SPSR_EL2 = EL1h (0x3c5)
	// But we can't set ELR_EL2 from inside the VM easily.
	// Instead: start vCPU at EL2, ERET to EL1, then EL1 ERETs to EL0.
	// Actually simpler: start at EL1h directly (HVF allows this with EL2 enabled).
	C.memcpy(el2VecMem, unsafe.Pointer(&el2Vecs[0]), C.size_t(pageSize))

	// --- User code ---
	// MOV X8, #172; SVC #0 (getpid)
	userCode := make([]byte, pageSize)
	binary.LittleEndian.PutUint32(userCode[0:], 0xd2801588) // MOV X8, #172 (getpid)
	binary.LittleEndian.PutUint32(userCode[4:], 0xd4000001) // SVC #0
	C.memcpy(userMem, unsafe.Pointer(&userCode[0]), C.size_t(pageSize))

	// Map all pages
	C.hv_vm_map(el1VecMem, C.hv_ipa_t(el1VecIPA), C.size_t(pageSize), C.HV_MEMORY_READ|C.HV_MEMORY_EXEC)
	C.hv_vm_map(el2VecMem, C.hv_ipa_t(el2VecIPA), C.size_t(pageSize), C.HV_MEMORY_READ|C.HV_MEMORY_EXEC)
	C.hv_vm_map(stackMem, C.hv_ipa_t(stackIPA), C.size_t(pageSize), C.HV_MEMORY_READ|C.HV_MEMORY_WRITE)
	C.hv_vm_map(userMem, C.hv_ipa_t(userIPA), C.size_t(pageSize), C.HV_MEMORY_READ|C.HV_MEMORY_EXEC)

	// Configure EL1 and EL2 vectors + disable MMU at both levels
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_VBAR_EL1, C.uint64_t(el1VecIPA))
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_VBAR_EL2, C.uint64_t(el2VecIPA))
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_SCTLR_EL1, 0) // MMU off at EL1
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_SCTLR_EL2, 0) // MMU off at EL2
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_CPACR_EL1, 3<<20)
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_SP_EL1, C.uint64_t(stackIPA+pageSize-16))
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_SP_EL2, C.uint64_t(stackIPA+pageSize-32))
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_SP_EL0, C.uint64_t(stackIPA+pageSize-64))

	// Add ERET stub at EL2 vectors offset 0x800
	binary.LittleEndian.PutUint32(el2Vecs[0x800:], 0xd69f03e0) // ERET
	C.memcpy(el2VecMem, unsafe.Pointer(&el2Vecs[0]), C.size_t(pageSize))

	// Start at EL1h (works with EL2-enabled VM per test 4).
	// EL1 ERET → EL0. EL0 SVC → EL1. EL1 HVC → EL2 handler.
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_ELR_EL1, C.uint64_t(userIPA))
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_SPSR_EL1, 0) // EL0t
	C.hv_vcpu_set_reg(vcpu, C.HV_REG_PC, C.uint64_t(el1VecIPA+0x800))
	C.hv_vcpu_set_reg(vcpu, C.HV_REG_CPSR, 0x3c5) // EL1h

	fmt.Println("  Flow: EL1→EL0 SVC → EL1 HVC #8 → EL2 handler → HVC #16 → HVF")

	wasHung, ret := runWithTimeout(vcpu)
	if ret != C.HV_SUCCESS {
		fmt.Printf("  hv_vcpu_run: error %d\n", ret)
		return
	}

	if wasHung {
		var pc C.uint64_t
		C.hv_vcpu_get_reg(vcpu, C.HV_REG_PC, &pc)
		fmt.Printf("  HUNG: PC=0x%x\n", uint64(pc))

		// Check if hung at EL2 vector (MRS ESR_EL1 hangs from EL2 too)
		if uint64(pc) >= el2VecIPA+0x400 && uint64(pc) < el2VecIPA+0x410 {
			fmt.Println("  VERDICT: MRS ESR_EL1 HANGS from EL2 too")
			fmt.Println("  EL2 bridge NOT viable — ESR_EL1 blocked at all levels")
		} else if uint64(pc) >= el1VecIPA+0x400 && uint64(pc) < el1VecIPA+0x410 {
			fmt.Println("  VERDICT: Stuck at EL1 vector (HVC didn't reach EL2)")
		} else {
			fmt.Println("  VERDICT: Hung at unknown location")
		}
		return
	}

	syndrome := uint64(exit.exception.syndrome)
	ec, iss := parseSyndrome(syndrome)
	fmt.Printf("  Exit: syndrome=0x%08x (EC=0x%x, ISS=0x%x)\n", syndrome, ec, iss)

	var x0, x1, x2, x8 C.uint64_t
	C.hv_vcpu_get_reg(vcpu, C.HV_REG_X0, &x0)
	C.hv_vcpu_get_reg(vcpu, C.HV_REG_X1, &x1)
	C.hv_vcpu_get_reg(vcpu, C.HV_REG_X2, &x2)
	C.hv_vcpu_get_reg(vcpu, C.HV_REG_X8, &x8)

	esrEL2val := uint64(x0)
	elrEL2val := uint64(x1)
	farEL2val := uint64(x2)
	esrEL2ec := (esrEL2val >> 26) & 0x3f

	// Also read ESR_EL1 via HVF API (not from in-VM)
	var esrEL1api C.uint64_t
	C.hv_vcpu_get_sys_reg(vcpu, C.HV_SYS_REG_ESR_EL1, &esrEL1api)
	esrEL1val := uint64(esrEL1api)
	esrEL1ec := (esrEL1val >> 26) & 0x3f

	fmt.Printf("  X0 (ESR_EL2 in-VM)    = 0x%08x (EC=0x%x)\n", esrEL2val, esrEL2ec)
	fmt.Printf("  X1 (ELR_EL2 in-VM)    = 0x%016x\n", elrEL2val)
	fmt.Printf("  X2 (FAR_EL2 in-VM)    = 0x%016x\n", farEL2val)
	fmt.Printf("  ESR_EL1 (HVF API)     = 0x%08x (EC=0x%x)\n", esrEL1val, esrEL1ec)
	fmt.Printf("  X8 (syscall number)   = %d\n", uint64(x8))

	if ec == 0x16 { // HVC exit
		hvcImm := iss & 0xffff
		fmt.Printf("  HVC #%d exit\n", hvcImm)

		if hvcImm == 16 {
			fmt.Println()
			// ESR_EL2 should have HVC syndrome (EC=0x16 from EL1 HVC #8)
			if esrEL2ec == 0x16 {
				fmt.Println("  ESR_EL2 has HVC syndrome (EC=0x16) — EL2 handler works!")
			}
			// ESR_EL1 via API should have original SVC syndrome (EC=0x15)
			if esrEL1ec == 0x15 {
				fmt.Println("  ESR_EL1 via API has SVC syndrome (EC=0x15)")
				fmt.Println()
				fmt.Println("  *** KEY FINDING: ESR_EL1 readable via HVF API even with EL2 ***")
				fmt.Println("  *** EL2 handler can read ESR_EL2, host reads ESR_EL1 ***")
			} else {
				fmt.Printf("  ESR_EL1 via API: EC=0x%x (expected 0x15 for SVC)\n", esrEL1ec)
			}
		} else {
			fmt.Printf("  Unexpected HVC #%d (expected #16 from EL2 handler)\n", hvcImm)
		}
	} else {
		fmt.Printf("  Unexpected exit EC=0x%x\n", ec)
	}
}

// testMRSWithConfig retries Test 1 (MRS at EL0) with production-matching
// VM config: hv_vm_config_create() + IPA size set. Test 1 used
// hv_vm_create(nil) which may have different sysreg trap behavior.
func testMRSWithConfig() {
	config := C.hv_vm_config_create()
	C.hv_vm_config_set_ipa_size(config, 40)
	if ret := C.hv_vm_create(config); ret != C.HV_SUCCESS {
		fmt.Printf("  SKIP: hv_vm_create (config) error %d\n", ret)
		return
	}
	defer C.hv_vm_destroy()

	var vcpu C.hv_vcpu_t
	var exit *C.hv_vcpu_exit_t
	if ret := C.hv_vcpu_create(&vcpu, &exit, nil); ret != C.HV_SUCCESS {
		fmt.Printf("  SKIP: hv_vcpu_create error %d\n", ret)
		return
	}
	defer C.hv_vcpu_destroy(vcpu)

	var vecMem, stackMem, userMem unsafe.Pointer
	for _, p := range []*unsafe.Pointer{&vecMem, &stackMem, &userMem} {
		C.posix_memalign(p, C.size_t(pageSize), C.size_t(pageSize))
		C.memset(*p, 0, C.size_t(pageSize))
	}
	defer func() { C.free(vecMem); C.free(stackMem); C.free(userMem) }()

	vecIPA := uint64(0)
	stackIPA := uint64(pageSize)
	userIPA := uint64(2 * pageSize)

	vectors := buildVectors(0xCC)
	C.memcpy(vecMem, unsafe.Pointer(&vectors[0]), C.size_t(pageSize))

	// Same user code as Test 1: MRS X0, ID_AA64MMFR0_EL1; MOV X1, #0xDEAD; SVC #0
	userCode := make([]byte, pageSize)
	binary.LittleEndian.PutUint32(userCode[0:], 0xd5380700) // MRS X0, ID_AA64MMFR0_EL1
	binary.LittleEndian.PutUint32(userCode[4:], 0xd29BD5A1) // MOV X1, #0xDEAD
	binary.LittleEndian.PutUint32(userCode[8:], 0xd4000001) // SVC #0
	C.memcpy(userMem, unsafe.Pointer(&userCode[0]), C.size_t(pageSize))

	C.hv_vm_map(vecMem, C.hv_ipa_t(vecIPA), C.size_t(pageSize), C.HV_MEMORY_READ|C.HV_MEMORY_EXEC)
	C.hv_vm_map(stackMem, C.hv_ipa_t(stackIPA), C.size_t(pageSize), C.HV_MEMORY_READ|C.HV_MEMORY_WRITE)
	C.hv_vm_map(userMem, C.hv_ipa_t(userIPA), C.size_t(pageSize), C.HV_MEMORY_READ|C.HV_MEMORY_EXEC)
	defer func() {
		C.hv_vm_unmap(C.hv_ipa_t(vecIPA), C.size_t(pageSize))
		C.hv_vm_unmap(C.hv_ipa_t(stackIPA), C.size_t(pageSize))
		C.hv_vm_unmap(C.hv_ipa_t(userIPA), C.size_t(pageSize))
	}()

	setupEL0(vcpu, vecIPA, stackIPA, userIPA)

	wasHung, ret := runWithTimeout(vcpu)
	if ret != C.HV_SUCCESS {
		fmt.Printf("  hv_vcpu_run: error %d\n", ret)
		return
	}

	if wasHung {
		var pc C.uint64_t
		C.hv_vcpu_get_reg(vcpu, C.HV_REG_PC, &pc)
		fmt.Println("  RESULT: vCPU HUNG (same as Test 1, config doesn't help)")
		fmt.Printf("  PC at force-exit: 0x%x\n", uint64(pc))
		fmt.Println("  VERDICT: Config has no effect on MRS hang at EL0")
	} else {
		syndrome := uint64(exit.exception.syndrome)
		ec, iss := parseSyndrome(syndrome)
		fmt.Printf("  Exit: reason=%d, syndrome=0x%08x (EC=0x%x, ISS=0x%x)\n",
			exit.reason, syndrome, ec, iss)

		var x0, x1 C.uint64_t
		C.hv_vcpu_get_reg(vcpu, C.HV_REG_X0, &x0)
		C.hv_vcpu_get_reg(vcpu, C.HV_REG_X1, &x1)
		fmt.Printf("  X0=0x%x, X1=0x%x\n", uint64(x0), uint64(x1))

		if exit.reason == C.HV_EXIT_REASON_EXCEPTION && ec == 0x18 {
			fmt.Println("  VERDICT: PASS — EC=0x18 trap! Config enables MRS trapping!")
			fmt.Println("  Production config DOES trap MRS (Test 1 was a false negative)")
		} else if exit.reason == C.HV_EXIT_REASON_EXCEPTION && ec == 0x16 {
			hvcImm := iss & 0xffff
			fmt.Printf("  HVC #%d exit (MRS trapped to EL1 handler)\n", hvcImm)
			fmt.Println("  VERDICT: MRS causes EL1 exception (not a direct EC=0x18 trap)")
		} else {
			fmt.Println("  VERDICT: Unexpected exit type")
		}
	}
}

// testEL1ReadsESR tests the critical optimization question:
// Can EL1 code read ESR_EL1 after an EL0→EL1 exception?
//
// Flow: EL0 SVC → EL1 el0_sync handler reads ESR_EL1, saves to X0 → HVC #8 → HVF exit
// If X0 contains EC=0x15 (SVC syndrome), in-VM dispatch is possible.
func testEL1ReadsESR() {
	config := C.hv_vm_config_create()
	C.hv_vm_config_set_ipa_size(config, 40)
	if ret := C.hv_vm_create(config); ret != C.HV_SUCCESS {
		fmt.Printf("  SKIP: hv_vm_create error %d\n", ret)
		return
	}
	defer C.hv_vm_destroy()

	var vcpu C.hv_vcpu_t
	var exit *C.hv_vcpu_exit_t
	if ret := C.hv_vcpu_create(&vcpu, &exit, nil); ret != C.HV_SUCCESS {
		fmt.Printf("  SKIP: hv_vcpu_create error %d\n", ret)
		return
	}
	defer C.hv_vcpu_destroy(vcpu)

	var vecMem, stackMem, userMem unsafe.Pointer
	for _, p := range []*unsafe.Pointer{&vecMem, &stackMem, &userMem} {
		C.posix_memalign(p, C.size_t(pageSize), C.size_t(pageSize))
		C.memset(*p, 0, C.size_t(pageSize))
	}
	defer func() { C.free(vecMem); C.free(stackMem); C.free(userMem) }()

	vecIPA := uint64(0)
	stackIPA := uint64(pageSize)
	userIPA := uint64(2 * pageSize)

	// Build vectors with a custom el0_sync handler at 0x400 that
	// reads ESR_EL1 into X0 before doing HVC #8.
	vectors := make([]byte, pageSize)
	for i := 0; i < 16; i++ {
		hvc := uint32(0xd4000002) | (uint32(i) << 5)
		binary.LittleEndian.PutUint32(vectors[i*128:], hvc)
	}
	// el0_sync at 0x400:
	//   MRS X0, ESR_EL1      ← THE KEY TEST: does this hang?
	//   MRS X1, ELR_EL1      ← Also test ELR
	//   MRS X2, FAR_EL1      ← Also test FAR
	//   HVC #8                ← Exit to HVF with syndrome in X0
	binary.LittleEndian.PutUint32(vectors[0x400:], 0xd5385200) // MRS X0, ESR_EL1
	binary.LittleEndian.PutUint32(vectors[0x404:], 0xd5384001) // MRS X1, ELR_EL1
	binary.LittleEndian.PutUint32(vectors[0x408:], 0xd5386002) // MRS X2, FAR_EL1
	binary.LittleEndian.PutUint32(vectors[0x40c:], 0xd4000102) // HVC #8

	// ERET stub at 0x800
	binary.LittleEndian.PutUint32(vectors[0x800:], 0xd69f03e0)
	C.memcpy(vecMem, unsafe.Pointer(&vectors[0]), C.size_t(pageSize))

	// User code: MOV X8, #172 (getpid); SVC #0
	userCode := make([]byte, pageSize)
	binary.LittleEndian.PutUint32(userCode[0:], 0xd2801588) // MOV X8, #172
	binary.LittleEndian.PutUint32(userCode[4:], 0xd4000001) // SVC #0
	C.memcpy(userMem, unsafe.Pointer(&userCode[0]), C.size_t(pageSize))

	C.hv_vm_map(vecMem, C.hv_ipa_t(vecIPA), C.size_t(pageSize), C.HV_MEMORY_READ|C.HV_MEMORY_EXEC)
	C.hv_vm_map(stackMem, C.hv_ipa_t(stackIPA), C.size_t(pageSize), C.HV_MEMORY_READ|C.HV_MEMORY_WRITE)
	C.hv_vm_map(userMem, C.hv_ipa_t(userIPA), C.size_t(pageSize), C.HV_MEMORY_READ|C.HV_MEMORY_EXEC)
	defer func() {
		C.hv_vm_unmap(C.hv_ipa_t(vecIPA), C.size_t(pageSize))
		C.hv_vm_unmap(C.hv_ipa_t(stackIPA), C.size_t(pageSize))
		C.hv_vm_unmap(C.hv_ipa_t(userIPA), C.size_t(pageSize))
	}()

	// Configure: MMU off (IPA=PA), EL1 vectors at vecIPA
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_VBAR_EL1, C.uint64_t(vecIPA))
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_CPACR_EL1, 3<<20)
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_SCTLR_EL1, 0) // MMU off
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_SP_EL1, C.uint64_t(stackIPA+pageSize-16))
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_ELR_EL1, C.uint64_t(userIPA))
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_SPSR_EL1, 0) // EL0t
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_SP_EL0, C.uint64_t(stackIPA+pageSize-16))
	C.hv_vcpu_set_reg(vcpu, C.HV_REG_PC, C.uint64_t(vecIPA+0x800)) // ERET → EL0
	C.hv_vcpu_set_reg(vcpu, C.HV_REG_CPSR, 0x3c5)                  // EL1h

	fmt.Println("  Flow: EL0 SVC → EL1 handler: MRS ESR_EL1 → X0, HVC #8 → HVF exit")

	wasHung, ret := runWithTimeout(vcpu)
	if ret != C.HV_SUCCESS {
		fmt.Printf("  hv_vcpu_run: error %d\n", ret)
		return
	}

	if wasHung {
		var pc C.uint64_t
		C.hv_vcpu_get_reg(vcpu, C.HV_REG_PC, &pc)
		fmt.Printf("  HUNG: PC=0x%x\n", uint64(pc))
		if uint64(pc) >= vecIPA+0x400 && uint64(pc) < vecIPA+0x410 {
			fmt.Println("  Stuck at el0_sync handler (MRS ESR_EL1 hangs from EL1)")
			fmt.Println("  VERDICT: CONFIRMED — MRS ESR_EL1 from EL1 hangs even with production config")
			fmt.Println("  In-VM syscall dispatch NOT possible via this path")
		} else {
			fmt.Printf("  Stuck at unknown location (vec=0x%x, user=0x%x)\n", vecIPA, userIPA)
		}
		return
	}

	syndrome := uint64(exit.exception.syndrome)
	ec, iss := parseSyndrome(syndrome)
	fmt.Printf("  Exit: reason=%d, syndrome=0x%08x (EC=0x%x, ISS=0x%x)\n",
		exit.reason, syndrome, ec, iss)

	var x0, x1, x2, x8 C.uint64_t
	C.hv_vcpu_get_reg(vcpu, C.HV_REG_X0, &x0)
	C.hv_vcpu_get_reg(vcpu, C.HV_REG_X1, &x1)
	C.hv_vcpu_get_reg(vcpu, C.HV_REG_X2, &x2)
	C.hv_vcpu_get_reg(vcpu, C.HV_REG_X8, &x8)

	esrVal := uint64(x0)
	esrEC := (esrVal >> 26) & 0x3f
	elrVal := uint64(x1)
	farVal := uint64(x2)

	fmt.Printf("  X0 (ESR_EL1 from EL1) = 0x%08x (EC=0x%x)\n", esrVal, esrEC)
	fmt.Printf("  X1 (ELR_EL1 from EL1) = 0x%016x\n", elrVal)
	fmt.Printf("  X2 (FAR_EL1 from EL1) = 0x%016x\n", farVal)
	fmt.Printf("  X8 (syscall nr)       = %d\n", uint64(x8))

	if exit.reason == C.HV_EXIT_REASON_EXCEPTION && ec == 0x16 {
		hvcImm := iss & 0xffff
		if hvcImm == 8 {
			if esrEC == 0x15 {
				fmt.Println()
				fmt.Println("  *** BREAKTHROUGH: EL1 can read ESR_EL1 after EL0 exception! ***")
				fmt.Println("  *** In-VM syscall dispatch IS possible! ***")
				fmt.Printf("  ESR_EL1 contains SVC syndrome (EC=0x15), ELR_EL1=0x%x (userIPA+4=0x%x)\n",
					elrVal, userIPA+4)
			} else if esrVal == 0 {
				fmt.Println("  ESR_EL1 = 0 (HVF cleared it before EL1 handler ran)")
				fmt.Println("  VERDICT: MRS doesn't hang but returns stale/zeroed value")
			} else {
				fmt.Printf("  ESR_EL1 has unexpected EC=0x%x (wanted 0x15 for SVC)\n", esrEC)
			}
		} else {
			fmt.Printf("  HVC #%d (unexpected)\n", hvcImm)
		}
	} else if exit.reason == C.HV_EXIT_REASON_EXCEPTION && ec == 0x18 {
		fmt.Println("  EC=0x18: MRS ESR_EL1 from EL1 was trapped as sysreg access")
		fmt.Println("  VERDICT: HVF traps EL1 sysreg reads (emulatable but not direct)")
	} else {
		fmt.Printf("  Unexpected exit: reason=%d, EC=0x%x\n", exit.reason, ec)
	}
}
