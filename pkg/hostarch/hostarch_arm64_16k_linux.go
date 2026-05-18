// Copyright 2024 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0

//go:build arm64 && linux && pagesize_16k

package hostarch

const (
	PageShift      = 14  // 16K pages: 2^14 = 16384
	HugePageShift  = 25  // 2^14 + (14-3) = 25 → 32MB huge pages
	GuestPageShift = PageShift // On Linux 16K hosts, guest and host page sizes are identical.
)
