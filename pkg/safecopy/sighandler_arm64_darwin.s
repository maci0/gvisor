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

#include "textflag.h"

// The signals handled by sigHandler.
#define SIGBUS 10
#define SIGSEGV 11

// macOS ARM64 ucontext_t layout:
//   ucontext_t.uc_mcontext is a POINTER at offset 0x30 from ucontext_t.
//   The pointer points to struct __darwin_mcontext64:
//     __es (exception state): offset 0x00, size 16
//     __ss (thread state):    offset 0x10
//       __x[0]:  offset 0x10
//       __x[1]:  offset 0x18
//       ...
//       __pc:    offset 0x110
//       __cpsr:  offset 0x118

#define UC_MCONTEXT 0x30

#define MC_REG_R0 0x10
#define MC_REG_R1 0x18
#define MC_REG_PC 0x110

// Offset to the si_code and si_addr fields of siginfo.
#define SI_CODE 0x08
#define SI_ADDR 0x10

// signalHandler is the signal handler for SIGSEGV and SIGBUS signals.
// macOS ARM64 version: must dereference uc_mcontext pointer.
//
// Arguments:
// R0 - The signal number.
// R1 - Pointer to siginfo_t structure.
// R2 - Pointer to ucontext_t structure.
TEXT ·signalHandler(SB),NOSPLIT,$0
	// Check if the signal is from the kernel, si_code > 0 means a kernel signal.
	MOVD SI_CODE(R1), R7
	CMPW $0x0, R7
	BLE original_handler

	// On macOS, uc_mcontext is a POINTER. Dereference it.
	MOVD UC_MCONTEXT(R2), R6  // R6 = mcontext pointer

	// Check if PC is within the area we care about.
	MOVD MC_REG_PC(R6), R7
	MOVD ·memcpyBegin(SB), R8
	CMP R8, R7
	BLO not_memcpy
	MOVD ·memcpyEnd(SB), R8
	CMP R8, R7
	BHS not_memcpy

	// Modify the context such that execution will resume in the fault handler.
	MOVD $handleMemcpyFault(SB), R7
	B handle_fault

not_memcpy:
	MOVD ·memclrBegin(SB), R8
	CMP R8, R7
	BLO not_memclr
	MOVD ·memclrEnd(SB), R8
	CMP R8, R7
	BHS not_memclr

	MOVD $handleMemclrFault(SB), R7
	B handle_fault

not_memclr:
	MOVD ·swapUint32Begin(SB), R8
	CMP R8, R7
	BLO not_swapuint32
	MOVD ·swapUint32End(SB), R8
	CMP R8, R7
	BHS not_swapuint32

	MOVD $handleSwapUint32Fault(SB), R7
	B handle_fault

not_swapuint32:
	MOVD ·swapUint64Begin(SB), R8
	CMP R8, R7
	BLO not_swapuint64
	MOVD ·swapUint64End(SB), R8
	CMP R8, R7
	BHS not_swapuint64

	MOVD $handleSwapUint64Fault(SB), R7
	B handle_fault

not_swapuint64:
	MOVD ·compareAndSwapUint32Begin(SB), R8
	CMP R8, R7
	BLO not_casuint32
	MOVD ·compareAndSwapUint32End(SB), R8
	CMP R8, R7
	BHS not_casuint32

	MOVD $handleCompareAndSwapUint32Fault(SB), R7
	B handle_fault

not_casuint32:
	MOVD ·loadUint32Begin(SB), R8
	CMP R8, R7
	BLO not_loaduint32
	MOVD ·loadUint32End(SB), R8
	CMP R8, R7
	BHS not_loaduint32

	MOVD $handleLoadUint32Fault(SB), R7
	B handle_fault

not_loaduint32:
original_handler:
	// Jump to the previous signal handler, which is likely the golang one.
	MOVD ·savedSigBusHandler(SB), R7
	MOVD ·savedSigSegVHandler(SB), R8
	CMPW $SIGSEGV, R0
	CSEL EQ, R8, R7, R7
	B (R7)

handle_fault:
	// Entered with the address of the fault handler in R7.
	// R6 still holds the mcontext pointer.
	// Store fault handler address in PC.
	MOVD R7, MC_REG_PC(R6)

	// Store the faulting address in R0.
	MOVD SI_ADDR(R1), R7
	MOVD R7, MC_REG_R0(R6)

	// Store the signal number in R1.
	MOVW R0, MC_REG_R1(R6)

	RET

// func addrOfSignalHandler() uintptr
TEXT ·addrOfSignalHandler(SB), $0-8
	MOVD	$·signalHandler(SB), R0
	MOVD	R0, ret+0(FP)
	RET
