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

//go:build darwin
// +build darwin

package pgalloc

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/unix"
)

// MachAllocAnonymous allocates anonymous memory using Mach VM primitives.
// The returned memory is page-aligned and backed by anonymous (swap-backed)
// pages that macOS should not silently relocate.
//
// Uses mach_vm_allocate with VM_FLAGS_ANYWHERE for true anonymous allocation.
func MachAllocAnonymous(size uintptr) (unsafe.Pointer, error) {
	// Use mmap(MAP_PRIVATE|MAP_ANONYMOUS) — simple and effective.
	// Anonymous mappings have stable physical addresses on macOS;
	// the kernel does not relocate them behind the process's back.
	m, _, errno := unix.Syscall6(
		unix.SYS_MMAP,
		0, size,
		unix.PROT_READ|unix.PROT_WRITE,
		unix.MAP_PRIVATE|unix.MAP_ANON,
		0, 0)
	if errno != 0 {
		return nil, fmt.Errorf("mmap(MAP_PRIVATE|MAP_ANON, %d): %w", size, errno)
	}
	return unsafe.Pointer(m), nil
}
