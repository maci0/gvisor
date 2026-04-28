// Copyright 2018 The gVisor Authors.
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

package unet

import "golang.org/x/sys/unix"

// socketPairFDs creates a pair of connected socket file descriptors.
// On macOS, SOCK_CLOEXEC is not available as a socket type flag, so we set
// FD_CLOEXEC via fcntl after creation.
func socketPairFDs(packet bool) ([2]int, error) {
	fds, err := unix.Socketpair(unix.AF_UNIX, socketType(packet), 0)
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
