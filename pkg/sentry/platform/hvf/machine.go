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
#include <pthread.h>
#include <signal.h>
#include <stdlib.h>
#include <string.h>

// emptyHandler is a no-op signal handler. Installing it (vs SIG_IGN)
// ensures the signal interrupts blocking syscalls like hv_vcpu_run.
static void emptyHandler(int sig) { (void)sig; }

// installSIGUSR1 installs the empty handler for SIGUSR1.
static inline void installSIGUSR1(void) {
    struct sigaction sa;
    sa.sa_handler = emptyHandler;
    sa.sa_flags = 0;
    sigemptyset(&sa.sa_mask);
    sigaction(SIGUSR1, &sa, NULL);
}

// signalThread sends SIGUSR1 to a specific pthread.
static inline void signalThread(pthread_t t) {
    pthread_kill(t, SIGUSR1);
}
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
type vCPU struct {
	id          int
	vcpuID      C.hv_vcpu_t       // HVF vCPU handle
	exit        *C.hv_vcpu_exit_t // Exit information (mapped by HVF)
	tid         uint64            // OS thread ID that owns this vCPU
	pthread     C.pthread_t       // pthread handle for signal delivery
	machine     *machine          // Parent machine (shared resources)
	asidCounter uint32            // Incrementing ASID for TLB invalidation
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

	// kernelPT is the shared kernel page table for TTBR1_EL1.
	// It maps upper-half VAs for the sentry (Go heap, stacks).
	// Shared across all vCPUs.
	kernelPT *kernelPageTable

	// sentryPT is the 4-level page table for bluepill tests.
	// Not used in production Switch() — TCR swap races with fork.
	sentryPT *sentryPageTable
}

func newMachine() (*machine, error) {
	// Install SIGUSR1 handler for synchronous vCPU interruption.
	C.installSIGUSR1()

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
	// This allows the ring0 entry stub to execute from TTBR1 while
	// changing TTBR0/TCR — avoids page table format mismatch.
	if err := kpt.mapPage(uint64(kernelVABase), 0, false); err != nil {
		return nil, fmt.Errorf("mapping vectors in kernel PT: %w", err)
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

	c := &vCPU{
		id:      id,
		vcpuID:  vcpuID,
		exit:    exit,
		tid:     hosttid.Current(),
		pthread: C.pthread_self(),
		machine: m,
	}

	// Initialize vCPU state (system registers, etc.)
	if err := c.initialize(); err != nil {
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
	id := len(m.vcpus)
	m.mu.Unlock()

	c, err := m.createVCPU(id)
	if err != nil {
		runtime.UnlockOSThread()
		panic(fmt.Sprintf("failed to create vCPU: %v", err))
	}

	m.mu.Lock()
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
