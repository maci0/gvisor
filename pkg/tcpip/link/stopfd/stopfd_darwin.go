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

// Package stopfd provides a type that can be used to signal the stop of a dispatcher.
package stopfd

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// StopFD is a pipe-based stop signal for dispatchers on darwin, since eventfd
// is not available on macOS.
type StopFD struct {
	// EFD is the read end of the pipe, used for polling.
	EFD int
	// wfd is the write end of the pipe.
	wfd int
}

// New returns a new, initialized StopFD backed by a pipe.
func New() (StopFD, error) {
	var fds [2]int
	if err := unix.Pipe(fds[:]); err != nil {
		return StopFD{EFD: -1}, fmt.Errorf("failed to create pipe: %w", err)
	}
	if err := unix.SetNonblock(fds[0], true); err != nil {
		unix.Close(fds[0])
		unix.Close(fds[1])
		return StopFD{EFD: -1}, fmt.Errorf("failed to set nonblock on read end: %w", err)
	}
	if err := unix.SetNonblock(fds[1], true); err != nil {
		unix.Close(fds[0])
		unix.Close(fds[1])
		return StopFD{EFD: -1}, fmt.Errorf("failed to set nonblock on write end: %w", err)
	}
	return StopFD{EFD: fds[0], wfd: fds[1]}, nil
}

// Stop writes to the pipe and notifies the dispatcher to stop. It does not
// block.
func (sf *StopFD) Stop() {
	buf := []byte{1}
	if _, err := unix.Write(sf.wfd, buf); err != nil {
		panic(fmt.Sprintf("write(pipe) failed: %s", err))
	}
}
