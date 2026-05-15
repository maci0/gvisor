// Copyright 2026 The gVisor Authors.
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

//go:build arm64 && !pagesize_64k && !darwin

package hostarch

import "golang.org/x/sys/unix"

const (
	// PageShift is the binary log of the system page size.
	// 4K pages: 2^12 = 4096
	PageShift = 12

	// HugePageShift is the binary log of the system huge page size.
	// For 4K pages: PageShift + (PageShift - 3) = 12 + 9 = 21
	// This gives 2MB huge pages.
	HugePageShift = 21
)

func init() {
	// Accept both 4K and 16K page sizes. On macOS gVisor (16K pages),
	// guest binaries compiled for 4K still work because the sentry
	// rounds up mmap requests to the host page size.
	if size := unix.Getpagesize(); size != PageSize && size != 16384 {
		println("WARNING: host page size mismatch")
	}
}
