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
	"fmt"
	"unsafe"

	"gvisor.dev/gvisor/pkg/sync"
)

// ipaAllocator manages the guest IPA (Intermediate Physical Address)
// space for the HVF VM. It assigns unique IPAs to host memory pages,
// enabling per-process page tables where different VAs can map to
// different physical pages.
type ipaAllocator struct {
	mu      sync.Mutex
	nextIPA uint64
	// hostToIPA maps host page address → assigned IPA.
	hostToIPA map[uintptr]uint64
	// refCount tracks how many address spaces reference each IPA.
	refCount map[uint64]int
	// freeIPAs holds IPAs that were unmapped and can be reused.
	// Prevents nextIPA from growing without bound.
	freeIPAs []uint64
}

// ipaBase is the start of the allocatable IPA range. The first 16MB
// is reserved for vectors, page tables, and other fixed mappings.
// Page tables start at ptBase (0x10000) and each 16K page consumes
// one IPA slot, so 16MB allows ~1020 page table pages — enough for
// hundreds of concurrent processes.
const ipaBase = 0x1000000 // 16MB

func newIPAAllocator() *ipaAllocator {
	return &ipaAllocator{
		nextIPA:   ipaBase,
		hostToIPA: make(map[uintptr]uint64),
		refCount:  make(map[uint64]int),
	}
}

// mapPage ensures a host page is mapped in the HVF IPA space and
// returns its IPA. If already mapped, returns the existing IPA and
// increments the reference count.
func (a *ipaAllocator) mapPage(hostAddr uintptr, size uintptr) (uint64, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	// Check if already mapped.
	if ipa, ok := a.hostToIPA[hostAddr]; ok {
		a.refCount[ipa]++
		return ipa, nil
	}

	// Prefer fresh IPAs until we hit 512GB, then reuse freed ones.
	// Early reuse causes stage-2 TLB staleness. Deferring reuse
	// until we've consumed significant IPA space gives HVF time
	// to flush stage-2 TLB entries for unmapped IPAs.
	var ipa uint64
	// ipaReuseThreshold: prefer fresh IPAs until 512GB to avoid
	// stage-2 TLB staleness from immediate IPA reuse.
	const ipaReuseThreshold = 1 << 39 // 512GB
	if a.nextIPA < ipaReuseThreshold || len(a.freeIPAs) == 0 {
		ipa = a.nextIPA
		a.nextIPA += uint64(size)
	} else {
		ipa = a.freeIPAs[len(a.freeIPAs)-1]
		a.freeIPAs = a.freeIPAs[:len(a.freeIPAs)-1]
	}

	// Map into HVF.
	ret := C.hv_vm_map(unsafe.Pointer(hostAddr), C.hv_ipa_t(ipa), C.size_t(size),
		C.HV_MEMORY_READ|C.HV_MEMORY_WRITE|C.HV_MEMORY_EXEC)
	if ret != C.HV_SUCCESS {
		return 0, fmt.Errorf("hv_vm_map(host=%#x, ipa=%#x, len=%d): %d", hostAddr, ipa, size, ret)
	}

	a.hostToIPA[hostAddr] = ipa
	a.refCount[ipa] = 1
	return ipa, nil
}

// unmapIPA decrements the refcount for an IPA mapping. When the
// refcount reaches zero, the IPA is unmapped from HVF's stage-2.
// TLB coherency is guaranteed by the ring0 entry stub which executes
// TLBI VMALLE1IS on every guest entry.
func (a *ipaAllocator) unmapIPA(ipa uint64) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if _, ok := a.refCount[ipa]; !ok {
		return
	}
	a.refCount[ipa]--
	if a.refCount[ipa] <= 0 {
		for h, i := range a.hostToIPA {
			if i == ipa {
				delete(a.hostToIPA, h)
				break
			}
		}
		delete(a.refCount, ipa)
		C.hv_vm_unmap(C.hv_ipa_t(ipa), C.size_t(hvfPageSize))
		a.freeIPAs = append(a.freeIPAs, ipa)
	}
}

// lookupIPA returns the IPA for a host address, or 0 if not mapped.
func (a *ipaAllocator) lookupIPA(hostAddr uintptr) (uint64, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	ipa, ok := a.hostToIPA[hostAddr]
	return ipa, ok
}

// ptPageAllocator allocates page table pages in the HVF IPA space.
// Page table pages must be accessible to the guest MMU via IPAs.
// Released pages are recycled via a free list.
type ptPageAllocator struct {
	mu       sync.Mutex
	nextIPA  uint64
	freeList []ptPage // Recycled pages ready for reuse.
}

type ptPage struct {
	hostMem unsafe.Pointer
	ipa     uint64
}

// ptBase is the start of the page-table IPA range (within the first 1MB).
const ptBase = 0x10000 // 64K, after vectors (0-16K) and old PT (16K-32K)

func newPTPageAllocator() *ptPageAllocator {
	return &ptPageAllocator{
		nextIPA: ptBase,
	}
}

// allocPage allocates a 16K page table page and maps it into HVF.
// It reuses pages from the free list when available.
func (p *ptPageAllocator) allocPage() (hostMem unsafe.Pointer, ipa uint64, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Try to reuse a free page.
	if n := len(p.freeList); n > 0 {
		page := p.freeList[n-1]
		p.freeList = p.freeList[:n-1]
		C.memset(page.hostMem, 0, C.size_t(hvfPageSize))
		return page.hostMem, page.ipa, nil
	}

	var mem unsafe.Pointer
	C.posix_memalign(&mem, C.size_t(hvfPageSize), C.size_t(hvfPageSize))
	if mem == nil {
		return nil, 0, fmt.Errorf("posix_memalign failed for page table page")
	}
	C.memset(mem, 0, C.size_t(hvfPageSize))

	ipa = p.nextIPA
	p.nextIPA += hvfPageSize

	ret := C.hv_vm_map(mem, C.hv_ipa_t(ipa), C.size_t(hvfPageSize),
		C.HV_MEMORY_READ|C.HV_MEMORY_WRITE)
	if ret != C.HV_SUCCESS {
		C.free(mem)
		return nil, 0, fmt.Errorf("hv_vm_map page table page: %d", ret)
	}

	return mem, ipa, nil
}

// freePage returns a page table page to the free list for reuse.
func (p *ptPageAllocator) freePage(hostMem unsafe.Pointer, ipa uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.freeList = append(p.freeList, ptPage{hostMem: hostMem, ipa: ipa})
}
