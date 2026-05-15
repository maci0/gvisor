// Copyright 2019 The gVisor Authors.
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

package fdchannel

import (
	"unsafe"

	"golang.org/x/sys/unix"
)

// NewConnectedSockets returns a pair of file descriptors, owned by the caller,
// representing connected sockets that may be passed to separate calls to
// NewEndpoint to create connected Endpoints.
//
// On macOS, SOCK_CLOEXEC is not available as a socket type flag, so we set
// FD_CLOEXEC via fcntl after creation.
func NewConnectedSockets() ([2]int, error) {
	// macOS doesn't support SOCK_SEQPACKET for AF_UNIX.
	// Use SOCK_STREAM — the fdchannel protocol sends exactly one
	// control message per sendmsg, so message boundaries aren't needed.
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		return fds, err
	}
	for _, fd := range fds {
		if _, err := unix.FcntlInt(uintptr(fd), unix.F_SETFD, unix.FD_CLOEXEC); err != nil {
			unix.Close(fds[0])
			unix.Close(fds[1])
			return [2]int{-1, -1}, err
		}
	}
	return fds, nil
}

// iovByte is a per-Endpoint byte used as iov data for sendmsg/recvmsg.
// macOS SOCK_STREAM requires at least 1 byte of data to deliver
// SCM_RIGHTS control messages.
var iovDummy [1]byte

// initIov sets up a 1-byte iov on the msghdr. macOS uses SOCK_STREAM
// (not SOCK_SEQPACKET), which requires at least 1 data byte for
// sendmsg/recvmsg to deliver SCM_RIGHTS control messages.
func (ep *Endpoint) initIov() {
	ep.msghdr.Iov = (*unix.Iovec)(unsafe.Pointer(&ep.iov))
	ep.msghdr.Iovlen = 1
	ep.iov.Base = &iovDummy[0]
	ep.iov.SetLen(1)
}

// sendFDFlags returns MSG_DONTWAIT on macOS to prevent SendFD from
// blocking when the SOCK_STREAM receive buffer is full. This avoids
// deadlocks in the gofer when sending donated FDs via flipcall channels.
func sendFDFlags() uintptr { return unix.MSG_DONTWAIT }
