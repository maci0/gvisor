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

// Package hostmm provides tools for interacting with the host Linux kernel's
// virtual memory management subsystem.
package hostmm

import "fmt"

// ReadTransparentHugepageEnum is not supported on darwin.
func ReadTransparentHugepageEnum(filename string) (string, error) {
	return "", fmt.Errorf("transparent hugepages not supported on darwin")
}

// GetTransparentHugepageEnum is not supported on darwin.
func GetTransparentHugepageEnum(data string) (string, error) {
	return "", fmt.Errorf("transparent hugepages not supported on darwin")
}

// NotifyCurrentMemcgPressureCallback is not supported on darwin.
func NotifyCurrentMemcgPressureCallback(f func(), level string) (func(), error) {
	return nil, fmt.Errorf("memory cgroup pressure notifications not supported on darwin")
}
