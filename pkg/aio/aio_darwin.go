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

package aio

import (
	"fmt"

	"golang.org/x/sys/unix"
)

func executeRequest(r Request) Completion {
	var sysno uintptr
	switch r.Op {
	case OpRead:
		// macOS uses SYS_PREAD instead of SYS_PREAD64.
		sysno = unix.SYS_PREAD
	case OpWrite:
		// macOS uses SYS_PWRITE instead of SYS_PWRITE64.
		sysno = unix.SYS_PWRITE
	case OpReadv:
		// macOS does not have preadv2. Fall back to preadv is not
		// available either (no SYS_PREADV on darwin). Return ENOSYS.
		return Completion{
			ID:     r.ID,
			Result: -int64(unix.ENOSYS),
		}
	case OpWritev:
		// macOS does not have pwritev2. Fall back to pwritev is not
		// available either (no SYS_PWRITEV on darwin). Return ENOSYS.
		return Completion{
			ID:     r.ID,
			Result: -int64(unix.ENOSYS),
		}
	default:
		panic(fmt.Sprintf("unknown op %v", r.Op))
	}
	n, _, e := unix.Syscall6(sysno, uintptr(r.FD), uintptr(r.Buf), uintptr(r.Len), uintptr(r.Off), 0, 0)
	c := Completion{
		ID:     r.ID,
		Result: int64(n),
	}
	if e != 0 {
		c.Result = -int64(e)
	}
	return c
}
