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
// tlbiVMALLE1ISC executes TLBI VMALLE1IS + DSB ISH + ISB.
static inline void tlbiVMALLE1ISC(void) {
	__asm__ __volatile__(
		"tlbi vmalle1is\n\t"
		"dsb ish\n\t"
		"isb"
		::: "memory"
	);
}
*/
import "C"

// Direct TLBI execution for sentry-as-ring0 (EL1) architecture.
//
// When the sentry runs at EL1 inside the VM, it can execute TLB
// invalidation instructions directly. This is fundamentally simpler
// and more correct than the quarantine/kick/zero-page approach used
// when the sentry runs at EL0 (where TLBI traps to EL1 vectors).
//
// The key insight: at EL1, a single TLBI VMALLE1IS + DSB ISH + ISB
// sequence provides a full, architecturally-guaranteed TLB flush
// across all cores in the inner-shareable domain. No need for:
//   - RCU-style quarantine with generation counters
//   - Kicking vCPUs via pthread_kill/SIGUSR1
//   - Zero-page remapping as a TLB-safe intermediate state
//   - ASID rotation to force stale entry eviction
//
// When the sentry-as-ring0 architecture is fully active, the following
// TLB mitigation code becomes unnecessary and can be removed:
//
// From ipa_allocator.go:
//   - quarantinedIPA type and quarantine slice
//   - drainQuarantine() method
//   - generation counter
//   - zeroPage allocation and remapping
//
// From machine.go:
//   - kickAllVCPUs() and pthread_kill infrastructure
//   - minVCPUGen() method
//   - epoch/lastGen fields on vCPU
//   - SIGUSR1 handler
//
// From context.go:
//   - Quarantine drain on periodic vCPU exit
//   - Epoch/generation tracking before hv_vcpu_run
//
// From address_space.go:
//   - Two-phase unmap (remap to zero page + clear PTE)
//   - Can simplify to: clear PTE + tlbiVMALLE1IS()

// tlbiVMALLE1IS invalidates all EL1 TLB entries in the inner-shareable
// domain. This can only be executed when the sentry runs at EL1 inside
// the VM (ring0 architecture). At EL0 or outside the VM, this will
// trap/fault.
//
// When the ring0 architecture is active, this replaces the entire
// quarantine/kick/zero-page/epoch TLB mitigation infrastructure with
// a single instruction sequence.
//
func tlbiVMALLE1IS() {
	C.tlbiVMALLE1ISC()
}

// flushTLBDirect executes TLBI VMALLE1IS directly from EL1.
// Only usable when the sentry runs at EL1 inside the VM.
// Returns true if the flush was executed, false if not available
// (e.g., running outside the VM or before EL1 initialization).
func flushTLBDirect() bool {
	if vmmSharedPage == nil {
		// Not running at EL1 in VM — direct TLBI not available.
		return false
	}
	tlbiVMALLE1IS()
	return true
}
