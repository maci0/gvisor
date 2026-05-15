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

// These functions return the address of assembly fault handlers
// defined in memcpy_arm64.s, memclr_arm64.s, atomic_arm64.s.
// The Mach exception handler uses these addresses to redirect
// execution on EXC_BAD_ACCESS faults.

TEXT ·addrOfHandleMemcpyFault(SB), NOSPLIT, $0-8
	MOVD	$handleMemcpyFault(SB), R0
	MOVD	R0, ret+0(FP)
	RET

TEXT ·addrOfHandleMemclrFault(SB), NOSPLIT, $0-8
	MOVD	$handleMemclrFault(SB), R0
	MOVD	R0, ret+0(FP)
	RET

TEXT ·addrOfHandleSwapUint32Fault(SB), NOSPLIT, $0-8
	MOVD	$handleSwapUint32Fault(SB), R0
	MOVD	R0, ret+0(FP)
	RET

TEXT ·addrOfHandleSwapUint64Fault(SB), NOSPLIT, $0-8
	MOVD	$handleSwapUint64Fault(SB), R0
	MOVD	R0, ret+0(FP)
	RET

TEXT ·addrOfHandleCompareAndSwapUint32Fault(SB), NOSPLIT, $0-8
	MOVD	$handleCompareAndSwapUint32Fault(SB), R0
	MOVD	R0, ret+0(FP)
	RET

TEXT ·addrOfHandleLoadUint32Fault(SB), NOSPLIT, $0-8
	MOVD	$handleLoadUint32Fault(SB), R0
	MOVD	R0, ret+0(FP)
	RET
