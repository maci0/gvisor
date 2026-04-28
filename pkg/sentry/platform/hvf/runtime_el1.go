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

/*
#include <stdint.h>

// hvccallC executes HVC #0 to exit to the VMM.
static inline void hvccallC(void) {
	__asm__ __volatile__("hvc #0" ::: "memory");
}
*/
import "C"

import (
	"sync/atomic"
	"unsafe"

	"golang.org/x/sys/unix"
)

// vmmSharedPage points to the shared memory page used for VMM communication.
// Set during initialization when the sentry boots at EL1.
var vmmSharedPage unsafe.Pointer

// hostSyscall proxies a host syscall through the VMM. When the sentry
// runs at EL1 inside the VM, it cannot execute SVC (which would trap
// to its own EL1 vectors). Instead, it writes the request to the
// shared page and executes HVC to exit to the VMM, which performs
// the actual host syscall.
func hostSyscall(num, a0, a1, a2, a3, a4, a5 uintptr) (r0 uintptr, err unix.Errno) {
	if vmmSharedPage == nil {
		// Not running at EL1 in VM — use normal syscall path.
		// This allows the code to work in both modes.
		r, _, e := unix.RawSyscall6(uintptr(num), a0, a1, a2, a3, a4, a5)
		return r, e
	}

	req := (*VMMRequest)(vmmSharedPage)
	req.Op = VMMOpSyscall
	req.Args[0] = uint64(num)
	req.Args[1] = uint64(a0)
	req.Args[2] = uint64(a1)
	req.Args[3] = uint64(a2)
	req.Args[4] = uint64(a3)
	req.Args[5] = uint64(a4)
	atomic.StoreUint32(&req.Done, 0)

	// HVC #0 exits to VMM which processes the request.
	C.hvccallC()

	// VMM has processed the request and resumed us.
	return uintptr(req.Result), unix.Errno(req.Errno)
}

// hvccall executes an HVC #0 instruction to exit to the VMM.
func hvccall() {
	C.hvccallC()
}
