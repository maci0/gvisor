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

// ring0State is the per-vCPU shared memory page for the EL1 el0_sync
// handler. The handler saves guest registers here, and the host reads
// them after HVC exit. This avoids per-register HVF API calls.
//
// The address is stored in TPIDR_EL1 before guest entry.
// The handler loads TPIDR_EL1 into X18 (scratch) on exception entry.
//
// Memory layout (offsets in bytes):
//
//	Offset  Size  Field
//	0       248   Guest GPRs (X0-X30, 31*8 bytes)
//	248     8     Guest SP (SP_EL0)
//	256     8     Guest PC (ELR_EL1)
//	264     8     Guest PSTATE (SPSR_EL1)
//	272     8     Guest TLS (TPIDR_EL0)
//	280     8     ESR_EL1 (exception syndrome)
//	288     8     FAR_EL1 (fault address)
//	296     8     vecCode (exception type: 0=unknown, 1=SVC, 2=fault)
//	304     8     saved X18 (handler scratch register)
const (
	r0StateGPR0    = 0   // X0
	r0StateGPR8    = 64  // X8 (syscall number)
	r0StateGPR18   = 144 // X18 (saved before use as scratch)
	r0StateGPR30   = 240 // X30
	r0StateSP      = 248
	r0StatePC      = 256
	r0StatePSTATE  = 264
	r0StateTLS     = 272
	r0StateESR     = 280
	r0StateFAR     = 288
	r0StateVecCode = 296
	r0StateSavedX18 = 304

	// vecCode values
	r0VecUnknown = 0
	r0VecSVC     = 1
	r0VecFault   = 2
)
