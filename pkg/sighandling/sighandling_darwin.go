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

package sighandling

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/unix"
)

// IgnoreChildStop sets SA_NOCLDSTOP on SIGCHLD, causing child processes to not
// generate SIGCHLD when they stop.
func IgnoreChildStop() error {
	var sa, osa sigactionDarwin
	// Get the existing handler.
	if err := sigactionRaw(unix.SIGCHLD, nil, &osa); err != nil {
		return err
	}
	sa = osa
	sa.flags |= _SA_NOCLDSTOP
	return sigactionRaw(unix.SIGCHLD, &sa, nil)
}

// ReplaceSignalHandler replaces the existing signal handler for the provided
// signal with the function pointer at `handler`. This bypasses the Go runtime
// signal handlers, and should only be used for low-level signal handlers where
// use of signal.Notify is not appropriate.
//
// It stores the value of the previously set handler in previous.
func ReplaceSignalHandler(sig unix.Signal, handler uintptr, previous *uintptr) error {
	var sa, osa sigactionDarwin

	// Get the existing signal handler.
	if err := sigactionRaw(sig, nil, &osa); err != nil {
		return err
	}

	if osa.handler == 0 {
		return fmt.Errorf("previous handler for signal %x isn't set", sig)
	}

	*previous = osa.handler

	// Install our own handler.
	sa = osa
	sa.handler = handler
	sa.flags |= _SA_SIGINFO
	return sigactionRaw(sig, &sa, nil)
}

const (
	_SA_NOCLDSTOP = 0x8
	_SA_SIGINFO   = 0x40
)

// sigactionDarwin is the macOS sigaction struct layout for ARM64.
// struct sigaction {
//     union __sigaction_u __sigaction_u; // handler or sigaction (8 bytes)
//     sigset_t sa_mask;                 // 4 bytes
//     int sa_flags;                     // 4 bytes
// }
type sigactionDarwin struct {
	handler uintptr
	mask    uint32
	flags   int32
}

// sigactionRaw calls the sigaction syscall directly.
func sigactionRaw(sig unix.Signal, act *sigactionDarwin, oact *sigactionDarwin) error {
	_, _, errno := unix.RawSyscall(
		unix.SYS_SIGACTION,
		uintptr(sig),
		uintptr(unsafe.Pointer(act)),
		uintptr(unsafe.Pointer(oact)),
	)
	if errno != 0 {
		return errno
	}
	return nil
}
