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
//
// 16K granule (default): 4-level walk, T0SZ=16, 48-bit VA:
//   L0: VA[47]      → 2 entries
//   L1: VA[46:36]   → 2048 entries
//   L2: VA[35:25]   → 2048 entries
//   L3: VA[24:14]   → 2048 entries, 16K page offset
//
// 4K granule (--page4k): 4-level walk, T0SZ=16, 48-bit VA:
//   L0: VA[47:39]   → 512 entries
//   L1: VA[38:30]   → 512 entries
//   L2: VA[29:21]   → 512 entries
//   L3: VA[20:12]   → 512 entries, 4K page offset
type guestPageTable struct {
	mu      sync.Mutex
	machine *machine

	l0Host unsafe.Pointer
	l0IPA  uint64

	l1Tables map[int]*ptTable // indexed by L0 entry
	l2Tables map[l2Key]*ptTable
	l3Tables map[l3Key]*ptTable
}

type l2Key struct{ l0, l1 int }
type l3Key struct{ l0, l1, l2 int }

type ptTable struct {
	hostMem unsafe.Pointer
	ipa     uint64
}

// ARM64 page table parameters. Defaults to 16K granule, 48-bit VA.
// setGuestPageSize(4096) switches to 4K granule.
var (
	ptL0Shift   = 47
	ptL1Shift   = 36
	ptL2Shift   = 25
	ptL3Shift   = 14
	ptL0Entries = 2    // 1-bit index (VA[47]) for 16K
	ptL1Entries = 2048 // 11-bit index for 16K
	ptL2Entries = 2048
	ptL3Entries = 2048
	ptIPAMask   = uint64(0x0000FFFFFFFFC000) // bits [47:14] for 16K
)

const (
	validBit   = 1 << 0
	tableBit   = 1 << 1
	ap1Bit     = 1 << 6
	ap2Bit     = 1 << 7
	afBit      = 1 << 10
	ngBit      = 1 << 11
	shBits     = 3 << 8
	normalAttr = 0 << 2
)

// ptPageBytes returns the allocation size for a page table page.
func ptPageBytes() uintptr {
	return uintptr(hvfPageSize)
}

func initPageTableFor4K() {
	ptL0Shift = 39
	ptL1Shift = 30
	ptL2Shift = 21
	ptL3Shift = 12
	ptL0Entries = 512
	ptL1Entries = 512
	ptL2Entries = 512
	ptL3Entries = 512
	ptIPAMask = 0x0000FFFFFFFFF000 // bits [47:12] for 4K
}

func newGuestPageTable(m *machine) (*guestPageTable, error) {
	l0Host, l0IPA, err := m.ptAlloc.allocPage()
	if err != nil {
		return nil, fmt.Errorf("allocating L0 table: %w", err)
	}

	pt := &guestPageTable{
		machine:  m,
		l0Host:   l0Host,
		l0IPA:    l0IPA,
		l1Tables: make(map[int]*ptTable),
		l2Tables: make(map[l2Key]*ptTable),
		l3Tables: make(map[l3Key]*ptTable),
	}

	if err := pt.mapPage(0, 0, false); err != nil {
		return nil, fmt.Errorf("mapping vectors: %w", err)
	}

	return pt, nil
}

func (pt *guestPageTable) ttbr0() uint64 {
	return pt.l0IPA
}

func (pt *guestPageTable) mapPage(guestVA, ipa uint64, writable bool) error {
	pt.mu.Lock()
	defer pt.mu.Unlock()

	l0Idx := int((guestVA >> ptL0Shift) & uint64(ptL0Entries-1))
	l1Idx := int((guestVA >> ptL1Shift) & uint64(ptL1Entries-1))
	l2Idx := int((guestVA >> ptL2Shift) & uint64(ptL2Entries-1))
	l3Idx := int((guestVA >> ptL3Shift) & uint64(ptL3Entries-1))

	// Ensure L1 table exists.
	l1, ok := pt.l1Tables[l0Idx]
	if !ok {
		host, tipa, err := pt.machine.ptAlloc.allocPage()
		if err != nil {
			return fmt.Errorf("allocating L1 table: %w", err)
		}
		l1 = &ptTable{hostMem: host, ipa: tipa}
		pt.l1Tables[l0Idx] = l1
		writeTableEntry(pt.l0Host, l0Idx, tipa)
	}

	// Ensure L2 table exists.
	k2 := l2Key{l0Idx, l1Idx}
	l2, ok := pt.l2Tables[k2]
	if !ok {
		host, tipa, err := pt.machine.ptAlloc.allocPage()
		if err != nil {
			return fmt.Errorf("allocating L2 table: %w", err)
		}
		l2 = &ptTable{hostMem: host, ipa: tipa}
		pt.l2Tables[k2] = l2
		writeTableEntry(l1.hostMem, l1Idx, tipa)
	}

	// Ensure L3 table exists.
	k3 := l3Key{l0Idx, l1Idx, l2Idx}
	l3, ok := pt.l3Tables[k3]
	if !ok {
		host, tipa, err := pt.machine.ptAlloc.allocPage()
		if err != nil {
			return fmt.Errorf("allocating L3 table: %w", err)
		}
		l3 = &ptTable{hostMem: host, ipa: tipa}
		pt.l3Tables[k3] = l3
		writeTableEntry(l2.hostMem, l2Idx, tipa)
	}

	// Write L3 page descriptor.
	apBits := uint64(ap1Bit)
	if !writable {
		apBits |= ap2Bit
	}
	l3Entry := (ipa &^ (uint64(hvfPageSize) - 1)) | apBits | ngBit | afBit | shBits | normalAttr | tableBit | validBit

	l3Slice := unsafe.Slice((*byte)(l3.hostMem), ptPageBytes())
	oldEntry := binary.LittleEndian.Uint64(l3Slice[l3Idx*8:])
	if oldEntry&validBit != 0 {
		if oldEntry == l3Entry {
			return nil
		}
		oldIPA := oldEntry & ptIPAMask
		if oldIPA != ipa {
			pt.machine.ipaAlloc.unmapIPA(oldIPA)
		}
		binary.LittleEndian.PutUint64(l3Slice[l3Idx*8:], 0)
		C.ptBarrier()
	}
	binary.LittleEndian.PutUint64(l3Slice[l3Idx*8:], l3Entry)
	C.ptBarrier()

	return nil
}

func (pt *guestPageTable) unmapPage(guestVA uint64) uint64 {
	pt.mu.Lock()
	defer pt.mu.Unlock()

	l0Idx := int((guestVA >> ptL0Shift) & uint64(ptL0Entries-1))
	l1Idx := int((guestVA >> ptL1Shift) & uint64(ptL1Entries-1))
	l2Idx := int((guestVA >> ptL2Shift) & uint64(ptL2Entries-1))
	l3Idx := int((guestVA >> ptL3Shift) & uint64(ptL3Entries-1))

	k3 := l3Key{l0Idx, l1Idx, l2Idx}
	l3, ok := pt.l3Tables[k3]
	if !ok {
		return 0
	}

	l3Slice := unsafe.Slice((*byte)(l3.hostMem), ptPageBytes())
	oldEntry := binary.LittleEndian.Uint64(l3Slice[l3Idx*8:])
	if oldEntry&validBit == 0 {
		return 0
	}
	ipa := oldEntry & ptIPAMask

	binary.LittleEndian.PutUint64(l3Slice[l3Idx*8:], 0)
	C.ptBarrier()
	return ipa
}

// unmapRange efficiently unmaps all pages in [start, end) by walking
// only L3 tables that overlap the range, instead of iterating every page.
func (pt *guestPageTable) unmapRange(start, end uint64, ipaAlloc *ipaAllocator) {
	pt.mu.Lock()
	defer pt.mu.Unlock()

	for k3, l3 := range pt.l3Tables {
		// Compute the VA range covered by this L3 table.
		tableBase := uint64(k3.l0)<<ptL0Shift | uint64(k3.l1)<<ptL1Shift | uint64(k3.l2)<<ptL2Shift
		tableEnd := tableBase + uint64(ptL3Entries)<<ptL3Shift
		if tableEnd <= start || tableBase >= end {
			continue
		}

		l3Slice := unsafe.Slice((*byte)(l3.hostMem), ptPageBytes())
		for i := 0; i < ptL3Entries; i++ {
			pageVA := tableBase + uint64(i)<<ptL3Shift
			if pageVA < start || pageVA >= end {
				continue
			}
			entry := binary.LittleEndian.Uint64(l3Slice[i*8:])
			if entry&validBit == 0 {
				continue
			}
			ipa := entry & ptIPAMask
			binary.LittleEndian.PutUint64(l3Slice[i*8:], 0)
			if ipa != 0 {
				ipaAlloc.unmapIPA(ipa)
			}
		}
	}
	C.ptBarrier()
}

func (pt *guestPageTable) release() {
	pt.mu.Lock()
	defer pt.mu.Unlock()

	for _, l3 := range pt.l3Tables {
		l3Slice := unsafe.Slice((*byte)(l3.hostMem), ptPageBytes())
		for i := 0; i < ptL3Entries; i++ {
			entry := binary.LittleEndian.Uint64(l3Slice[i*8:])
			if entry&validBit != 0 {
				ipa := entry & ptIPAMask
				if ipa != 0 {
					pt.machine.ipaAlloc.unmapIPA(ipa)
				}
			}
		}
		pt.machine.ptAlloc.freePage(l3.hostMem, l3.ipa)
	}
	pt.l3Tables = nil

	for _, l2 := range pt.l2Tables {
		pt.machine.ptAlloc.freePage(l2.hostMem, l2.ipa)
	}
	pt.l2Tables = nil

	for _, l1 := range pt.l1Tables {
		pt.machine.ptAlloc.freePage(l1.hostMem, l1.ipa)
	}
	pt.l1Tables = nil

	if pt.l0Host != nil {
		pt.machine.ptAlloc.freePage(pt.l0Host, pt.l0IPA)
		pt.l0Host = nil
	}
}

func writeTableEntry(tableHost unsafe.Pointer, idx int, childIPA uint64) {
	entry := childIPA | tableBit | validBit
	slice := unsafe.Slice((*byte)(tableHost), ptPageBytes())
	binary.LittleEndian.PutUint64(slice[idx*8:], entry)
	C.ptBarrier()
}
