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

//go:build linux
// +build linux

package fdchannel

import "golang.org/x/sys/unix"

// initIov is a no-op on Linux. SOCK_SEQPACKET supports zero-length
// datagrams, so no iov is needed.
func (ep *Endpoint) initIov() {}

// sendFDFlags returns flags for sendmsg. No special flags on Linux.
func sendFDFlags() uintptr { return 0 }

// NewConnectedSockets returns a pair of file descriptors, owned by the caller,
// representing connected sockets that may be passed to separate calls to
// NewEndpoint to create connected Endpoints.
func NewConnectedSockets() ([2]int, error) {
	return unix.Socketpair(unix.AF_UNIX, unix.SOCK_SEQPACKET|unix.SOCK_CLOEXEC, 0)
}
