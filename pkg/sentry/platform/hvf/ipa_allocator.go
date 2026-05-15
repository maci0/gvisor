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

	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/sync"
)

// ipaAllocator manages the guest IPA (Intermediate Physical Address)
// space for the HVF VM. It assigns unique IPAs to host memory pages,
// enabling per-process page tables where different VAs can map to
// different physical pages.
//
// Shadow pages: macOS can silently relocate the physical page backing
// a file-mapped host VA (e.g., MAP_PRIVATE of a gofer file) without
// updating HVF's stage-2 tables. This causes the guest to read stale
// data, crashing dynamically-linked binaries. To prevent this, guest
// user pages are "shadow-copied": the file-backed content is copied
// into anonymous memory (MAP_PRIVATE|MAP_ANONYMOUS), and the anonymous
// VA is passed to hv_vm_map. Anonymous pages have stable physical
// addresses that macOS will not relocate.
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
	// ipaToHost is the reverse of hostToIPA for O(1) cleanup.
	ipaToHost map[uint64]uintptr
	// shadowPages maps IPA → anonymous shadow page pointer.
	shadowPages map[uint64]unsafe.Pointer
}

// ipaBase is the start of the allocatable IPA range. The first 16MB
// is reserved for vectors, page tables, and other fixed mappings.
// Page tables start at ptBase (0x10000) and each 16K page consumes
// one IPA slot, so 16MB allows ~1020 page table pages — enough for
// hundreds of concurrent processes.
const ipaBase = 0x1000000 // 16MB

// ipaMax is the maximum IPA (40-bit, matching hv_vm_config IPA size).
const ipaMax = 1 << 40 // 1TB

func newIPAAllocator() *ipaAllocator {
	return &ipaAllocator{
		nextIPA:     ipaBase,
		hostToIPA:   make(map[uintptr]uint64),
		ipaToHost:   make(map[uint64]uintptr),
		refCount:    make(map[uint64]int),
		shadowPages: make(map[uint64]unsafe.Pointer),
	}
}

// mapPage ensures a host page is mapped in the HVF IPA space and
// returns its IPA. If already mapped, returns the existing IPA and
// increments the reference count.
//
// This method maps the host VA directly into HVF. It is used for
// kernel (sentry) memory that is already anonymous and must remain
// writable by the sentry (Go heap, goroutine stacks, etc.).
func (a *ipaAllocator) mapPage(hostAddr uintptr, size uintptr) (uint64, error) {
	return a.mapPageInternal(hostAddr, size, false)
}

// mapPageShadow maps a host page into HVF via an anonymous shadow
// copy. The content at hostAddr is copied into freshly allocated
// anonymous memory, and the anonymous VA is passed to hv_vm_map.
// This prevents macOS from relocating the physical page backing
// file-mapped host VAs (MAP_PRIVATE of gofer files), which would
// cause HVF's stage-2 tables to point at the wrong physical page.
//
// Used for guest user pages from MapFile.
func (a *ipaAllocator) mapPageShadow(hostAddr uintptr, size uintptr) (uint64, error) {
	return a.mapPageInternal(hostAddr, size, true)
}

func (a *ipaAllocator) mapPageInternal(hostAddr uintptr, size uintptr, shadow bool) (uint64, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	// Check if already mapped.
	if ipa, ok := a.hostToIPA[hostAddr]; ok {
		if shadow {
			if sp, hasShadow := a.shadowPages[ipa]; hasShadow {
				// Refresh existing shadow copy.
				C.memcpy(sp, unsafe.Pointer(hostAddr), C.size_t(size))
			} else {
				// Was mapped direct, now requested as shadow. Create
				// shadow copy and remap to prevent PA relocation.
				var anonMem unsafe.Pointer
				if ret := C.posix_memalign(&anonMem, C.size_t(size), C.size_t(size)); ret != 0 || anonMem == nil {
					return 0, fmt.Errorf("posix_memalign failed for shadow upgrade (size=%d)", size)
				}
				C.memcpy(anonMem, unsafe.Pointer(hostAddr), C.size_t(size))
				patchIDRegisterReads(anonMem, size)
				C.hv_vm_unmap(C.hv_ipa_t(ipa), C.size_t(size))
				C.hv_vm_map(anonMem, C.hv_ipa_t(ipa), C.size_t(size),
					C.HV_MEMORY_READ|C.HV_MEMORY_WRITE|C.HV_MEMORY_EXEC)
				a.shadowPages[ipa] = anonMem
			}
		}
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
		if a.nextIPA+uint64(size) > ipaMax {
			return 0, fmt.Errorf("IPA space exhausted (next=%#x, max=%#x)", a.nextIPA, uint64(ipaMax))
		}
		ipa = a.nextIPA
		a.nextIPA += uint64(size)
	} else {
		ipa = a.freeIPAs[len(a.freeIPAs)-1]
		a.freeIPAs = a.freeIPAs[:len(a.freeIPAs)-1]
	}

	// Determine the VA to pass to hv_vm_map.
	mapAddr := unsafe.Pointer(hostAddr)
	if shadow {
		// Allocate anonymous memory and copy the page content.
		var anonMem unsafe.Pointer
		C.posix_memalign(&anonMem, C.size_t(size), C.size_t(size))
		if anonMem == nil {
			return 0, fmt.Errorf("posix_memalign failed for shadow page (size=%d)", size)
		}
		C.memcpy(anonMem, unsafe.Pointer(hostAddr), C.size_t(size))
		patchIDRegisterReads(anonMem, size)
		mapAddr = anonMem
		a.shadowPages[ipa] = anonMem
	}

	// Map into HVF.
	ret := C.hv_vm_map(mapAddr, C.hv_ipa_t(ipa), C.size_t(size),
		C.HV_MEMORY_READ|C.HV_MEMORY_WRITE|C.HV_MEMORY_EXEC)
	if ret != C.HV_SUCCESS {
		if shadow {
			C.free(a.shadowPages[ipa])
			delete(a.shadowPages, ipa)
		}
		return 0, fmt.Errorf("hv_vm_map(host=%#x, ipa=%#x, len=%d): %d", hostAddr, ipa, size, ret)
	}

	a.hostToIPA[hostAddr] = ipa
	a.ipaToHost[ipa] = hostAddr
	a.refCount[ipa] = 1
	return ipa, nil
}

// unmapIPA decrements the refcount for an IPA mapping. When the
// refcount reaches zero, the IPA is unmapped from HVF's stage-2.
// TLB coherency is enforced by:
//  1. hv_vm_protect permission cycling (forces stage-2 TLB invalidation)
//  2. The ring0 entry stub (TLBI VMALLE1IS on every guest entry)
func (a *ipaAllocator) unmapIPA(ipa uint64) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if _, ok := a.refCount[ipa]; !ok {
		return
	}
	a.refCount[ipa]--
	if a.refCount[ipa] <= 0 {
		if h, ok := a.ipaToHost[ipa]; ok {
			delete(a.hostToIPA, h)
			delete(a.ipaToHost, ipa)
		}
		delete(a.refCount, ipa)

		// Force stage-2 TLB invalidation for this IPA before unmapping.
		// ARM64 architecture requires break-before-make: removing permissions
		// forces the kernel to issue TLBI for the affected IPA range.
		// This ensures all vCPUs see the unmapped page even if they have
		// stale TLB entries from a previous hv_vcpu_run.
		C.hv_vm_protect(C.hv_ipa_t(ipa), C.size_t(hvfPageSize), 0)

		C.hv_vm_unmap(C.hv_ipa_t(ipa), C.size_t(hvfPageSize))

		// Free the shadow page if one was allocated.
		if sp, ok := a.shadowPages[ipa]; ok {
			C.free(sp)
			delete(a.shadowPages, ipa)
		}

		a.freeIPAs = append(a.freeIPAs, ipa)
	}
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

// allocPage allocates a page table page and maps it into HVF.
// Uses 16K for kernel page tables (always 16K granule) when the
// caller's page table uses 16K entries. The size is determined by
// hvfPageSize for guest PTs, but kernel PTs always need 16K.
// It reuses pages from the free list when available.
func (p *ptPageAllocator) allocPage() (hostMem unsafe.Pointer, ipa uint64, err error) {
	return p.allocPageSize(uint64(hvfPageSize))
}

// allocKernelPage allocates a 16K page table page for kernel (TTBR1)
// page tables, which always use 16K granule regardless of hvfPageSize.
func (p *ptPageAllocator) allocKernelPage() (hostMem unsafe.Pointer, ipa uint64, err error) {
	return p.allocPageSize(16384)
}

func (p *ptPageAllocator) allocPageSize(size uint64) (hostMem unsafe.Pointer, ipa uint64, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Try to reuse a free page (only if same size).
	if size == uint64(hvfPageSize) {
		if n := len(p.freeList); n > 0 {
			page := p.freeList[n-1]
			p.freeList = p.freeList[:n-1]
			C.memset(page.hostMem, 0, C.size_t(size))
			return page.hostMem, page.ipa, nil
		}
	}

	var mem unsafe.Pointer
	if ret := C.posix_memalign(&mem, C.size_t(16384), C.size_t(size)); ret != 0 || mem == nil {
		return nil, 0, fmt.Errorf("posix_memalign failed for page table page (ret=%d)", ret)
	}
	C.memset(mem, 0, C.size_t(size))

	// Align nextIPA to the allocation size.
	aligned := (p.nextIPA + size - 1) &^ (size - 1)
	ipa = aligned
	if ipa+size > ipaBase {
		C.free(mem)
		return nil, 0, fmt.Errorf("page table IPA space exhausted (next=%#x, limit=%#x)", ipa, ipaBase)
	}
	p.nextIPA = ipa + size

	ret := C.hv_vm_map(mem, C.hv_ipa_t(ipa), C.size_t(size),
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

// patchIDRegisterReads scans a code page for MRS instructions that read
// ARM64 ID registers (trapped by HVF, causing the vCPU to hang).
// Replaces them with MOV instructions that load the correct values.
func patchIDRegisterReads(page unsafe.Pointer, size uintptr) {
	words := (*[1 << 20]uint32)(page)[:size/4]
	for i, instr := range words {
		// MRS Xt, <sysreg>: 1101 0101 0011 .... .... .... .... ....
		if instr&0xFFF00000 != 0xD5300000 {
			continue
		}
		rt := instr & 0x1f
		sysreg := (instr >> 5) & 0x7FFF

		var val uint64
		var name string
		// Values must match emulateSysreg() in context.go.
		// Only patch registers whose values fit in 16 bits (MOVZ limit).
		// Registers with >16-bit values (ISAR0, ISAR1, MIDR) are left
		// unpatched — they trap to emulateSysreg which returns the full value.
		switch sysreg {
		// ID_AA64MMFR0_EL1 (0x4038): value 0x101122 > 16 bits, skip
		// patching. Trap to emulateSysreg for correct full value.
		case 0x4039: // ID_AA64MMFR1_EL1
			val = 0
			name = "ID_AA64MMFR1_EL1"
		case 0x403a: // ID_AA64MMFR2_EL1
			val = 0
			name = "ID_AA64MMFR2_EL1"
		case 0x4020: // ID_AA64PFR0_EL1
			val = 0x0011 // EL0+EL1 AArch64
			name = "ID_AA64PFR0_EL1"
		case 0x4021: // ID_AA64PFR1_EL1
			val = 0
			name = "ID_AA64PFR1_EL1"
		case 0x4028: // ID_AA64DFR0_EL1
			val = 0
			name = "ID_AA64DFR0_EL1"
		// MPIDR_EL1 (0x4005): value 0x80000000 > 16 bits, skip patching.
		// Trap to emulateSysreg which returns the full value.
		case 0x4102: // TCR_EL1 (S3_0_C2_C0_2) — EL0 read traps here
			val = 0
			name = "TCR_EL1"
		// ID_AA64ISAR0/1, MIDR: values >16 bits, skip patching.
		// They trap to emulateSysreg() which returns the full value.
		default:
			continue
		}

		// Replace MRS with MOVZ Xt, #imm16.
		// All patched values fit in 16 bits (>16-bit registers are
		// skipped above and trap to emulateSysreg instead).
		words[i] = 0xD2800000 | (uint32(val) << 5) | rt
		log.Debugf("patched MRS %s → x%d at page offset %d", name, rt, i*4)
	}
}
