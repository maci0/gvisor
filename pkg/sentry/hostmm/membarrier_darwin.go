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

package hostmm

// HaveGlobalMemoryBarrier returns true if GlobalMemoryBarrier is supported.
func HaveGlobalMemoryBarrier() bool {
	return false
}

// GlobalMemoryBarrier is not supported on darwin.
func GlobalMemoryBarrier() error {
	panic("GlobalMemoryBarrier not supported on darwin")
}

// HaveProcessMemoryBarrier returns true if ProcessMemoryBarrier is supported.
func HaveProcessMemoryBarrier() bool {
	return false
}

// ProcessMemoryBarrier is not supported on darwin.
func ProcessMemoryBarrier() error {
	panic("ProcessMemoryBarrier not supported on darwin")
}
