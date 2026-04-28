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

// Dual-TTBR Memory Model for Sentry-as-Ring0
//
// ARM64 uses two translation table base registers to split the virtual
// address space into two halves:
//
// TTBR0 (lower-half, per-process):
//   Guest application memory (code, data, stack, heap, mmap regions).
//   Managed by MapFile/Unmap in address_space.go.
//   Each process has its own page table, switched via TTBR0_EL1.
//   Entries have AP[1]=1 (EL0 access) and nG=1 (ASID-tagged).
//
// TTBR1 (upper-half, shared):
//   Sentry kernel memory (Go binary, heap, goroutine stacks).
//   Shared across all vCPUs via a single kernel page table.
//   Mapped once during sentry boot, updated as heap grows.
//   Entries have AP[1]=0 (EL1-only) and nG=0 (global).
//
// VA layout with T0SZ=28, T1SZ=28 (36-bit VA each):
//
//   0x0000_0000_0000_0000 - 0x0000_000F_FFFF_FFFF  TTBR0 (guest, 64GB)
//   0xFFFF_FFF0_0000_0000 - 0xFFFF_FFFF_FFFF_FFFF  TTBR1 (kernel, 64GB)
//
// With direct TLBI at EL1, MapFile/Unmap can be simplified:
//
//   func (as *addressSpace) unmapSimplified(addr hostarch.Addr, length uint64) {
//       for off := uint64(0); off < length; off += hvfPageSize {
//           as.pt.unmapPage(uint64(addr) + off)
//       }
//       tlbiVMALLE1IS() // Direct flush -- no quarantine needed
//   }
//
// This replaces the current two-phase unmap (remap to zero page,
// then clear PTE) and eliminates the quarantine/epoch infrastructure.

// kernelVABase is the start of the kernel VA range in the upper half.
// With T1SZ=28 (36-bit VA), the kernel VA space spans:
//
//	0xFFFF_FFF0_0000_0000 to 0xFFFF_FFFF_FFFF_FFFF
const kernelVABase = 0xFFFFFFF000000000
