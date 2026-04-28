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

// Package fdnotifier contains an adapter that translates IO events (e.g., a
// file became readable/writable) from native FDs to the notifications in the
// waiter package. It uses kqueue in edge-triggered mode to receive notifications
// for registered FDs.
package fdnotifier

import (
	"fmt"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/sync"
	"gvisor.dev/gvisor/pkg/waiter"
)

type fdInfo struct {
	queue   *waiter.Queue
	waiting bool
}

// notifier holds all the state necessary to issue notifications when IO events
// occur in the observed FDs.
type notifier struct {
	// kqFD is the kqueue file descriptor used to register for io
	// notifications.
	kqFD int

	// pauseMu synchronizes notifications with save/restore.
	pauseMu sync.Mutex

	// mu protects fdMap.
	mu sync.Mutex

	// fdMap maps file descriptors to their notification queues and waiting
	// status.
	fdMap map[int32]*fdInfo
}

// newNotifier creates a new notifier object.
func newNotifier() (*notifier, error) {
	kqfd, err := unix.Kqueue()
	if err != nil {
		return nil, err
	}

	w := &notifier{
		kqFD:  kqfd,
		fdMap: make(map[int32]*fdInfo),
	}

	go w.waitAndNotify() // S/R-SAFE: no waiter exists during save / load.

	return w, nil
}

// waitFD waits on mask for fd. The fdMap mutex must be held.
func (n *notifier) waitFD(fd int32, fi *fdInfo, mask waiter.EventMask) error {
	if !fi.waiting && mask == 0 {
		return nil
	}

	linuxMask := mask.ToLinux()

	switch {
	case !fi.waiting && mask != 0:
		// Add filters for the requested events.
		var changes []unix.Kevent_t
		if linuxMask&0x1 != 0 { // POLLIN
			changes = append(changes, unix.Kevent_t{
				Ident:  uint64(fd),
				Filter: unix.EVFILT_READ,
				Flags:  unix.EV_ADD | unix.EV_CLEAR,
			})
		}
		if linuxMask&0x4 != 0 { // POLLOUT
			changes = append(changes, unix.Kevent_t{
				Ident:  uint64(fd),
				Filter: unix.EVFILT_WRITE,
				Flags:  unix.EV_ADD | unix.EV_CLEAR,
			})
		}
		if len(changes) > 0 {
			if _, err := unix.Kevent(n.kqFD, changes, nil, nil); err != nil {
				return err
			}
		}
		fi.waiting = true
	case fi.waiting && mask == 0:
		// Delete all filters for this fd.
		changes := []unix.Kevent_t{
			{Ident: uint64(fd), Filter: unix.EVFILT_READ, Flags: unix.EV_DELETE},
			{Ident: uint64(fd), Filter: unix.EVFILT_WRITE, Flags: unix.EV_DELETE},
		}
		// Ignore errors from EV_DELETE since the filter might not have
		// been registered.
		unix.Kevent(n.kqFD, changes, nil, nil)
		fi.waiting = false
	case fi.waiting && mask != 0:
		// Modify: delete old filters, then add new ones.
		delChanges := []unix.Kevent_t{
			{Ident: uint64(fd), Filter: unix.EVFILT_READ, Flags: unix.EV_DELETE},
			{Ident: uint64(fd), Filter: unix.EVFILT_WRITE, Flags: unix.EV_DELETE},
		}
		unix.Kevent(n.kqFD, delChanges, nil, nil)

		var addChanges []unix.Kevent_t
		if linuxMask&0x1 != 0 { // POLLIN
			addChanges = append(addChanges, unix.Kevent_t{
				Ident:  uint64(fd),
				Filter: unix.EVFILT_READ,
				Flags:  unix.EV_ADD | unix.EV_CLEAR,
			})
		}
		if linuxMask&0x4 != 0 { // POLLOUT
			addChanges = append(addChanges, unix.Kevent_t{
				Ident:  uint64(fd),
				Filter: unix.EVFILT_WRITE,
				Flags:  unix.EV_ADD | unix.EV_CLEAR,
			})
		}
		if len(addChanges) > 0 {
			if _, err := unix.Kevent(n.kqFD, addChanges, nil, nil); err != nil {
				return err
			}
		}
	}

	return nil
}

// addFD adds an FD to the list of FDs observed by n.
func (n *notifier) addFD(fd int32, queue *waiter.Queue) error {
	n.mu.Lock()
	defer n.mu.Unlock()

	// Panic if we're already notifying on this FD.
	if _, ok := n.fdMap[fd]; ok {
		panic(fmt.Sprintf("File descriptor %v added twice", fd))
	}

	info := &fdInfo{queue: queue}
	// We might already have something in queue to wait for.
	if err := n.waitFD(fd, info, queue.Events()); err != nil {
		return err
	}
	// Add it to the map.
	n.fdMap[fd] = info
	return nil
}

// updateFD updates the set of events the fd needs to be notified on.
func (n *notifier) updateFD(fd int32) error {
	n.mu.Lock()
	defer n.mu.Unlock()

	if fi, ok := n.fdMap[fd]; ok {
		return n.waitFD(fd, fi, fi.queue.Events())
	}

	return nil
}

// removeFD removes an FD from the list of FDs observed by n.
func (n *notifier) removeFD(fd int32) {
	n.mu.Lock()
	defer n.mu.Unlock()

	// Remove from map, then from kqueue object.
	n.waitFD(fd, n.fdMap[fd], 0)
	delete(n.fdMap, fd)
}

// hasFD returns true if the fd is in the list of observed FDs.
func (n *notifier) hasFD(fd int32) bool {
	n.mu.Lock()
	defer n.mu.Unlock()

	_, ok := n.fdMap[fd]
	return ok
}

// waitAndNotify runs in its own goroutine and loops waiting for io event
// notifications from the kqueue object. Once notifications arrive, they are
// dispatched to the registered queue.
func (n *notifier) waitAndNotify() error {
	events := make([]unix.Kevent_t, 100)
	for {
		v, err := unix.Kevent(n.kqFD, nil, events, nil)
		if err == unix.EINTR {
			continue
		}

		if err != nil {
			return err
		}

		notified := false
		n.pauseMu.Lock()
		n.mu.Lock()
		for i := 0; i < v; i++ {
			fd := int32(events[i].Ident)
			if fi, ok := n.fdMap[fd]; ok {
				var linuxEvents uint32
				switch events[i].Filter {
				case unix.EVFILT_READ:
					linuxEvents |= 0x1 // POLLIN
				case unix.EVFILT_WRITE:
					linuxEvents |= 0x4 // POLLOUT
				}
				if events[i].Flags&unix.EV_EOF != 0 {
					linuxEvents |= 0x10 // POLLHUP
				}
				if events[i].Flags&unix.EV_ERROR != 0 {
					linuxEvents |= 0x8 // POLLERR
				}
				fi.queue.Notify(waiter.EventMaskFromLinux(linuxEvents))
				notified = true
			}
		}
		n.mu.Unlock()
		n.pauseMu.Unlock()
		if notified {
			// Let goroutines woken by Notify get a chance to run
			// before we kevent again.
			sync.Goyield()
		}
	}
}

// pause suspends notifications until resume is called.
func (n *notifier) pause() {
	n.pauseMu.Lock()
}

// resume ends the effect of a previous call to pause.
func (n *notifier) resume() {
	n.pauseMu.Unlock()
}

var shared struct {
	notifier *notifier
	once     sync.Once
	initErr  error
}

func ensureSharedNotifier() {
	shared.once.Do(func() {
		shared.notifier, shared.initErr = newNotifier()
	})
}

// AddFD adds an FD to the list of observed FDs.
func AddFD(fd int32, queue *waiter.Queue) error {
	ensureSharedNotifier()
	if shared.initErr != nil {
		return shared.initErr
	}

	return shared.notifier.addFD(fd, queue)
}

// UpdateFD updates the set of events the fd needs to be notified on.
func UpdateFD(fd int32) error {
	return shared.notifier.updateFD(fd)
}

// RemoveFD removes an FD from the list of observed FDs.
func RemoveFD(fd int32) {
	shared.notifier.removeFD(fd)
}

// HasFD returns true if the FD is in the list of observed FDs.
//
// This should only be used by tests to assert that FDs are correctly registered.
func HasFD(fd int32) bool {
	return shared.notifier.hasFD(fd)
}

// Pause suspends notifications until Resume is called.
func Pause() {
	ensureSharedNotifier()
	shared.notifier.pause()
}

// Resume ends the effect of a previous call to Pause.
func Resume() {
	shared.notifier.resume()
}
