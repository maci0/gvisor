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
// VA layout with T0SZ=16, T1SZ=16 (48-bit VA each):
//
//   0x0000_0000_0000_0000 - 0x0000_FFFF_FFFF_FFFF  TTBR0 (guest, 256TB)
//   0xFFFF_0000_0000_0000 - 0xFFFF_FFFF_FFFF_FFFF  TTBR1 (kernel, 256TB)
//
// TLB coherency: the ERET stub executes TLBI ASIDE1IS (selective
// flush by ASID) on every guest entry. On ASID wrap, TLBI VMALLE1IS
// (full flush) is used. MapFile/Unmap clear PTEs directly.

// kernelVABase is the start of the kernel VA range in the upper half.
// With T1SZ=16 (48-bit VA), the valid range is 0xFFFF_0000_0000_0000
// to 0xFFFF_FFFF_FFFF_FFFF. kernelVABase is placed high within this
// range to leave room for future expansion.
const kernelVABase = 0xFFFFFFF000000000
