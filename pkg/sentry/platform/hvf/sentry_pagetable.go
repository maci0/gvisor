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
#include <sys/mman.h>

static inline hv_return_t vmMapForSentry(uint64_t hostAddr, uint64_t ipa,
                                          size_t size, hv_memory_flags_t flags) {
	return hv_vm_map((void *)hostAddr, (hv_ipa_t)ipa, size, flags);
}

static inline void copyPage(void *dst, uint64_t src, size_t size) {
	memcpy(dst, (void *)src, size);
}
*/
import "C"

import (
	"encoding/binary"
	"fmt"
	"unsafe"

	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/sync"
)

// sentryPageTable manages 4-level ARM64 page tables for running the
// sentry at EL1 with MMU enabled. Uses 16K granule with T0SZ=16
// (48-bit VA), mapping high Go VAs to IPAs within the 40-bit range.
//
// VA → IPA mapping: IPA = VA & ((1<<40)-1). This truncates the top
// 8 bits, mapping any 48-bit VA to a 40-bit IPA. Works because Go's
// memory layout doesn't have VAs that differ only in bits [47:40].
//
// Page table levels for 16K granule, T0SZ=16:
//   L0: VA[47]     → 1 bit,  2 entries  (each covers 128TB)
//   L1: VA[46:36]  → 11 bits, 2048 entries (each covers 64GB)
//   L2: VA[35:25]  → 11 bits, 2048 entries (each covers 32MB)
//   L3: VA[24:14]  → 11 bits, 2048 entries (each covers 16KB)
type sentryPageTable struct {
	mu      sync.Mutex
	machine *machine

	// L0 table (root, 2 entries but allocated as full page).
	l0Host unsafe.Pointer
	l0IPA  uint64

	// Allocated tables for cleanup.
	tables []sentryPTPage
}

type sentryPTPage struct {
	host unsafe.Pointer
	ipa  uint64
}

// Page table constants for 4-level, 16K granule.
const (
	sptL0Shift = 47 // L0 covers bit 47
	sptL1Shift = 36 // L1 covers bits [46:36]
	sptL2Shift = 25 // L2 covers bits [35:25]
	sptL3Shift = 14 // L3 covers bits [24:14]

	sptL0Entries = 2    // Only 1 bit for L0 at T0SZ=16
	sptL1Entries = 2048 // 11 bits
	sptL2Entries = 2048 // 11 bits
	sptL3Entries = 2048 // 11 bits

	sptIdxMask = 0x7FF // 11-bit index mask

	// IPA mask: truncate VA to 40-bit IPA.
	sptIPAMask = (uint64(1) << 40) - 1
)

func newSentryPageTable(m *machine) (*sentryPageTable, error) {
	l0Host, l0IPA, err := m.ptAlloc.allocPage()
	if err != nil {
		return nil, fmt.Errorf("alloc sentry L0: %w", err)
	}

	return &sentryPageTable{
		machine: m,
		l0Host:  l0Host,
		l0IPA:   l0IPA,
	}, nil
}

// ttbr0 returns the IPA of the L0 table for TTBR0_EL1.
func (spt *sentryPageTable) ttbr0() uint64 {
	return spt.l0IPA
}

// mapPage maps a single page: VA → IPA = VA & sptIPAMask.
// The page table entry points to the truncated IPA.
func (spt *sentryPageTable) mapPage(va uint64, writable bool) error {
	spt.mu.Lock()
	defer spt.mu.Unlock()

	ipa := va & sptIPAMask

	l0Idx := int((va >> sptL0Shift) & 1)
	l1Idx := int((va >> sptL1Shift) & sptIdxMask)
	l2Idx := int((va >> sptL2Shift) & sptIdxMask)
	l3Idx := int((va >> sptL3Shift) & sptIdxMask)

	// Walk L0 → L1.
	l1Host, err := spt.ensureTable(spt.l0Host, l0Idx)
	if err != nil {
		return fmt.Errorf("L0[%d]→L1: %w", l0Idx, err)
	}

	// Walk L1 → L2.
	l2Host, err := spt.ensureTable(l1Host, l1Idx)
	if err != nil {
		return fmt.Errorf("L1[%d]→L2: %w", l1Idx, err)
	}

	// Walk L2 → L3.
	l3Host, err := spt.ensureTable(l2Host, l2Idx)
	if err != nil {
		return fmt.Errorf("L2[%d]→L3: %w", l2Idx, err)
	}

	// Write L3 page descriptor.
	l3Slice := unsafe.Slice((*byte)(l3Host), hvfPageSize)
	// AP[1]=0 (EL1-only), nG=0 (global), AF=1, SH=inner-shareable
	l3Entry := (ipa &^ (hvfPageSize - 1)) | afBit | shBits | normalAttr | tableBit | validBit
	if !writable {
		l3Entry |= ap2Bit
	}
	binary.LittleEndian.PutUint64(l3Slice[l3Idx*8:], l3Entry)

	return nil
}

// ensureTable ensures a table descriptor exists at parent[idx].
// Returns the host pointer to the child table.
func (spt *sentryPageTable) ensureTable(parent unsafe.Pointer, idx int) (unsafe.Pointer, error) {
	parentSlice := unsafe.Slice((*byte)(parent), hvfPageSize)
	entry := binary.LittleEndian.Uint64(parentSlice[idx*8:])

	if entry&validBit != 0 {
		// Table already exists. Extract IPA and find host pointer.
		childIPA := entry &^ 0xFFF // Clear attribute bits (low 12)
		childIPA &= 0x0000FFFFFFFC000  // Extract IPA bits for 16K granule
		for _, t := range spt.tables {
			if t.ipa == childIPA {
				return t.host, nil
			}
		}
		return nil, fmt.Errorf("table at IPA %#x not found in tracking list", childIPA)
	}

	// Allocate new table.
	host, ipa, err := spt.machine.ptAlloc.allocPage()
	if err != nil {
		return nil, err
	}
	spt.tables = append(spt.tables, sentryPTPage{host: host, ipa: ipa})

	// Write table descriptor.
	tableDesc := ipa | tableBit | validBit
	binary.LittleEndian.PutUint64(parentSlice[idx*8:], tableDesc)

	return host, nil
}

// demandMapSentryPage handles a page fault during sentry EL1 execution
// with MMU on. Maps the host page at IPA = VA & sptIPAMask.
func (spt *sentryPageTable) demandMapSentryPage(va uint64, writable bool) error {
	ipa := va & sptIPAMask
	pageVA := va &^ (hvfPageSize - 1)
	pageIPA := ipa &^ (hvfPageSize - 1)

	log.Debugf("sentry demand-page: VA=%#x → IPA=%#x", pageVA, pageIPA)

	// Unmap first (might be stale from previous mapping).
	C.hv_vm_unmap(C.hv_ipa_t(pageIPA), C.size_t(hvfPageSize))

	// Try direct mapping.
	flags := C.hv_memory_flags_t(C.HV_MEMORY_READ | C.HV_MEMORY_EXEC)
	if writable {
		flags |= C.HV_MEMORY_WRITE
	}

	ret := C.vmMapForSentry(C.uint64_t(pageVA), C.uint64_t(pageIPA), C.size_t(hvfPageSize), flags)
	if ret == C.HV_SUCCESS {
		return spt.mapPage(pageVA, writable)
	}

	// Direct mapping failed (code-signed page). Copy and map.
	copy := C.mmap(nil, C.size_t(hvfPageSize), C.PROT_READ|C.PROT_WRITE,
		C.MAP_ANON|C.MAP_PRIVATE, -1, 0)
	if copy == nil {
		return fmt.Errorf("mmap copy failed for VA=%#x", pageVA)
	}
	C.copyPage(copy, C.uint64_t(pageVA), C.size_t(hvfPageSize))

	ret = C.hv_vm_map(copy, C.hv_ipa_t(pageIPA), C.size_t(hvfPageSize), flags)
	if ret != C.HV_SUCCESS {
		C.munmap(copy, C.size_t(hvfPageSize))
		return fmt.Errorf("hv_vm_map copy VA=%#x IPA=%#x: %d", pageVA, pageIPA, ret)
	}

	return spt.mapPage(pageVA, writable)
}

// release frees all page table pages.
func (spt *sentryPageTable) release() {
	spt.mu.Lock()
	defer spt.mu.Unlock()

	for _, t := range spt.tables {
		spt.machine.ptAlloc.freePage(t.host, t.ipa)
	}
	spt.tables = nil

	if spt.l0Host != nil {
		spt.machine.ptAlloc.freePage(spt.l0Host, spt.l0IPA)
		spt.l0Host = nil
	}
}
