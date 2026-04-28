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
*/
import "C"

import (
	"unsafe"

	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/platform"
	"gvisor.dev/gvisor/pkg/sync"
)

// addressSpace implements platform.AddressSpace using per-MM page tables.
//
// Each address space has its own ARM64 L2+L3 page table tree. The guest
// MMU translates VA → IPA using these tables (stage 1). HVF then
// translates IPA → HPA using its own stage 2 tables (managed via
// hv_vm_map). This enables fork+exec: different address spaces can
// map the same VA to different physical pages.
type addressSpace struct {
	platform.NoAddressSpaceIO

	mu      sync.Mutex
	machine *machine
	pt      *guestPageTable
}

// hvfPageSize is the minimum page size for HVF mappings on macOS ARM64.
const hvfPageSize = 16384

func newAddressSpace(m *machine) (*addressSpace, error) {
	pt, err := newGuestPageTable(m)
	if err != nil {
		return nil, err
	}
	return &addressSpace{
		machine: m,
		pt:      pt,
	}, nil
}

// MapFile implements platform.AddressSpace.MapFile.
func (as *addressSpace) MapFile(addr hostarch.Addr, f memmap.File, fr memmap.FileRange,
	at hostarch.AccessType, precommit bool) error {

	as.mu.Lock()
	defer as.mu.Unlock()


	// Get the host virtual address mappings for this file region.
	bs, err := f.MapInternal(fr, hostarch.AccessType{
		Read:  at.Read || at.Execute || precommit,
		Write: at.Write,
	})
	if err != nil {
		return err
	}

	// Map each block: assign IPA via allocator, then update page table.
	for !bs.IsEmpty() {
		b := bs.Head()
		bs = bs.Tail()

		bLen := uintptr(b.Len())
		gva := uintptr(addr)
		srcAddr := uintptr(b.Addr())

		// Map each 16K page within this block.
		for off := uintptr(0); off < bLen; off += hvfPageSize {
			pageHost := (srcAddr + off) &^ (hvfPageSize - 1)
			pageGVA := (gva + off) &^ (hvfPageSize - 1)

			// Ensure this host page has an IPA in the HVF VM.
			ipa, err := as.machine.ipaAlloc.mapPage(pageHost, hvfPageSize)
			if err != nil {
				return err
			}

			// Update our per-MM page table to map GVA → IPA.
			// Pass write permission so COW pages are mapped read-only.
			if err := as.pt.mapPage(uint64(pageGVA), ipa, at.Write); err != nil {
				return err
			}
		}

		addr += hostarch.Addr(bLen)
	}

	return nil
}

// Unmap implements platform.AddressSpace.Unmap.
func (as *addressSpace) Unmap(addr hostarch.Addr, length uint64) {
	as.mu.Lock()
	defer as.mu.Unlock()
	as.unmapLocked(addr, length)
}

// unmapLocked clears PTEs and releases IPA mappings.
// TLB coherency is guaranteed by the ring0 entry stub which executes
// TLBI VMALLE1IS at EL1 on every guest entry — no quarantine needed.
func (as *addressSpace) unmapLocked(addr hostarch.Addr, length uint64) {
	for off := uint64(0); off < length; off += hvfPageSize {
		ipa := as.pt.unmapPage(uint64(addr) + off)
		if ipa != 0 {
			as.machine.ipaAlloc.unmapIPA(ipa)
		}
	}
}

// Release implements platform.AddressSpace.Release.
func (as *addressSpace) Release() {
	as.mu.Lock()
	defer as.mu.Unlock()
	if as.pt != nil {
		as.pt.release()
	}
}

// PreFork implements platform.AddressSpace.PreFork.
func (as *addressSpace) PreFork() {}

// PostFork implements platform.AddressSpace.PostFork.
func (as *addressSpace) PostFork() {}

// Suppress unused import warning.
var _ = unsafe.Pointer(nil)
