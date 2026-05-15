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
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/unix"

	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/safemem"
)

// UseMachMemory controls whether MemoryFile chunks are backed by Mach
// anonymous memory entries instead of file-backed MAP_SHARED mmap.
//
// When enabled, mmapChunks uses mach_make_memory_entry_64 with
// MAP_MEM_NAMED_CREATE to create anonymous VM objects. These pages
// should have stable physical addresses because they aren't subject
// to file-cache eviction/reload, potentially fixing the sequential
// exec crash where macOS silently relocates physical pages.
//
// EXPERIMENTAL: Set to 1 via SetUseMachMemory before MemoryFile init.
var useMachMemory atomic.Int32

// SetUseMachMemory enables or disables Mach anonymous memory backing.
func SetUseMachMemory(enabled bool) {
	if enabled {
		useMachMemory.Store(1)
	} else {
		useMachMemory.Store(0)
	}
}

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

// fallocateDecommit deallocates a range of the file using F_PUNCHHOLE
// on macOS. This is equivalent to Linux's fallocate(PUNCH_HOLE) —
// it zeroes the region and releases the underlying storage. Available
// on APFS since macOS 10.12.
func fallocateDecommit(fd int, off, length int64) error {
	type fpunchhole struct {
		Flags    uint32
		Reserved uint32
		Offset   int64
		Length   int64
	}
	ph := fpunchhole{Offset: off, Length: length}
	_, _, errno := unix.Syscall(unix.SYS_FCNTL, uintptr(fd), 99, uintptr(unsafe.Pointer(&ph)))
	if errno != 0 {
		return errno
	}
	return nil
}

// mmapChunks maps MemoryFile chunks.
//
// When useMachMemory is enabled, chunks are backed by Mach anonymous
// memory entries (mach_make_memory_entry_64) instead of file-backed
// MAP_SHARED. This is an experimental fix for the sequential exec
// crash where macOS silently relocates physical pages backing
// file-mapped memory without notifying HVF's stage-2 tables.
//
// When disabled (default), uses MAP_SHARED for file-backed mapping.
func mmapChunks(fd uintptr, size, offset uintptr) (uintptr, error) {
	if useMachMemory.Load() != 0 {
		ptr, err := MachAllocAnonymous(size)
		if err != nil {
			log.Warningf("MachAllocAnonymous(%d) failed, falling back to MAP_SHARED: %v", size, err)
		} else {
			addr := uintptr(ptr)
			// Copy existing file content into the anonymous mapping
			// so that pages allocated before this chunk still work.
			if offset > 0 || size > 0 {
				// Read file content into the anonymous memory.
				buf := unsafe.Slice((*byte)(unsafe.Pointer(addr)), size)
				unix.Pread(int(fd), buf, int64(offset))
			}
			return addr, nil
		}
	}

	// Default: file-backed MAP_SHARED.
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

// IsSentryOwned marks MemoryFile as sentry-owned memory. The HVF platform
// uses this to distinguish MemoryFile pages (which the sentry writes to
// directly) from file-backed pages (which must be shadow-copied to prevent
// macOS from relocating their physical backing).
func (f *MemoryFile) IsSentryOwned() {}
