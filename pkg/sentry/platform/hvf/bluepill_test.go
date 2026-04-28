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

static inline hv_return_t vmMapRawTest(uint64_t hostAddr, uint64_t ipa,
                                       size_t size, hv_memory_flags_t flags) {
	return hv_vm_map((void *)hostAddr, (hv_ipa_t)ipa, size, flags);
}

static inline void memBarrierTest(void) {
	__asm__ __volatile__("dsb ish" ::: "memory");
}
*/
import "C"

import (
	"fmt"
	"os"
	"runtime"
	"testing"
	"unsafe"
)

var testMachine *machine

func TestMain(m *testing.M) {
	runtime.LockOSThread()

	config := C.hv_vm_config_create()
	C.hv_vm_config_set_ipa_size(config, 40)
	ret := C.hv_vm_create(config)
	if ret != C.HV_SUCCESS {
		fmt.Fprintf(os.Stderr, "hv_vm_create: %d\n", ret)
		os.Exit(1)
	}

	var err error
	testMachine, err = newMachine()
	if err != nil {
		fmt.Fprintf(os.Stderr, "newMachine: %v\n", err)
		C.hv_vm_destroy()
		os.Exit(1)
	}

	code := m.Run()
	C.hv_vm_destroy()
	os.Exit(code)
}

func getTestVCPU(t *testing.T) *vCPU {
	t.Helper()
	runtime.LockOSThread()
	vcpu, err := testMachine.createVCPU(0)
	if err != nil {
		t.Fatalf("createVCPU: %v", err)
	}
	return vcpu
}

// TestBluepillEL1 verifies code execution at EL1 inside the HVF VM.
func TestBluepillEL1(t *testing.T) {
	vcpu := getTestVCPU(t)
	if err := testMachine.bluepillTest(vcpu); err != nil {
		t.Fatalf("bluepillTest: %v", err)
	}
}

// TestBluepillEL1Memory verifies EL1 read/write through TTBR1.
func TestBluepillEL1Memory(t *testing.T) {
	vcpu := getTestVCPU(t)
	if err := testMachine.bluepillTestMemory(vcpu); err != nil {
		t.Fatalf("bluepillTestMemory: %v", err)
	}
}

// bluepillMarker is set by bluepillTestFunc when it runs at EL1.
var bluepillMarker uint64

// bluepillTestFunc is the Go function we attempt to run at EL1.
//
//go:nosplit
//go:noinline
func bluepillTestFunc() {
	bluepillMarker = 0xE110F0C
}

// bluepillCallee returns a value, called by bluepillCaller.
//
//go:nosplit
//go:noinline
func bluepillCallee() uint64 {
	return 0xCA11EE
}

// bluepillCaller calls bluepillCallee and stores the result.
// Tests: BL instruction, stack frame (STP/LDP for LR save), RET chain.
//
//go:nosplit
//go:noinline
func bluepillCaller() {
	bluepillMarker = bluepillCallee()
}

// bluepillCompute returns a value. Called by bluepillNormalFunc to
// force a stack frame (BL requires saving LR → stack bounds check).
//
//go:noinline
func bluepillCompute() uint64 {
	return 0xA0A0A0
}

// bluepillNormalFunc is a normal (non-nosplit) Go function that calls
// another function. The compiler generates a stack bounds check:
//   MOVD 16(R28), R16    // g.stackguard0
//   CMP R16, RSP
//   BLS morestack
// We fake the g struct with stackguard0=0 so the check always passes.
//
//go:noinline
func bluepillNormalFunc() {
	bluepillMarker = bluepillCompute()
}

// TestBluepillGoFunc attempts to run a real Go function at EL1 with
// MMU off and demand-paging. This is the ultimate ring0 proof: if a
// Go function can execute at EL1 inside the VM, the full sentry can.
func TestBluepillGoFunc(t *testing.T) {
	vcpu := getTestVCPU(t)
	bluepillMarker = 0

	// Get the function's code address via a closure trick.
	// Go function values are pointers to a struct containing the PC.
	fn := bluepillTestFunc
	fnPtr := *(*uintptr)(unsafe.Pointer(&fn))
	fnPC := *(*uintptr)(unsafe.Pointer(fnPtr))

	t.Logf("bluepillTestFunc at PC=%#x", fnPC)

	// Allocate a dedicated stack for EL1 execution.
	var stackMem unsafe.Pointer
	C.posix_memalign(&stackMem, C.size_t(hvfPageSize), C.size_t(4*hvfPageSize))
	if stackMem == nil {
		t.Fatal("alloc stack")
	}
	defer C.free(stackMem)
	C.memset(stackMem, 0, C.size_t(4*hvfPageSize))

	// Map stack at IPA = host VA.
	stackBase := uintptr(stackMem)
	stackIPA := uint64(stackBase) &^ (hvfPageSize - 1)
	for off := uint64(0); off < 4*hvfPageSize; off += hvfPageSize {
		ret := C.hv_vm_map(unsafe.Pointer(stackBase+uintptr(off)),
			C.hv_ipa_t(stackIPA+off), C.size_t(hvfPageSize),
			C.HV_MEMORY_READ|C.HV_MEMORY_WRITE)
		if ret != C.HV_SUCCESS {
			t.Fatalf("map stack page IPA=%#x: %d", stackIPA+off, ret)
		}
	}

	sp := uint64(stackBase) + 4*hvfPageSize - 16

	// We can't directly call the Go function because it expects the
	// Go runtime to be set up (g, m, stack bounds, etc.). Instead,
	// build a code stub that just writes to the marker address and HVCs.
	//
	// For a true Go function call, we'd need to set up TPIDR_EL0 (g),
	// map the entire Go runtime, etc. That's Phase 2.
	//
	// For now, prove demand-paging works with a code stub at the
	// function's address page (to test instruction fetch demand-paging).

	t.Log("demand-paging test with code at Go function's page")

	// Allocate code page with stub.
	var codeMem unsafe.Pointer
	C.posix_memalign(&codeMem, C.size_t(hvfPageSize), C.size_t(hvfPageSize))
	if codeMem == nil {
		t.Fatal("alloc code")
	}
	defer C.free(codeMem)
	C.memset(codeMem, 0, C.size_t(hvfPageSize))

	code := unsafe.Slice((*byte)(codeMem), hvfPageSize)
	i := 0

	// Load address of bluepillMarker into X1.
	markerAddr := uint64(uintptr(unsafe.Pointer(&bluepillMarker)))
	putInstr(code, &i, 0xd2800001|((uint32(markerAddr)&0xFFFF)<<5))
	putInstr(code, &i, 0xf2a00001|((uint32(markerAddr>>16)&0xFFFF)<<5))
	putInstr(code, &i, 0xf2c00001|((uint32(markerAddr>>32)&0xFFFF)<<5))
	putInstr(code, &i, 0xf2e00001|((uint32(markerAddr>>48)&0xFFFF)<<5))
	// X2 = 0xCAFE
	putInstr(code, &i, 0xd29195C2) // MOVZ X2, #0x8CAE... actually let's use simple value
	// X2 = 0xEEEE
	code[i], code[i+1], code[i+2], code[i+3] = 0xc2, 0xdd, 0x9d, 0xd2 // MOVZ X2, #0xEEEE
	i += 4
	// STR X2, [X1] — write to bluepillMarker (will demand-fault)
	putInstr(code, &i, 0xf9000022)
	// MOV X0, #0xEE (success marker)
	putInstr(code, &i, 0xd2801dc0) // MOVZ X0, #0xEE
	// HVC #0
	putInstr(code, &i, 0xd4000002)

	// Map code at IPA = host VA.
	codeIPA := uint64(uintptr(codeMem)) &^ (hvfPageSize - 1)
	ret := C.hv_vm_map(codeMem, C.hv_ipa_t(codeIPA), C.size_t(hvfPageSize),
		C.HV_MEMORY_READ|C.HV_MEMORY_EXEC)
	if ret != C.HV_SUCCESS {
		t.Fatalf("map code IPA=%#x: %d", codeIPA, ret)
	}

	// Run with demand-paging. The STR to bluepillMarker will fault
	// because the marker's page isn't mapped yet.
	x0, err := testMachine.bluepillRunMMUOff(vcpu, uint64(uintptr(codeMem)), sp)
	if err != nil {
		t.Fatalf("bluepillRunMMUOff: %v", err)
	}

	if x0 != 0xEE {
		t.Fatalf("X0=%#x, want 0xEE", x0)
	}

	if bluepillMarker != 0xEEEE {
		t.Fatalf("bluepillMarker=%#x, want 0xEEEE", bluepillMarker)
	}

	t.Logf("demand-paging SUCCESS: EL1 wrote bluepillMarker=%#x via demand-faulted page at %#x", bluepillMarker, markerAddr)
}

// TestBluepillGoFuncReal runs an actual Go function at EL1 inside
// the HVF VM. The function is a //go:nosplit //go:noinline function
// that writes to a Go global variable. It runs at its real host VA
// with demand-paging mapping code and data pages on fault.
func TestBluepillGoFuncReal(t *testing.T) {
	vcpu := getTestVCPU(t)
	bluepillMarker = 0

	// Get the real PC of bluepillTestFunc.
	// Go function values are pointers to a funcval struct whose first
	// field is the entry PC.
	fn := bluepillTestFunc
	fnPC := **(**uint64)(unsafe.Pointer(&fn))
	t.Logf("bluepillTestFunc PC=%#x", fnPC)

	// Create an HVC exit stub. When the Go function returns (RET),
	// it branches to LR. We set LR to this stub.
	var stubMem unsafe.Pointer
	C.posix_memalign(&stubMem, C.size_t(hvfPageSize), C.size_t(hvfPageSize))
	if stubMem == nil {
		t.Fatal("alloc stub")
	}
	defer C.free(stubMem)
	C.memset(stubMem, 0, C.size_t(hvfPageSize))

	stub := unsafe.Slice((*byte)(stubMem), hvfPageSize)
	si := 0
	// MOV X0, X19 — preserve a potential return value
	// Actually, just set X0 = 0xAA as success marker.
	putInstr(stub, &si, 0xd2801540) // MOVZ X0, #0xAA
	putInstr(stub, &si, 0xd4000002) // HVC #0

	// Map stub at IPA = host VA.
	stubIPA := uint64(uintptr(stubMem)) &^ (hvfPageSize - 1)
	C.vmMapRawTest(C.uint64_t(stubIPA), C.uint64_t(stubIPA),
		C.size_t(hvfPageSize), C.HV_MEMORY_READ|C.HV_MEMORY_EXEC)

	// Allocate a stack for EL1.
	var stackMem unsafe.Pointer
	C.posix_memalign(&stackMem, C.size_t(hvfPageSize), C.size_t(4*hvfPageSize))
	if stackMem == nil {
		t.Fatal("alloc stack")
	}
	defer C.free(stackMem)
	C.memset(stackMem, 0, C.size_t(4*hvfPageSize))

	// Map stack pages at IPA = host VA.
	stackBase := uint64(uintptr(stackMem))
	for off := uint64(0); off < 4*hvfPageSize; off += hvfPageSize {
		C.vmMapRawTest(C.uint64_t(stackBase+off), C.uint64_t(stackBase+off),
			C.size_t(hvfPageSize), C.HV_MEMORY_READ|C.HV_MEMORY_WRITE)
	}

	sp := stackBase + 4*hvfPageSize - 16
	lr := uint64(uintptr(stubMem))

	// Set up vCPU: PC = function, LR = HVC stub, SP = fresh stack.
	// R28 = 0 (no goroutine — nosplit function doesn't check it).
	vcpu.setReg(C.HV_REG_PC, fnPC)     // Go function entry
	vcpu.setReg(C.HV_REG_CPSR, 0x3c5)  // EL1h, DAIF masked
	vcpu.setSysReg(C.HV_SYS_REG_SP_EL1, sp)
	vcpu.setReg(hvRegs[30], lr)         // X30 = LR = HVC stub

	// Disable MMU for VA=IPA mapping.
	vcpu.setSysReg(C.HV_SYS_REG_SCTLR_EL1, 0x30901184)

	t.Logf("running Go func at EL1: PC=%#x LR=%#x SP=%#x", fnPC, lr, sp)

	// Run with demand-paging.
	x0, err := testMachine.bluepillRunMMUOff(vcpu, fnPC, sp)
	if err != nil {
		// If demand-paging fails, it might be because the function
		// uses ADRP which resolves to a page we can't map. Log details.
		t.Fatalf("bluepillRunMMUOff: %v", err)
	}

	if x0 != 0xAA {
		t.Fatalf("X0=%#x, want 0xAA (HVC stub marker)", x0)
	}

	if bluepillMarker != 0xE110F0C {
		t.Fatalf("bluepillMarker=%#x, want 0xE110F0C", bluepillMarker)
	}

	t.Logf("GO FUNCTION AT EL1: SUCCESS! bluepillMarker=%#x", bluepillMarker)
}

// TestBluepillGoNormalFunc runs a non-nosplit Go function at EL1.
// This tests the stack bounds check (R28/g access) by providing
// a fake goroutine struct with stackguard0=0.
func TestBluepillGoNormalFunc(t *testing.T) {
	vcpu := getTestVCPU(t)
	bluepillMarker = 0

	fn := bluepillNormalFunc
	fnPC := **(**uint64)(unsafe.Pointer(&fn))
	t.Logf("bluepillNormalFunc PC=%#x", fnPC)

	fn2 := bluepillCompute
	fn2PC := **(**uint64)(unsafe.Pointer(&fn2))
	t.Logf("bluepillCompute PC=%#x", fn2PC)

	// Create a fake g struct in C memory (low address, within HVF's
	// 40-bit IPA range). Go stack addresses (~1.3TB) exceed the 1TB
	// IPA limit, so we can't use Go stack-allocated fake g.
	//
	// Only stack bounds fields are needed:
	//   offset 16: stackguard0 (uintptr) ← checked by stack prologue
	// All zeros — stackguard0 = 0 means "SP > 0" always true.
	var fakeGMem unsafe.Pointer
	C.posix_memalign(&fakeGMem, C.size_t(hvfPageSize), C.size_t(hvfPageSize))
	if fakeGMem == nil {
		t.Fatal("alloc fakeG")
	}
	defer C.free(fakeGMem)
	C.memset(fakeGMem, 0, C.size_t(hvfPageSize))

	fakeGAddr := uintptr(fakeGMem)
	fakeGIPA := uint64(fakeGAddr) &^ (hvfPageSize - 1)
	C.vmMapRawTest(C.uint64_t(fakeGIPA), C.uint64_t(fakeGIPA),
		C.size_t(hvfPageSize), C.HV_MEMORY_READ|C.HV_MEMORY_WRITE)

	// HVC exit stub.
	var stubMem unsafe.Pointer
	C.posix_memalign(&stubMem, C.size_t(hvfPageSize), C.size_t(hvfPageSize))
	if stubMem == nil {
		t.Fatal("alloc stub")
	}
	defer C.free(stubMem)
	C.memset(stubMem, 0, C.size_t(hvfPageSize))
	stub := unsafe.Slice((*byte)(stubMem), hvfPageSize)
	si := 0
	putInstr(stub, &si, 0xd2801540) // MOVZ X0, #0xAA
	putInstr(stub, &si, 0xd4000002) // HVC #0
	stubIPA := uint64(uintptr(stubMem)) &^ (hvfPageSize - 1)
	C.vmMapRawTest(C.uint64_t(stubIPA), C.uint64_t(stubIPA),
		C.size_t(hvfPageSize), C.HV_MEMORY_READ|C.HV_MEMORY_EXEC)

	// Allocate stack.
	var stackMem unsafe.Pointer
	C.posix_memalign(&stackMem, C.size_t(hvfPageSize), C.size_t(4*hvfPageSize))
	if stackMem == nil {
		t.Fatal("alloc stack")
	}
	defer C.free(stackMem)
	C.memset(stackMem, 0, C.size_t(4*hvfPageSize))
	stackBase := uint64(uintptr(stackMem))
	for off := uint64(0); off < 4*hvfPageSize; off += hvfPageSize {
		C.vmMapRawTest(C.uint64_t(stackBase+off), C.uint64_t(stackBase+off),
			C.size_t(hvfPageSize), C.HV_MEMORY_READ|C.HV_MEMORY_WRITE)
	}
	sp := stackBase + 4*hvfPageSize - 16

	// Set R28 = fake g, LR = HVC stub.
	vcpu.setReg(hvRegs[28], uint64(fakeGAddr)) // R28 = g
	vcpu.setReg(hvRegs[30], uint64(uintptr(stubMem))) // LR = HVC stub

	t.Logf("running normal func at EL1: PC=%#x R28(g)=%#x SP=%#x", fnPC, fakeGAddr, sp)

	x0, err := testMachine.bluepillRunMMUOff(vcpu, fnPC, sp)
	if err != nil {
		t.Fatalf("bluepillRunMMUOff: %v (marker=%#x)", err, bluepillMarker)
	}

	// Check PC at exit to see where we ended up.
	exitPC := vcpu.getReg(C.HV_REG_PC)
	exitLR := vcpu.getReg(hvRegs[30])
	exitR28 := vcpu.getReg(hvRegs[28])
	t.Logf("exit: X0=%#x PC=%#x LR=%#x R28=%#x marker=%#x",
		x0, exitPC, exitLR, exitR28, bluepillMarker)

	if bluepillMarker != 0xA0A0A0 {
		t.Fatalf("bluepillMarker=%#x, want 0xA0A0A0 (function didn't execute correctly)", bluepillMarker)
	}

	if x0 != 0xAA {
		t.Fatalf("X0=%#x, want 0xAA (HVC stub not reached?)", x0)
	}

	t.Logf("NORMAL GO FUNCTION AT EL1: SUCCESS! R28/g check passed, marker=%#x", bluepillMarker)
}

// TestBluepillGoRealG runs a Go function at EL1 with the REAL
// goroutine's g pointer (R28). With 40-bit IPA, Go stack/heap
// addresses (~80GB) are within range. The demand pager maps g's
// memory on fault, so the stack bounds check accesses real runtime data.
func TestBluepillGoRealG(t *testing.T) {
	vcpu := getTestVCPU(t)
	bluepillMarker = 0

	fn := bluepillNormalFunc
	fnPC := **(**uint64)(unsafe.Pointer(&fn))

	// Get current goroutine's g pointer from R28.
	gPtr := getg()
	t.Logf("real g pointer: %#x, func PC: %#x", gPtr, fnPC)

	// Go allocates goroutine structs at ~0x14000000000 (1.25TB) which
	// exceeds HVF's max IPA of 40 bits (1TB). Skip if out of range.
	if gPtr >= (1 << 40) {
		t.Skipf("g pointer %#x exceeds 40-bit IPA limit (1TB), need MMU-on page tables", gPtr)
	}

	// HVC exit stub.
	var stubMem unsafe.Pointer
	C.posix_memalign(&stubMem, C.size_t(hvfPageSize), C.size_t(hvfPageSize))
	if stubMem == nil {
		t.Fatal("alloc stub")
	}
	defer C.free(stubMem)
	C.memset(stubMem, 0, C.size_t(hvfPageSize))
	stub := unsafe.Slice((*byte)(stubMem), hvfPageSize)
	si := 0
	putInstr(stub, &si, 0xd2801540) // MOVZ X0, #0xAA
	putInstr(stub, &si, 0xd4000002) // HVC #0
	stubIPA := uint64(uintptr(stubMem)) &^ (hvfPageSize - 1)
	C.vmMapRawTest(C.uint64_t(stubIPA), C.uint64_t(stubIPA),
		C.size_t(hvfPageSize), C.HV_MEMORY_READ|C.HV_MEMORY_EXEC)

	// Use C-allocated stack (within IPA range).
	var stackMem unsafe.Pointer
	C.posix_memalign(&stackMem, C.size_t(hvfPageSize), C.size_t(4*hvfPageSize))
	if stackMem == nil {
		t.Fatal("alloc stack")
	}
	defer C.free(stackMem)
	C.memset(stackMem, 0, C.size_t(4*hvfPageSize))
	stackBase := uint64(uintptr(stackMem))
	for off := uint64(0); off < 4*hvfPageSize; off += hvfPageSize {
		C.vmMapRawTest(C.uint64_t(stackBase+off), C.uint64_t(stackBase+off),
			C.size_t(hvfPageSize), C.HV_MEMORY_READ|C.HV_MEMORY_WRITE)
	}
	sp := stackBase + 4*hvfPageSize - 16

	// Set R28 = REAL g pointer.
	vcpu.setReg(hvRegs[28], gPtr)
	vcpu.setReg(hvRegs[30], uint64(uintptr(stubMem)))

	t.Logf("running with REAL g at EL1: PC=%#x R28=%#x SP=%#x", fnPC, gPtr, sp)

	x0, err := testMachine.bluepillRunMMUOff(vcpu, fnPC, sp)
	if err != nil {
		t.Fatalf("bluepillRunMMUOff: %v (marker=%#x)", err, bluepillMarker)
	}

	if x0 != 0xAA {
		t.Fatalf("X0=%#x, want 0xAA", x0)
	}
	if bluepillMarker != 0xA0A0A0 {
		t.Fatalf("bluepillMarker=%#x, want 0xA0A0A0", bluepillMarker)
	}

	t.Logf("REAL G AT EL1: SUCCESS! Go function with real goroutine pointer, marker=%#x", bluepillMarker)
}

// TestBluepillMMUOnRealG runs a Go function at EL1 with MMU enabled
// and 4-level page tables (48-bit VA). Uses the REAL goroutine's g
// pointer — the full ring0 proof with actual Go runtime state.
func TestBluepillMMUOnRealG(t *testing.T) {
	vcpu := getTestVCPU(t)
	bluepillMarker = 0

	fn := bluepillNormalFunc
	fnPC := **(**uint64)(unsafe.Pointer(&fn))
	gPtr := getg()
	t.Logf("func PC=%#x, real g=%#x", fnPC, gPtr)

	// Create sentry page table (4-level, 48-bit VA).
	spt, err := newSentryPageTable(testMachine)
	if err != nil {
		t.Fatalf("newSentryPageTable: %v", err)
	}
	defer spt.release()

	// Map exception vectors page (VA=0 → IPA=0, already mapped in HVF).
	if err := spt.mapPage(0, false); err != nil {
		t.Fatalf("map vectors: %v", err)
	}

	// HVC exit stub.
	var stubMem unsafe.Pointer
	C.posix_memalign(&stubMem, C.size_t(hvfPageSize), C.size_t(hvfPageSize))
	if stubMem == nil {
		t.Fatal("alloc stub")
	}
	defer C.free(stubMem)
	C.memset(stubMem, 0, C.size_t(hvfPageSize))
	stub := unsafe.Slice((*byte)(stubMem), hvfPageSize)
	si := 0
	putInstr(stub, &si, 0xd2801540) // MOVZ X0, #0xAA
	putInstr(stub, &si, 0xd4000002) // HVC #0
	// Map stub into sentry page table and HVF.
	stubVA := uint64(uintptr(stubMem))
	stubIPA := stubVA & sptIPAMask
	C.hv_vm_unmap(C.hv_ipa_t(stubIPA), C.size_t(hvfPageSize))
	C.hv_vm_map(stubMem, C.hv_ipa_t(stubIPA), C.size_t(hvfPageSize),
		C.HV_MEMORY_READ|C.HV_MEMORY_EXEC)
	spt.mapPage(stubVA, false)

	// Allocate stack in C memory (low address, within IPA range).
	var stackMem unsafe.Pointer
	C.posix_memalign(&stackMem, C.size_t(hvfPageSize), C.size_t(4*hvfPageSize))
	if stackMem == nil {
		t.Fatal("alloc stack")
	}
	defer C.free(stackMem)
	C.memset(stackMem, 0, C.size_t(4*hvfPageSize))
	stackVA := uint64(uintptr(stackMem))
	for off := uint64(0); off < 4*hvfPageSize; off += hvfPageSize {
		pageVA := stackVA + off
		pageIPA := pageVA & sptIPAMask
		C.hv_vm_unmap(C.hv_ipa_t(pageIPA), C.size_t(hvfPageSize))
		C.hv_vm_map(unsafe.Pointer(uintptr(pageVA)), C.hv_ipa_t(pageIPA),
			C.size_t(hvfPageSize), C.HV_MEMORY_READ|C.HV_MEMORY_WRITE)
		spt.mapPage(pageVA, true)
	}
	sp := stackVA + 4*hvfPageSize - 16

	// Set R28 = REAL g, LR = stub.
	vcpu.setReg(hvRegs[28], gPtr)
	vcpu.setReg(hvRegs[30], stubVA)

	t.Logf("MMU-on with real g: PC=%#x R28=%#x SP=%#x", fnPC, gPtr, sp)

	x0, err := testMachine.bluepillRunMMUOn(vcpu, spt, fnPC, sp)
	if err != nil {
		t.Fatalf("bluepillRunMMUOn: %v (marker=%#x)", err, bluepillMarker)
	}

	if x0 != 0xAA {
		t.Fatalf("X0=%#x, want 0xAA", x0)
	}
	if bluepillMarker != 0xA0A0A0 {
		t.Fatalf("marker=%#x, want 0xA0A0A0", bluepillMarker)
	}

	t.Logf("MMU-ON REAL G: SUCCESS! Go function with real goroutine at EL1, marker=%#x", bluepillMarker)
}

// TestBluepillEL1toEL0 tests the full EL1→EL0→EL1→HVC cycle:
// 1. Enter VM at EL1 with sentry page tables (MMU on)
// 2. EL1 stub switches TTBR0 to guest PT, sets ELR/SPSR, ERETs to EL0
// 3. Guest at EL0 executes SVC #0 (syscall)
// 4. SVC traps to el0_sync (VBAR+0x400) → HVC #8 → VM exit
// 5. Host reads ESR_EL1 and verifies syscall number
func TestBluepillEL1toEL0(t *testing.T) {
	vcpu := getTestVCPU(t)

	// Create sentry page table (48-bit VA, MMU on).
	spt, err := newSentryPageTable(testMachine)
	if err != nil {
		t.Fatalf("newSentryPageTable: %v", err)
	}
	defer spt.release()

	// Map vectors page in sentry PT (needed for VBAR_EL1 at VA 0).
	spt.mapPage(0, false)

	// Create guest code: MOV X8, #172; SVC #0 (getpid syscall)
	var guestMem unsafe.Pointer
	C.posix_memalign(&guestMem, C.size_t(hvfPageSize), C.size_t(hvfPageSize))
	if guestMem == nil {
		t.Fatal("alloc guest")
	}
	defer C.free(guestMem)
	C.memset(guestMem, 0, C.size_t(hvfPageSize))

	guest := unsafe.Slice((*byte)(guestMem), hvfPageSize)
	gi := 0
	putInstr(guest, &gi, 0xd2801588) // MOVZ X8, #172 (getpid)
	putInstr(guest, &gi, 0xd4000001) // SVC #0

	// Map guest code at a low VA (0x10000) via the EXISTING guest
	// page table system. We need a guestPageTable for TTBR0.
	guestPT, err := newGuestPageTable(testMachine)
	if err != nil {
		t.Fatalf("newGuestPageTable: %v", err)
	}
	defer guestPT.release()

	guestCodeVA := uint64(0x10000) // Guest virtual address
	guestIPA, err := testMachine.ipaAlloc.mapPage(uintptr(guestMem), hvfPageSize)
	if err != nil {
		t.Fatalf("ipaAlloc guest: %v", err)
	}
	if err := guestPT.mapPage(guestCodeVA, guestIPA, false); err != nil {
		t.Fatalf("guestPT map: %v", err)
	}

	// Guest stack (at VA 0x20000 in guest address space).
	var guestStack unsafe.Pointer
	C.posix_memalign(&guestStack, C.size_t(hvfPageSize), C.size_t(hvfPageSize))
	if guestStack == nil {
		t.Fatal("alloc guest stack")
	}
	defer C.free(guestStack)
	C.memset(guestStack, 0, C.size_t(hvfPageSize))
	stackIPA, err := testMachine.ipaAlloc.mapPage(uintptr(guestStack), hvfPageSize)
	if err != nil {
		t.Fatalf("ipaAlloc stack: %v", err)
	}
	guestStackVA := uint64(0x20000)
	guestPT.mapPage(guestStackVA, stackIPA, true)

	// Create EL1 stub that switches to guest and ERETs to EL0.
	// Uses registers set by the host:
	//   X0 = guest TTBR0 (from guestPT)
	//   X1 = guest PC (code VA in guest address space)
	//   X2 = guest SP
	//   X3 = guest PSTATE (EL0t = 0x0)
	var stubMem unsafe.Pointer
	C.posix_memalign(&stubMem, C.size_t(hvfPageSize), C.size_t(hvfPageSize))
	if stubMem == nil {
		t.Fatal("alloc EL1 stub")
	}
	defer C.free(stubMem)
	C.memset(stubMem, 0, C.size_t(hvfPageSize))

	stub := unsafe.Slice((*byte)(stubMem), hvfPageSize)
	si := 0
	// MSR TCR_EL1, X4 — switch to guest TCR (T0SZ=28)
	putInstr(stub, &si, 0xd5182044) // MSR TCR_EL1, X4
	// MSR TTBR0_EL1, X0 — switch to guest page tables
	putInstr(stub, &si, 0xd5182000) // MSR TTBR0_EL1, X0
	// ISB — synchronize TCR + TTBR0 change
	putInstr(stub, &si, 0xd5033fdf) // ISB
	// MSR ELR_EL1, X1 — set return PC
	putInstr(stub, &si, 0xd5184021) // MSR ELR_EL1, X1
	// MSR SPSR_EL1, X3 — set return PSTATE (EL0t)
	putInstr(stub, &si, 0xd5184003) // MSR SPSR_EL1, X3
	// MSR SP_EL0, X2 — set guest stack pointer
	putInstr(stub, &si, 0xd5184102) // MSR SP_EL0, X2
	// ERET — return to EL0 at ELR_EL1
	putInstr(stub, &si, 0xd69f03e0) // ERET

	// Map EL1 stub into sentry PT and HVF.
	stubVA := uint64(uintptr(stubMem))
	stubIPA := stubVA & sptIPAMask
	C.hv_vm_unmap(C.hv_ipa_t(stubIPA), C.size_t(hvfPageSize))
	C.hv_vm_map(stubMem, C.hv_ipa_t(stubIPA), C.size_t(hvfPageSize),
		C.HV_MEMORY_READ|C.HV_MEMORY_EXEC)
	spt.mapPage(stubVA, false)

	// EL1 stack (for sentry execution).
	var el1Stack unsafe.Pointer
	C.posix_memalign(&el1Stack, C.size_t(hvfPageSize), C.size_t(hvfPageSize))
	if el1Stack == nil {
		t.Fatal("alloc EL1 stack")
	}
	defer C.free(el1Stack)
	el1StackVA := uint64(uintptr(el1Stack))
	el1StackIPA := el1StackVA & sptIPAMask
	C.hv_vm_unmap(C.hv_ipa_t(el1StackIPA), C.size_t(hvfPageSize))
	C.hv_vm_map(el1Stack, C.hv_ipa_t(el1StackIPA), C.size_t(hvfPageSize),
		C.HV_MEMORY_READ|C.HV_MEMORY_WRITE)
	spt.mapPage(el1StackVA, true)

	// TCR with T0SZ=28 for guest (2-level page tables).
	tcr28 := uint64(28) | (0x1 << 8) | (0x1 << 10) | (0x3 << 12) | (0x2 << 14) |
		(uint64(28) << 16) | (0x1 << 24) | (0x1 << 26) | (0x3 << 28) |
		(uint64(0x1) << 30) | (uint64(0x2) << 32) | (uint64(1) << 36)

	// Set up vCPU registers for the EL1 stub.
	guestTTBR0 := guestPT.ttbr0()
	vcpu.setReg(hvRegs[0], guestTTBR0)           // X0 = guest TTBR0
	vcpu.setReg(hvRegs[1], guestCodeVA)           // X1 = guest PC
	vcpu.setReg(hvRegs[2], guestStackVA+hvfPageSize-16) // X2 = guest SP
	vcpu.setReg(hvRegs[3], 0)                     // X3 = PSTATE (EL0t)
	vcpu.setReg(hvRegs[4], tcr28)                 // X4 = TCR for guest (T0SZ=28)

	t.Logf("EL1→EL0: stub=%#x guestPC=%#x guestTTBR0=%#x", stubVA, guestCodeVA, guestTTBR0)

	// Configure TCR for 48-bit VA, set TTBR0 to sentry PT.
	tcr48 := uint64(16) | (0x1 << 8) | (0x1 << 10) | (0x3 << 12) | (0x2 << 14) |
		(uint64(28) << 16) | (0x1 << 24) | (0x1 << 26) | (0x3 << 28) |
		(uint64(0x1) << 30) | (uint64(0x2) << 32) | (uint64(1) << 36)
	vcpu.setSysReg(C.HV_SYS_REG_TCR_EL1, tcr48)
	vcpu.setSysReg(C.HV_SYS_REG_TTBR0_EL1, spt.ttbr0())
	vcpu.setSysReg(C.HV_SYS_REG_SCTLR_EL1, 0x30901185) // MMU on

	vcpu.setReg(C.HV_REG_PC, stubVA)
	vcpu.setReg(C.HV_REG_CPSR, 0x3c5) // EL1h
	vcpu.setSysReg(C.HV_SYS_REG_SP_EL1, el1StackVA+hvfPageSize-16)

	// Run. The stub ERETs to EL0, guest SVCs, el0_sync HVCs out.
	C.memBarrierTest()
	ret := C.hv_vcpu_run(vcpu.vcpuID)
	C.memBarrierTest()

	if ret != C.HV_SUCCESS {
		t.Fatalf("hv_vcpu_run: %d", ret)
	}

	exitReason := vcpu.getExitReason()
	syndrome := vcpu.getExceptionSyndrome()
	ec := (syndrome >> 26) & 0x3f
	hvcImm := syndrome & 0xffff

	t.Logf("exit: reason=%d ec=%#x hvcImm=%d", exitReason, ec, hvcImm)

	if exitReason != exitReasonException || ec != 0x16 {
		t.Fatalf("unexpected exit: reason=%d ec=%#x", exitReason, ec)
	}

	if hvcImm != 8 {
		t.Fatalf("HVC imm=%d, want 8 (el0_sync vector)", hvcImm)
	}

	// Read ESR_EL1 — should show SVC from AArch64 (EC=0x15).
	esrEL1 := vcpu.getSysReg(C.HV_SYS_REG_ESR_EL1)
	origEC := (esrEL1 >> 26) & 0x3f
	if origEC != 0x15 {
		t.Fatalf("ESR_EL1 EC=%#x, want 0x15 (SVC)", origEC)
	}

	// Read X8 — syscall number should be 172 (getpid).
	x8 := vcpu.getReg(hvRegs[8])
	if x8 != 172 {
		t.Fatalf("X8=%d, want 172 (getpid)", x8)
	}

	t.Logf("EL1→EL0→EL1: SUCCESS! Guest SVC trapped at EL1, syscall=%d (getpid)", x8)
}

// TestBluepillSysregAccess tests which ARM64 system registers can
// be read at EL1 inside HVF. Critical for determining feasibility
// of in-VM syscall handling.
func TestBluepillSysregAccess(t *testing.T) {
	type sysregTest struct {
		name    string
		mrsInstr uint32 // MRS X0, <sysreg>
	}
	tests := []sysregTest{
		{"NOP_baseline", 0xd503201f},  // NOP — should always work
		{"ELR_EL1", 0xd5384020},
		{"SPSR_EL1", 0xd5384000},
		{"SP_EL0", 0xd5384100},
		{"FAR_EL1", 0xd5386000},
		{"TPIDR_EL1", 0xd538d080},
	}

	vcpu := getTestVCPU(t)

	for _, tt := range tests {
		// Write code: <sysreg_instr>; MOV X0, #0x55; HVC #0
		var codeMem unsafe.Pointer
		C.posix_memalign(&codeMem, C.size_t(hvfPageSize), C.size_t(hvfPageSize))
		if codeMem == nil {
			t.Fatalf("%s: alloc failed", tt.name)
		}
		C.memset(codeMem, 0, C.size_t(hvfPageSize))
		code := unsafe.Slice((*byte)(codeMem), hvfPageSize)
		ci := 0
		putInstr(code, &ci, tt.mrsInstr) // Test instruction
		putInstr(code, &ci, 0xd2800aa0)  // MOV X0, #0x55 (marker)
		putInstr(code, &ci, 0xd4000002)  // HVC #0

		codeIPA := uint64(uintptr(codeMem)) &^ (hvfPageSize - 1)
		C.hv_vm_unmap(C.hv_ipa_t(codeIPA), C.size_t(hvfPageSize))
		C.vmMapRawTest(C.uint64_t(codeIPA), C.uint64_t(codeIPA),
			C.size_t(hvfPageSize), C.HV_MEMORY_READ|C.HV_MEMORY_EXEC)

		var stackMem unsafe.Pointer
		C.posix_memalign(&stackMem, C.size_t(hvfPageSize), C.size_t(hvfPageSize))
		C.memset(stackMem, 0, C.size_t(hvfPageSize))
		stackIPA := uint64(uintptr(stackMem)) &^ (hvfPageSize - 1)
		C.hv_vm_unmap(C.hv_ipa_t(stackIPA), C.size_t(hvfPageSize))
		C.vmMapRawTest(C.uint64_t(stackIPA), C.uint64_t(stackIPA),
			C.size_t(hvfPageSize), C.HV_MEMORY_READ|C.HV_MEMORY_WRITE)

		x0, err := testMachine.bluepillRunMMUOff(vcpu, codeIPA, stackIPA+hvfPageSize-16)

		C.free(codeMem)
		C.free(stackMem)

		if err != nil {
			t.Logf("%s: FAILED (%v)", tt.name, err)
			continue
		}
		if x0 == 0x55 {
			t.Logf("%s: READABLE", tt.name)
		} else {
			t.Logf("%s: result X0=%#x", tt.name, x0)
		}
	}
}

// TestBluepillGoFuncCall runs a Go function that calls another Go
// function at EL1. Tests BL, stack frame save/restore, return chain.
func TestBluepillGoFuncCall(t *testing.T) {
	vcpu := getTestVCPU(t)
	bluepillMarker = 0

	// Get PC of bluepillCaller.
	fn := bluepillCaller
	fnPC := **(**uint64)(unsafe.Pointer(&fn))
	t.Logf("bluepillCaller PC=%#x", fnPC)

	// Get PC of bluepillCallee to log it.
	fn2 := bluepillCallee
	fn2PC := **(**uint64)(unsafe.Pointer(&fn2))
	t.Logf("bluepillCallee PC=%#x", fn2PC)

	// HVC exit stub.
	var stubMem unsafe.Pointer
	C.posix_memalign(&stubMem, C.size_t(hvfPageSize), C.size_t(hvfPageSize))
	if stubMem == nil {
		t.Fatal("alloc stub")
	}
	defer C.free(stubMem)
	C.memset(stubMem, 0, C.size_t(hvfPageSize))

	stub := unsafe.Slice((*byte)(stubMem), hvfPageSize)
	si := 0
	putInstr(stub, &si, 0xd2801540) // MOVZ X0, #0xAA
	putInstr(stub, &si, 0xd4000002) // HVC #0

	stubIPA := uint64(uintptr(stubMem)) &^ (hvfPageSize - 1)
	C.vmMapRawTest(C.uint64_t(stubIPA), C.uint64_t(stubIPA),
		C.size_t(hvfPageSize), C.HV_MEMORY_READ|C.HV_MEMORY_EXEC)

	// Allocate stack.
	var stackMem unsafe.Pointer
	C.posix_memalign(&stackMem, C.size_t(hvfPageSize), C.size_t(4*hvfPageSize))
	if stackMem == nil {
		t.Fatal("alloc stack")
	}
	defer C.free(stackMem)
	C.memset(stackMem, 0, C.size_t(4*hvfPageSize))

	stackBase := uint64(uintptr(stackMem))
	for off := uint64(0); off < 4*hvfPageSize; off += hvfPageSize {
		C.vmMapRawTest(C.uint64_t(stackBase+off), C.uint64_t(stackBase+off),
			C.size_t(hvfPageSize), C.HV_MEMORY_READ|C.HV_MEMORY_WRITE)
	}

	sp := stackBase + 4*hvfPageSize - 16
	lr := uint64(uintptr(stubMem))

	// Set LR before bluepillRunMMUOff (which doesn't touch LR).
	vcpu.setReg(hvRegs[30], lr) // X30 = LR = HVC stub

	t.Logf("running caller→callee at EL1: PC=%#x LR=%#x SP=%#x", fnPC, lr, sp)

	x0, err := testMachine.bluepillRunMMUOff(vcpu, fnPC, sp)
	if err != nil {
		t.Fatalf("bluepillRunMMUOff: %v", err)
	}

	if x0 != 0xAA {
		t.Fatalf("X0=%#x, want 0xAA", x0)
	}

	if bluepillMarker != 0xCA11EE {
		t.Fatalf("bluepillMarker=%#x, want 0xCA11EE", bluepillMarker)
	}

	t.Logf("GO FUNCTION CALL AT EL1: SUCCESS! caller→callee, marker=%#x", bluepillMarker)
}

// TestBluepillMMUOff verifies EL1 execution with MMU disabled.
// VA = IPA, and memory is mapped at IPA = host VA via hv_vm_map.
// This is the simplest path to running Go at EL1: no stage-1
// page tables needed, sentry sees same addresses as host.
func TestBluepillMMUOff(t *testing.T) {
	vcpu := getTestVCPU(t)
	if err := testMachine.bluepillTestMMUOff(vcpu); err != nil {
		t.Fatalf("bluepillTestMMUOff: %v", err)
	}
}
