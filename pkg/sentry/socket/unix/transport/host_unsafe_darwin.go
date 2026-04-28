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

// fdReadVec receives from fd to bufs.
//
// If the total length of bufs is > maxlen, fdReadVec will do a partial read
// and err will indicate why the message was truncated.
func fdReadVec(fd int, bufs [][]byte, control []byte, peek bool, maxlen int64) (readLen int64, msgLen int64, controlLen uint64, controlTrunc bool, err error) {
	flags := uintptr(unix.MSG_DONTWAIT | unix.MSG_TRUNC)
	if peek {
		flags |= unix.MSG_PEEK
	}

	// Always truncate the receive buffer. All socket types will truncate
	// received messages.
	length, iovecs, intermediate, err := buildIovec(bufs, maxlen, true)
	if err != nil && len(iovecs) == 0 {
		// No partial write to do, return error immediately.
		return 0, 0, 0, false, err
	}

	var msg unix.Msghdr
	if len(control) != 0 {
		msg.Control = &control[0]
		msg.SetControllen(len(control))
	}

	if len(iovecs) != 0 {
		msg.Iov = &iovecs[0]
		msg.SetIovlen(len(iovecs))
	}

	rawN, _, e := unix.RawSyscall(unix.SYS_RECVMSG, uintptr(fd), uintptr(unsafe.Pointer(&msg)), flags)
	if e != 0 {
		// N.B. prioritize the syscall error over the buildIovec error.
		return 0, 0, 0, false, e
	}
	n := int64(rawN)

	// Copy data back to bufs.
	if intermediate != nil {
		copyToMulti(bufs, intermediate)
	}

	controlTrunc = msg.Flags&unix.MSG_CTRUNC == unix.MSG_CTRUNC

	if n > length {
		return length, n, uint64(msg.Controllen), controlTrunc, nil
	}

	return n, n, uint64(msg.Controllen), controlTrunc, nil
}

// fdWriteVec sends from bufs to fd.
//
// If the total length of bufs is > maxlen && truncate, fdWriteVec will do
// a partial write and err will indicate why the message was truncated.
func fdWriteVec(fd int, bufs [][]byte, maxlen int64, truncate bool, creds bool) (int64, int64, error) {
	if creds {
		// Credential passing via SCM_CREDENTIALS is not supported on macOS.
		// macOS uses SCM_CREDS with a different struct, and the semantics
		// differ. Return EINVAL since this feature should not be used on macOS.
		return 0, 0, unix.EINVAL
	}

	length, iovecs, intermediate, err := buildIovec(bufs, maxlen, truncate)
	if err != nil && len(iovecs) == 0 {
		// No partial write to do, return error immediately.
		return 0, length, err
	}

	// Copy data to intermediate buf.
	if intermediate != nil {
		copyFromMulti(intermediate, bufs)
	}

	var msg unix.Msghdr
	if len(iovecs) > 0 {
		msg.Iov = &iovecs[0]
		msg.SetIovlen(len(iovecs))
	}

	// macOS does not support MSG_NOSIGNAL for sendmsg; SO_NOSIGPIPE should
	// be set on the socket instead. Use MSG_DONTWAIT only.
	n, _, e := unix.RawSyscall(unix.SYS_SENDMSG, uintptr(fd), uintptr(unsafe.Pointer(&msg)), unix.MSG_DONTWAIT)
	if e != 0 {
		// N.B. prioritize the syscall error over the buildIovec error.
		return 0, length, e
	}

	return int64(n), length, err
}

// passcredsEnabled checks if credential passing is enabled on the socket.
// macOS does not support SO_PASSCRED; always returns false.
func passcredsEnabled(fd int) bool {
	return false
}
