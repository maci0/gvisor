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
	"golang.org/x/sys/unix"

	"gvisor.dev/gvisor/pkg/safemem"
)

// madviseHugepage is a no-op on darwin (no huge page madvise).
func madviseHugepage(addr, length uintptr) {}

// madviseNohugepage is a no-op on darwin.
func madviseNohugepage(addr, length uintptr) {}

// madvisePopulateWrite is not supported on darwin.
func madvisePopulateWrite(b safemem.Block) unix.Errno {
	return unix.ENOSYS
}

// fallocateCommit is a no-op on darwin (fallocate doesn't exist).
// On macOS, file space is allocated on write.
func fallocateCommit(fd int, off, length int64) error {
	return nil
}

// fallocateDecommit returns an error on darwin because macOS does not
// support fallocate with PUNCH_HOLE. Callers fall back to manual zeroing.
func fallocateDecommit(fd int, off, length int64) error {
	return unix.ENOSYS
}

// mmapChunks maps MemoryFile chunks using MAP_SHARED on macOS.
// MAP_SHARED is needed so that pwrite-based sync (syncFromGuest)
// can update the MemoryFile's content for copy-mapped HVF pages.
// Direct-mapped HVF pages (16K-aligned) share these MAP_SHARED
// pages directly with the guest.
func mmapChunks(fd uintptr, size, offset uintptr) (uintptr, error) {
	m, _, errno := unix.Syscall6(
		unix.SYS_MMAP,
		0, size,
		unix.PROT_READ|unix.PROT_WRITE,
		unix.MAP_SHARED, fd, offset)
	if errno != 0 {
		return 0, errno
	}
	return m, nil
}
