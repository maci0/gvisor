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

// proxy handles userspace NAT for outbound traffic. macOS pfctl NAT
// can translate outbound packets but doesn't route de-NAT'd replies
// back through the utun device. This proxy intercepts outbound UDP/TCP
// packets, opens host sockets, and injects replies back into the utun.
type proxy struct {
	fd      int // utun fd for injecting reply packets
	guestIP net.IP
	hostIP  net.IP

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

func newProxy(fd int, guestIP, hostIP net.IP) *proxy {
	p := &proxy{
		fd:       fd,
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
func (p *proxy) handleOutbound(ipPkt []byte) bool {
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

func (p *proxy) handleUDP(ipPkt []byte, ihl int, dstIP net.IP) bool {
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

func (p *proxy) udpReadLoop(key udpKey, sess *udpSession, guestPort uint16, origDstIP net.IP, origDstPort uint16) {
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

func (p *proxy) injectUDPReply(srcIP net.IP, srcPort uint16, dstIP net.IP, dstPort uint16, payload []byte) {
	// Build IP + UDP packet
	ipLen := 20 + 8 + len(payload)
	pkt := make([]byte, 4+ipLen) // 4-byte utun header + IP packet

	// utun AF header
	binary.BigEndian.PutUint32(pkt[:4], unix.AF_INET)

	ip := pkt[4:]
	ip[0] = 0x45 // Version 4, IHL 5
	binary.BigEndian.PutUint16(ip[2:], uint16(ipLen))
	ip[8] = 64 // TTL
	ip[9] = 17 // UDP
	copy(ip[12:16], srcIP.To4())
	copy(ip[16:20], dstIP.To4())
	// IP checksum
	binary.BigEndian.PutUint16(ip[10:], ipChecksum(ip[:20]))

	// UDP header
	udp := ip[20:]
	binary.BigEndian.PutUint16(udp[0:], srcPort)
	binary.BigEndian.PutUint16(udp[2:], dstPort)
	binary.BigEndian.PutUint16(udp[4:], uint16(8+len(payload)))
	copy(udp[8:], payload)

	unix.Write(p.fd, pkt)
}

type tcpKey struct {
	srcPort uint16
	dstIP   [4]byte
	dstPort uint16
}

type tcpSession struct {
	conn      net.Conn
	mu        sync.Mutex
	mySeq     uint32 // our (proxy's) next sequence number to send to guest
	peerAck   uint32 // next seq we expect from the guest (= our ACK value)
	guestAck  uint32 // last ACK the guest sent (how far it's consumed our data)
	closed    bool
}

func (p *proxy) handleTCP(ipPkt []byte, ihl int, dstIP net.IP) bool {
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
			mySeq:    isn,
			peerAck:  guestSeq + 1,
			guestAck: isn,
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
	}

	// Data or pure ACK
	if ihl+dataOff < len(ipPkt) {
		payload := ipPkt[ihl+dataOff:]
		if len(payload) > 0 {
			if guestSeq == sess.peerAck {
				sess.conn.Write(payload)
				sess.peerAck += uint32(len(payload))
			}
			p.injectTCPPacket(dstIP, dstPort, p.guestIP, srcPort,
				sess.mySeq, sess.peerAck, 0x10, nil)
		}
	}
	return true
}

func (p *proxy) tcpReadLoop(key tcpKey, sess *tcpSession, origDstIP net.IP, origDstPort, guestPort uint16) {
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

	buf := make([]byte, 1400)
	for {
		// Pace delivery: don't send too far ahead of what guest has ACKed.
		for i := 0; i < 100; i++ {
			sess.mu.Lock()
			inflight := sess.mySeq - sess.guestAck
			sess.mu.Unlock()
			if inflight <= 16*1400 {
				break
			}
			time.Sleep(500 * time.Microsecond)
		}

		sess.conn.SetReadDeadline(time.Now().Add(120 * time.Second))
		n, err := sess.conn.Read(buf)
		if n > 0 {
			sess.mu.Lock()
			p.injectTCPPacket(origDstIP, origDstPort, p.guestIP, guestPort,
				sess.mySeq, sess.peerAck, 0x18, buf[:n])
			sess.mySeq += uint32(n)
			sess.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

func (p *proxy) injectTCPReset(srcIP net.IP, srcPort uint16, dstIP net.IP, dstPort uint16, ackSeq uint32) {
	p.injectTCPPacket(srcIP, srcPort, dstIP, dstPort, 0, ackSeq+1, 0x14, nil)
}

func (p *proxy) injectTCPPacket(srcIP net.IP, srcPort uint16, dstIP net.IP, dstPort uint16, seq, ack uint32, flags byte, payload []byte) {
	tcpLen := 20 + len(payload)
	ipLen := 20 + tcpLen
	pkt := make([]byte, 4+ipLen)

	binary.BigEndian.PutUint32(pkt[:4], unix.AF_INET)

	ip := pkt[4:]
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
	tcp[12] = 5 << 4 // data offset = 5 (20 bytes)
	tcp[13] = flags
	binary.BigEndian.PutUint16(tcp[14:], 65535) // window
	if len(payload) > 0 {
		copy(tcp[20:], payload)
	}

	// TCP checksum (pseudo-header + TCP)
	binary.BigEndian.PutUint16(tcp[16:], tcpChecksum(srcIP.To4(), dstIP.To4(), tcp[:tcpLen]))

	// IP checksum
	binary.BigEndian.PutUint16(ip[10:], ipChecksum(ip[:20]))

	unix.Write(p.fd, pkt)
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

func (p *proxy) handleICMP(ipPkt []byte, ihl int, dstIP net.IP) bool {
	if len(ipPkt) < ihl+8 {
		return false
	}
	// ICMP echo request
	if ipPkt[ihl] != 8 { // type 8 = echo request
		return false
	}

	go func() {
		conn, err := net.DialTimeout("ip4:icmp", dstIP.String(), 5*time.Second)
		if err != nil {
			log.Debugf("proxy icmp dial %s: %v", dstIP, err)
			return
		}
		defer conn.Close()

		// Send the ICMP payload (without IP header)
		icmpPayload := ipPkt[ihl:]
		conn.Write(icmpPayload)

		// Read reply
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		buf := make([]byte, 1500)
		n, err := conn.Read(buf)
		if err != nil {
			return
		}

		// Inject ICMP reply
		p.injectICMPReply(dstIP, p.guestIP, buf[:n])
	}()
	return true
}

func (p *proxy) injectICMPReply(srcIP, dstIP net.IP, icmpData []byte) {
	ipLen := 20 + len(icmpData)
	pkt := make([]byte, 4+ipLen)

	binary.BigEndian.PutUint32(pkt[:4], unix.AF_INET)

	ip := pkt[4:]
	ip[0] = 0x45
	binary.BigEndian.PutUint16(ip[2:], uint16(ipLen))
	ip[8] = 64
	ip[9] = 1 // ICMP
	copy(ip[12:16], srcIP.To4())
	copy(ip[16:20], dstIP.To4())
	binary.BigEndian.PutUint16(ip[10:], ipChecksum(ip[:20]))

	copy(ip[20:], icmpData)

	unix.Write(p.fd, pkt)
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
func (p *proxy) cleanupStale() {
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
