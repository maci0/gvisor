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

package safecopy

import (
	"fmt"

	"gvisor.dev/gvisor/pkg/errors"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/safecopy/machexc"
)

func init() {
	// Set the platform-specific handler that safecopy.go's init() will call.
	platformInit = darwinInit
}

func darwinInit() {
	if err := installMachExceptionHandler(); err != nil {
		panic(fmt.Sprintf("Unable to install Mach exception handler for safecopy: %v", err))
	}
	linuxerr.AddErrorUnwrapper(func(e error) (*errors.Error, bool) {
		switch e.(type) {
		case SegvError, BusError, AlignmentError:
			return linuxerr.EFAULT, true
		default:
			return nil, false
		}
	})
}

// installMachExceptionHandler sets up a Mach exception port for
// handling EXC_BAD_ACCESS in safecopy functions on macOS ARM64.
//
// macOS ARM64 sigreturn uses PAC tokens that prevent modified mcontext
// register restoration. The Mach exception handler bypasses this by
// using thread_get_state/thread_set_state from a separate thread.
func installMachExceptionHandler() error {
	// Register safecopy function ranges and their fault handlers.
	machexc.AddFaultRange(memcpyBegin, memcpyEnd,
		addrOfHandleMemcpyFault())
	machexc.AddFaultRange(memclrBegin, memclrEnd,
		addrOfHandleMemclrFault())
	machexc.AddFaultRange(swapUint32Begin, swapUint32End,
		addrOfHandleSwapUint32Fault())
	machexc.AddFaultRange(swapUint64Begin, swapUint64End,
		addrOfHandleSwapUint64Fault())
	machexc.AddFaultRange(compareAndSwapUint32Begin, compareAndSwapUint32End,
		addrOfHandleCompareAndSwapUint32Fault())
	machexc.AddFaultRange(loadUint32Begin, loadUint32End,
		addrOfHandleLoadUint32Fault())

	if err := machexc.Install(); err != nil {
		return fmt.Errorf("machexc.Install: %w", err)
	}
	return nil
}

// addrOfHandle* returns the PC of the assembly fault handler.
// Defined in mach_fault_addr_darwin.s.
func addrOfHandleMemcpyFault() uintptr
func addrOfHandleMemclrFault() uintptr
func addrOfHandleSwapUint32Fault() uintptr
func addrOfHandleSwapUint64Fault() uintptr
func addrOfHandleCompareAndSwapUint32Fault() uintptr
func addrOfHandleLoadUint32Fault() uintptr
