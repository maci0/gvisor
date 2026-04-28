// Copyright 2021 The gVisor Authors.
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

package lisafs

import "golang.org/x/sys/unix"

// STATX constants used by the lisafs protocol. These are protocol-level
// constants with the same values as on Linux, used to interpret wire messages
// rather than being passed to macOS syscalls.
const (
	statxMode  = 0x2
	statxUID   = 0x8
	statxGID   = 0x10
	statxSize  = 0x200
	statxAtime = 0x20
	statxMtime = 0x40
)

// errREMOTEIO is the errno used for panics in RPC handlers. EREMOTEIO does not
// exist on macOS; use EIO as a fallback.
const errREMOTEIO = unix.EIO

// pollRDHUP is the poll event for detecting remote hangup. POLLRDHUP does not
// exist on macOS; on macOS POLLHUP covers both local and remote hangup.
const pollRDHUP = int16(0)

// ppoll wraps poll functionality. macOS does not have ppoll(2), so we use
// poll(2) instead (ignoring the sigmask, which is acceptable for this use).
func ppoll(fds []unix.PollFd, timeout *unix.Timespec, sigmask *byte) (int, error) {
	// Convert timeout to milliseconds for poll(2). nil timeout means block
	// indefinitely.
	timeoutMs := -1 // block indefinitely
	if timeout != nil {
		timeoutMs = int(timeout.Sec*1000 + int64(timeout.Nsec)/1e6)
	}
	return unix.Poll(fds, timeoutMs)
}
