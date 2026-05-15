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

package memutil

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// CreateMemFD creates an anonymous memory file and returns the fd.
// On darwin, this is emulated using a temporary file that is immediately
// unlinked.
func CreateMemFD(name string, flags int) (int, error) {
	f, err := os.CreateTemp("", fmt.Sprintf("gvisor_%s_", name))
	if err != nil {
		return -1, fmt.Errorf("failed to create temp file: %w", err)
	}
	path := f.Name()
	fd := int(f.Fd())
	// Duplicate the fd so we own it after closing the *os.File.
	newFD, err := unix.Dup(fd)
	if err != nil {
		f.Close()
		os.Remove(path)
		return -1, fmt.Errorf("dup failed: %w", err)
	}
	f.Close()
	// Unlink so the file is anonymous (only accessible via fd).
	os.Remove(path)
	return newFD, nil
}
