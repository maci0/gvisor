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

//go:build darwin && arm64

package hvf

import (
	"testing"

	"golang.org/x/sys/unix"
)

func TestHostSyscallFallback(t *testing.T) {
	// With vmmSharedPage == nil, hostSyscall should use normal syscall.
	// Test with getpid which always succeeds.
	r, err := hostSyscall(uintptr(unix.SYS_GETPID), 0, 0, 0, 0, 0, 0)
	if err != 0 {
		t.Fatalf("hostSyscall(getpid) failed: %v", err)
	}
	if r == 0 {
		t.Fatal("hostSyscall(getpid) returned 0")
	}
	t.Logf("hostSyscall(getpid) = %d", r)
}
