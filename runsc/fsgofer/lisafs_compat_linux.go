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

package fsgofer

import (
	"fmt"
	"strconv"

	"golang.org/x/sys/unix"
	rwfd "gvisor.dev/gvisor/pkg/fd"
	"gvisor.dev/gvisor/pkg/lisafs"
)

// STATX_* constants - aliases for the unix package constants on Linux.
const (
	_STATX_MODE  = unix.STATX_MODE
	_STATX_SIZE  = unix.STATX_SIZE
	_STATX_ATIME = unix.STATX_ATIME
	_STATX_MTIME = unix.STATX_MTIME
	_STATX_UID   = unix.STATX_UID
	_STATX_GID   = unix.STATX_GID
)

// utimeOmit is the sentinel value for not modifying a timestamp.
const utimeOmit = int64(unix.UTIME_OMIT)

// mknodat creates a filesystem node.
func mknodat(dirfd int, name string, mode uint32, dev int) error {
	return unix.Mknodat(dirfd, name, mode, dev)
}

// statDevMinor extracts the minor device number.
func statDevMinor(dev uint64) uint32 {
	return unix.Minor(dev)
}

// statDevMajor extracts the major device number.
func statDevMajor(dev uint64) uint32 {
	return unix.Major(dev)
}

var procSelfFD *rwfd.FD

// initProcSelfFD opens /proc/self/fd for FD reopening on Linux.
func initProcSelfFD() error {
	d, err := unix.Open("/proc/self/fd", unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		return fmt.Errorf("error opening /proc/self/fd: %v", err)
	}
	procSelfFD = rwfd.New(d)
	return nil
}

// reopenFD reopens a file descriptor with new flags via /proc/self/fd.
func reopenFD(hostFD int, flags int) (int, error) {
	return unix.Openat(int(procSelfFD.FD()), strconv.Itoa(hostFD), flags, 0)
}

// openParentDir opens the parent directory of filePath with O_PATH.
func openParentDir(filePath string) (int, error) {
	return unix.Open(filePath, openFlags|unix.O_PATH, 0)
}

// readlinkatFD reads the symlink target via readlinkat with empty name.
func readlinkatFD(hostFD int, buf []byte) (int, error) {
	return unix.Readlinkat(hostFD, "", buf)
}

// fchownFD changes ownership of a file descriptor.
func fchownFD(hostFD int, uid, gid int) error {
	return unix.Fchownat(hostFD, "", uid, gid, unix.AT_EMPTY_PATH|unix.AT_SYMLINK_NOFOLLOW)
}

// getdentsFD reads directory entries from a file descriptor.
func getdentsFD(fd int, buf []byte) (int, error) {
	return unix.Getdents(fd, buf)
}

// fallocateFD pre-allocates space on a file descriptor.
func fallocateFD(fd int, mode uint32, off, length int64) error {
	return unix.Fallocate(fd, mode, off, length)
}

// donateFD duplicates an FD for donation to the sentry.
func donateFD(fd int) (int, error) {
	return unix.Dup(fd)
}

// accept4FD accepts a connection with flags.
func accept4FD(fd int, flags int) (int, error) {
	nfd, _, err := unix.Accept4(fd, flags)
	return nfd, err
}

// tryOpenFallbackFlags returns platform-specific fallback open flags.
func tryOpenFallbackFlags() []int {
	return []int{unix.O_PATH}
}

// fstatToStatx converts an Fstat result to lisafs.Statx.
func fstatToStatx(hostFD int) (lisafs.Statx, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(hostFD, &stat); err != nil {
		return lisafs.Statx{}, err
	}

	return lisafs.Statx{
		Mask:      unix.STATX_TYPE | unix.STATX_MODE | unix.STATX_INO | unix.STATX_NLINK | unix.STATX_UID | unix.STATX_GID | unix.STATX_SIZE | unix.STATX_BLOCKS | unix.STATX_ATIME | unix.STATX_MTIME | unix.STATX_CTIME,
		Mode:      uint16(stat.Mode),
		DevMinor:  unix.Minor(stat.Dev),
		DevMajor:  unix.Major(stat.Dev),
		Ino:       stat.Ino,
		Nlink:     uint32(stat.Nlink),
		UID:       stat.Uid,
		GID:       stat.Gid,
		RdevMinor: unix.Minor(stat.Rdev),
		RdevMajor: unix.Major(stat.Rdev),
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
// On Linux this is not normally needed because O_PATH|O_NOFOLLOW works for
// symlinks, but it's provided for compatibility with the cross-platform
// WalkStat code.
func walkStatAt(dirfd int, name string) (lisafs.Statx, error) {
	stat, err := fsutil.StatAt(dirfd, name)
	if err != nil {
		return lisafs.Statx{}, err
	}
	return fstatToStatxFromStat(&stat)
}

func fstatToStatxFromStat(stat *unix.Stat_t) (lisafs.Statx, error) {
	return lisafs.Statx{
		Mask:      unix.STATX_TYPE | unix.STATX_MODE | unix.STATX_INO | unix.STATX_NLINK | unix.STATX_UID | unix.STATX_GID | unix.STATX_SIZE | unix.STATX_BLOCKS | unix.STATX_ATIME | unix.STATX_MTIME | unix.STATX_CTIME,
		Mode:      uint16(stat.Mode),
		DevMinor:  unix.Minor(stat.Dev),
		DevMajor:  unix.Major(stat.Dev),
		Ino:       stat.Ino,
		Nlink:     uint32(stat.Nlink),
		UID:       stat.Uid,
		GID:       stat.Gid,
		RdevMinor: unix.Minor(stat.Rdev),
		RdevMajor: unix.Major(stat.Rdev),
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

// statfsNameLen returns the Namelen field from Statfs_t.
func statfsNameLen(s *unix.Statfs_t) uint64 {
	return uint64(s.Namelen)
}
