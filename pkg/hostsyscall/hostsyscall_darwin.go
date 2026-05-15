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

// Package hostsyscall provides functions like unix.RawSyscall, but without the
// overhead of multiple stack frame allocations.
//
// On macOS, we delegate to the unix package rather than using raw assembly
// because the macOS syscall ABI differs from Linux (e.g. X16-based syscall
// number on arm64, carry-flag-based error signaling).
package hostsyscall

import (
	"golang.org/x/sys/unix"
)

// RawSyscall6 performs a raw syscall with 6 arguments.
func RawSyscall6(trap, a1, a2, a3, a4, a5, a6 uintptr) (r1 uintptr, errno unix.Errno) {
	r, _, e := unix.RawSyscall6(trap, a1, a2, a3, a4, a5, a6)
	return r, unix.Errno(e)
}

// RawSyscall performs a raw syscall with 3 arguments.
func RawSyscall(trap, a1, a2, a3 uintptr) (r1 uintptr, errno unix.Errno) {
	r, _, e := unix.RawSyscall(trap, a1, a2, a3)
	return r, unix.Errno(e)
}

// RawSyscallErrno6 is like RawSyscall6, but only returns errno,
// and 0 if successful.
func RawSyscallErrno6(trap, a1, a2, a3, a4, a5, a6 uintptr) unix.Errno {
	_, _, e := unix.RawSyscall6(trap, a1, a2, a3, a4, a5, a6)
	return unix.Errno(e)
}

// RawSyscallErrno is like RawSyscall, but only returns errno,
// and 0 if successful.
func RawSyscallErrno(trap, a1, a2, a3 uintptr) unix.Errno {
	_, _, e := unix.RawSyscall(trap, a1, a2, a3)
	return unix.Errno(e)
}
