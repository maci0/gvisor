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
	"runtime"
	"unsafe"

	"gvisor.dev/gvisor/pkg/hosttid"
	"gvisor.dev/gvisor/pkg/sync"
)

// vCPU represents a single Hypervisor.framework virtual CPU.
//
// On macOS, each HVF vCPU is bound to the OS thread that created it.
// Therefore, vCPUs are created lazily in Get() on the calling OS thread
// and cached per-thread for reuse.
// statePageVABase is the kernel VA where per-vCPU state pages start.
// Each vCPU gets one 16K page: statePageVABase + id * 16K.
const statePageVABase = kernelVABase + 0x4000

type vCPU struct {
	id          int
	vcpuID      C.hv_vcpu_t       // HVF vCPU handle
	exit        *C.hv_vcpu_exit_t // Exit information (mapped by HVF)
	tid         uint64            // OS thread ID that owns this vCPU
	machine     *machine          // Parent machine (shared resources)
	asidCounter    uint32 // Incrementing ASID for TLB invalidation
	asidWrapped    bool   // True when ASID just wrapped (need full TLBI)
	fpLoaded      bool // FP regs loaded at least once
	saveFP        bool // Save FP on next saveRegisters call
	gpInStatePage bool // True when GP regs were saved to state page by EL1 handler

	// Per-vCPU state page for in-VM register save/restore.
	statePageHost unsafe.Pointer // host VA (for direct read/write)
	statePageVA   uint64         // kernel VA in TTBR1 (for EL1 access)
}

// NotifyInterrupt implements interrupt.Receiver.NotifyInterrupt.
//
// On HVF, we cancel the running vCPU to cause it to exit with
// HV_EXIT_REASON_CANCELED.
func (c *vCPU) NotifyInterrupt() {
	C.hv_vcpus_exit(&c.vcpuID, 1)
}

// machine manages the Hypervisor.framework VM and vCPU pool.
//
// HVF vCPUs are thread-local: a vCPU can only be used on the OS thread
// that created it. The machine creates vCPUs lazily per-thread and
// caches them for reuse.
type machine struct {
	mu       sync.Mutex
	vcpus    []*vCPU
	maxVCPUs int

	// ipaAlloc assigns unique IPAs to host memory pages.
	ipaAlloc *ipaAllocator

	// ptAlloc allocates page table pages in the HVF IPA space.
	ptAlloc *ptPageAllocator

	// vectorsAddr is the guest IPA of the shared exception vector table.
	vectorsAddr uint64

	// vectorsMem is the host memory for the shared vectors page.
	vectorsMem unsafe.Pointer

	// dispatchMem is the host memory for the EL1 dispatch code page.
	// Mapped in TTBR1 at dispatchKVA. Contains syscall handlers that
	// run at EL1 without VM exit (called via BLR from el0_sync vector).
	dispatchMem unsafe.Pointer
	dispatchIPA uint64
	dispatchKVA uint64

	// fullTLBIStubOff is the offset of the full TLBI+LDP ERET stub
	// (used on ASID wrap). Set by setupSharedMemory().
	fullTLBIStubOff uint64

	// kernelPT is the shared kernel page table for TTBR1_EL1.
	// It maps upper-half VAs for the sentry (Go heap, stacks).
	// Shared across all vCPUs.
	kernelPT *kernelPageTable
}

func newMachine() (*machine, error) {
	maxVCPUs := runtime.NumCPU()
	if maxVCPUs > 64 {
		maxVCPUs = 64
	}

	m := &machine{
		maxVCPUs: maxVCPUs,
		ipaAlloc: newIPAAllocator(),
		ptAlloc:  newPTPageAllocator(),
	}

	// Set up shared VM resources (vectors + page tables).
	if err := m.setupSharedMemory(); err != nil {
		return nil, err
	}

	// Allocate the shared kernel page table for TTBR1 (upper-half VAs).
	kpt, err := newKernelPageTable(m)
	if err != nil {
		return nil, fmt.Errorf("creating kernel page table: %w", err)
	}
	m.kernelPT = kpt

	// Map vectors page (IPA 0) into kernel PT at upper-half VA.
	if err := kpt.mapPage(uint64(kernelVABase), 0, false); err != nil {
		return nil, fmt.Errorf("mapping vectors in kernel PT: %w", err)
	}

	// Allocate and map the EL1 dispatch code page.
	var dispMem unsafe.Pointer
	if ret := C.posix_memalign(&dispMem, C.size_t(16384), C.size_t(16384)); ret != 0 {
		return nil, fmt.Errorf("allocating dispatch page")
	}
	C.memset(dispMem, 0, C.size_t(16384))
	dispIPA, err := m.ipaAlloc.mapPage(uintptr(dispMem), 16384)
	if err != nil {
		C.free(dispMem)
		return nil, fmt.Errorf("mapping dispatch page IPA: %w", err)
	}
	dispKVA := uint64(kernelVABase) + 0x100000 // 1MB above base, before state pages
	if err := kpt.mapPage(dispKVA, dispIPA, false /* read-only exec */); err != nil {
		C.free(dispMem)
		return nil, fmt.Errorf("mapping dispatch in kernel PT: %w", err)
	}
	m.dispatchMem = dispMem
	m.dispatchIPA = dispIPA
	m.dispatchKVA = dispKVA

	if err := m.buildDispatchCode(); err != nil {
		return nil, fmt.Errorf("building dispatch code: %w", err)
	}

	return m, nil
}

// createVCPU creates a new HVF vCPU on the current OS thread.
//
// Precondition: runtime.LockOSThread() must have been called.
func (m *machine) createVCPU(id int) (*vCPU, error) {
	var vcpuID C.hv_vcpu_t
	var exit *C.hv_vcpu_exit_t

	ret := C.hv_vcpu_create(&vcpuID, &exit, nil)
	if ret != C.HV_SUCCESS {
		return nil, fmt.Errorf("hv_vcpu_create failed: %d", ret)
	}

	// Allocate per-vCPU state page for in-VM register save/restore.
	var stateMem unsafe.Pointer
	if ret := C.posix_memalign(&stateMem, C.size_t(16384), C.size_t(16384)); ret != 0 || stateMem == nil {
		C.hv_vcpu_destroy(vcpuID)
		return nil, fmt.Errorf("posix_memalign failed for state page")
	}
	C.memset(stateMem, 0, C.size_t(16384))

	// Map state page into HVF IPA space.
	stateIPA, err := m.ipaAlloc.mapPage(uintptr(stateMem), 16384)
	if err != nil {
		C.free(stateMem)
		C.hv_vcpu_destroy(vcpuID)
		return nil, fmt.Errorf("mapping state page IPA: %w", err)
	}

	// Map state page into TTBR1 kernel page table for EL1 access.
	stateVA := statePageVABase + uint64(id)*16384
	if err := m.kernelPT.mapPage(stateVA, stateIPA, true /* writable */); err != nil {
		m.ipaAlloc.unmapIPA(stateIPA)
		C.free(stateMem)
		C.hv_vcpu_destroy(vcpuID)
		return nil, fmt.Errorf("mapping state page in kernel PT: %w", err)
	}

	c := &vCPU{
		id:            id,
		vcpuID:        vcpuID,
		exit:          exit,
		tid:           hosttid.Current(),
		machine:       m,
		statePageHost: stateMem,
		statePageVA:   stateVA,
	}

	if err := c.initialize(); err != nil {
		m.ipaAlloc.unmapIPA(stateIPA)
		C.free(stateMem)
		C.hv_vcpu_destroy(vcpuID)
		return nil, err
	}

	return c, nil
}

// Get acquires a vCPU for the current thread.
//
// HVF vCPUs are bound to the creating thread, so Get either returns
// an existing vCPU for the current thread or creates a new one.
// The caller must call Put when done.
func (m *machine) Get() *vCPU {
	runtime.LockOSThread()
	tid := hosttid.Current()

	m.mu.Lock()

	// Check if this thread already has a vCPU.
	for _, c := range m.vcpus {
		if c.tid == tid {
			m.mu.Unlock()
			return c
		}
	}

	// Create a new vCPU on this thread (HVF binds vCPUs to threads).
	// Hold the lock across createVCPU to prevent concurrent goroutines
	// from assigning duplicate IDs or racing on the vcpus slice.
	id := len(m.vcpus)

	c, err := m.createVCPU(id)
	if err != nil {
		m.mu.Unlock()
		runtime.UnlockOSThread()
		// Matches KVM platform behavior: vCPU creation failure is fatal.
		panic(fmt.Sprintf("failed to create vCPU: %v", err))
	}

	m.vcpus = append(m.vcpus, c)
	m.mu.Unlock()

	return c
}

// Put releases a vCPU back to the pool.
//
// The vCPU remains associated with the current thread for reuse.
func (m *machine) Put(_ *vCPU) {
	runtime.UnlockOSThread()
}
