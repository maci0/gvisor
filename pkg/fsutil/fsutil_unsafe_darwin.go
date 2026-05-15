// Copyright 2018 The gVisor Authors.
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

package fsutil

import (
	"bytes"
	"fmt"
	"unsafe"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/syserr"
)

// UnixDirentMaxSize is the maximum size of unix.Dirent in bytes.
var UnixDirentMaxSize = int(unsafe.Sizeof(unix.Dirent{}))

// Utimensat is a convenience wrapper to make the utimensat(2) syscall. It
// additionally handles empty name.
//
// On macOS, this uses the utimensat function from x/sys/unix rather than a
// direct syscall, since SYS_UTIMENSAT is not exposed as a constant.
func Utimensat(dirFd int, name string, times [2]unix.Timespec, flags int) error {
	// Use the Go wrapper which handles the syscall properly on darwin.
	if err := unix.UtimesNanoAt(dirFd, name, []unix.Timespec{times[0], times[1]}, flags); err != nil {
		if errno, ok := err.(unix.Errno); ok {
			return syserr.FromHost(errno).ToError()
		}
		return err
	}
	return nil
}

// RenameAt2 is a convenience wrapper to make the renameat syscall.
//
// On macOS, renameat2 does not exist. We use renameat instead.
// Non-zero flags are not supported and will return ENOTSUP.
func RenameAt2(oldDirFD int, oldName string, newDirFD int, newName string, flags uint32) error {
	if flags != 0 {
		return syserr.FromHost(unix.ENOTSUP).ToError()
	}

	var oldNamePtr unsafe.Pointer
	if oldName != "" {
		nameBytes, err := unix.BytePtrFromString(oldName)
		if err != nil {
			return err
		}
		oldNamePtr = unsafe.Pointer(nameBytes)
	}
	var newNamePtr unsafe.Pointer
	if newName != "" {
		nameBytes, err := unix.BytePtrFromString(newName)
		if err != nil {
			return err
		}
		newNamePtr = unsafe.Pointer(nameBytes)
	}

	if _, _, errno := unix.Syscall6(
		unix.SYS_RENAMEAT,
		uintptr(oldDirFD),
		uintptr(oldNamePtr),
		uintptr(newDirFD),
		uintptr(newNamePtr),
		0,
		0); errno != 0 {

		return syserr.FromHost(errno).ToError()
	}
	return nil
}

// ParseDirents parses dirents from buf. buf must have been populated by
// Getdirentries. It calls the handleDirent callback for each dirent.
//
// On macOS, the Dirent struct has Seekoff instead of Off.
func ParseDirents(buf []byte, handleDirent DirentHandler) {
	// On macOS, Dirent has a 1024-byte Name field making sizeof(Dirent)=1048,
	// but actual dirents are much smaller (Reclen gives the true size).
	// We only need enough bytes for the fixed header (up to and including Namlen).
	const minDirentSize = 21 // offsetof(Name) on macOS arm64
	for len(buf) > 0 {
		if len(buf) < minDirentSize {
			break
		}
		// Interpret the buf populated by Getdirentries as unix.Dirent.
		dirent := *(*unix.Dirent)(unsafe.Pointer(&buf[0]))

		if dirent.Reclen == 0 {
			break
		}
		if int(dirent.Reclen) > len(buf) {
			break
		}

		// Extract the name from buf. On macOS, Namlen gives the actual
		// length of the name (no null terminator counting needed), but we
		// still look for null terminator for safety.
		nameBuf := buf[unsafe.Offsetof(dirent.Name):dirent.Reclen]
		nameLen := int(dirent.Namlen)
		if nullIdx := bytes.IndexByte(nameBuf, 0); nullIdx >= 0 && nullIdx < nameLen {
			nameLen = nullIdx
		}
		if nameLen > len(nameBuf) {
			nameLen = len(nameBuf)
		}
		name := string(nameBuf[:nameLen])

		// Advance buf for the next dirent.
		buf = buf[dirent.Reclen:]

		// Skip `.` and `..` entries.
		if name == "." || name == ".." {
			continue
		}

		// On macOS, Dirent has Seekoff instead of Off. Use Seekoff as
		// the offset value.
		handleDirent(dirent.Ino, int64(dirent.Seekoff), dirent.Type, name, dirent.Reclen)
	}
}

// StatAt is a convenience wrapper around fstatat(2).
func StatAt(dirFd int, name string) (unix.Stat_t, error) {
	nameBytes, err := unix.BytePtrFromString(name)
	if err != nil {
		return unix.Stat_t{}, err
	}
	namePtr := unsafe.Pointer(nameBytes)

	var stat unix.Stat_t
	statPtr := unsafe.Pointer(&stat)

	if _, _, errno := unix.Syscall6(
		unix.SYS_FSTATAT,
		uintptr(dirFd),
		uintptr(namePtr),
		uintptr(statPtr),
		unix.AT_SYMLINK_NOFOLLOW,
		0,
		0); errno != 0 {

		return unix.Stat_t{}, syserr.FromHost(errno).ToError()
	}
	return stat, nil
}

// init performs a sanity check on Dirent name field type.
func init() {
	// The Name field on macOS Dirent is [1024]int8, verify our offset calculation
	// is correct at startup.
	var d unix.Dirent
	_ = unsafe.Offsetof(d.Name)
	if unsafe.Sizeof(d) == 0 {
		panic(fmt.Sprintf("unexpected Dirent size: %d", unsafe.Sizeof(d)))
	}
}
