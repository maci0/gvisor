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

package eventfd

import "fmt"

// createHostFD is not supported on macOS (no eventfd syscall).
//
// Precondition: efd.mu must be held.
func (efd *EventFileDescription) createHostFD() (int, error) {
	return -1, fmt.Errorf("host eventfd not supported on macOS")
}
