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

// Package utun provides a link-layer endpoint backed by a macOS utun device.
package utun

import (
	"encoding/binary"
	"fmt"
	"net"
	"unsafe"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

const (
	utunControlName = "com.apple.net.utun_control"
	utunOptIfname   = 2

	// utun prepends a 4-byte protocol family header to each packet.
	utunHeaderLen = 4

	// defaultMTU for the utun interface.
	defaultMTU = 1500
)

// Endpoint is a link-layer endpoint backed by a macOS utun device.
type Endpoint struct {
	fd       int
	ifname   string
	mtu      uint32
	closed   chan struct{}
	dispatch stack.NetworkDispatcher
	nat      *proxy // userspace NAT proxy

	// Embed a channel endpoint for the WritePackets path.
	channel.Endpoint
}

// New creates a utun device and returns an Endpoint.
// The caller must have root privileges or the appropriate entitlement.
func New() (*Endpoint, error) {
	fd, ifname, err := createUtun()
	if err != nil {
		return nil, fmt.Errorf("utun: %w", err)
	}

	ep := &Endpoint{
		fd:     fd,
		ifname: ifname,
		mtu:    defaultMTU,
		closed: make(chan struct{}),
	}
	return ep, nil
}

// Name returns the utun interface name (e.g., "utun5").
func (e *Endpoint) Name() string {
	return e.ifname
}

// FD returns the utun file descriptor for external configuration.
func (e *Endpoint) FD() int {
	return e.fd
}

// EnableProxy starts a userspace NAT proxy for outbound traffic.
// This replaces pfctl NAT which can't route de-NAT'd replies on macOS.
func (e *Endpoint) EnableProxy(guestIP, hostIP net.IP) {
	e.nat = newProxy(e.fd, guestIP, hostIP)
}

// MTU implements stack.LinkEndpoint.MTU.
func (e *Endpoint) MTU() uint32 {
	return e.mtu
}

// MaxHeaderLength implements stack.LinkEndpoint.MaxHeaderLength.
func (e *Endpoint) MaxHeaderLength() uint16 {
	return 0 // No link-layer header; utun is L3.
}

// LinkAddress implements stack.LinkEndpoint.LinkAddress.
func (e *Endpoint) LinkAddress() tcpip.LinkAddress {
	return "" // No link-layer address for utun.
}

// Capabilities implements stack.LinkEndpoint.Capabilities.
func (e *Endpoint) Capabilities() stack.LinkEndpointCapabilities {
	return 0
}

// Attach implements stack.LinkEndpoint.Attach.
func (e *Endpoint) Attach(dispatcher stack.NetworkDispatcher) {
	e.dispatch = dispatcher
	if dispatcher != nil {
		go e.readLoop()
	}
}

// IsAttached implements stack.LinkEndpoint.IsAttached.
func (e *Endpoint) IsAttached() bool {
	return e.dispatch != nil
}

// Wait implements stack.LinkEndpoint.Wait.
func (e *Endpoint) Wait() {
	<-e.closed
}

// ARPHardwareType implements stack.LinkEndpoint.ARPHardwareType.
func (e *Endpoint) ARPHardwareType() header.ARPHardwareType {
	return header.ARPHardwareNone
}

// AddHeader implements stack.LinkEndpoint.AddHeader.
func (e *Endpoint) AddHeader(*stack.PacketBuffer) {}

// ParseHeader implements stack.LinkEndpoint.ParseHeader.
func (e *Endpoint) ParseHeader(*stack.PacketBuffer) bool { return true }

// Close closes the utun device.
func (e *Endpoint) Close() {
	unix.Close(e.fd)
	close(e.closed)
}

// WritePackets implements stack.LinkEndpoint.WritePackets.
func (e *Endpoint) WritePackets(pkts stack.PacketBufferList) (int, tcpip.Error) {
	var n int
	for _, pkt := range pkts.AsSlice() {
		if err := e.writePacket(pkt); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

func (e *Endpoint) writePacket(pkt *stack.PacketBuffer) tcpip.Error {
	// Determine the protocol family for the utun header.
	var af uint32
	switch pkt.NetworkProtocolNumber {
	case header.IPv4ProtocolNumber:
		af = unix.AF_INET
	case header.IPv6ProtocolNumber:
		af = unix.AF_INET6
	default:
		return &tcpip.ErrNotSupported{}
	}

	// Build the packet: 4-byte AF header + IP packet.
	views := pkt.AsSlices()
	totalLen := utunHeaderLen
	for _, v := range views {
		totalLen += len(v)
	}

	buf := make([]byte, totalLen)
	binary.BigEndian.PutUint32(buf[:utunHeaderLen], af)
	offset := utunHeaderLen
	for _, v := range views {
		copy(buf[offset:], v)
		offset += len(v)
	}

	// Try userspace proxy for internet-bound IPv4 traffic.
	if e.nat != nil && af == unix.AF_INET {
		if e.nat.handleOutbound(buf[utunHeaderLen:]) {
			return nil
		}
	} else if af == unix.AF_INET && e.nat == nil {
		log.Warningf("utun write: no proxy configured, sending raw")
	}

	_, err := unix.Write(e.fd, buf)
	if err != nil {
		log.Debugf("utun write: %v", err)
		return &tcpip.ErrAborted{}
	}
	return nil
}

// readLoop reads packets from the utun device and dispatches them.
func (e *Endpoint) readLoop() {
	defer close(e.closed)

	buf := make([]byte, utunHeaderLen+e.mtu+100)
	for {
		n, err := unix.Read(e.fd, buf)
		if err != nil {
			if err == unix.EINTR {
				continue
			}
			log.Debugf("utun read: %v", err)
			return
		}
		if n <= utunHeaderLen {
			continue
		}

		// Parse the 4-byte AF header.
		af := binary.BigEndian.Uint32(buf[:utunHeaderLen])
		var proto tcpip.NetworkProtocolNumber
		switch af {
		case unix.AF_INET:
			proto = header.IPv4ProtocolNumber
		case unix.AF_INET6:
			proto = header.IPv6ProtocolNumber
		default:
			continue
		}

		// Create a PacketBuffer and dispatch.
		pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
			Payload: buffer.MakeWithData(buf[utunHeaderLen:n]),
		})
		e.dispatch.DeliverNetworkPacket(proto, pkt)
		pkt.DecRef()
	}
}

// createUtun creates a macOS utun device and returns its fd and interface name.
func createUtun() (int, string, error) {
	fd, err := unix.Socket(unix.AF_SYSTEM, unix.SOCK_DGRAM, 2) // SYSPROTO_CONTROL
	if err != nil {
		return -1, "", fmt.Errorf("socket: %w", err)
	}

	// Get the control ID for utun.
	type ctlInfo struct {
		ID   uint32
		Name [96]byte
	}
	var ci ctlInfo
	copy(ci.Name[:], utunControlName)

	// CTLIOCGINFO = 0xc0644e03
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), 0xc0644e03, uintptr(unsafe.Pointer(&ci)))
	if errno != 0 {
		unix.Close(fd)
		return -1, "", fmt.Errorf("CTLIOCGINFO: %w", errno)
	}

	// Connect to create the utun device.
	// struct sockaddr_ctl { sc_len, sc_family, ss_sysaddr, sc_id, sc_unit, sc_reserved[5] }
	type sockaddrCtl struct {
		Len     uint8
		Family  uint8
		SsType  uint16
		ID      uint32     // sc_id comes before sc_unit
		Unit    uint32
		Padding [5]uint32
	}
	sa := sockaddrCtl{
		Len:    32,
		Family: unix.AF_SYSTEM,
		SsType: 2, // AF_SYS_CONTROL
		ID:     ci.ID,
		Unit:   0, // auto-assign
	}

	_, _, errno = unix.Syscall(unix.SYS_CONNECT, uintptr(fd), uintptr(unsafe.Pointer(&sa)), 32)
	if errno != 0 {
		unix.Close(fd)
		return -1, "", fmt.Errorf("connect: %w", errno)
	}

	// Get the interface name.
	var ifname [16]byte
	ifnameLen := uint32(len(ifname))
	_, _, errno = unix.Syscall6(unix.SYS_GETSOCKOPT, uintptr(fd), 2, // SYSPROTO_CONTROL
		utunOptIfname, uintptr(unsafe.Pointer(&ifname[0])), uintptr(unsafe.Pointer(&ifnameLen)), 0)
	if errno != 0 {
		unix.Close(fd)
		return -1, "", fmt.Errorf("getsockopt UTUN_OPT_IFNAME: %w", errno)
	}

	name := string(ifname[:ifnameLen-1]) // trim null
	return fd, name, nil
}
