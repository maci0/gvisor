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
// +build darwin,arm64

package hvf

import (
	"testing"
)

func TestVMCreate(t *testing.T) {
	p, err := New()
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	t.Logf("HVF platform created successfully, maxVCPUs=%d", p.ConcurrencyCount())
}

func TestVCPUCreate(t *testing.T) {
	p, err := New()
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}

	vcpu := p.machine.Get()
	if vcpu == nil {
		t.Fatal("Get() returned nil vCPU")
	}
	t.Logf("vCPU created: id=%d", vcpu.id)

	p.machine.Put(vcpu)
	t.Log("vCPU released successfully")
}

func TestAddressSpace(t *testing.T) {
	p, err := New()
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}

	as, err := p.NewAddressSpace()
	if err != nil {
		t.Fatalf("NewAddressSpace() failed: %v", err)
	}
	defer as.Release()
	t.Log("AddressSpace created successfully")
}
