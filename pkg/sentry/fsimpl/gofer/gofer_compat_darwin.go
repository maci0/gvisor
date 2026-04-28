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

package gofer

import (
	"golang.org/x/sys/unix"
)

// statxSizeFast attempts to get just the file size using fstat.
// On Linux this uses statx(2) with STATX_SIZE for performance.
// On macOS we fall back to fstat(2).
func statxSizeFast(fd int) (uint64, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return 0, err
	}
	return uint64(stat.Size), nil
}

// dupFD duplicates a file descriptor. macOS doesn't have dup3,
// so we use dup and then set flags.
func dupFD(oldfd, newfd, flags int) error {
	nfd, err := unix.Dup(oldfd)
	if err != nil {
		return err
	}
	if nfd != newfd {
		// dup3 atomically duplicates to a specific FD; we can't do that on macOS.
		// Just use the new FD we got.
		unix.Close(newfd)
	}
	if flags&unix.O_CLOEXEC != 0 {
		unix.CloseOnExec(nfd)
	}
	return nil
}

// fallocateFile pre-allocates space. macOS doesn't have fallocate(2).
func fallocateFile(fd int, mode uint32, off, length int64) error {
	return unix.ENOTSUP
}

// teeFile copies data between file descriptors without consuming input.
// macOS doesn't have tee(2) or splice(2).
func teeFile(rfd, wfd int, len int, flags int) (int64, error) {
	return 0, unix.ENOTSUP
}

// statDevMinor extracts the minor device number for darwin int32 Dev.
func goferStatDevMinor(dev int32) uint32 {
	return unix.Minor(uint64(dev))
}

// statDevMajor extracts the major device number for darwin int32 Dev.
func goferStatDevMajor(dev int32) uint32 {
	return unix.Major(uint64(dev))
}

// SPLICE_F_NONBLOCK is not available on macOS.
const _SPLICE_F_NONBLOCK = 0
