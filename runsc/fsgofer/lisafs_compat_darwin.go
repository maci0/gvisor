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

package fsgofer

import (
	"unsafe"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/lisafs"
)

// STATX_* constants used in SetStat masks. These are protocol-level
// constants with the same values as Linux, not macOS syscall constants.
const (
	_STATX_MODE  = 0x2
	_STATX_SIZE  = 0x200
	_STATX_ATIME = 0x20
	_STATX_MTIME = 0x40
	_STATX_UID   = 0x8
	_STATX_GID   = 0x10
)

// UTIME_OMIT is not defined on macOS. Use the Linux value since it's
// only used as a sentinel in Timespec.Nsec, not passed to macOS syscalls.
const utimeOmit = int64(((1 << 30) - 2))

// mknodat creates a filesystem node. macOS doesn't have Mknodat in x/sys/unix.
func mknodat(dirfd int, name string, mode uint32, dev int) error {
	// For regular files (the main use case), use open+close instead.
	if mode&unix.S_IFMT == unix.S_IFREG {
		fd, err := unix.Openat(dirfd, name, unix.O_CREAT|unix.O_EXCL|unix.O_WRONLY|unix.O_CLOEXEC, mode&^unix.S_IFMT)
		if err != nil {
			return err
		}
		unix.Close(fd)
		return nil
	}
	return unix.ENOTSUP
}

// statDevMinor extracts the minor device number, handling darwin int32 Dev.
func statDevMinor(dev int32) uint32 {
	return unix.Minor(uint64(dev))
}

// statDevMajor extracts the major device number, handling darwin int32 Dev.
func statDevMajor(dev int32) uint32 {
	return unix.Major(uint64(dev))
}

// initProcSelfFD is a no-op on darwin since /proc/self/fd doesn't exist.
// FD reopening uses file paths instead.
func initProcSelfFD() error {
	return nil
}

// darwinOpenMask contains only the open flags valid on macOS.
// Linux-only flags like O_LARGEFILE (0x20000 on Linux, but O_EVTONLY
// on macOS) must be stripped to avoid unexpected behavior.
const darwinOpenMask = unix.O_RDONLY | unix.O_WRONLY | unix.O_RDWR |
	unix.O_NONBLOCK | unix.O_APPEND | unix.O_CREAT | unix.O_TRUNC |
	unix.O_EXCL | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC

// reopenFD reopens a file descriptor with new flags.
// macOS doesn't have /proc/self/fd. We use fcntl(F_GETPATH) + open.
// Linux-only flags are stripped before opening.
func reopenFD(hostFD int, flags int) (int, error) {
	flags &= darwinOpenMask
	// Try to get the path and reopen.
	path, err := fcntlGetpath(hostFD)
	if err == nil {
		newFD, err := unix.Open(path, flags, 0)
		if err == nil {
			return newFD, nil
		}
	}
	// Fallback: just dup the FD.
	return unix.Dup(hostFD)
}

// openParentDir opens the parent directory of filePath.
// macOS doesn't support O_PATH, so we use O_RDONLY.
func openParentDir(filePath string) (int, error) {
	return unix.Open(filePath, openFlags|unix.O_RDONLY, 0)
}

// readlinkatFD reads the symlink target. On macOS, readlinkat doesn't
// support empty name, so we use fcntl F_GETPATH to get the path and
// then readlink it.
func readlinkatFD(hostFD int, buf []byte) (int, error) {
	// Get the file path from the FD.
	path, err := fcntlGetpath(hostFD)
	if err != nil {
		return 0, err
	}
	return unix.Readlink(path, buf)
}

// fcntlGetpath uses F_GETPATH to get the file path from an FD.
func fcntlGetpath(fd int) (string, error) {
	var buf [1024]byte
	_, _, errno := unix.Syscall(unix.SYS_FCNTL, uintptr(fd), uintptr(unix.F_GETPATH), uintptr(unsafe.Pointer(&buf[0])))
	if errno != 0 {
		return "", errno
	}
	// Find null terminator.
	for i, b := range buf {
		if b == 0 {
			return string(buf[:i]), nil
		}
	}
	return string(buf[:]), nil
}

// fchownFD changes ownership of a file descriptor.
// macOS doesn't support AT_EMPTY_PATH, so we use fchown directly.
func fchownFD(hostFD int, uid, gid int) error {
	return unix.Fchown(hostFD, uid, gid)
}

// getdentsFD reads directory entries using Getdirentries on macOS.
func getdentsFD(fd int, buf []byte) (int, error) {
	var basep uintptr
	n, err := unix.Getdirentries(fd, buf, &basep)
	return n, err
}

// donateFD skips FD donation on macOS. The fdchannel's SOCK_STREAM
// SendFD hangs when donating via flipcall channels, causing the
// gofer to deadlock. Without FD donation, Translate uses the page
// cache path (slower but correct).
func donateFD(_ int) (int, error) {
	return -1, nil
}

// fallocateFD is not supported on macOS. Return EOPNOTSUPP.
func fallocateFD(fd int, mode uint32, off, length int64) error {
	return unix.ENOTSUP
}

// accept4FD accepts a connection. macOS doesn't have accept4,
// so we use accept + fcntl.
func accept4FD(fd int, flags int) (int, error) {
	nfd, _, err := unix.Accept(fd)
	if err != nil {
		return -1, err
	}
	// Set flags.
	if flags&unix.O_CLOEXEC != 0 {
		unix.CloseOnExec(nfd)
	}
	if flags&unix.O_NONBLOCK != 0 {
		if err := unix.SetNonblock(nfd, true); err != nil {
			unix.Close(nfd)
			return -1, err
		}
	}
	return nfd, nil
}

// tryOpenFallbackFlags returns platform-specific fallback open flags.
// macOS doesn't have O_PATH, so we try O_RDONLY for symlinks too.
func tryOpenFallbackFlags() []int {
	// On macOS, O_SYMLINK lets us open a symlink itself.
	return []int{unix.O_SYMLINK | unix.O_NOFOLLOW}
}

// fstatToStatx converts an Fstat result to lisafs.Statx on darwin.
func fstatToStatx(hostFD int) (lisafs.Statx, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(hostFD, &stat); err != nil {
		return lisafs.Statx{}, err
	}

	// Darwin Stat_t has different field types than Linux.
	return lisafs.Statx{
		Mask:      0x17ff, // All fields valid (matches Linux STATX_BASIC_STATS)
		Mode:      stat.Mode,
		DevMinor:  unix.Minor(uint64(stat.Dev)),
		DevMajor:  unix.Major(uint64(stat.Dev)),
		Ino:       stat.Ino,
		Nlink:     uint32(stat.Nlink),
		UID:       stat.Uid,
		GID:       stat.Gid,
		RdevMinor: unix.Minor(uint64(stat.Rdev)),
		RdevMajor: unix.Major(uint64(stat.Rdev)),
		Size:      uint64(stat.Size),
		Blksize:   uint32(stat.Blksize),
		Blocks:    uint64(stat.Blocks),
		Atime: lisafs.StatxTimestamp{
			Sec:  stat.Atim.Sec,
			Nsec: uint32(stat.Atim.Nsec),
		},
		Mtime: lisafs.StatxTimestamp{
			Sec:  stat.Mtim.Sec,
			Nsec: uint32(stat.Mtim.Nsec),
		},
		Ctime: lisafs.StatxTimestamp{
			Sec:  stat.Ctim.Sec,
			Nsec: uint32(stat.Ctim.Nsec),
		},
	}, nil
}

// walkStatAt stats a file relative to dirfd without following symlinks.
// Used when O_NOFOLLOW returns ELOOP for symlinks on macOS.
// Note: we use unix.Fstatat instead of fsutil.StatAt because the raw
// syscall in fsutil returns incorrect Stat_t on macOS ARM64.
func walkStatAt(dirfd int, name string) (lisafs.Statx, error) {
	var stat unix.Stat_t
	err := unix.Fstatat(dirfd, name, &stat, unix.AT_SYMLINK_NOFOLLOW)
	if err != nil {
		return lisafs.Statx{}, err
	}
	return lisafs.Statx{
		Mask:      0x17ff,
		Mode:      stat.Mode,
		DevMinor:  unix.Minor(uint64(stat.Dev)),
		DevMajor:  unix.Major(uint64(stat.Dev)),
		Ino:       stat.Ino,
		Nlink:     uint32(stat.Nlink),
		UID:       stat.Uid,
		GID:       stat.Gid,
		RdevMinor: unix.Minor(uint64(stat.Rdev)),
		RdevMajor: unix.Major(uint64(stat.Rdev)),
		Size:      uint64(stat.Size),
		Blksize:   uint32(stat.Blksize),
		Blocks:    uint64(stat.Blocks),
		Atime: lisafs.StatxTimestamp{
			Sec:  stat.Atim.Sec,
			Nsec: uint32(stat.Atim.Nsec),
		},
		Mtime: lisafs.StatxTimestamp{
			Sec:  stat.Mtim.Sec,
			Nsec: uint32(stat.Mtim.Nsec),
		},
		Ctime: lisafs.StatxTimestamp{
			Sec:  stat.Ctim.Sec,
			Nsec: uint32(stat.Ctim.Nsec),
		},
	}, nil
}

// statfsNameLen returns a reasonable name length for macOS.
// Darwin's Statfs_t doesn't have a Namelen field.
func statfsNameLen(_ *unix.Statfs_t) uint64 {
	return 255 // NAME_MAX on macOS
}
