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

//go:build darwin && arm64

// Package hvf implements a platform using macOS Hypervisor.framework.
package hvf

/*
#cgo LDFLAGS: -framework Hypervisor -framework CoreFoundation
#include <Hypervisor/Hypervisor.h>
#include <CoreFoundation/CoreFoundation.h>
#include <stdlib.h>
*/
import "C"

import (
	"fmt"
	"unsafe"

	pkgcontext "gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/fd"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/sentry/platform"
)

// HVF implements platform.Platform using macOS Hypervisor.framework.
type HVF struct {
	platform.NoCPUPreemptionDetection
	platform.NoCPUNumbers
	machine *machine
}

// New creates a new HVF platform instance. It initializes the
// Hypervisor.framework VM and creates the vCPU management machine.
// HVFOpts holds options for creating an HVF platform.
type HVFOpts struct {
	Page4K bool
}

func New(opts ...HVFOpts) (*HVF, error) {
	var o HVFOpts
	if len(opts) > 0 {
		o = opts[0]
	}
	// Create VM with 40-bit IPA (1TB) to cover Go stack addresses
	// which can be at ~80GB+ (exceeding the 36-bit/64GB default).
	config := C.hv_vm_config_create()
	defer C.CFRelease(C.CFTypeRef(unsafe.Pointer(config)))
	C.hv_vm_config_set_ipa_size(config, 40)

	if o.Page4K {
		ret := C.hv_vm_config_set_ipa_granule(config, C.HV_IPA_GRANULE_4KB)
		if ret != C.HV_SUCCESS {
			log.Warningf("hv_vm_config_set_ipa_granule(4KB) failed: %d — using 16K with page4K rounding shim", ret)
		} else {
			setGuestPageSize(4096)
			log.Infof("HVF: 4K IPA granule enabled (stage-1 TG0=4K, stage-2 4K)")
		}
	}

	ret := C.hv_vm_create(config)
	if ret != C.HV_SUCCESS {
		return nil, fmt.Errorf("error creating HVF VM (hv_vm_create: %d)", ret)
	}

	m, err := newMachine()
	if err != nil {
		C.hv_vm_destroy()
		return nil, err
	}

	return &HVF{machine: m}, nil
}

// SupportsAddressSpaceIO implements platform.Platform.SupportsAddressSpaceIO.
func (*HVF) SupportsAddressSpaceIO() bool { return false }

// HaveGlobalMemoryBarrier implements platform.Platform.HaveGlobalMemoryBarrier.
func (*HVF) HaveGlobalMemoryBarrier() bool { return false }

// GlobalMemoryBarrier implements platform.Platform.GlobalMemoryBarrier.
func (*HVF) GlobalMemoryBarrier() error {
	panic("GlobalMemoryBarrier not supported on HVF")
}

// MapUnit implements platform.Platform.MapUnit.
func (*HVF) MapUnit() uint64 { return uint64(hvfPageSize) } //nolint:unconvert

// MinUserAddress implements platform.Platform.MinUserAddress.
func (*HVF) MinUserAddress() hostarch.Addr { return hostarch.Addr(2 * hvfPageSize) }

// MaxUserAddress implements platform.Platform.MaxUserAddress.
func (*HVF) MaxUserAddress() hostarch.Addr { return hostarch.Addr(maxUserAddress.Load()) }

// NewAddressSpace implements platform.Platform.NewAddressSpace.
func (h *HVF) NewAddressSpace() (platform.AddressSpace, error) {
	return newAddressSpace(h.machine)
}

// NewContext implements platform.Platform.NewContext.
func (h *HVF) NewContext(_ pkgcontext.Context) platform.Context {
	return &hvfContext{machine: h.machine}
}

// PatchInitFastPath writes the init task's IDs into the vectors page
// for in-VM fast-path syscall dispatch at EL1.
func PatchInitFastPath(p platform.Platform, pid, ppid, tid, uid, euid, gid, egid, pgid, sid uint16) {
	if h, ok := p.(*HVF); ok {
		h.machine.PatchFastPathSyscalls(pid, ppid, tid, uid, euid, gid, egid, pgid, sid)
	}
}

// SeccompInfo implements platform.Platform.SeccompInfo.
func (h *HVF) SeccompInfo() platform.SeccompInfo {
	// No seccomp on macOS.
	return platform.StaticSeccompInfo{
		PlatformName: "hvf",
	}
}

// ConcurrencyCount implements platform.Platform.ConcurrencyCount.
func (h *HVF) ConcurrencyCount() int { return h.machine.maxVCPUs }

// constructor implements platform.Constructor.
type constructor struct{}

// New implements platform.Constructor.New.
func (*constructor) New(_ platform.Options) (platform.Platform, error) {
	return New()
}

// OpenDevice implements platform.Constructor.OpenDevice.
func (*constructor) OpenDevice(_ string) (*fd.FD, error) {
	// Hypervisor.framework doesn't use a device file.
	return nil, nil
}

// Requirements implements platform.Constructor.Requirements.
func (*constructor) Requirements() platform.Requirements {
	return platform.Requirements{}
}

// PrecompiledSeccompInfo implements platform.Constructor.PrecompiledSeccompInfo.
func (*constructor) PrecompiledSeccompInfo() []platform.SeccompInfo {
	return nil
}

func init() {
	platform.Register("hvf", &constructor{})
}
