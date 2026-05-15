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

package gofer

import (
	"golang.org/x/sys/unix"
)

// statxSizeFast uses statx(2) to get just the file size efficiently.
func statxSizeFast(fd int) (uint64, error) {
	var stat unix.Statx_t
	err := unix.Statx(fd, "", unix.AT_EMPTY_PATH, unix.STATX_SIZE, &stat)
	if err != nil {
		return 0, err
	}
	return stat.Size, nil
}

// dupFD duplicates a file descriptor to a specific number using dup3.
func dupFD(oldfd, newfd, flags int) error {
	return unix.Dup3(oldfd, newfd, flags)
}

// fallocateFile pre-allocates space using fallocate(2).
func fallocateFile(fd int, mode uint32, off, length int64) error {
	return unix.Fallocate(fd, mode, off, length)
}

// teeFile copies data between file descriptors using tee(2).
func teeFile(rfd, wfd int, len int, flags int) (int64, error) {
	return unix.Tee(rfd, wfd, len, flags)
}

// statDevMinor extracts the minor device number.
func goferStatDevMinor(dev uint64) uint32 {
	return unix.Minor(dev)
}

// statDevMajor extracts the major device number.
func goferStatDevMajor(dev uint64) uint32 {
	return unix.Major(dev)
}
