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

// ptBarrier ensures page table writes are visible to ALL cores in
// the inner-shareable domain. Uses DSB ISH (not ISHST) so that
// other cores' MMU walkers see the updated PTE.
static inline void ptBarrier(void) {
    __asm__ __volatile__("dsb ish" ::: "memory");
    __asm__ __volatile__("isb" ::: "memory");
}
*/
import "C"

import (
	"encoding/binary"
	"fmt"
	"unsafe"

	"gvisor.dev/gvisor/pkg/sync"
)

// guestPageTable manages ARM64 page tables for a single address space.
// It uses 16K granule with T0SZ=28 (36-bit VA, L2 start).
//
// L2 table: 2048 entries, each either invalid, a 32MB block descriptor,
// or a table descriptor pointing to an L3 table.
// L3 table: 2048 entries, each a 16K page descriptor or invalid.
type guestPageTable struct {
	mu      sync.Mutex
	machine *machine

	// L2 table (the root).
	l2Host unsafe.Pointer // Host VA of L2 table
	l2IPA  uint64         // IPA where L2 is mapped

	// L3 tables, indexed by L2 entry index.
	l3Tables map[int]*l3Table
}

type l3Table struct {
	hostMem unsafe.Pointer
	ipa     uint64
}

// ARM64 page table constants for 16K granule.
const (
	l2Shift    = 25            // 32MB per L2 entry
	l3Shift    = 14            // 16K per L3 entry
	l2Entries  = 2048          // 16K / 8 bytes
	l3Entries  = 2048          // 16K / 8 bytes
	l2Mask     = l2Entries - 1 // 0x7FF
	l3Mask     = l3Entries - 1 // 0x7FF
	validBit   = 1 << 0
	tableBit   = 1 << 1
	ap1Bit     = 1 << 6  // AP[1]: 1=EL0 access allowed
	ap2Bit     = 1 << 7  // AP[2]: 0=read-write, 1=read-only
	afBit      = 1 << 10
	ngBit      = 1 << 11 // non-Global: TLB entry tagged with ASID
	shBits     = 3 << 8  // Inner Shareable
	normalAttr = 0 << 2  // AttrIdx=0 (Normal WB from MAIR)
)

// newGuestPageTable creates a new per-MM page table.
func newGuestPageTable(m *machine) (*guestPageTable, error) {
	// Allocate L2 table page.
	l2Host, l2IPA, err := m.ptAlloc.allocPage()
	if err != nil {
		return nil, fmt.Errorf("allocating L2 table: %w", err)
	}

	pt := &guestPageTable{
		machine:  m,
		l2Host:   l2Host,
		l2IPA:    l2IPA,
		l3Tables: make(map[int]*l3Table),
	}

	// Map the vectors page (IPA 0) and sigreturn trampoline into
	// every address space. VBAR_EL1 points to VA 0, which must
	// resolve to IPA 0 (where the vectors are in the HVF VM).
	if err := pt.mapPage(0, 0, false); err != nil {
		return nil, fmt.Errorf("mapping vectors: %w", err)
	}

	return pt, nil
}

// ttbr0 returns the IPA of the L2 table for TTBR0_EL1.
func (pt *guestPageTable) ttbr0() uint64 {
	return pt.l2IPA
}

// mapPage creates a page table entry mapping guestVA → ipa.
// If writable is false, the entry is mapped read-only (AP[2]=1),
// which is used to enforce copy-on-write after fork.
func (pt *guestPageTable) mapPage(guestVA, ipa uint64, writable bool) error {
	pt.mu.Lock()
	defer pt.mu.Unlock()

	l2Idx := int((guestVA >> l2Shift) & l2Mask)
	l3Idx := int((guestVA >> l3Shift) & l3Mask)

	// Ensure L3 table exists for this L2 entry.
	l3, ok := pt.l3Tables[l2Idx]
	if !ok {
		// Allocate a new L3 table.
		host, l3ipa, err := pt.machine.ptAlloc.allocPage()
		if err != nil {
			return fmt.Errorf("allocating L3 table: %w", err)
		}
		l3 = &l3Table{hostMem: host, ipa: l3ipa}
		pt.l3Tables[l2Idx] = l3

		// Write L2 table descriptor pointing to this L3.
		l2Entry := l3ipa | tableBit | validBit
		l2Slice := unsafe.Slice((*byte)(pt.l2Host), hvfPageSize)
		binary.LittleEndian.PutUint64(l2Slice[l2Idx*8:], l2Entry)
		C.ptBarrier()
	}

	// ARM64 break-before-make: if the old PTE is valid and maps a
	// different IPA, we must invalidate it first. Writing a new valid
	// PTE over an old valid PTE without break-before-make can cause
	// TLB conflicts on other vCPUs.
	l3Slice := unsafe.Slice((*byte)(l3.hostMem), hvfPageSize)
	// AP[1]=1 grants EL0 access (guest runs at EL0).
	// AP[2]=1 makes the page read-only for both EL1 and EL0.
	apBits := uint64(ap1Bit) // EL0 access
	if !writable {
		apBits |= ap2Bit // read-only
	}
	l3Entry := (ipa &^ (hvfPageSize - 1)) | apBits | ngBit | afBit | shBits | normalAttr | tableBit | validBit
	oldEntry := binary.LittleEndian.Uint64(l3Slice[l3Idx*8:])
	if oldEntry&validBit != 0 {
		oldIPA := oldEntry & 0x0000FFFFFFFC000
		if oldIPA != ipa {
			pt.machine.ipaAlloc.unmapIPA(oldIPA)
		}
		binary.LittleEndian.PutUint64(l3Slice[l3Idx*8:], 0)
		C.ptBarrier()
	}
	// Make: write new entry.
	binary.LittleEndian.PutUint64(l3Slice[l3Idx*8:], l3Entry)
	C.ptBarrier()

	return nil
}

// unmapPage clears the page table entry for guestVA and returns
// the IPA that was mapped (or 0 if the entry was already invalid).
func (pt *guestPageTable) unmapPage(guestVA uint64) uint64 {
	pt.mu.Lock()
	defer pt.mu.Unlock()

	l2Idx := int((guestVA >> l2Shift) & l2Mask)
	l3Idx := int((guestVA >> l3Shift) & l3Mask)

	l3, ok := pt.l3Tables[l2Idx]
	if !ok {
		return 0
	}

	l3Slice := unsafe.Slice((*byte)(l3.hostMem), hvfPageSize)
	oldEntry := binary.LittleEndian.Uint64(l3Slice[l3Idx*8:])
	if oldEntry&validBit == 0 {
		return 0
	}
	ipa := oldEntry & 0x0000FFFFFFFC000 // bits [47:14] = output address for 16K granule

	binary.LittleEndian.PutUint64(l3Slice[l3Idx*8:], 0)
	C.ptBarrier()
	return ipa
}

// release returns all page table pages to the allocator's free list.
func (pt *guestPageTable) release() {
	pt.mu.Lock()
	defer pt.mu.Unlock()

	// Return all L3 table pages.
	for _, l3 := range pt.l3Tables {
		pt.machine.ptAlloc.freePage(l3.hostMem, l3.ipa)
	}
	pt.l3Tables = nil

	// Return the L2 table page.
	if pt.l2Host != nil {
		pt.machine.ptAlloc.freePage(pt.l2Host, pt.l2IPA)
		pt.l2Host = nil
	}
}
