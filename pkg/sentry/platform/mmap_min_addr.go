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

package platform

import (
	"gvisor.dev/gvisor/pkg/hostarch"
)

// SystemMMapMinAddr returns the minimum system address.
func SystemMMapMinAddr() hostarch.Addr {
	return hostarch.Addr(systemMMapMinAddr)
}

// MMapMinAddr is a size zero struct that implements MinUserAddress based on
// the system minimum address. It is suitable for embedding in platforms that
// rely on the system mmap, and thus require the system minimum.
type MMapMinAddr struct{}

// MinUserAddress implements platform.MinUserAddresss.
func (*MMapMinAddr) MinUserAddress() hostarch.Addr {
	return SystemMMapMinAddr()
}
