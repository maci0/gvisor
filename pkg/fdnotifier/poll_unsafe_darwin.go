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
// +build darwin

package fdnotifier

import (
	"unsafe"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/waiter"
)

// NonBlockingPoll polls the given FD in non-blocking fashion. It is used just
// to query the FD's current state.
func NonBlockingPoll(fd int32, mask waiter.EventMask) waiter.EventMask {
	e := struct {
		fd      int32
		events  int16
		revents int16
	}{
		fd:     fd,
		events: int16(mask.ToLinux()),
	}

	for {
		n, _, err := unix.RawSyscall(unix.SYS_POLL, uintptr(unsafe.Pointer(&e)), 1, 0)
		// Interrupted by signal, try again.
		if err == unix.EINTR {
			continue
		}
		// If an error occur we'll conservatively say the FD is ready for
		// whatever is being checked.
		if err != 0 {
			return mask
		}

		// If no FDs were returned, it wasn't ready for anything.
		if n == 0 {
			return 0
		}

		// Otherwise we got the ready events in the revents field.
		return waiter.EventMaskFromLinux(uint32(e.revents))
	}
}
