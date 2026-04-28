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

// VMMRequest is a request from the sentry (EL1) to the VMM (host).
// Stored in shared memory at VMMSharedPageIPA, signaled via HVC.
type VMMRequest struct {
	Op     uint64    // VMMOp* constant
	Args   [6]uint64 // Syscall arguments or operation parameters
	Result int64     // Return value from host operation
	Errno  int64     // Errno from host operation
	Done   uint32    // Atomic: 0=pending, 1=done
	_pad   uint32    // Alignment padding
}

// VMM operation codes.
const (
	VMMOpSyscall = 1 // Proxy a host syscall
	VMMOpMmap    = 2 // mmap backing memory
	VMMOpMunmap  = 3 // munmap backing memory
	VMMOpExit    = 4 // Sentry is shutting down
)

// VMMSharedPageIPA is the IPA of the shared memory page used for
// VMM<->Sentry communication. Located below ipaBase (0x100000) in
// the reserved first 1MB of IPA space.
const VMMSharedPageIPA = 0x80000 // 512KB
