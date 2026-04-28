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
	"encoding/binary"
	"fmt"
	"unsafe"

	"gvisor.dev/gvisor/pkg/sync"
)

// kernelPageTable manages the TTBR1 page table for upper-half VAs
// (0xFFFF_FFF0_0000_0000 - 0xFFFF_FFFF_FFFF_FFFF). This maps the
// sentry kernel's memory (Go heap, stacks) and is shared across all
// vCPUs via TTBR1_EL1.
//
// For Phase 1, the kernel page table is allocated but empty -- no
// sentry memory is mapped yet. With EPD1=0 in TCR_EL1, accesses to
// upper-half VAs will trigger translation faults, which is correct
// since nothing uses them yet.
//
// Phase 2 will populate this table to map sentry memory into the
// guest's upper-half VA space, enabling sentry-as-ring0 execution.
type kernelPageTable struct {
	mu       sync.Mutex
	machine  *machine
	l2Host   unsafe.Pointer   // Host VA of L2 table
	l2IPA    uint64           // IPA where L2 is mapped
	l3Tables map[int]*l3Table // L3 tables, indexed by L2 entry index
}

// newKernelPageTable creates the shared kernel page table (TTBR1).
// The L2 table is allocated empty; Phase 2 will populate it with
// sentry memory mappings.
func newKernelPageTable(m *machine) (*kernelPageTable, error) {
	l2Host, l2IPA, err := m.ptAlloc.allocPage()
	if err != nil {
		return nil, fmt.Errorf("allocating kernel L2 table: %w", err)
	}

	return &kernelPageTable{
		machine:  m,
		l2Host:   l2Host,
		l2IPA:    l2IPA,
		l3Tables: make(map[int]*l3Table),
	}, nil
}

// ttbr1 returns the L2 IPA for setting TTBR1_EL1.
func (kpt *kernelPageTable) ttbr1() uint64 {
	return kpt.l2IPA
}

// mapKernelRange maps a contiguous range of memory into the kernel
// page table (upper-half VAs via TTBR1). This is used to map sentry
// code, Go heap, and goroutine stacks.
//
// va is the upper-half virtual address (must be in 0xFFFF_FFF0_0000_0000 range).
// hostAddr is the host virtual address of the memory to map.
// size is the size in bytes (rounded up to page size).
// writable controls AP[2] (read-only vs read-write).
func (kpt *kernelPageTable) mapKernelRange(va, hostAddr uintptr, size uint64, writable bool) error {
	for off := uint64(0); off < size; off += hvfPageSize {
		pageVA := va + uintptr(off)
		pageHost := hostAddr + uintptr(off)

		// Get IPA for this host page.
		ipa, err := kpt.machine.ipaAlloc.mapPage(pageHost, hvfPageSize)
		if err != nil {
			return err
		}

		// Map VA -> IPA in the kernel L2/L3 page table.
		if err := kpt.mapPage(uint64(pageVA), ipa, writable); err != nil {
			return err
		}
	}
	return nil
}

// mapPage maps a single page in the kernel page table.
// Similar to guestPageTable.mapPage but for TTBR1 upper-half VAs.
//
// Kernel page table entries differ from guest entries:
//   - No ap1Bit: kernel pages are EL1-only, not accessible from EL0.
//   - No ngBit: kernel pages are global (shared across all ASIDs).
func (kpt *kernelPageTable) mapPage(va, ipa uint64, writable bool) error {
	kpt.mu.Lock()
	defer kpt.mu.Unlock()

	l2Idx := int((va >> l2Shift) & l2Mask)
	l3Idx := int((va >> l3Shift) & l3Mask)

	// Ensure L3 table exists for this L2 entry.
	l3, ok := kpt.l3Tables[l2Idx]
	if !ok {
		host, l3ipa, err := kpt.machine.ptAlloc.allocPage()
		if err != nil {
			return fmt.Errorf("allocating kernel L3 table: %w", err)
		}
		l3 = &l3Table{hostMem: host, ipa: l3ipa}
		kpt.l3Tables[l2Idx] = l3

		// Write L2 table descriptor pointing to this L3.
		l2Entry := l3ipa | tableBit | validBit
		l2Slice := unsafe.Slice((*byte)(kpt.l2Host), hvfPageSize)
		binary.LittleEndian.PutUint64(l2Slice[l2Idx*8:], l2Entry)
	}

	// Write L3 page descriptor.
	// Kernel pages: no AP[1] (EL1-only), no nG (global/shared).
	l3Slice := unsafe.Slice((*byte)(l3.hostMem), hvfPageSize)
	l3Entry := (ipa &^ (hvfPageSize - 1)) | afBit | shBits | normalAttr | tableBit | validBit
	if !writable {
		l3Entry |= ap2Bit
	}
	binary.LittleEndian.PutUint64(l3Slice[l3Idx*8:], l3Entry)

	return nil
}

// release returns all page table pages (L2 and L3) to the allocator.
func (kpt *kernelPageTable) release() {
	kpt.mu.Lock()
	defer kpt.mu.Unlock()

	// Return all L3 table pages.
	for _, l3 := range kpt.l3Tables {
		kpt.machine.ptAlloc.freePage(l3.hostMem, l3.ipa)
	}
	kpt.l3Tables = nil

	// Return the L2 table page.
	if kpt.l2Host != nil {
		kpt.machine.ptAlloc.freePage(kpt.l2Host, kpt.l2IPA)
		kpt.l2Host = nil
	}
}
