// Copyright 2020 The gVisor Authors.
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

package hostfd

import (
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	sizeofIovec  = unsafe.Sizeof(unix.Iovec{})
	sizeofMsghdr = unsafe.Sizeof(unix.Msghdr{})
)

// iovecsReadWrite performs a multi-iovec read or write using sequential
// pread/pwrite calls, since macOS does not have preadv2/pwritev2.
func iovecsReadWrite(read bool, fd int32, iovs []unix.Iovec, offset int64) (uintptr, unix.Errno) {
	var total uintptr
	for start := 0; start < len(iovs); start += MaxReadWriteIov {
		size := len(iovs) - start
		if size > MaxReadWriteIov {
			size = MaxReadWriteIov
		}
		for i := 0; i < size; i++ {
			iov := &iovs[start+i]
			curOff := offset
			if offset >= 0 {
				curOff = offset + int64(total)
			}
			var (
				cur uintptr
				e   unix.Errno
			)
			if read {
				if curOff == -1 {
					cur, _, e = unix.Syscall(unix.SYS_READ, uintptr(fd), uintptr(unsafe.Pointer(iov.Base)), uintptr(iov.Len))
				} else {
					cur, _, e = unix.Syscall6(unix.SYS_PREAD, uintptr(fd), uintptr(unsafe.Pointer(iov.Base)), uintptr(iov.Len), uintptr(curOff), 0, 0)
				}
			} else {
				if curOff == -1 {
					cur, _, e = unix.Syscall(unix.SYS_WRITE, uintptr(fd), uintptr(unsafe.Pointer(iov.Base)), uintptr(iov.Len))
				} else {
					cur, _, e = unix.Syscall6(unix.SYS_PWRITE, uintptr(fd), uintptr(unsafe.Pointer(iov.Base)), uintptr(iov.Len), uintptr(curOff), 0, 0)
				}
			}
			if cur > 0 {
				total += cur
			}
			if e != 0 {
				return total, e
			}
			// Short read/write: stop early.
			if uint64(cur) < iov.Len {
				return total, 0
			}
		}
	}
	return total, 0
}
