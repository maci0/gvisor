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

package time

import (
	"golang.org/x/sys/unix"
)

// darwinClockID translates a Linux clock ID to a macOS clock ID.
// Linux CLOCK_MONOTONIC = 1, macOS CLOCK_MONOTONIC = 6.
func darwinClockID(id ClockID) int32 {
	switch id {
	case Realtime:
		return 0 // CLOCK_REALTIME (same on both)
	case Monotonic:
		return 6 // macOS CLOCK_MONOTONIC
	default:
		return int32(id)
	}
}

// vdsoClockGettime calls clock_gettime via the Go runtime on macOS.
// On Linux, this goes through the VDSO for performance.
// On macOS, we use the standard libc clock_gettime with translated
// clock IDs (Linux and macOS use different values for CLOCK_MONOTONIC).
func vdsoClockGettime(clockid ClockID, ts *unix.Timespec) int {
	if err := unix.ClockGettime(darwinClockID(clockid), ts); err != nil {
		return -1
	}
	return 0
}
