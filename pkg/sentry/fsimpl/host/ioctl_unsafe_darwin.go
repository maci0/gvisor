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
	"unsafe"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/abi/linux"
)

func ioctlGetTermios(fd int) (*linux.Termios, error) {
	var macT unix.Termios
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), unix.TIOCGETA, uintptr(unsafe.Pointer(&macT)))
	if errno != 0 {
		return nil, errno
	}
	t := macTermiosToLinux(&macT)
	return t, nil
}

// IsTTY returns whether the given file descriptor is a terminal.
func IsTTY(fd int) bool {
	_, err := ioctlGetTermios(fd)
	return err == nil
}

func ioctlSetTermios(fd int, req uint64, t *linux.Termios) error {
	macT := linuxTermiosToMac(t)
	macReq := uintptr(unix.TIOCSETA)
	switch req {
	case linux.TCSETSW:
		macReq = unix.TIOCSETAW
	case linux.TCSETSF:
		macReq = unix.TIOCSETAF
	}
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), macReq, uintptr(unsafe.Pointer(macT)))
	if errno != 0 {
		return errno
	}
	return nil
}

func ioctlGetTermios2(fd int) (*linux.KernelTermios, error) {
	t, err := ioctlGetTermios(fd)
	if err != nil {
		return nil, err
	}
	kt := linux.KernelTermios{
		InputFlags:         t.InputFlags,
		OutputFlags:        t.OutputFlags,
		ControlFlags:       t.ControlFlags,
		LocalFlags:         t.LocalFlags,
		LineDiscipline:     t.LineDiscipline,
		InputSpeed:         0,
		OutputSpeed:        0,
	}
	copy(kt.ControlCharacters[:], t.ControlCharacters[:])
	return &kt, nil
}

func ioctlSetTermios2(fd int, req uint64, t *linux.KernelTermios) error {
	lt := &linux.Termios{
		InputFlags:     t.InputFlags,
		OutputFlags:    t.OutputFlags,
		ControlFlags:   t.ControlFlags,
		LocalFlags:     t.LocalFlags,
		LineDiscipline: t.LineDiscipline,
	}
	copy(lt.ControlCharacters[:], t.ControlCharacters[:])
	return ioctlSetTermios(fd, req, lt)
}

func ioctlGetWinsize(fd int) (*linux.Winsize, error) {
	var w linux.Winsize
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), unix.TIOCGWINSZ, uintptr(unsafe.Pointer(&w)))
	if errno != 0 {
		return nil, errno
	}
	return &w, nil
}

func ioctlSetWinsize(fd int, w *linux.Winsize) error {
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), unix.TIOCSWINSZ, uintptr(unsafe.Pointer(w)))
	if errno != 0 {
		return errno
	}
	return nil
}

func macTermiosToLinux(m *unix.Termios) *linux.Termios {
	t := &linux.Termios{
		InputFlags:     uint32(m.Iflag),
		OutputFlags:    uint32(m.Oflag),
		ControlFlags:   uint32(m.Cflag),
		LocalFlags:     uint32(m.Lflag),
	}
	n := len(t.ControlCharacters)
	if n > len(m.Cc) {
		n = len(m.Cc)
	}
	for i := 0; i < n; i++ {
		t.ControlCharacters[i] = m.Cc[i]
	}
	return t
}

func linuxTermiosToMac(t *linux.Termios) *unix.Termios {
	m := &unix.Termios{
		Iflag: uint64(t.InputFlags),
		Oflag: uint64(t.OutputFlags),
		Cflag: uint64(t.ControlFlags),
		Lflag: uint64(t.LocalFlags),
	}
	n := len(m.Cc)
	if n > len(t.ControlCharacters) {
		n = len(t.ControlCharacters)
	}
	for i := 0; i < n; i++ {
		m.Cc[i] = t.ControlCharacters[i]
	}
	return m
}
