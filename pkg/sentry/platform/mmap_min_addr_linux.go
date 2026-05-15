// Copyright 2018 The gVisor Authors.
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

//go:build linux
// +build linux

package platform

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// systemMMapMinAddrSource is the source file.
const systemMMapMinAddrSource = "/proc/sys/vm/mmap_min_addr"

// systemMMapMinAddr is the system's minimum map address.
var systemMMapMinAddr uint64

func init() {
	// Open the source file.
	b, err := os.ReadFile(systemMMapMinAddrSource)
	if err != nil {
		panic(fmt.Sprintf("couldn't open %s: %v", systemMMapMinAddrSource, err))
	}

	// Parse the result.
	systemMMapMinAddr, err = strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		panic(fmt.Sprintf("couldn't parse %s from %s: %v", string(b), systemMMapMinAddrSource, err))
	}
}
