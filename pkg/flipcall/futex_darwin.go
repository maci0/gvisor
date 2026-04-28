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
// +build darwin

package flipcall

import (
	"fmt"
	"runtime"
	"time"
)

func (ep *Endpoint) futexSetPeerActive() error {
	if ep.connState().CompareAndSwap(ep.activeState, ep.inactiveState) {
		return nil
	}
	switch cs := ep.connState().Load(); cs {
	case csShutdown:
		return ShutdownError{}
	default:
		return fmt.Errorf("unexpected connection state before FUTEX_WAKE: %v", cs)
	}
}

func (ep *Endpoint) futexWakePeer() error {
	// On darwin, the waiter polls, so wake is a no-op.
	// The state change in futexSetPeerActive is sufficient.
	return nil
}

func (ep *Endpoint) futexWaitUntilActive() error {
	for {
		switch cs := ep.connState().Load(); cs {
		case ep.activeState:
			return nil
		case ep.inactiveState:
			if ep.isShutdownLocally() {
				return ShutdownError{}
			}
			// Spin briefly then yield.
			for i := 0; i < 100; i++ {
				if ep.connState().Load() != ep.inactiveState {
					break
				}
				runtime.Gosched()
			}
			if ep.connState().Load() == ep.inactiveState {
				// Still not active, sleep briefly.
				time.Sleep(time.Microsecond)
			}
			continue
		case csShutdown:
			return ShutdownError{}
		default:
			return fmt.Errorf("unexpected connection state before FUTEX_WAIT: %v", cs)
		}
	}
}

func (ep *Endpoint) futexWakeConnState(numThreads int32) error {
	// No-op on darwin - waiters poll.
	return nil
}

func (ep *Endpoint) futexWaitConnState(curState uint32) error {
	// Poll until state changes.
	for i := 0; i < 1000; i++ {
		if ep.connState().Load() != curState {
			return nil
		}
		runtime.Gosched()
	}
	// Longer sleep if still unchanged.
	for ep.connState().Load() == curState {
		time.Sleep(time.Microsecond)
	}
	return nil
}

func yieldThread() {
	runtime.Gosched()
}
