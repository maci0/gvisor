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
// +build linux

package eventfd

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// Create returns an initialized eventfd.
func Create() (Eventfd, error) {
	fd, _, err := unix.RawSyscall(unix.SYS_EVENTFD2, 0, 0, 0)
	if err != 0 {
		return Eventfd{}, fmt.Errorf("failed to create eventfd: %v", error(err))
	}
	if err := unix.SetNonblock(int(fd), true); err != nil {
		unix.Close(int(fd))
		return Eventfd{}, err
	}
	return Eventfd{fd: int(fd)}, nil
}

// closeExtra is a no-op on linux.
func closeExtra(fd int) {}
