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

package nvproxy

import (
	"golang.org/x/sys/unix"

	"gvisor.dev/gvisor/pkg/abi/linux"
)

func mremap(oldAddr uintptr, oldSize uintptr, newSize uintptr, flags uintptr, newAddr uintptr) error {
	if _, _, errno := unix.RawSyscall6(unix.SYS_MREMAP, oldAddr, oldSize, newSize, flags, newAddr, 0); errno != 0 {
		return errno
	}
	return nil
}

func mremapFlags() uintptr {
	return linux.MREMAP_MAYMOVE | linux.MREMAP_FIXED
}

func madvisePopulateWrite(addr uintptr, length uintptr) error {
	if _, _, errno := unix.Syscall(unix.SYS_MADVISE, addr, length, unix.MADV_POPULATE_WRITE); errno != 0 {
		return errno
	}
	return nil
}
