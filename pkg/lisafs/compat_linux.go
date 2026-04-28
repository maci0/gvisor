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

// STATX constants used by the lisafs protocol. On Linux these come from
// golang.org/x/sys/unix.
const (
	statxMode  = unix.STATX_MODE
	statxUID   = unix.STATX_UID
	statxGID   = unix.STATX_GID
	statxSize  = unix.STATX_SIZE
	statxAtime = unix.STATX_ATIME
	statxMtime = unix.STATX_MTIME
)

// errREMOTEIO is the errno used for panics in RPC handlers. On Linux this is
// EREMOTEIO.
const errREMOTEIO = unix.EREMOTEIO

// pollRDHUP is the poll event for detecting remote hangup. On Linux this is
// POLLRDHUP.
const pollRDHUP = unix.POLLRDHUP

// ppoll wraps the ppoll(2) syscall.
func ppoll(fds []unix.PollFd, timeout *unix.Timespec, sigmask *unix.Sigset_t) (int, error) {
	return unix.Ppoll(fds, timeout, sigmask)
}
