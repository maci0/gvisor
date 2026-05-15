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

package eventfd

import (
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/fdnotifier"
	"gvisor.dev/gvisor/pkg/log"
)

// createHostFD creates a host eventfd for this event file description.
//
// Precondition: efd.mu must be held.
func (efd *EventFileDescription) createHostFD() (int, error) {
	flags := linux.EFD_NONBLOCK
	if efd.semMode {
		flags |= linux.EFD_SEMAPHORE
	}

	fd, _, errno := unix.Syscall(unix.SYS_EVENTFD2, uintptr(efd.val), uintptr(flags), 0)
	if errno != 0 {
		return -1, errno
	}

	if err := fdnotifier.AddFD(int32(fd), &efd.queue); err != nil {
		if closeErr := unix.Close(int(fd)); closeErr != nil {
			log.Warningf("close(%d) eventfd failed: %v", fd, closeErr)
		}
		return -1, err
	}

	efd.hostfd = int(fd)
	efd.sentryOwnedHostfd = true
	return efd.hostfd, nil
}
