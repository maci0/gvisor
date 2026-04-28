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

package host

import (
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
)

// isEpollable checks if the fd supports kqueue (all fds on macOS do).
func isEpollable(fd int) bool {
	kq, err := unix.Kqueue()
	if err != nil {
		return false
	}
	defer unix.Close(kq)

	ev := unix.Kevent_t{
		Ident:  uint64(fd),
		Filter: unix.EVFILT_READ,
		Flags:  unix.EV_ADD | unix.EV_ENABLE,
	}
	_, err = unix.Kevent(kq, []unix.Kevent_t{ev}, nil, nil)
	return err == nil
}

// statxHost returns ENOSYS on macOS (no statx syscall), forcing fallback to fstat.
func statxHost(hostFD int, sync uint32, mask uint32) (linux.Statx, bool, error) {
	return linux.Statx{}, false, linuxerr.ENOSYS
}

func isHostEventFdDevice(dev uint64) bool {
	return false
}

// hostStatMode converts a uint32 mode to the host Stat_t.Mode type (uint16 on macOS).
func hostStatMode(mode uint32) uint16 { return uint16(mode) }

// fallocateHost is a no-op on macOS (no fallocate syscall).
func fallocateHost(fd int, mode uint32, off int64, len int64) error {
	return linuxerr.ENOTSUP
}
