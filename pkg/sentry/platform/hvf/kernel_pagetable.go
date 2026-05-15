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

static inline void kptBarrier(void) {
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

// Kernel page table constants — always 16K granule (TTBR1 TG1=16K).
const (
	kptL0Shift   = 47
	kptL1Shift   = 36
	kptL2Shift   = 25
	kptL3Shift   = 14
	kptL0Entries = 2
	kptL1Entries = 2048
	kptL2Entries = 2048
	kptL3Entries = 2048
	kptL0Mask    = kptL0Entries - 1
	kptL1Mask    = kptL1Entries - 1
	kptL2Mask    = kptL2Entries - 1
	kptL3Mask    = kptL3Entries - 1
	kptTableSize = kptL3Entries * 8 // 16K
)

func kptWriteTableEntry(tableHost unsafe.Pointer, idx int, childIPA uint64) {
	entry := childIPA | tableBit | validBit
	slice := unsafe.Slice((*byte)(tableHost), kptTableSize)
	binary.LittleEndian.PutUint64(slice[idx*8:], entry)
	C.kptBarrier()
}

// kernelPageTable manages the TTBR1 page table for upper-half VAs
// (0xFFFF_FFF0_0000_0000 - 0xFFFF_FFFF_FFFF_FFFF). This maps the
// sentry kernel's memory (Go heap, stacks) and is shared across all
// vCPUs via TTBR1_EL1.
//
// Maps the vectors page, per-vCPU state pages, and (in future Ring0
// mode) sentry memory. With EPD1=0 in TCR_EL1, unmapped upper-half
// VAs trigger translation faults.
type kernelPageTable struct {
	mu       sync.Mutex
	machine  *machine
	l0Host   unsafe.Pointer
	l0IPA    uint64
	l1Tables map[int]*ptTable
	l2Tables map[l2Key]*ptTable
	l3Tables map[l3Key]*ptTable
}

// newKernelPageTable creates the shared kernel page table (TTBR1).
// The L2 table is allocated empty; Phase 2 will populate it with
// sentry memory mappings.
func newKernelPageTable(m *machine) (*kernelPageTable, error) {
	l0Host, l0IPA, err := m.ptAlloc.allocKernelPage()
	if err != nil {
		return nil, fmt.Errorf("allocating kernel L0 table: %w", err)
	}

	return &kernelPageTable{
		machine:  m,
		l0Host:   l0Host,
		l0IPA:    l0IPA,
		l1Tables: make(map[int]*ptTable),
		l2Tables: make(map[l2Key]*ptTable),
		l3Tables: make(map[l3Key]*ptTable),
	}, nil
}

func (kpt *kernelPageTable) ttbr1() uint64 {
	return kpt.l0IPA
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

	l0Idx := int((va >> kptL0Shift) & kptL0Mask)
	l1Idx := int((va >> kptL1Shift) & kptL1Mask)
	l2Idx := int((va >> kptL2Shift) & kptL2Mask)
	l3Idx := int((va >> kptL3Shift) & kptL3Mask)

	l1, ok := kpt.l1Tables[l0Idx]
	if !ok {
		host, tipa, err := kpt.machine.ptAlloc.allocKernelPage()
		if err != nil {
			return fmt.Errorf("allocating kernel L1: %w", err)
		}
		l1 = &ptTable{hostMem: host, ipa: tipa}
		kpt.l1Tables[l0Idx] = l1
		kptWriteTableEntry(kpt.l0Host, l0Idx, tipa)
	}

	k2 := l2Key{l0Idx, l1Idx}
	l2, ok := kpt.l2Tables[k2]
	if !ok {
		host, tipa, err := kpt.machine.ptAlloc.allocKernelPage()
		if err != nil {
			return fmt.Errorf("allocating kernel L2: %w", err)
		}
		l2 = &ptTable{hostMem: host, ipa: tipa}
		kpt.l2Tables[k2] = l2
		kptWriteTableEntry(l1.hostMem, l1Idx, tipa)
	}

	k3 := l3Key{l0Idx, l1Idx, l2Idx}
	l3, ok := kpt.l3Tables[k3]
	if !ok {
		host, tipa, err := kpt.machine.ptAlloc.allocKernelPage()
		if err != nil {
			return fmt.Errorf("allocating kernel L3: %w", err)
		}
		l3 = &ptTable{hostMem: host, ipa: tipa}
		kpt.l3Tables[k3] = l3
		kptWriteTableEntry(l2.hostMem, l2Idx, tipa)
	}

	l3Slice := unsafe.Slice((*byte)(l3.hostMem), kptTableSize)
	l3Entry := (ipa &^ ((16384) - 1)) | afBit | shBits | normalAttr | tableBit | validBit
	if !writable {
		l3Entry |= ap2Bit
	}

	// Break-before-make: clear existing valid entry before writing new one.
	oldEntry := binary.LittleEndian.Uint64(l3Slice[l3Idx*8:])
	if oldEntry&validBit != 0 && oldEntry != l3Entry {
		binary.LittleEndian.PutUint64(l3Slice[l3Idx*8:], 0)
		C.kptBarrier()
	}
	binary.LittleEndian.PutUint64(l3Slice[l3Idx*8:], l3Entry)
	C.kptBarrier()

	return nil
}

// release returns all page table pages (L2 and L3) to the allocator.
func (kpt *kernelPageTable) release() {
	kpt.mu.Lock()
	defer kpt.mu.Unlock()

	for _, l3 := range kpt.l3Tables {
		kpt.machine.ptAlloc.freePage(l3.hostMem, l3.ipa)
	}
	kpt.l3Tables = nil
	for _, l2 := range kpt.l2Tables {
		kpt.machine.ptAlloc.freePage(l2.hostMem, l2.ipa)
	}
	kpt.l2Tables = nil
	for _, l1 := range kpt.l1Tables {
		kpt.machine.ptAlloc.freePage(l1.hostMem, l1.ipa)
	}
	kpt.l1Tables = nil
	if kpt.l0Host != nil {
		kpt.machine.ptAlloc.freePage(kpt.l0Host, kpt.l0IPA)
		kpt.l0Host = nil
	}
}
