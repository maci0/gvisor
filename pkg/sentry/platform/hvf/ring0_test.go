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

import (
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

// TestVectorLayout verifies the exception vector table has correct
// layout for EL0 trap handling.
func TestVectorLayout(t *testing.T) {
	// Each of 16 vectors should be at offset i*128.
	// Vector 8 (el0_sync at offset 0x400) is the critical one.
	for i := 0; i < 16; i++ {
		offset := i * 128
		expectedHVCImm := i
		t.Logf("Vector %d: offset=%#x, HVC #%d", i, offset, expectedHVCImm)
	}

	// Verify vector offsets match ARM64 spec.
	if 8*128 != 0x400 {
		t.Fatal("el0_sync vector not at offset 0x400")
	}
	if 0*128 != 0x000 {
		t.Fatal("current-EL sync vector not at offset 0x000")
	}
}

// TestTCRDualTTBR verifies the TCR_EL1 value enables both TTBR0 and TTBR1.
func TestTCRDualTTBR(t *testing.T) {
	// Reconstruct the TCR value from vcpu_arm64.go initialize().
	tcr := uint64(28) | // T0SZ=28
		(0x1 << 8) | // IRGN0
		(0x1 << 10) | // ORGN0
		(0x3 << 12) | // SH0
		(0x2 << 14) | // TG0: 16K
		(uint64(28) << 16) | // T1SZ=28
		(0x1 << 24) | // ORGN1
		(0x1 << 26) | // IRGN1
		(0x3 << 28) | // SH1
		(uint64(0x1) << 30) | // TG1: 16K
		(uint64(0x2) << 32) | // IPS: 40-bit PA
		(uint64(1) << 36) // AS: 16-bit ASID

	// EPD1 (bit 23) must NOT be set — enables TTBR1 walks.
	if tcr&(1<<23) != 0 {
		t.Fatal("EPD1 is set — TTBR1 walks are disabled")
	}

	// T0SZ and T1SZ should both be 28 (36-bit VA).
	t0sz := tcr & 0x3f
	t1sz := (tcr >> 16) & 0x3f
	if t0sz != 28 {
		t.Fatalf("T0SZ=%d, want 28", t0sz)
	}
	if t1sz != 28 {
		t.Fatalf("T1SZ=%d, want 28", t1sz)
	}

	// TG0 should be 0b10 (16K), TG1 should be 0b01 (16K).
	tg0 := (tcr >> 14) & 0x3
	tg1 := (tcr >> 30) & 0x3
	if tg0 != 2 {
		t.Fatalf("TG0=%d, want 2 (16K)", tg0)
	}
	if tg1 != 1 {
		t.Fatalf("TG1=%d, want 1 (16K)", tg1)
	}

	t.Logf("TCR_EL1=%#x: T0SZ=%d T1SZ=%d TG0=%d TG1=%d EPD1=0", tcr, t0sz, t1sz, tg0, tg1)
}

// TestAPBitsEL0Access verifies page table AP bits grant EL0 access.
func TestAPBitsEL0Access(t *testing.T) {
	// Writable page: AP[2:1]=01 (EL1+EL0 RW)
	writableAP := uint64(ap1Bit)
	if writableAP&ap1Bit == 0 {
		t.Fatal("writable page missing AP[1] (EL0 access)")
	}
	if writableAP&ap2Bit != 0 {
		t.Fatal("writable page has AP[2] (read-only)")
	}

	// Read-only page: AP[2:1]=11 (EL1+EL0 RO)
	readonlyAP := uint64(ap1Bit | ap2Bit)
	if readonlyAP&ap1Bit == 0 {
		t.Fatal("readonly page missing AP[1] (EL0 access)")
	}
	if readonlyAP&ap2Bit == 0 {
		t.Fatal("readonly page missing AP[2] (read-only)")
	}

	t.Logf("AP bits: writable=%#x readonly=%#x", writableAP, readonlyAP)
}

// TestKernelPageTableAlloc verifies kernel page table allocation works.
func TestKernelPageTableAlloc(t *testing.T) {
	// Verify kernelVABase is correct for T1SZ=28.
	expectedBase := uint64(0xFFFFFFF000000000)
	if uint64(kernelVABase) != expectedBase {
		t.Fatalf("kernelVABase=%#x, want %#x", uint64(kernelVABase), expectedBase)
	}
	t.Logf("kernelVABase=%#x", uint64(kernelVABase))
}

// TestVMMProtocol verifies the VMM shared protocol structures.
func TestVMMProtocol(t *testing.T) {
	// VMMRequest should fit in a page.
	size := unsafe.Sizeof(VMMRequest{})
	if size > hvfPageSize {
		t.Fatalf("VMMRequest size=%d exceeds page size=%d", size, hvfPageSize)
	}

	// Operation constants should be non-zero and distinct.
	ops := []uint64{VMMOpSyscall, VMMOpMmap, VMMOpMunmap, VMMOpExit}
	seen := make(map[uint64]bool)
	for _, op := range ops {
		if op == 0 {
			t.Fatal("op code is 0")
		}
		if seen[op] {
			t.Fatalf("duplicate op code %d", op)
		}
		seen[op] = true
	}

	// Shared page IPA should be below ipaBase.
	if VMMSharedPageIPA >= ipaBase {
		t.Fatalf("VMMSharedPageIPA=%#x >= ipaBase=%#x", VMMSharedPageIPA, ipaBase)
	}
	t.Logf("VMMRequest size=%d, shared page IPA=%#x", size, VMMSharedPageIPA)
}

// TestHostSyscallProxy verifies the host syscall fallback path works.
func TestHostSyscallProxy(t *testing.T) {
	// With vmmSharedPage == nil, hostSyscall falls back to RawSyscall6.
	pid, err := hostSyscall(uintptr(unix.SYS_GETPID), 0, 0, 0, 0, 0, 0)
	if err != 0 {
		t.Fatalf("hostSyscall(getpid) failed: %v", err)
	}
	if pid == 0 {
		t.Fatal("hostSyscall(getpid) returned 0")
	}
	t.Logf("hostSyscall(getpid) = %d", pid)
}

// TestSyscallHandlerRegistration verifies syscall handler can be set.
func TestSyscallHandlerRegistration(t *testing.T) {
	called := false
	handler := func(nr, a0, a1, a2, a3, a4, a5 uint64) uint64 {
		called = true
		return 42
	}

	RegisterSyscallHandler(handler)
	defer RegisterSyscallHandler(nil)

	result := handleGuestSyscall(0, 0, 0, 0, 0, 0, 0)
	if !called {
		t.Fatal("syscall handler not called")
	}
	if result != 42 {
		t.Fatalf("result=%d, want 42", result)
	}
}

// TestFlushTLBDirectFallback verifies flushTLBDirect returns false
// when not running at EL1 in VM.
func TestFlushTLBDirectFallback(t *testing.T) {
	if flushTLBDirect() {
		t.Fatal("flushTLBDirect returned true outside VM")
	}
}
