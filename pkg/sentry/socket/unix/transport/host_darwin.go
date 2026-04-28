// Copyright 2020 The gVisor Authors.
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

package transport

import (
	"unsafe"

	"golang.org/x/sys/unix"
)

// checkSocketDomain verifies that the socket fd is a Unix domain socket.
// macOS does not have SO_DOMAIN, so we use getsockname and check the
// address family instead.
func checkSocketDomain(fd int) error {
	// Use getsockname to determine the socket address family.
	// RawSockaddrAny is large enough for any address.
	var rsa unix.RawSockaddrAny
	var len uint32 = unix.SizeofSockaddrAny
	_, _, e := unix.Syscall(unix.SYS_GETSOCKNAME, uintptr(fd), uintptr(unsafe.Pointer(&rsa)), uintptr(unsafe.Pointer(&len)))
	if e != 0 {
		return e
	}
	if rsa.Addr.Family != unix.AF_UNIX {
		return unix.EINVAL
	}
	return nil
}
