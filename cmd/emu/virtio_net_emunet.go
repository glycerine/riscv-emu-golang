//go:build tsnet

package main

import (
	"encoding/binary"
	"net/netip"
	"time"
)

const (
	ipProtoICMP = byte(1)
	ipProtoTCP  = byte(6)

	icmpEchoReply   = byte(0)
	icmpEchoRequest = byte(8)

	emunetLocalPortID = "local"

	emunetICMPIdleTimeout = 30 * time.Second
	emunetUDPIdleTimeout  = 2 * time.Minute
	emunetTCPIdleTimeout  = 10 * time.Minute
)

var (
	emunetRouterIPv4 = netip.MustParseAddr("10.77.0.1")
	emunetDNSIPv4    = netip.MustParseAddr("100.100.100.100")
)

type emunetPort struct {
	id       string
	lease    netip.Addr
	guestMAC [6]byte
	emit     func([]byte)
}

type emunetNATOutKey struct {
	proto      byte
	portID     string
	guestIP    netip.Addr
	guestPort  uint16
	remoteIP   netip.Addr
	remotePort uint16
}

type emunetNATInKey struct {
	proto      byte
	external   uint16
	remoteIP   netip.Addr
	remotePort uint16
}

type emunetNATEntry struct {
	proto      byte
	portID     string
	guestIP    netip.Addr
	guestPort  uint16
	external   uint16
	remoteIP   netip.Addr
	remotePort uint16
	lastUsed   time.Time
}

func (s *tsnetVirtioStack) emunetPortLocked(id string, emit func([]byte)) *emunetPort {
	if s.ports == nil {
		s.ports = make(map[string]*emunetPort)
	}
	p := s.ports[id]
	if p == nil {
		p = &emunetPort{id: id}
		s.ports[id] = p
	}
	if emit != nil {
		p.emit = emit
	}
	if !p.lease.Is4() {
		p.lease = s.nextLeaseLocked()
	}
	return p
}

func (s *tsnetVirtioStack) nextLeaseLocked() netip.Addr {
	if s.nextLease < 2 || s.nextLease > 254 {
		s.nextLease = 2
	}
	octet := s.nextLease
	s.nextLease++
	if s.nextLease > 254 {
		s.nextLease = 2
	}
	return netip.AddrFrom4([4]byte{10, 77, 0, octet})
}

func (s *tsnetVirtioStack) portLease(id string, emit func([]byte)) netip.Addr {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.emunetPortLocked(id, emit).lease
}

func (s *tsnetVirtioStack) learnPortMAC(id string, mac [6]byte, emit func([]byte)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.emunetPortLocked(id, emit)
	p.guestMAC = mac
}

func (s *tsnetVirtioStack) removeEmunetPort(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.ports, id)
	for key, ent := range s.natByOut {
		if key.portID != id {
			continue
		}
		delete(s.natByOut, key)
		delete(s.natByIn, emunetNATInKey{
			proto:      ent.proto,
			external:   ent.external,
			remoteIP:   ent.remoteIP,
			remotePort: ent.remotePort,
		})
	}
}

func (s *tsnetVirtioStack) arpReplyForPort(portID string, req []byte, emit func([]byte)) []byte {
	if len(req) < 42 || binary.BigEndian.Uint16(req[14:16]) != 1 ||
		binary.BigEndian.Uint16(req[16:18]) != etherTypeIPv4 ||
		req[18] != 6 || req[19] != 4 ||
		binary.BigEndian.Uint16(req[20:22]) != 1 {
		return nil
	}
	if netip.AddrFrom4([4]byte{req[38], req[39], req[40], req[41]}) != emunetRouterIPv4 {
		return nil
	}
	var guestMAC [6]byte
	copy(guestMAC[:], req[22:28])
	s.learnPortMAC(portID, guestMAC, emit)

	router4 := emunetRouterIPv4.As4()
	reply := make([]byte, 42)
	copy(reply[0:6], req[6:12])
	copy(reply[6:12], emunetRouterMAC[:])
	binary.BigEndian.PutUint16(reply[12:14], etherTypeARP)
	copy(reply[14:20], req[14:20])
	binary.BigEndian.PutUint16(reply[20:22], 2)
	copy(reply[22:28], emunetRouterMAC[:])
	copy(reply[28:32], router4[:])
	copy(reply[32:38], req[22:28])
	copy(reply[38:42], req[28:32])
	return reply
}

func (s *tsnetVirtioStack) gatewayICMPEchoReply(portID string, frame []byte, emit func([]byte)) []byte {
	if len(frame) < 14+20 {
		return nil
	}
	ip := frame[14:]
	ihl := int(ip[0]&0x0f) * 4
	if ip[0]>>4 != 4 || ihl < 20 || len(ip) < ihl+8 || ip[9] != ipProtoICMP {
		return nil
	}
	totalLen := int(binary.BigEndian.Uint16(ip[2:4]))
	if totalLen < ihl+8 || totalLen > len(ip) {
		return nil
	}
	dst := netip.AddrFrom4([4]byte{ip[16], ip[17], ip[18], ip[19]})
	if dst != emunetRouterIPv4 || ip[ihl] != icmpEchoRequest {
		return nil
	}
	var guestMAC [6]byte
	copy(guestMAC[:], frame[6:12])
	s.learnPortMAC(portID, guestMAC, emit)

	reply := make([]byte, 14+totalLen)
	copy(reply[0:6], frame[6:12])
	copy(reply[6:12], emunetRouterMAC[:])
	binary.BigEndian.PutUint16(reply[12:14], etherTypeIPv4)
	rip := reply[14:]
	copy(rip, ip[:totalLen])
	copy(rip[12:16], ip[16:20])
	copy(rip[16:20], ip[12:16])
	rip[8] = 64
	rip[10], rip[11] = 0, 0
	binary.BigEndian.PutUint16(rip[10:12], ipv4HeaderChecksum(rip[:ihl]))
	icmp := rip[ihl:]
	icmp[0] = icmpEchoReply
	icmp[2], icmp[3] = 0, 0
	binary.BigEndian.PutUint16(icmp[2:4], internetChecksum(icmp[:totalLen-ihl]))
	return reply
}

func (s *tsnetVirtioStack) natOutbound(portID string, packet []byte, emit func([]byte)) []byte {
	tailIP, ok := s.tailscaleIPv4()
	if !ok || !tailIP.Is4() {
		return nil
	}
	return s.translateOutboundIPv4(portID, packet, tailIP, emit)
}

func (s *tsnetVirtioStack) translateOutboundIPv4(portID string, packet []byte, tailIP netip.Addr, emit func([]byte)) []byte {
	if len(packet) < 20 || packet[0]>>4 != 4 {
		return nil
	}
	ihl := int(packet[0]&0x0f) * 4
	if ihl < 20 || len(packet) < ihl {
		return nil
	}
	totalLen := int(binary.BigEndian.Uint16(packet[2:4]))
	if totalLen < ihl || totalLen > len(packet) {
		return nil
	}
	frag := binary.BigEndian.Uint16(packet[6:8])
	if frag&0x3fff != 0 {
		return nil
	}
	if packet[8] <= 1 {
		return nil
	}
	guestIP := netip.AddrFrom4([4]byte{packet[12], packet[13], packet[14], packet[15]})
	remoteIP := netip.AddrFrom4([4]byte{packet[16], packet[17], packet[18], packet[19]})
	out := append([]byte(nil), packet[:totalLen]...)
	out[8]--
	tail4 := tailIP.As4()
	copy(out[12:16], tail4[:])

	proto := out[9]
	switch proto {
	case ipProtoUDP:
		if totalLen < ihl+8 {
			return nil
		}
		udp := out[ihl:]
		guestPort := binary.BigEndian.Uint16(udp[0:2])
		remotePort := binary.BigEndian.Uint16(udp[2:4])
		ext := s.natExternalLocked(emunetNATOutKey{proto: proto, portID: portID, guestIP: guestIP, guestPort: guestPort, remoteIP: remoteIP, remotePort: remotePort})
		binary.BigEndian.PutUint16(udp[0:2], ext)
		if binary.BigEndian.Uint16(udp[6:8]) != 0 {
			udp[6], udp[7] = 0, 0
			binary.BigEndian.PutUint16(udp[6:8], transportChecksum(out[:ihl], udp, proto))
		}
	case ipProtoTCP:
		if totalLen < ihl+20 {
			return nil
		}
		tcp := out[ihl:]
		tcpHeaderLen := int(tcp[12]>>4) * 4
		if tcpHeaderLen < 20 || len(tcp) < tcpHeaderLen {
			return nil
		}
		guestPort := binary.BigEndian.Uint16(tcp[0:2])
		remotePort := binary.BigEndian.Uint16(tcp[2:4])
		ext := s.natExternalLocked(emunetNATOutKey{proto: proto, portID: portID, guestIP: guestIP, guestPort: guestPort, remoteIP: remoteIP, remotePort: remotePort})
		binary.BigEndian.PutUint16(tcp[0:2], ext)
		tcp[16], tcp[17] = 0, 0
		binary.BigEndian.PutUint16(tcp[16:18], transportChecksum(out[:ihl], tcp, proto))
	case ipProtoICMP:
		if totalLen < ihl+8 || out[ihl] != icmpEchoRequest {
			return nil
		}
		icmp := out[ihl:]
		guestID := binary.BigEndian.Uint16(icmp[4:6])
		ext := s.natExternalLocked(emunetNATOutKey{proto: proto, portID: portID, guestIP: guestIP, guestPort: guestID, remoteIP: remoteIP})
		binary.BigEndian.PutUint16(icmp[4:6], ext)
		icmp[2], icmp[3] = 0, 0
		binary.BigEndian.PutUint16(icmp[2:4], internetChecksum(icmp))
	default:
		return nil
	}
	out[10], out[11] = 0, 0
	binary.BigEndian.PutUint16(out[10:12], ipv4HeaderChecksum(out[:ihl]))
	return out
}

func (s *tsnetVirtioStack) natExternalLocked(key emunetNATOutKey) uint16 {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.nowTime()
	if s.natByOut == nil {
		s.natByOut = make(map[emunetNATOutKey]*emunetNATEntry)
		s.natByIn = make(map[emunetNATInKey]*emunetNATEntry)
	}
	s.cleanupExpiredNATLocked(now)
	if ent := s.natByOut[key]; ent != nil {
		ent.lastUsed = now
		return ent.external
	}
	ext := s.nextNATLocked()
	ent := &emunetNATEntry{
		proto:      key.proto,
		portID:     key.portID,
		guestIP:    key.guestIP,
		guestPort:  key.guestPort,
		external:   ext,
		remoteIP:   key.remoteIP,
		remotePort: key.remotePort,
		lastUsed:   now,
	}
	s.natByOut[key] = ent
	s.natByIn[emunetNATInKey{proto: key.proto, external: ext, remoteIP: key.remoteIP, remotePort: key.remotePort}] = ent
	return ext
}

func (s *tsnetVirtioStack) nextNATLocked() uint16 {
	if s.nextNATID < 40000 || s.nextNATID > 60999 {
		s.nextNATID = 40000
	}
	for {
		ext := s.nextNATID
		s.nextNATID++
		if s.nextNATID > 60999 {
			s.nextNATID = 40000
		}
		used := false
		for key := range s.natByIn {
			if key.external == ext {
				used = true
				break
			}
		}
		if !used {
			return ext
		}
	}
}

func (s *tsnetVirtioStack) natInbound(packet []byte) (string, [6]byte, []byte, func([]byte), bool) {
	tailIP, ok := s.tailscaleIPv4()
	if !ok || !tailIP.Is4() || len(packet) < 20 || packet[0]>>4 != 4 {
		return "", [6]byte{}, nil, nil, false
	}
	ihl := int(packet[0]&0x0f) * 4
	totalLen := int(binary.BigEndian.Uint16(packet[2:4]))
	if ihl < 20 || totalLen < ihl || totalLen > len(packet) {
		return "", [6]byte{}, nil, nil, false
	}
	dst := netip.AddrFrom4([4]byte{packet[16], packet[17], packet[18], packet[19]})
	if dst != tailIP {
		return "", [6]byte{}, nil, nil, false
	}
	proto := packet[9]
	remoteIP := netip.AddrFrom4([4]byte{packet[12], packet[13], packet[14], packet[15]})
	var key emunetNATInKey
	switch proto {
	case ipProtoUDP:
		if totalLen < ihl+8 {
			return "", [6]byte{}, nil, nil, false
		}
		udp := packet[ihl:]
		key = emunetNATInKey{proto: proto, external: binary.BigEndian.Uint16(udp[2:4]), remoteIP: remoteIP, remotePort: binary.BigEndian.Uint16(udp[0:2])}
	case ipProtoTCP:
		if totalLen < ihl+20 {
			return "", [6]byte{}, nil, nil, false
		}
		tcp := packet[ihl:]
		key = emunetNATInKey{proto: proto, external: binary.BigEndian.Uint16(tcp[2:4]), remoteIP: remoteIP, remotePort: binary.BigEndian.Uint16(tcp[0:2])}
	case ipProtoICMP:
		if totalLen < ihl+8 || packet[ihl] != icmpEchoReply {
			return "", [6]byte{}, nil, nil, false
		}
		key = emunetNATInKey{proto: proto, external: binary.BigEndian.Uint16(packet[ihl+4 : ihl+6]), remoteIP: remoteIP}
	default:
		return "", [6]byte{}, nil, nil, false
	}

	now := s.nowTime()
	s.mu.Lock()
	s.cleanupExpiredNATLocked(now)
	ent := s.natByIn[key]
	if ent == nil {
		s.mu.Unlock()
		return "", [6]byte{}, nil, nil, false
	}
	ent.lastUsed = now
	p := s.ports[ent.portID]
	if p == nil || p.emit == nil {
		s.mu.Unlock()
		return "", [6]byte{}, nil, nil, false
	}
	guestMAC := p.guestMAC
	emit := p.emit
	s.mu.Unlock()

	out := append([]byte(nil), packet[:totalLen]...)
	guest4 := ent.guestIP.As4()
	copy(out[16:20], guest4[:])
	switch proto {
	case ipProtoUDP:
		udp := out[ihl:]
		binary.BigEndian.PutUint16(udp[2:4], ent.guestPort)
		if binary.BigEndian.Uint16(udp[6:8]) != 0 {
			udp[6], udp[7] = 0, 0
			binary.BigEndian.PutUint16(udp[6:8], transportChecksum(out[:ihl], udp, proto))
		}
	case ipProtoTCP:
		tcp := out[ihl:]
		binary.BigEndian.PutUint16(tcp[2:4], ent.guestPort)
		tcp[16], tcp[17] = 0, 0
		binary.BigEndian.PutUint16(tcp[16:18], transportChecksum(out[:ihl], tcp, proto))
	case ipProtoICMP:
		icmp := out[ihl:]
		binary.BigEndian.PutUint16(icmp[4:6], ent.guestPort)
		icmp[2], icmp[3] = 0, 0
		binary.BigEndian.PutUint16(icmp[2:4], internetChecksum(icmp))
	}
	out[10], out[11] = 0, 0
	binary.BigEndian.PutUint16(out[10:12], ipv4HeaderChecksum(out[:ihl]))
	return ent.portID, guestMAC, out, emit, true
}

func (s *tsnetVirtioStack) cleanupExpiredNAT() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cleanupExpiredNATLocked(s.nowTime())
}

func (s *tsnetVirtioStack) cleanupExpiredNATLocked(now time.Time) int {
	removed := 0
	for key, ent := range s.natByOut {
		if now.Sub(ent.lastUsed) <= emunetNATIdleTimeout(ent.proto) {
			continue
		}
		delete(s.natByOut, key)
		delete(s.natByIn, emunetNATInKey{
			proto:      ent.proto,
			external:   ent.external,
			remoteIP:   ent.remoteIP,
			remotePort: ent.remotePort,
		})
		removed++
	}
	return removed
}

func (s *tsnetVirtioStack) nowTime() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

func emunetNATIdleTimeout(proto byte) time.Duration {
	switch proto {
	case ipProtoICMP:
		return emunetICMPIdleTimeout
	case ipProtoTCP:
		return emunetTCPIdleTimeout
	default:
		return emunetUDPIdleTimeout
	}
}

func ethernetIPv4Frame(dst, src [6]byte, packet []byte) []byte {
	frame := make([]byte, 14+len(packet))
	copy(frame[0:6], dst[:])
	copy(frame[6:12], src[:])
	binary.BigEndian.PutUint16(frame[12:14], etherTypeIPv4)
	copy(frame[14:], packet)
	return frame
}

func transportChecksum(ipHeader, segment []byte, proto byte) uint16 {
	sum := uint32(0)
	for i := 12; i < 20; i += 2 {
		sum += uint32(binary.BigEndian.Uint16(ipHeader[i:]))
	}
	sum += uint32(proto)
	sum += uint32(len(segment))
	for i := 0; i+1 < len(segment); i += 2 {
		sum += uint32(binary.BigEndian.Uint16(segment[i:]))
	}
	if len(segment)%2 == 1 {
		sum += uint32(segment[len(segment)-1]) << 8
	}
	return foldInternetChecksum(sum)
}

func internetChecksum(buf []byte) uint16 {
	sum := uint32(0)
	for i := 0; i+1 < len(buf); i += 2 {
		sum += uint32(binary.BigEndian.Uint16(buf[i:]))
	}
	if len(buf)%2 == 1 {
		sum += uint32(buf[len(buf)-1]) << 8
	}
	return foldInternetChecksum(sum)
}

func foldInternetChecksum(sum uint32) uint16 {
	for sum > 0xffff {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	return ^uint16(sum)
}
