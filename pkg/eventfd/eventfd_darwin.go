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

package eventfd

import (
	"fmt"
	gosync "sync"

	"golang.org/x/sys/unix"
)

// darwinWriteFDs maps read fd -> write fd for pipe-based eventfd emulation.
var (
	darwinWriteFDs   = make(map[int]int)
	darwinWriteFDsMu gosync.Mutex
)

// Create returns an initialized eventfd.
// On darwin, this is emulated using a pipe.
func Create() (Eventfd, error) {
	var fds [2]int
	if err := unix.Pipe(fds[:]); err != nil {
		return Eventfd{}, fmt.Errorf("failed to create pipe for eventfd emulation: %v", err)
	}
	// fds[0] is the read end, fds[1] is the write end.
	if err := unix.SetNonblock(fds[0], true); err != nil {
		unix.Close(fds[0])
		unix.Close(fds[1])
		return Eventfd{}, err
	}
	if err := unix.SetNonblock(fds[1], true); err != nil {
		unix.Close(fds[0])
		unix.Close(fds[1])
		return Eventfd{}, err
	}
	darwinWriteFDsMu.Lock()
	darwinWriteFDs[fds[0]] = fds[1]
	darwinWriteFDsMu.Unlock()
	return Eventfd{fd: fds[0]}, nil
}

// closeExtra closes the write end of the pipe associated with readFD.
func closeExtra(readFD int) {
	darwinWriteFDsMu.Lock()
	defer darwinWriteFDsMu.Unlock()
	if wfd, ok := darwinWriteFDs[readFD]; ok {
		unix.Close(wfd)
		delete(darwinWriteFDs, readFD)
	}
}
