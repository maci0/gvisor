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

package host

import (
	"sync"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/log"
)

func isEpollable(fd int) bool {
	epollfd, err := unix.EpollCreate1(0)
	if err != nil {
		return false
	}
	defer unix.Close(epollfd)

	event := unix.EpollEvent{
		Fd:     int32(fd),
		Events: unix.EPOLLIN,
	}
	err = unix.EpollCtl(epollfd, unix.EPOLL_CTL_ADD, fd, &event)
	return err == nil
}

func unixToLinuxStatxTimestamp(ts unix.StatxTimestamp) linux.StatxTimestamp {
	return linux.StatxTimestamp{Sec: ts.Sec, Nsec: ts.Nsec}
}

// statxHost calls statx(2) on the host.
func statxHost(hostFD int, sync uint32, mask uint32) (linux.Statx, bool, error) {
	var s unix.Statx_t
	err := unix.Statx(hostFD, "", int(unix.AT_EMPTY_PATH|sync), int(mask), &s)
	if err != nil {
		return linux.Statx{}, false, err
	}
	ls := linux.Statx{
		Mask:           s.Mask,
		Blksize:        s.Blksize,
		Attributes:     s.Attributes,
		Nlink:          s.Nlink,
		UID:            s.Uid,
		GID:            s.Gid,
		Mode:           uint16(s.Mode),
		Size:           s.Size,
		Blocks:         s.Blocks,
		AttributesMask: s.Attributes_mask,
		Atime:          unixToLinuxStatxTimestamp(s.Atime),
		Btime:          unixToLinuxStatxTimestamp(s.Btime),
		Ctime:          unixToLinuxStatxTimestamp(s.Ctime),
		Mtime:          unixToLinuxStatxTimestamp(s.Mtime),
		RdevMajor:      s.Rdev_major,
		RdevMinor:      s.Rdev_minor,
		DevMajor:       s.Dev_major,
		DevMinor:       s.Dev_minor,
	}
	return ls, true, nil
}

var hostEventFdDevice uint64
var hostEventFdDeviceOnce sync.Once

func isHostEventFdDevice(dev uint64) bool {
	hostEventFdDeviceOnce.Do(func() {
		efd, _, err := unix.RawSyscall(unix.SYS_EVENTFD2, 0, 0, 0)
		if err != 0 {
			log.Warningf("failed to create dummy eventfd: %v. Importing eventfds will fail", error(err))
			return
		}
		defer unix.Close(int(efd))
		var stat unix.Stat_t
		if err := unix.Fstat(int(efd), &stat); err != nil {
			log.Warningf("failed to stat dummy eventfd: %v. Importing eventfds will fail", error(err))
			return
		}
		hostEventFdDevice = stat.Dev
	})
	return dev == hostEventFdDevice
}

// hostStatMode converts a uint32 mode to the host Stat_t.Mode type.
func hostStatMode(mode uint32) uint32 { return mode }

func fallocateHost(fd int, mode uint32, off int64, len int64) error {
	return unix.Fallocate(fd, mode, off, len)
}
