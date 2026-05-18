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

//go:build arm64 && darwin

package hostarch

const (
	// PageShift is the binary log of the system page size.
	// macOS ARM64 uses 16K pages (2^14 = 16384).
	PageShift = 14

	// HugePageShift is the binary log of the system huge page size.
	// For 16K pages: 32MB huge pages (2^25).
	HugePageShift = 25

	// GuestPageShift is the binary log of the guest page size.
	// Linux ARM64 uses 4K pages (2^12 = 4096), so the guest presents
	// 4K alignment to applications while the host operates on 16K pages.
	GuestPageShift = 12
)
