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
// +build darwin,arm64

// Command hvftest tests the Hypervisor.framework platform by running ARM64
// guest code that makes Linux syscalls, intercepting them, and emulating them
// on the macOS host.
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
	"unsafe"
)

const (
	pageSize = 16384 // ARM64 macOS uses 16K pages

	// Guest memory layout (4 pages):
	vectorsAddr  = 1 * pageSize // Exception vectors + ERET stub
	userCodeAddr = 2 * pageSize // Guest user code + data
	stackAddr    = 3 * pageSize // Stack (grows down)
	stackTop     = 4*pageSize - 16

	// Linux ARM64 syscall numbers
	sysWrite     = 64
	sysExit      = 93
	sysExitGroup = 94
	sysBrk       = 214
)

func main() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	fmt.Println("=== gVisor HVF Syscall Emulation Demo ===")
	fmt.Println()

	// Test 1: Basic HVC from EL1 (sanity check)
	fmt.Println("--- Test 1: HVC from EL1 (sanity) ---")
	testHVC()

	fmt.Println()

	// Test 2: Full syscall emulation - guest writes "Hello" and exits
	fmt.Println("--- Test 2: Full syscall emulation ---")
	testSyscallEmulation()
}

func check(ret C.hv_return_t, msg string) {
	if ret != C.HV_SUCCESS {
		fmt.Fprintf(os.Stderr, "%s: error %d\n", msg, ret)
		os.Exit(1)
	}
}

func testHVC() {
	check(C.hv_vm_create(nil), "hv_vm_create")
	defer C.hv_vm_destroy()

	var mem unsafe.Pointer
	C.posix_memalign(&mem, C.size_t(pageSize), C.size_t(pageSize))
	C.memset(mem, 0, C.size_t(pageSize))
	defer C.free(mem)

	code := [2]uint32{0xd2800540, 0xd4000002} // MOV X0, #42; HVC #0
	C.memcpy(mem, unsafe.Pointer(&code[0]), 8)
	check(C.hv_vm_map(mem, C.hv_ipa_t(pageSize), C.size_t(pageSize),
		C.HV_MEMORY_READ|C.HV_MEMORY_EXEC), "hv_vm_map")

	var vcpu C.hv_vcpu_t
	var exit *C.hv_vcpu_exit_t
	check(C.hv_vcpu_create(&vcpu, &exit, nil), "hv_vcpu_create")
	defer C.hv_vcpu_destroy(vcpu)

	C.hv_vcpu_set_reg(vcpu, C.HV_REG_PC, C.uint64_t(pageSize))
	C.hv_vcpu_set_reg(vcpu, C.HV_REG_CPSR, 0x3c5)
	check(C.hv_vcpu_run(vcpu), "hv_vcpu_run")

	var x0 C.uint64_t
	C.hv_vcpu_get_reg(vcpu, C.HV_REG_X0, &x0)
	if x0 == 42 {
		fmt.Println("  PASSED (X0=42)")
	} else {
		fmt.Printf("  FAILED (X0=%d)\n", x0)
	}
}

func testSyscallEmulation() {
	check(C.hv_vm_create(nil), "hv_vm_create")
	defer C.hv_vm_destroy()

	// --- Allocate pages ---
	pages := make([]unsafe.Pointer, 3)
	for i := range pages {
		C.posix_memalign(&pages[i], C.size_t(pageSize), C.size_t(pageSize))
		C.memset(pages[i], 0, C.size_t(pageSize))
	}
	defer func() {
		for _, p := range pages {
			C.free(p)
		}
	}()
	vectorsMem, userMem, stackMem := pages[0], pages[1], pages[2]

	// --- Build exception vector table ---
	vectors := make([]byte, pageSize)
	for i := 0; i < 16; i++ {
		hvcInstr := uint32(0xd4000002) | (uint32(i) << 5)
		binary.LittleEndian.PutUint32(vectors[i*128:], hvcInstr)
	}
	// ERET stub at offset 0x800
	binary.LittleEndian.PutUint32(vectors[0x800:], 0xd69f03e0) // ERET
	C.memcpy(vectorsMem, unsafe.Pointer(&vectors[0]), C.size_t(pageSize))

	// --- Build guest program ---
	// This guest program:
	//   1. write(1, msg, 14)   -- prints "Hello, gVisor!"
	//   2. write(1, msg2, 31)  -- prints a status message
	//   3. exit_group(0)       -- exits cleanly
	//
	// The message data is placed after the code.

	msg := "Hello, gVisor!\n"
	msg2 := "Syscall emulation works! :)\n"
	// Place data well after the code (max ~15 instructions = 60 bytes).
	msgOffset := 256
	msg2Offset := msgOffset + len(msg)

	userPage := make([]byte, pageSize)

	// Instruction encoding helpers
	put := func(off int, instr uint32) {
		binary.LittleEndian.PutUint32(userPage[off:], instr)
	}

	i := 0
	// --- syscall 1: write(1, msg, len(msg)) ---
	put(i*4, 0xd2800808) // MOV X8, #64  (__NR_write)
	i++
	put(i*4, 0xd2800020) // MOV X0, #1   (fd=stdout)
	i++
	// ADR X1, msg: PC-relative address to message data
	// ADR encoding: imm = msgOffset - (i*4), but we use the absolute address instead
	// Simpler: load the absolute address with MOVZ/MOVK
	msgAddr := uint64(userCodeAddr + msgOffset)
	put(i*4, uint32(0xd2800001)|uint32((msgAddr&0xFFFF)<<5)) // MOVZ X1, #low16
	i++
	if msgAddr > 0xFFFF {
		put(i*4, uint32(0xf2a00001)|uint32(((msgAddr>>16)&0xFFFF)<<5)) // MOVK X1, #high16, LSL #16
		i++
	}
	put(i*4, uint32(0xd2800002)|uint32((uint64(len(msg))&0xFFFF)<<5)) // MOVZ X2, #len
	i++
	put(i*4, 0xd4000001) // SVC #0
	i++

	// --- syscall 2: write(1, msg2, len(msg2)) ---
	msg2Addr := uint64(userCodeAddr + msg2Offset)
	put(i*4, 0xd2800020) // MOV X0, #1   (fd=stdout)
	i++
	put(i*4, uint32(0xd2800001)|uint32((msg2Addr&0xFFFF)<<5)) // MOVZ X1, #low16
	i++
	put(i*4, uint32(0xd2800002)|uint32((uint64(len(msg2))&0xFFFF)<<5)) // MOVZ X2, #len
	i++

	// X8 is still 64 from above
	put(i*4, 0xd4000001) // SVC #0
	i++

	// --- syscall 3: exit_group(0) ---
	put(i*4, uint32(0xd2800008)|uint32((sysExitGroup&0xFFFF)<<5)) // MOVZ X8, #94
	i++
	put(i*4, 0xd2800000) // MOVZ X0, #0 (status)
	i++
	put(i*4, 0xd4000001) // SVC #0
	i++

	// Place message data after code
	copy(userPage[msgOffset:], msg)
	copy(userPage[msg2Offset:], msg2)

	C.memcpy(userMem, unsafe.Pointer(&userPage[0]), C.size_t(pageSize))

	// --- Map pages ---
	check(C.hv_vm_map(vectorsMem, C.hv_ipa_t(vectorsAddr), C.size_t(pageSize),
		C.HV_MEMORY_READ|C.HV_MEMORY_EXEC), "map vectors")
	check(C.hv_vm_map(userMem, C.hv_ipa_t(userCodeAddr), C.size_t(pageSize),
		C.HV_MEMORY_READ|C.HV_MEMORY_EXEC), "map user")
	check(C.hv_vm_map(stackMem, C.hv_ipa_t(stackAddr), C.size_t(pageSize),
		C.HV_MEMORY_READ|C.HV_MEMORY_WRITE), "map stack")

	// --- Create and configure vCPU ---
	var vcpu C.hv_vcpu_t
	var exit *C.hv_vcpu_exit_t
	check(C.hv_vcpu_create(&vcpu, &exit, nil), "hv_vcpu_create")
	defer C.hv_vcpu_destroy(vcpu)

	check(C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_VBAR_EL1,
		C.uint64_t(vectorsAddr)), "VBAR_EL1")
	check(C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_CPACR_EL1,
		3<<20), "CPACR_EL1")
	check(C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_SP_EL1,
		C.uint64_t(stackTop)), "SP_EL1")

	// Set up EL1-to-EL0 transition via ERET stub
	check(C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_ELR_EL1,
		C.uint64_t(userCodeAddr)), "ELR_EL1")
	check(C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_SPSR_EL1, 0), "SPSR_EL1") // EL0t
	check(C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_SP_EL0,
		C.uint64_t(stackTop)), "SP_EL0")

	C.hv_vcpu_set_reg(vcpu, C.HV_REG_PC, C.uint64_t(vectorsAddr+0x800)) // ERET stub
	C.hv_vcpu_set_reg(vcpu, C.HV_REG_CPSR, 0x3c5)                       // EL1h

	// --- Syscall emulation loop ---
	fmt.Println("  Running guest code with syscall emulation...")
	fmt.Println()

	syscallCount := 0
	for {
		check(C.hv_vcpu_run(vcpu), "hv_vcpu_run")

		if exit.reason != C.HV_EXIT_REASON_EXCEPTION {
			fmt.Printf("  Unexpected exit: reason=%d\n", exit.reason)
			break
		}

		syndrome := exit.exception.syndrome
		ec := (syndrome >> 26) & 0x3f
		if ec != 0x16 { // Not HVC
			fmt.Printf("  Unexpected exception: EC=0x%x\n", ec)
			break
		}

		// Read original exception from ESR_EL1
		var esrEL1 C.uint64_t
		C.hv_vcpu_get_sys_reg(vcpu, C.HV_SYS_REG_ESR_EL1, &esrEL1)
		origEC := (uint64(esrEL1) >> 26) & 0x3f
		if origEC != 0x15 { // Not SVC
			fmt.Printf("  Non-SVC exception forwarded: EC=0x%x\n", origEC)
			break
		}

		// Read syscall arguments
		var x0, x1, x2, x8 C.uint64_t
		C.hv_vcpu_get_reg(vcpu, C.HV_REG_X0, &x0)
		C.hv_vcpu_get_reg(vcpu, C.HV_REG_X1, &x1)
		C.hv_vcpu_get_reg(vcpu, C.HV_REG_X2, &x2)
		C.hv_vcpu_get_reg(vcpu, C.HV_REG_X8, &x8)

		syscallCount++
		sysno := uint64(x8)

		switch sysno {
		case sysWrite:
			fd := int(x0)
			bufAddr := uint64(x1)
			count := int(x2)

			// Read the buffer from guest memory.
			// bufAddr is in the user code page.
			offset := int(bufAddr) - userCodeAddr
			if offset < 0 || offset+count > pageSize {
				fmt.Printf("  write: buffer out of range (addr=0x%x)\n", bufAddr)
				C.hv_vcpu_set_reg(vcpu, C.HV_REG_X0, C.uint64_t(0xfffffffffffffffe)) // -EFAULT
			} else {
				buf := make([]byte, count)
				src := (*[pageSize]byte)(userMem)
				copy(buf, src[offset:offset+count])

				// Actually write to the host fd!
				var n int
				var err error
				switch fd {
				case 1:
					n, err = os.Stdout.Write(buf)
				case 2:
					n, err = os.Stderr.Write(buf)
				default:
					n, err = 0, fmt.Errorf("unsupported fd %d", fd)
				}
				if err != nil {
					fmt.Printf("  write(%d) error: %v\n", fd, err)
					C.hv_vcpu_set_reg(vcpu, C.HV_REG_X0, C.uint64_t(0xfffffffffffffff2)) // -EBADF
				} else {
					C.hv_vcpu_set_reg(vcpu, C.HV_REG_X0, C.uint64_t(n))
				}
			}

		case sysExit, sysExitGroup:
			exitCode := int(x0)
			fmt.Printf("\n  Guest called exit_group(%d) after %d syscalls\n", exitCode, syscallCount)
			fmt.Println()
			fmt.Println("=== DEMO COMPLETE: gVisor HVF syscall emulation works! ===")
			return

		case sysBrk:
			// Stub: return 0 (success)
			C.hv_vcpu_set_reg(vcpu, C.HV_REG_X0, 0)

		default:
			fmt.Printf("  Unhandled syscall %d (X0=%d, X1=0x%x, X2=%d)\n",
				sysno, x0, x1, x2)
			C.hv_vcpu_set_reg(vcpu, C.HV_REG_X0, C.uint64_t(0xffffffffffffffda)) // -ENOSYS
		}

		// Resume guest: set up ERET back to EL0.
		// ELR_EL1 already points to the instruction after SVC (ARM auto-sets it).
		C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_SPSR_EL1, 0) // EL0t
		C.hv_vcpu_set_reg(vcpu, C.HV_REG_PC, C.uint64_t(vectorsAddr+0x800))
		C.hv_vcpu_set_reg(vcpu, C.HV_REG_CPSR, 0x3c5) // EL1h
	}
}
