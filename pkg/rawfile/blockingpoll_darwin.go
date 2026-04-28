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

package rawfile

import (
	"unsafe"

	"golang.org/x/sys/unix"
)

// BlockingPoll on darwin uses poll(2) since ppoll is not available on macOS.
// The timeout parameter is converted from a Timespec to milliseconds for
// compatibility with the poll(2) interface.
func BlockingPoll(fds *PollEvent, nfds int, timeout *unix.Timespec) (int, unix.Errno) {
	// Convert timespec to milliseconds for poll(2).
	msec := -1 // block forever
	if timeout != nil {
		msec = int(timeout.Sec)*1000 + int(timeout.Nsec)/1000000
	}
	n, _, e := unix.Syscall(unix.SYS_POLL, uintptr(unsafe.Pointer(fds)), uintptr(nfds), uintptr(msec))
	return int(n), e
}
