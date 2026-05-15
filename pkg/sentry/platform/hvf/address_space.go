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

// hvfPageSize is the page size for HVF mappings on macOS ARM64.
// Defaults to 16K; set to 4K via setGuestPageSize(4096) for --page4k.
var hvfPageSize uintptr = 16384

func setGuestPageSize(size uintptr) {
	hvfPageSize = size
	if size == 4096 {
		initPageTableFor4K()
	}
}

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

// sentryOwnedFile is a marker interface for memmap.File implementations
// whose MapInternal host VAs are written to by the sentry and must
// remain directly mapped (not shadow-copied) so that sentry writes
// are immediately visible to the guest. pgalloc.MemoryFile implements
// this interface.
type sentryOwnedFile interface {
	// IsSentryOwned is a marker method. If a memmap.File implements this,
	// its host VAs are mapped directly into HVF without shadow-copying.
	IsSentryOwned()
}

// MapFile implements platform.AddressSpace.MapFile.
func (as *addressSpace) MapFile(addr hostarch.Addr, f memmap.File, fr memmap.FileRange,
	at hostarch.AccessType, precommit bool) error {

	as.mu.Lock()
	defer as.mu.Unlock()

	// Determine whether to shadow-copy this file's pages.
	// MemoryFile pages are written by the sentry and must be mapped
	// directly so writes are visible to the guest. File-backed pages
	// (gofer file mmaps) must be shadow-copied because macOS can
	// relocate their physical pages without updating HVF's stage-2.
	_, sentryOwned := f.(sentryOwnedFile)
	shadow := !sentryOwned

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

		pageSz := uintptr(hvfPageSize)
		for off := uintptr(0); off < bLen; off += pageSz {
			pageHost := (srcAddr + off) &^ (pageSz - 1)
			pageGVA := (gva + off) &^ (pageSz - 1)

			var ipa uint64
			if shadow || at.Execute {
				ipa, err = as.machine.ipaAlloc.mapPageShadow(pageHost, pageSz)
			} else {
				ipa, err = as.machine.ipaAlloc.mapPage(pageHost, pageSz)
			}
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
// For large ranges (>1GB), uses the page table's internal structure
// to skip unmapped regions instead of iterating every page.
func (as *addressSpace) unmapLocked(addr hostarch.Addr, length uint64) {
	end := uint64(addr) + length
	// For ranges larger than 1GB, iterate only mapped L3 entries
	// to avoid O(n) iteration over sparse address spaces.
	if length > 1<<30 {
		as.pt.unmapRange(uint64(addr), end, as.machine.ipaAlloc)
		return
	}
	for off := uint64(0); off < length; off += uint64(hvfPageSize) {
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
