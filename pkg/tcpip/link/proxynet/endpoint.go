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

// Package proxynet provides a link-layer endpoint that forwards guest
// TCP/UDP/ICMP traffic through host sockets. No root privileges or
// external daemons are required — all networking is handled in pure
// userspace via the utun proxy.
package proxynet

import (
	"net"
	"sync"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/utun"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

const defaultMTU = 1500

// Endpoint implements stack.LinkEndpoint by forwarding all outbound
// packets through a userspace proxy. Reply packets are injected back
// into netstack via DeliverNetworkPacket.
type Endpoint struct {
	mtu      uint32
	closed   chan struct{}
	once     sync.Once
	dispatch stack.NetworkDispatcher
	proxy    *utun.Proxy
}

// New creates a proxynet endpoint. All guest traffic is proxied through
// host sockets — no TUN device, no vmnet, no root required.
func New(guestIP, hostIP net.IP) *Endpoint {
	ep := &Endpoint{
		mtu:    defaultMTU,
		closed: make(chan struct{}),
	}
	ep.proxy = utun.NewProxy(ep, guestIP, hostIP)
	return ep
}

// InjectPacket implements utun.PacketInjector. Called by the proxy when
// it has a reply packet to deliver back to the guest.
func (e *Endpoint) InjectPacket(ipPkt []byte) {
	if e.dispatch == nil || len(ipPkt) < 1 {
		return
	}

	var proto tcpip.NetworkProtocolNumber
	switch ipPkt[0] >> 4 {
	case 4:
		proto = header.IPv4ProtocolNumber
	case 6:
		proto = header.IPv6ProtocolNumber
	default:
		return
	}

	pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
		Payload: buffer.MakeWithData(ipPkt),
	})
	e.dispatch.DeliverNetworkPacket(proto, pkt)
	pkt.DecRef()
}

func (e *Endpoint) MTU() uint32                                          { return e.mtu }
func (e *Endpoint) SetMTU(mtu uint32)                                    { e.mtu = mtu }
func (e *Endpoint) MaxHeaderLength() uint16                              { return 0 }
func (e *Endpoint) LinkAddress() tcpip.LinkAddress                       { return "" }
func (e *Endpoint) SetLinkAddress(tcpip.LinkAddress)                     {}
func (e *Endpoint) Capabilities() stack.LinkEndpointCapabilities         { return 0 }
func (e *Endpoint) IsAttached() bool                                     { return e.dispatch != nil }
func (e *Endpoint) Wait()                                                { <-e.closed }
func (e *Endpoint) ARPHardwareType() header.ARPHardwareType              { return header.ARPHardwareNone }
func (e *Endpoint) AddHeader(*stack.PacketBuffer)                        {}
func (e *Endpoint) ParseHeader(*stack.PacketBuffer) bool                 { return true }
func (e *Endpoint) SetOnCloseAction(func())                              {}

// Attach implements stack.LinkEndpoint.
func (e *Endpoint) Attach(dispatcher stack.NetworkDispatcher) {
	e.dispatch = dispatcher
}

// Close implements stack.LinkEndpoint.
func (e *Endpoint) Close() {
	e.once.Do(func() { close(e.closed) })
}

// WritePackets implements stack.LinkEndpoint.
func (e *Endpoint) WritePackets(pkts stack.PacketBufferList) (int, tcpip.Error) {
	var n int
	for _, pkt := range pkts.AsSlice() {
		views := pkt.AsSlices()
		var ipPkt []byte
		for _, v := range views {
			ipPkt = append(ipPkt, v...)
		}

		if e.proxy.HandleOutbound(ipPkt) {
			n++
			continue
		}

		log.Debugf("proxynet: unhandled packet (len=%d)", len(ipPkt))
		n++
	}
	return n, nil
}
