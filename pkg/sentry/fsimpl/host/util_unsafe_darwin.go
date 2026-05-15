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

package host

import (
	"golang.org/x/sys/unix"
)

func setTimestamps(fd int, ts *[2]unix.Timespec) error {
	// macOS doesn't have SYS_UTIMENSAT but has futimens via libc.
	// Use Futimesat as fallback which is available via x/sys/unix.
	times := [2]unix.Timeval{
		{Sec: ts[0].Sec, Usec: int32(ts[0].Nsec / 1000)},
		{Sec: ts[1].Sec, Usec: int32(ts[1].Nsec / 1000)},
	}
	return unix.Futimes(fd, times[:])
}
