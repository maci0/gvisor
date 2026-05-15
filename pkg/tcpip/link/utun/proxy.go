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

package utun

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"math/rand"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/log"
)


// PacketInjector receives raw IP packets (no link header) for injection
// back into the network stack.
type PacketInjector interface {
	InjectPacket(ipPkt []byte)
}

// Proxy handles userspace NAT for outbound traffic. It intercepts outbound
// UDP/TCP/ICMP packets, opens host sockets, and injects replies back via
// the PacketInjector callback.
type Proxy struct {
	injector PacketInjector
	guestIP  net.IP
	hostIP   net.IP

	hostDNS string // host system's DNS server

	mu       sync.Mutex
	udpConns map[udpKey]*udpSession
	tcpConns map[tcpKey]*tcpSession
}

type udpKey struct {
	srcPort uint16
	dstIP   [4]byte
	dstPort uint16
}

type udpSession struct {
	conn    net.Conn
	created time.Time
}

// NewProxy creates a userspace NAT proxy. Reply packets are sent via the
// injector callback. For utun, the injector prepends the AF header and
// writes to the utun FD. For proxynet, it delivers directly to netstack.
func NewProxy(injector PacketInjector, guestIP, hostIP net.IP) *Proxy {
	p := &Proxy{
		injector: injector,
		guestIP:  guestIP,
		hostIP:   hostIP,
		udpConns: make(map[udpKey]*udpSession),
	}
	// Read host DNS servers for DNS query forwarding.
	if data, err := os.ReadFile("/etc/resolv.conf"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "nameserver") {
				parts := strings.Fields(line)
				if len(parts) >= 2 && parts[1] != "127.0.0.1" {
					p.hostDNS = parts[1]
					break
				}
			}
		}
	}
	if p.hostDNS == "" {
		p.hostDNS = "8.8.8.8"
	}
	log.Infof("proxy: using host DNS %s", p.hostDNS)
	return p
}

// handleOutbound inspects an outbound IP packet. If it's destined for
// the internet (not the gateway subnet), it proxies via host sockets.
// Returns true if the packet was handled (caller should NOT write to utun).
func (p *Proxy) HandleOutbound(ipPkt []byte) bool {
	if len(ipPkt) < 20 {
		return false
	}
	version := ipPkt[0] >> 4
	if version != 4 {
		return false
	}

	ihl := int(ipPkt[0]&0xf) * 4
	if len(ipPkt) < ihl {
		return false
	}

	dstIP := net.IP(ipPkt[16:20])
	protocol := ipPkt[9]

	// Don't proxy gateway traffic (local subnet)
	if dstIP.Equal(p.hostIP) {
		return false
	}

	switch protocol {
	case 17: // UDP
		return p.handleUDP(ipPkt, ihl, dstIP)
	case 6: // TCP
		return p.handleTCP(ipPkt, ihl, dstIP)
	case 1: // ICMP
		return p.handleICMP(ipPkt, ihl, dstIP)
	}
	return false
}

func (p *Proxy) handleUDP(ipPkt []byte, ihl int, dstIP net.IP) bool {
	if len(ipPkt) < ihl+8 {
		return false
	}
	srcPort := binary.BigEndian.Uint16(ipPkt[ihl:])
	dstPort := binary.BigEndian.Uint16(ipPkt[ihl+2:])
	payload := ipPkt[ihl+8:]

	key := udpKey{srcPort: srcPort, dstPort: dstPort}
	copy(key.dstIP[:], dstIP.To4())

	p.mu.Lock()
	sess, ok := p.udpConns[key]
	// Expire stale sessions (readLoop may have exited).
	if ok && time.Since(sess.created) > 25*time.Second {
		sess.conn.Close()
		delete(p.udpConns, key)
		ok = false
	}
	if !ok {
		dialIP := dstIP
		if dstPort == 53 && p.hostDNS != "" {
			dialIP = net.ParseIP(p.hostDNS)
		}
		addr := fmt.Sprintf("%s:%d", dialIP, dstPort)
		conn, err := net.DialTimeout("udp4", addr, 5*time.Second)
		if err != nil {
			p.mu.Unlock()
			log.Debugf("proxy udp dial %s: %v", addr, err)
			return false
		}
		sess = &udpSession{conn: conn, created: time.Now()}
		p.udpConns[key] = sess
		p.mu.Unlock()

		go p.udpReadLoop(key, sess, srcPort, dstIP, dstPort)
	} else {
		p.mu.Unlock()
	}

	sess.conn.Write(payload)
	return true
}

func (p *Proxy) udpReadLoop(key udpKey, sess *udpSession, guestPort uint16, origDstIP net.IP, origDstPort uint16) {
	defer func() {
		sess.conn.Close()
		p.mu.Lock()
		delete(p.udpConns, key)
		p.mu.Unlock()
	}()

	buf := make([]byte, 65536)
	for {
		sess.conn.SetReadDeadline(time.Now().Add(30 * time.Second))
		n, err := sess.conn.Read(buf)
		if err != nil {
			return
		}
		p.injectUDPReply(origDstIP, origDstPort, p.guestIP, guestPort, buf[:n])

	}
}

func (p *Proxy) injectUDPReply(srcIP net.IP, srcPort uint16, dstIP net.IP, dstPort uint16, payload []byte) {
	ipLen := 20 + 8 + len(payload)
	ip := make([]byte, ipLen)
	ip[0] = 0x45
	binary.BigEndian.PutUint16(ip[2:], uint16(ipLen))
	ip[8] = 64
	ip[9] = 17 // UDP
	copy(ip[12:16], srcIP.To4())
	copy(ip[16:20], dstIP.To4())
	binary.BigEndian.PutUint16(ip[10:], ipChecksum(ip[:20]))

	udp := ip[20:]
	binary.BigEndian.PutUint16(udp[0:], srcPort)
	binary.BigEndian.PutUint16(udp[2:], dstPort)
	binary.BigEndian.PutUint16(udp[4:], uint16(8+len(payload)))
	copy(udp[8:], payload)

	p.injector.InjectPacket(ip)
}

type tcpKey struct {
	srcPort uint16
	dstIP   [4]byte
	dstPort uint16
}

type tcpSession struct {
	conn     net.Conn
	writer   *bufio.Writer // coalesces small writes into larger TLS records
	mu       sync.Mutex
	ackCh    chan struct{} // signaled when guestAck advances
	mySeq    uint32       // our (proxy's) next sequence number to send to guest
	peerAck  uint32       // next seq we expect from the guest (= our ACK value)
	guestAck uint32       // last ACK the guest sent (how far it's consumed our data)
	closed   bool
}

func (p *Proxy) handleTCP(ipPkt []byte, ihl int, dstIP net.IP) bool {
	if len(ipPkt) < ihl+20 {
		return false
	}
	tcpHdr := ipPkt[ihl:]
	srcPort := binary.BigEndian.Uint16(tcpHdr[0:])
	dstPort := binary.BigEndian.Uint16(tcpHdr[2:])
	guestSeq := binary.BigEndian.Uint32(tcpHdr[4:])
	flags := tcpHdr[13]
	dataOff := int(tcpHdr[12]>>4) * 4

	key := tcpKey{srcPort: srcPort, dstPort: dstPort}
	copy(key.dstIP[:], dstIP.To4())

	isSYN := flags&0x02 != 0
	isACK := flags&0x10 != 0
	isFIN := flags&0x01 != 0
	isRST := flags&0x04 != 0

	p.mu.Lock()
	if p.tcpConns == nil {
		p.tcpConns = make(map[tcpKey]*tcpSession)
	}
	sess, exists := p.tcpConns[key]
	p.mu.Unlock()

	if isSYN && !isACK {
		if exists {
			sess.mu.Lock()
			p.injectTCPPacket(dstIP, dstPort, p.guestIP, srcPort,
				sess.guestAck-1, guestSeq+1, 0x12, nil)
			sess.mu.Unlock()
			return true
		}
		addr := fmt.Sprintf("%s:%d", dstIP, dstPort)
		conn, err := net.DialTimeout("tcp4", addr, 30*time.Second)
		if err != nil {
			log.Debugf("proxy tcp dial %s: %v", addr, err)
			p.injectTCPReset(dstIP, dstPort, p.guestIP, srcPort, guestSeq)
			return true
		}

		isn := uint32(rand.Intn(0x7FFFFFFF)) + 1
		sess = &tcpSession{
			conn:     conn,
			writer:   bufio.NewWriterSize(conn, 16384),
			mySeq:    isn,
			peerAck:  guestSeq + 1,
			guestAck: isn,
			ackCh:    make(chan struct{}, 1),
		}
		p.mu.Lock()
		p.tcpConns[key] = sess
		p.mu.Unlock()

		// SYN-ACK: seq = ISN-1 (SYN consumes one seq number)
		p.injectTCPPacket(dstIP, dstPort, p.guestIP, srcPort,
			isn-1, sess.peerAck, 0x12, nil)

		go p.tcpReadLoop(key, sess, dstIP, dstPort, srcPort)
		return true
	}

	if !exists {
		return false
	}

	sess.mu.Lock()
	defer sess.mu.Unlock()

	if isRST {
		sess.closed = true
		sess.conn.Close()
		p.mu.Lock()
		delete(p.tcpConns, key)
		p.mu.Unlock()
		return true
	}

	if isFIN {
		sess.peerAck = guestSeq + 1
		sess.writer.Flush()
		sess.conn.Close()
		// FIN-ACK
		p.injectTCPPacket(dstIP, dstPort, p.guestIP, srcPort,
			sess.mySeq, sess.peerAck, 0x11, nil)
		sess.closed = true
		p.mu.Lock()
		delete(p.tcpConns, key)
		p.mu.Unlock()
		return true
	}

	// Update guest's ACK (how much of our data it has consumed)
	guestAckVal := binary.BigEndian.Uint32(tcpHdr[8:])
	if isACK {
		sess.guestAck = guestAckVal
		select {
		case sess.ackCh <- struct{}{}:
		default:
		}
	}

	// Data or pure ACK
	isPSH := flags&0x08 != 0
	if ihl+dataOff < len(ipPkt) {
		payload := ipPkt[ihl+dataOff:]
		if len(payload) > 0 {
			if guestSeq == sess.peerAck {
				sess.writer.Write(payload)
				if isPSH {
					sess.writer.Flush()
				}
				sess.peerAck += uint32(len(payload))
			}
			p.injectTCPPacket(dstIP, dstPort, p.guestIP, srcPort,
				sess.mySeq, sess.peerAck, 0x10, nil)
		}
	}
	return true
}

func (p *Proxy) tcpReadLoop(key tcpKey, sess *tcpSession, origDstIP net.IP, origDstPort, guestPort uint16) {
	defer func() {
		sess.mu.Lock()
		if !sess.closed {
			sess.closed = true
			p.injectTCPPacket(origDstIP, origDstPort, p.guestIP, guestPort,
				sess.mySeq, sess.peerAck, 0x11, nil) // FIN
		}
		sess.mu.Unlock()
		sess.conn.Close()
		p.mu.Lock()
		delete(p.tcpConns, key)
		p.mu.Unlock()
	}()

	buf := make([]byte, 32768)
	for {
		// Pace delivery: don't send too far ahead of what guest has ACKed.
		for {
			sess.mu.Lock()
			inflight := sess.mySeq - sess.guestAck
			closed := sess.closed
			sess.mu.Unlock()
			if inflight <= 262144 || closed {
				break
			}
			select {
			case <-sess.ackCh:
			case <-time.After(50 * time.Millisecond):
			}
		}

		sess.conn.SetReadDeadline(time.Now().Add(120 * time.Second))
		n, err := sess.conn.Read(buf)
		if n > 0 {
			sess.mu.Lock()
			// Split large reads into MSS-sized segments.
			const mss = 1400
			for off := 0; off < n; off += mss {
				end := off + mss
				if end > n {
					end = n
				}
				flags := byte(0x10) // ACK
				if end == n {
					flags = 0x18 // ACK+PSH on last segment
				}
				p.injectTCPPacket(origDstIP, origDstPort, p.guestIP, guestPort,
					sess.mySeq, sess.peerAck, flags, buf[off:end])
				sess.mySeq += uint32(end - off)
			}
			sess.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

func (p *Proxy) injectTCPReset(srcIP net.IP, srcPort uint16, dstIP net.IP, dstPort uint16, ackSeq uint32) {
	p.injectTCPPacket(srcIP, srcPort, dstIP, dstPort, 0, ackSeq+1, 0x14, nil)
}

func (p *Proxy) injectTCPPacket(srcIP net.IP, srcPort uint16, dstIP net.IP, dstPort uint16, seq, ack uint32, flags byte, payload []byte) {
	tcpLen := 20 + len(payload)
	ipLen := 20 + tcpLen

	ip := make([]byte, ipLen)
	ip[0] = 0x45
	binary.BigEndian.PutUint16(ip[2:], uint16(ipLen))
	ip[8] = 64
	ip[9] = 6 // TCP
	copy(ip[12:16], srcIP.To4())
	copy(ip[16:20], dstIP.To4())

	tcp := ip[20:]
	binary.BigEndian.PutUint16(tcp[0:], srcPort)
	binary.BigEndian.PutUint16(tcp[2:], dstPort)
	binary.BigEndian.PutUint32(tcp[4:], seq)
	binary.BigEndian.PutUint32(tcp[8:], ack)
	tcp[12] = 5 << 4
	tcp[13] = flags
	binary.BigEndian.PutUint16(tcp[14:], 65535)
	if len(payload) > 0 {
		copy(tcp[20:], payload)
	}

	binary.BigEndian.PutUint16(tcp[16:], tcpChecksum(srcIP.To4(), dstIP.To4(), tcp[:tcpLen]))
	binary.BigEndian.PutUint16(ip[10:], ipChecksum(ip[:20]))

	p.injector.InjectPacket(ip)
}

func tcpChecksum(srcIP, dstIP []byte, tcpSeg []byte) uint16 {
	var sum uint32
	// Pseudo-header
	sum += uint32(binary.BigEndian.Uint16(srcIP[0:2]))
	sum += uint32(binary.BigEndian.Uint16(srcIP[2:4]))
	sum += uint32(binary.BigEndian.Uint16(dstIP[0:2]))
	sum += uint32(binary.BigEndian.Uint16(dstIP[2:4]))
	sum += 6 // protocol TCP
	sum += uint32(len(tcpSeg))
	// TCP segment (with checksum field zeroed)
	for i := 0; i < len(tcpSeg)-1; i += 2 {
		if i == 16 {
			continue // skip checksum field
		}
		sum += uint32(binary.BigEndian.Uint16(tcpSeg[i:]))
	}
	if len(tcpSeg)%2 == 1 {
		sum += uint32(tcpSeg[len(tcpSeg)-1]) << 8
	}
	for sum > 0xffff {
		sum = (sum >> 16) + (sum & 0xffff)
	}
	return ^uint16(sum)
}

func (p *Proxy) handleICMP(ipPkt []byte, ihl int, dstIP net.IP) bool {
	if len(ipPkt) < ihl+8 {
		return false
	}
	// ICMP echo request
	if ipPkt[ihl] != 8 { // type 8 = echo request
		return false
	}

	// Make a copy of the ICMP payload since ipPkt may be reused.
	icmpPayload := make([]byte, len(ipPkt)-ihl)
	copy(icmpPayload, ipPkt[ihl:])

	go func() {
		// Use SOCK_DGRAM ICMP socket instead of raw socket.
		// macOS supports unprivileged ICMP via SOCK_DGRAM — no root needed.
		fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, unix.IPPROTO_ICMP)
		if err != nil {
			log.Debugf("proxy icmp socket: %v", err)
			return
		}
		defer unix.Close(fd)

		// Set send/receive timeout.
		tv := unix.Timeval{Sec: 5}
		unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv)
		unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_SNDTIMEO, &tv)

		dst := &unix.SockaddrInet4{}
		copy(dst.Addr[:], dstIP.To4())

		// Send the ICMP echo request (type 8).
		log.Debugf("proxy icmp: sending %d bytes to %s (type=%d)", len(icmpPayload), dstIP, icmpPayload[0])
		if err := unix.Sendto(fd, icmpPayload, 0, dst); err != nil {
			log.Debugf("proxy icmp sendto %s: %v", dstIP, err)
			return
		}

		// Read reply.
		buf := make([]byte, 1500)
		n, from, err := unix.Recvfrom(fd, buf, 0)
		if err != nil {
			log.Debugf("proxy icmp recvfrom: %v", err)
			return
		}
		reply := buf[:n]

		// macOS SOCK_DGRAM ICMP returns the full IP+ICMP packet.
		// Strip the IP header to get just the ICMP payload.
		if n >= 20 && (reply[0]>>4) == 4 {
			ipHdrLen := int(reply[0]&0x0f) * 4
			if ipHdrLen <= n {
				reply = reply[ipHdrLen:]
			}
		}
		log.Debugf("proxy icmp: received %d bytes from %v, ICMP %d bytes (type=%d code=%d)", n, from, len(reply), reply[0], reply[1])

		// Inject ICMP reply.
		p.injectICMPReply(dstIP, p.guestIP, reply)
	}()
	return true
}

func (p *Proxy) injectICMPReply(srcIP, dstIP net.IP, icmpData []byte) {
	ipLen := 20 + len(icmpData)
	ip := make([]byte, ipLen)
	ip[0] = 0x45
	binary.BigEndian.PutUint16(ip[2:], uint16(ipLen))
	ip[8] = 64
	ip[9] = 1 // ICMP
	copy(ip[12:16], srcIP.To4())
	copy(ip[16:20], dstIP.To4())
	binary.BigEndian.PutUint16(ip[10:], ipChecksum(ip[:20]))
	copy(ip[20:], icmpData)

	p.injector.InjectPacket(ip)
}

func ipChecksum(hdr []byte) uint16 {
	var sum uint32
	for i := 0; i < len(hdr)-1; i += 2 {
		if i == 10 {
			continue // skip checksum field
		}
		sum += uint32(binary.BigEndian.Uint16(hdr[i:]))
	}
	for sum > 0xffff {
		sum = (sum >> 16) + (sum & 0xffff)
	}
	return ^uint16(sum)
}

// cleanupStale removes old UDP sessions.
func (p *Proxy) cleanupStale() {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	for k, s := range p.udpConns {
		if now.Sub(s.created) > 5*time.Minute {
			s.conn.Close()
			delete(p.udpConns, k)
		}
	}
}
