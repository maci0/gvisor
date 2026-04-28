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

// Command vmm is a host-side Virtual Machine Monitor that creates an HVF VM,
// maps sentry memory, and runs a request loop for host I/O operations. The
// sentry runs at EL1 inside the VM and communicates with the VMM via a shared
// memory page and HVC exits.
//
// Usage:
//
//	vmm [flags] <command> [args...]
package main

/*
#cgo LDFLAGS: -framework Hypervisor
#include <Hypervisor/Hypervisor.h>
#include <stdlib.h>
#include <string.h>
*/
import "C"

import (
	"flag"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/sentry/platform/hvf"
)

var (
	flagRootFS = flag.String("rootfs", "", "path to rootfs directory")
)

// handleHVCExit processes an HVC exit from the sentry. It reads the
// VMMRequest from the shared memory page, executes the requested host
// operation, writes the result, and returns whether the VM should
// continue running.
func handleHVCExit(sharedPage *hvf.VMMRequest) (cont bool) {
	switch sharedPage.Op {
	case hvf.VMMOpSyscall:
		// Proxy a host syscall.
		r, _, e := unix.RawSyscall6(
			uintptr(sharedPage.Args[0]),
			uintptr(sharedPage.Args[1]),
			uintptr(sharedPage.Args[2]),
			uintptr(sharedPage.Args[3]),
			uintptr(sharedPage.Args[4]),
			uintptr(sharedPage.Args[5]),
		)
		sharedPage.Result = int64(r)
		sharedPage.Errno = int64(e)
		sharedPage.Done = 1
		return true

	case hvf.VMMOpMmap:
		// Map host memory into VM via hv_vm_map.
		// Args[0] = host address, Args[1] = guest IPA, Args[2] = size, Args[3] = flags
		hostAddr := unsafe.Pointer(uintptr(sharedPage.Args[0]))
		ipa := C.hv_ipa_t(sharedPage.Args[1])
		size := C.size_t(sharedPage.Args[2])
		flags := C.hv_memory_flags_t(sharedPage.Args[3])
		ret := C.hv_vm_map(hostAddr, ipa, size, flags)
		sharedPage.Result = int64(ret)
		sharedPage.Done = 1
		return true

	case hvf.VMMOpMunmap:
		// Unmap memory from VM.
		ipa := C.hv_ipa_t(sharedPage.Args[0])
		size := C.size_t(sharedPage.Args[1])
		ret := C.hv_vm_unmap(ipa, size)
		sharedPage.Result = int64(ret)
		sharedPage.Done = 1
		return true

	case hvf.VMMOpExit:
		// Sentry is shutting down.
		fmt.Fprintf(os.Stderr, "VMM: sentry exit with code %d\n", sharedPage.Args[0])
		sharedPage.Done = 1
		return false

	default:
		fmt.Fprintf(os.Stderr, "VMM: unknown op %d\n", sharedPage.Op)
		sharedPage.Result = -1
		sharedPage.Errno = int64(unix.ENOSYS)
		sharedPage.Done = 1
		return true
	}
}

// runVCPU runs the main vCPU loop. It enters the VM, handles exits,
// and resumes. This is called on a dedicated OS thread.
func runVCPU(vcpuID C.hv_vcpu_t, sharedPage *hvf.VMMRequest) error {
	for {
		ret := C.hv_vcpu_run(vcpuID)
		if ret != C.HV_SUCCESS {
			return fmt.Errorf("hv_vcpu_run failed: %d", ret)
		}

		// Read exit info from the syndrome register.
		var syndrome C.uint64_t
		C.hv_vcpu_get_sys_reg(vcpuID, C.HV_SYS_REG_ESR_EL2, &syndrome)
		ec := (uint64(syndrome) >> 26) & 0x3f

		if ec == 0x16 { // HVC from AArch64
			if !handleHVCExit(sharedPage) {
				return nil // sentry requested exit
			}
			// Advance PC past the HVC instruction (4 bytes).
			var pc C.uint64_t
			C.hv_vcpu_get_reg(vcpuID, C.HV_REG_PC, &pc)
			C.hv_vcpu_set_reg(vcpuID, C.HV_REG_PC, pc+4)
			continue
		}

		return fmt.Errorf("unexpected exit: EC=%#x syndrome=%#x", ec, syndrome)
	}
}

func main() {
	flag.Parse()

	if flag.NArg() < 1 {
		fmt.Fprintf(os.Stderr, "usage: vmm [flags] <command> [args...]\n")
		os.Exit(1)
	}

	fmt.Fprintf(os.Stderr, "VMM: starting (rootfs=%s, cmd=%s)\n", *flagRootFS, flag.Args())

	// Create HVF VM.
	ret := C.hv_vm_create(nil)
	if ret != C.HV_SUCCESS {
		fmt.Fprintf(os.Stderr, "VMM: hv_vm_create failed: %d\n", ret)
		os.Exit(1)
	}
	defer C.hv_vm_destroy()

	// Allocate and map shared page.
	var sharedMem unsafe.Pointer
	C.posix_memalign(&sharedMem, C.size_t(16384), C.size_t(16384))
	if sharedMem == nil {
		fmt.Fprintf(os.Stderr, "VMM: failed to allocate shared page\n")
		os.Exit(1)
	}
	defer C.free(sharedMem)
	C.memset(sharedMem, 0, C.size_t(16384))

	ret = C.hv_vm_map(sharedMem, C.hv_ipa_t(hvf.VMMSharedPageIPA), C.size_t(16384),
		C.HV_MEMORY_READ|C.HV_MEMORY_WRITE)
	if ret != C.HV_SUCCESS {
		fmt.Fprintf(os.Stderr, "VMM: hv_vm_map shared page failed: %d\n", ret)
		os.Exit(1)
	}

	sharedPage := (*hvf.VMMRequest)(sharedMem)
	fmt.Fprintf(os.Stderr, "VMM: VM created, shared page at IPA %#x\n", hvf.VMMSharedPageIPA)

	// Phase 0: VM setup complete. Sentry loading not yet implemented.
	// The handleHVCExit and runVCPU functions are ready for Phase 2.
	_ = sharedPage
	fmt.Fprintf(os.Stderr, "VMM: sentry loading not yet implemented\n")
	os.Exit(1)
}
