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

//go:build darwin && arm64 && ring0_integration

package hvf

import "testing"

// Integration tests for sentry-as-ring0 architecture.
// These tests require the full ring0 stack to be running and are
// gated behind the "ring0_integration" build tag.
//
// Run with: go test -tags ring0_integration -v ./pkg/sentry/platform/hvf/

// TestRing0ShellBoot verifies that a busybox shell boots under ring0.
//
// Expected flow:
//   1. VMM creates HVF VM
//   2. Sentry boots at EL1 with kernel page table (TTBR1)
//   3. Guest busybox runs at EL0 with process page table (TTBR0)
//   4. SVC traps handled at EL1 without VM exit
//   5. Shell executes "echo OK" and exits
//
// Target performance: syscall latency ~100ns (vs ~11µs current).
func TestRing0ShellBoot(t *testing.T) {
	t.Skip("ring0 architecture not yet fully integrated")
}

// TestRing0SyscallLatency benchmarks syscall latency under ring0.
//
// Expected: ~100ns per getpid (vs ~11µs with HVC exit).
func TestRing0SyscallLatency(t *testing.T) {
	t.Skip("ring0 architecture not yet fully integrated")
}

// TestRing0TLBCoherency verifies TLB coherency with direct TLBI.
//
// This test runs the 14-goroutine stress test that previously
// failed at 86% pass rate due to TLB races. With direct TLBI
// at EL1, the target is 100% pass rate.
//
// The test validates:
//   1. Concurrent MapFile/Unmap across multiple vCPUs
//   2. No stale TLB entries after TLBI VMALLE1IS
//   3. No data corruption from recycled pages
func TestRing0TLBCoherency(t *testing.T) {
	t.Skip("ring0 architecture not yet fully integrated")
}

// TestRing0ConcurrentHTTP verifies concurrent HTTP requests under ring0.
//
// This test runs concurrent HTTP requests that previously failed
// at ~80-90% pass rate due to TLB-related corrupted return addresses.
// With direct TLBI, the target is 100% pass rate.
func TestRing0ConcurrentHTTP(t *testing.T) {
	t.Skip("ring0 architecture not yet fully integrated")
}
