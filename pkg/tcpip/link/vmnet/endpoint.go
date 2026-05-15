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

// Package vmnet provides a link-layer endpoint backed by a socket_vmnet
// daemon connection. socket_vmnet exposes macOS vmnet.framework networking
// over a Unix domain socket, enabling rootless host networking for VMs.
//
// Wire protocol: length-prefixed L2 Ethernet frames.
//
//	[4-byte big-endian uint32: frame length]
//	[variable: raw Ethernet frame]
package vmnet

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"sync"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

const defaultMTU = 1500

// DefaultSocketPaths lists common socket_vmnet socket locations.
var DefaultSocketPaths = []string{
	"/opt/homebrew/var/run/socket_vmnet", // Homebrew ARM
	"/usr/local/var/run/socket_vmnet",    // Homebrew Intel
	"/var/run/socket_vmnet",              // manual install
}

// Endpoint is a link-layer endpoint connected to a socket_vmnet daemon.
// It exchanges length-prefixed L2 Ethernet frames over a Unix socket.
type Endpoint struct {
	conn     net.Conn
	mac      tcpip.LinkAddress
	mtu      uint32
	closed   chan struct{}
	once     sync.Once
	dispatch stack.NetworkDispatcher
}

// New connects to a socket_vmnet daemon at the given Unix socket path.
// If socketPath is empty, it tries DefaultSocketPaths.
func New(socketPath string, mac tcpip.LinkAddress) (*Endpoint, error) {
	var conn net.Conn
	var err error

	if socketPath != "" {
		conn, err = net.Dial("unix", socketPath)
		if err != nil {
			return nil, fmt.Errorf("vmnet: connect %s: %w", socketPath, err)
		}
	} else {
		for _, p := range DefaultSocketPaths {
			conn, err = net.Dial("unix", p)
			if err == nil {
				socketPath = p
				break
			}
		}
		if conn == nil {
			return nil, fmt.Errorf("vmnet: no socket_vmnet socket found (tried %v); install with: brew install socket_vmnet && sudo brew services start socket_vmnet", DefaultSocketPaths)
		}
	}

	log.Infof("vmnet: connected to %s", socketPath)

	return &Endpoint{
		conn:   conn,
		mac:    mac,
		mtu:    defaultMTU,
		closed: make(chan struct{}),
	}, nil
}

// GenerateMAC creates a locally-administered MAC address from a seed string.
func GenerateMAC(seed string) tcpip.LinkAddress {
	mac := [6]byte{0x02, 0x00, 0x00, 0x00, 0x00, 0x00} // locally administered
	for i, b := range []byte(seed) {
		mac[1+(i%5)] ^= b
	}
	return tcpip.LinkAddress(mac[:])
}

// FindSocket returns the first accessible socket_vmnet socket path.
func FindSocket() string {
	for _, p := range DefaultSocketPaths {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// MTU implements stack.LinkEndpoint.
func (e *Endpoint) MTU() uint32 { return e.mtu }

// SetMTU implements stack.LinkEndpoint.
func (e *Endpoint) SetMTU(mtu uint32) { e.mtu = mtu }

// MaxHeaderLength implements stack.LinkEndpoint.
func (e *Endpoint) MaxHeaderLength() uint16 {
	return header.EthernetMinimumSize
}

// LinkAddress implements stack.LinkEndpoint.
func (e *Endpoint) LinkAddress() tcpip.LinkAddress { return e.mac }

// SetLinkAddress implements stack.LinkEndpoint.
func (e *Endpoint) SetLinkAddress(addr tcpip.LinkAddress) { e.mac = addr }

// Capabilities implements stack.LinkEndpoint.
func (e *Endpoint) Capabilities() stack.LinkEndpointCapabilities {
	return stack.CapabilityResolutionRequired
}

// Attach implements stack.LinkEndpoint.
func (e *Endpoint) Attach(dispatcher stack.NetworkDispatcher) {
	e.dispatch = dispatcher
	if dispatcher != nil {
		go e.readLoop()
	}
}

// IsAttached implements stack.LinkEndpoint.
func (e *Endpoint) IsAttached() bool { return e.dispatch != nil }

// Wait implements stack.LinkEndpoint.
func (e *Endpoint) Wait() { <-e.closed }

// ARPHardwareType implements stack.LinkEndpoint.
func (e *Endpoint) ARPHardwareType() header.ARPHardwareType {
	return header.ARPHardwareEther
}

// AddHeader implements stack.LinkEndpoint.
func (e *Endpoint) AddHeader(pkt *stack.PacketBuffer) {
	eth := header.Ethernet(pkt.LinkHeader().Push(header.EthernetMinimumSize))
	fields := header.EthernetFields{
		SrcAddr: e.mac,
		DstAddr: pkt.EgressRoute.RemoteLinkAddress,
		Type:    pkt.NetworkProtocolNumber,
	}
	eth.Encode(&fields)
}

// ParseHeader implements stack.LinkEndpoint.
func (e *Endpoint) ParseHeader(pkt *stack.PacketBuffer) bool {
	_, ok := pkt.LinkHeader().Consume(header.EthernetMinimumSize)
	return ok
}

// SetOnCloseAction implements stack.LinkEndpoint.
func (e *Endpoint) SetOnCloseAction(func()) {}

// Close implements stack.LinkEndpoint.
func (e *Endpoint) Close() {
	e.once.Do(func() {
		e.conn.Close()
		close(e.closed)
	})
}

// WritePackets implements stack.LinkEndpoint.
func (e *Endpoint) WritePackets(pkts stack.PacketBufferList) (int, tcpip.Error) {
	var n int
	for _, pkt := range pkts.AsSlice() {
		if err := e.writeFrame(pkt); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

func (e *Endpoint) writeFrame(pkt *stack.PacketBuffer) tcpip.Error {
	views := pkt.AsSlices()
	var frameLen uint32
	for _, v := range views {
		frameLen += uint32(len(v))
	}

	// Length-prefixed frame: [4-byte len][Ethernet frame]
	buf := make([]byte, 4+frameLen)
	binary.BigEndian.PutUint32(buf[:4], frameLen)
	offset := 4
	for _, v := range views {
		copy(buf[offset:], v)
		offset += len(v)
	}

	if _, err := e.conn.Write(buf); err != nil {
		log.Debugf("vmnet write: %v", err)
		return &tcpip.ErrAborted{}
	}
	log.Debugf("vmnet: sent %d-byte frame", frameLen)
	return nil
}

func (e *Endpoint) readLoop() {
	defer func() {
		e.once.Do(func() { close(e.closed) })
	}()

	var lenBuf [4]byte
	frameBuf := make([]byte, header.EthernetMinimumSize+e.mtu+100)

	for {
		// Read 4-byte length prefix.
		if _, err := io.ReadFull(e.conn, lenBuf[:]); err != nil {
			if err != io.EOF {
				log.Debugf("vmnet read len: %v", err)
			}
			return
		}
		frameLen := binary.BigEndian.Uint32(lenBuf[:])
		if frameLen > uint32(len(frameBuf)) {
			log.Debugf("vmnet: frame too large (%d bytes), dropping", frameLen)
			io.CopyN(io.Discard, e.conn, int64(frameLen))
			continue
		}

		// Read the Ethernet frame.
		if _, err := io.ReadFull(e.conn, frameBuf[:frameLen]); err != nil {
			log.Debugf("vmnet read frame: %v", err)
			return
		}

		if frameLen < header.EthernetMinimumSize {
			continue
		}

		pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
			Payload: buffer.MakeWithData(frameBuf[:frameLen]),
		})

		// Parse Ethernet header.
		hdr, ok := pkt.LinkHeader().Consume(header.EthernetMinimumSize)
		if !ok {
			pkt.DecRef()
			continue
		}
		eth := header.Ethernet(hdr)
		proto := eth.Type()

		dst := eth.DestinationAddress()
		if dst == header.EthernetBroadcastAddress {
			pkt.PktType = tcpip.PacketBroadcast
		} else if header.IsMulticastEthernetAddress(dst) {
			pkt.PktType = tcpip.PacketMulticast
		} else if dst == e.mac {
			pkt.PktType = tcpip.PacketHost
		} else {
			pkt.PktType = tcpip.PacketOtherHost
		}

		log.Debugf("vmnet: recv %d-byte frame, ethertype=0x%04x", frameLen, uint16(proto))
		e.dispatch.DeliverNetworkPacket(proto, pkt)
		pkt.DecRef()
	}
}
