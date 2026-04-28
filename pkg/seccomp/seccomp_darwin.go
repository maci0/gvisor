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

package seccomp

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/bpf"
)

const (
	// LINUX_AUDIT_ARCH is the native audit architecture.
	// On darwin/arm64, we use the aarch64 audit arch for compatibility
	// with seccomp program building.
	LINUX_AUDIT_ARCH = linux.AUDIT_ARCH_AARCH64

	// SYS_SECCOMP is not available on darwin.
	SYS_SECCOMP = 0
)

// SetFilter is not supported on darwin.
func SetFilter(instrs []bpf.Instruction) error {
	return fmt.Errorf("seccomp filters are not supported on darwin")
}

// SetFilterInChild is not supported on darwin.
//
//go:nosplit
func SetFilterInChild(instrs []bpf.Instruction) unix.Errno {
	return unix.ENOSYS
}

func isKillProcessAvailable() (bool, error) {
	return false, nil
}

// seccomp is not available on darwin.
//
//go:nosplit
func seccomp(op, flags uint32, ptr unsafe.Pointer) (uintptr, unix.Errno) {
	return 0, unix.ENOSYS
}
