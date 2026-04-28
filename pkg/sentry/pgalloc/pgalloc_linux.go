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

//go:build linux
// +build linux

package pgalloc

import (
	"golang.org/x/sys/unix"

	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/safemem"
)

func madviseHugepage(addr, length uintptr) {
	_, _, errno := unix.Syscall(unix.SYS_MADVISE, addr, length, unix.MADV_HUGEPAGE)
	if errno != 0 {
		log.Warningf("madvise(%#x, %d, MADV_HUGEPAGE) failed: %s", addr, length, errno)
	}
}

func madviseNohugepage(addr, length uintptr) {
	_, _, errno := unix.Syscall(unix.SYS_MADVISE, addr, length, unix.MADV_NOHUGEPAGE)
	if errno != 0 {
		log.Warningf("madvise(%#x, %d, MADV_NOHUGEPAGE) failed: %s", addr, length, errno)
	}
}

func madvisePopulateWrite(b safemem.Block) unix.Errno {
	_, _, errno := unix.Syscall(unix.SYS_MADVISE, b.Addr(), uintptr(b.Len()), unix.MADV_POPULATE_WRITE)
	return errno
}

func fallocateCommit(fd int, off, length int64) error {
	return unix.Fallocate(fd, 0, off, length)
}

func fallocateDecommit(fd int, off, length int64) error {
	return unix.Fallocate(fd, unix.FALLOC_FL_PUNCH_HOLE|unix.FALLOC_FL_KEEP_SIZE, off, length)
}

// mmapChunks maps MemoryFile chunks using MAP_SHARED on the backing file.
func mmapChunks(fd uintptr, size, offset uintptr) (uintptr, error) {
	m, _, errno := unix.Syscall6(
		unix.SYS_MMAP,
		0,
		size,
		unix.PROT_READ|unix.PROT_WRITE,
		unix.MAP_SHARED,
		fd,
		offset)
	if errno != 0 {
		return 0, errno
	}
	return m, nil
}