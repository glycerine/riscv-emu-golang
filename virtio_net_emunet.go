package riscv

import (
	"encoding/binary"
	"fmt"
	"net/netip"
	"time"
)

const (
	ipProtoICMP   = byte(1)
	ipProtoTCP    = byte(6)
	ipProtoICMPv6 = byte(58)

	icmpEchoReply               = byte(0)
	icmpEchoRequest             = byte(8)
	icmpv6RouterSolicitation    = byte(133)
	icmpv6RouterAdvertisement   = byte(134)
	icmpv6NeighborSolicitation  = byte(135)
	icmpv6NeighborAdvertisement = byte(136)
	icmpv6EchoRequest           = byte(128)
	icmpv6EchoReply             = byte(129)

	emunetLocalPortID = "local"

	emunetICMPIdleTimeout = 30 * time.Second
	emunetUDPIdleTimeout  = 2 * time.Minute
	emunetTCPIdleTimeout  = 10 * time.Minute

	NAT66Support = false
)

const (
	emunetDropNoTailIPv4        = "no_tail_ipv4"
	emunetDropNoTailIPv6        = "no_tail_ipv6"
	emunetDropNoNATMapping      = "no_nat_mapping"
	emunetDropIPv4Fragment      = "ipv4_fragment"
	emunetDropUnsupportedProto  = "unsupported_protocol"
	emunetDropBadPacketLength   = "bad_packet_length"
	emunetDropBadHeaderLength   = "bad_header_length"
	emunetDropTTLExpired        = "ttl_expired"
	emunetDropClosedEmunetPort  = "closed_emunet_port"
	emunetDropNoLocalPort       = "no_local_port"
	emunetDropUnsupportedEth    = "unsupported_ethertype"
	emunetDropMalformedEthernet = "malformed_ethernet"
)

type emunetCounters struct {
	DHCPOffers         uint64
	DHCPAcks           uint64
	ARPReplies         uint64
	GatewayICMPReplies uint64
	TailnetICMPReplies uint64
	NATOutboundUDP     uint64
	NATOutboundTCP     uint64
	NATOutboundICMP    uint64
	NATInboundUDP      uint64
	NATInboundTCP      uint64
	NATInboundICMP     uint64
	Drops              map[string]uint64
}

var (
	emunetRouterIPv4          = netip.MustParseAddr("10.77.0.1")
	emunetDNSIPv4             = netip.MustParseAddr("100.100.100.100")
	emunetIPv6Prefix          = netip.MustParsePrefix("fd7a:115c:a1e0:77::/64")
	emunetRouterIPv6          = netip.MustParseAddr("fd7a:115c:a1e0:77::1")
	emunetRouterIPv6LinkLocal = ipv6LinkLocalFromMAC(emunetRouterMAC)
	ipv6AllNodesMulticast     = netip.MustParseAddr("ff02::1")
)

type emunetPort struct {
	id       string
	lease    netip.Addr
	guestMAC [6]byte
	ipv6     map[netip.Addr]struct{}
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
		s.traceLocked("lease port=%q ip=%s", id, p.lease)
	}
	return p
}

func (s *tsnetVirtioStack) portByLeaseLocked(ip netip.Addr) *emunetPort {
	if !ip.Is4() {
		return nil
	}
	for _, p := range s.ports {
		if p != nil && p.lease == ip {
			return p
		}
	}
	return nil
}

func (s *tsnetVirtioStack) portByIPv6Locked(ip netip.Addr) *emunetPort {
	if !emunetIPv6PortAddress(ip) {
		return nil
	}
	for _, p := range s.ports {
		if p == nil {
			continue
		}
		if _, ok := p.ipv6[ip]; ok {
			return p
		}
	}
	return nil
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

func (s *tsnetVirtioStack) learnPortIPv6(id string, ip netip.Addr, mac [6]byte, emit func([]byte)) {
	if !emunetIPv6PortAddress(ip) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.emunetPortLocked(id, emit)
	if mac != ([6]byte{}) {
		p.guestMAC = mac
	}
	if p.ipv6 == nil {
		p.ipv6 = make(map[netip.Addr]struct{})
	}
	if _, ok := p.ipv6[ip]; !ok {
		p.ipv6[ip] = struct{}{}
		s.traceLocked("ipv6_learn port=%q ip=%s mac=%x", id, ip, p.guestMAC)
	}
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
	targetIP := netip.AddrFrom4([4]byte{req[38], req[39], req[40], req[41]})
	var guestMAC [6]byte
	copy(guestMAC[:], req[22:28])
	s.learnPortMAC(portID, guestMAC, emit)

	var senderMAC [6]byte
	senderIP := targetIP
	switch targetIP {
	case emunetRouterIPv4:
		senderMAC = emunetRouterMAC
	default:
		s.mu.Lock()
		target := s.portByLeaseLocked(targetIP)
		if target != nil {
			senderMAC = target.guestMAC
			if target.id == portID {
				senderMAC = [6]byte{}
			}
		}
		s.mu.Unlock()
		if senderMAC == ([6]byte{}) {
			s.trace("arp_no_reply port=%q target_ip=%s", portID, targetIP)
			return nil
		}
	}

	sender4 := senderIP.As4()
	reply := make([]byte, 42)
	copy(reply[0:6], req[6:12])
	copy(reply[6:12], senderMAC[:])
	binary.BigEndian.PutUint16(reply[12:14], etherTypeARP)
	copy(reply[14:20], req[14:20])
	binary.BigEndian.PutUint16(reply[20:22], 2)
	copy(reply[22:28], senderMAC[:])
	copy(reply[28:32], sender4[:])
	copy(reply[32:38], req[22:28])
	copy(reply[38:42], req[28:32])
	s.incARPReply()
	s.trace("arp_reply port=%q target_ip=%s sender_mac=%x", portID, targetIP, senderMAC)
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
	s.incGatewayICMPReply()
	return reply
}

func (s *tsnetVirtioStack) gatewayICMPv6EchoReply(portID string, frame []byte, emit func([]byte)) []byte {
	if len(frame) < 14+40 {
		return nil
	}
	packet := frame[14:]
	if packet[0]>>4 != 6 || packet[6] != ipProtoICMPv6 {
		return nil
	}
	payloadLen := int(binary.BigEndian.Uint16(packet[4:6]))
	totalLen := 40 + payloadLen
	if payloadLen < 8 || totalLen > len(packet) {
		return nil
	}
	dst := netipAddrFrom16(packet[24:40])
	if dst != emunetRouterIPv6 && dst != emunetRouterIPv6LinkLocal {
		return nil
	}
	reply := icmpEchoReplyIPv6(packet, dst)
	if len(reply) == 0 {
		return nil
	}
	var guestMAC [6]byte
	copy(guestMAC[:], frame[6:12])
	s.learnPortMAC(portID, guestMAC, emit)
	s.incGatewayICMPReply()
	return ethernetIPv6Frame(guestMAC, emunetRouterMAC, reply)
}

func (s *tsnetVirtioStack) handleICMPv6Control(portID string, frame []byte, emit func([]byte)) []byte {
	if len(frame) < 14+40+8 {
		return nil
	}
	packet := frame[14:]
	if packet[0]>>4 != 6 || packet[6] != ipProtoICMPv6 || packet[7] != 255 {
		return nil
	}
	payloadLen := int(binary.BigEndian.Uint16(packet[4:6]))
	totalLen := 40 + payloadLen
	if payloadLen < 8 || totalLen > len(packet) {
		return nil
	}
	icmp := packet[40:totalLen]
	var guestMAC [6]byte
	copy(guestMAC[:], frame[6:12])
	if guestMAC != ([6]byte{}) {
		s.learnPortMAC(portID, guestMAC, emit)
	}
	srcIP := netipAddrFrom16(packet[8:24])
	s.learnPortIPv6(portID, srcIP, guestMAC, emit)

	switch icmp[0] {
	case icmpv6RouterSolicitation:
		if icmp[1] != 0 || len(icmp) < 8 {
			return nil
		}
		return routerAdvertisementFrame(packet, guestMAC)
	case icmpv6NeighborSolicitation:
		if icmp[1] != 0 || len(icmp) < 24 {
			return nil
		}
		target := netipAddrFrom16(icmp[8:24])
		if ipv6AddrIsUnspecified(srcIP) && emunetIPv6PortAddress(target) {
			s.learnPortIPv6(portID, target, guestMAC, emit)
		}
		if target != emunetRouterIPv6 && target != emunetRouterIPv6LinkLocal {
			s.mu.Lock()
			targetPort := s.portByIPv6Locked(target)
			var targetID string
			var targetMAC [6]byte
			if targetPort != nil {
				targetID = targetPort.id
				targetMAC = targetPort.guestMAC
			}
			s.mu.Unlock()
			if targetID == "" || targetID == portID || targetMAC == ([6]byte{}) {
				return nil
			}
			return neighborAdvertisementFrame(packet, guestMAC, target, targetMAC, false)
		}
		return neighborAdvertisementFrame(packet, guestMAC, target, emunetRouterMAC, true)
	default:
		return nil
	}
}

func (s *tsnetVirtioStack) tsnetICMPEchoReplyForGuest(portID string, frame []byte, emit func([]byte)) []byte {
	if len(frame) < 14 {
		return nil
	}
	etherType := binary.BigEndian.Uint16(frame[12:14])
	packet := frame[14:]
	var reply []byte
	switch etherType {
	case etherTypeIPv4:
		tailIP, ok := s.tailscaleIPv4()
		if !ok {
			return nil
		}
		reply = icmpEchoReplyIPv4(packet, tailIP)
	case etherTypeIPv6:
		tailIP, ok := s.tailscaleIPv6()
		if !ok {
			return nil
		}
		reply = icmpEchoReplyIPv6(packet, tailIP)
	default:
		return nil
	}
	if len(reply) == 0 {
		return nil
	}
	var guestMAC [6]byte
	copy(guestMAC[:], frame[6:12])
	s.learnPortMAC(portID, guestMAC, emit)
	s.incTailnetICMPReply()
	return ethernetFrame(guestMAC, emunetRouterMAC, etherType, reply)
}

func (s *tsnetVirtioStack) tsnetICMPEchoReplyForTailnet(packet []byte) []byte {
	if len(packet) == 0 {
		return nil
	}
	var reply []byte
	switch packet[0] >> 4 {
	case 4:
		tailIP, ok := s.tailscaleIPv4()
		if !ok {
			return nil
		}
		reply = icmpEchoReplyIPv4(packet, tailIP)
	case 6:
		tailIP, ok := s.tailscaleIPv6()
		if !ok {
			return nil
		}
		reply = icmpEchoReplyIPv6(packet, tailIP)
	default:
		return nil
	}
	if len(reply) != 0 {
		s.incTailnetICMPReply()
	}
	return reply
}

func (s *tsnetVirtioStack) natOutbound(portID string, packet []byte, emit func([]byte)) []byte {
	tailIP, ok := s.tailscaleIPv4()
	if !ok || !tailIP.Is4() {
		s.incDrop(emunetDropNoTailIPv4)
		return nil
	}
	return s.translateOutboundIPv4(portID, packet, tailIP, emit)
}

func (s *tsnetVirtioStack) natOutboundIPv6(portID string, packet []byte, emit func([]byte)) []byte {
	tailIP, ok := s.tailscaleIPv6()
	if !ok || !tailIP.Is6() {
		s.incDrop(emunetDropNoTailIPv6)
		return nil
	}
	return s.translateOutboundIPv6(portID, packet, tailIP, emit)
}

func (s *tsnetVirtioStack) deliverLocalIPv4(portID string, frame []byte, emit func([]byte)) bool {
	if len(frame) < 14+20 {
		return false
	}
	ip := frame[14:]
	if ip[0]>>4 != 4 {
		return false
	}
	ihl := int(ip[0]&0x0f) * 4
	if ihl < 20 || len(ip) < ihl {
		return false
	}
	totalLen := int(binary.BigEndian.Uint16(ip[2:4]))
	if totalLen < ihl || totalLen > len(ip) {
		return false
	}
	dst := netip.AddrFrom4([4]byte{ip[16], ip[17], ip[18], ip[19]})
	if !emunetIPv4LANContains(dst) {
		return false
	}

	var srcMAC [6]byte
	copy(srcMAC[:], frame[6:12])
	if srcMAC != ([6]byte{}) {
		s.learnPortMAC(portID, srcMAC, emit)
	}

	s.mu.Lock()
	target := s.portByLeaseLocked(dst)
	var targetID string
	var targetEmit func([]byte)
	if target != nil {
		targetID = target.id
		targetEmit = target.emit
	}
	s.mu.Unlock()

	if targetID == "" || targetID == portID || targetEmit == nil {
		s.incDrop(emunetDropNoLocalPort)
		s.trace("local_ipv4_no_port src_port=%q dst=%s target_port=%q target_emit=%t", portID, dst, targetID, targetEmit != nil)
		return true
	}
	targetEmit(append([]byte(nil), frame...))
	s.trace("local_ipv4_deliver src_port=%q dst=%s target_port=%q len=%d", portID, dst, targetID, len(frame))
	return true
}

func (s *tsnetVirtioStack) deliverLocalIPv6(portID string, frame []byte, emit func([]byte)) bool {
	if len(frame) < 14+40 {
		return false
	}
	packet := frame[14:]
	if packet[0]>>4 != 6 {
		return false
	}
	payloadLen := int(binary.BigEndian.Uint16(packet[4:6]))
	totalLen := 40 + payloadLen
	if totalLen > len(packet) {
		return false
	}
	src := netipAddrFrom16(packet[8:24])
	dst := netipAddrFrom16(packet[24:40])

	var srcMAC [6]byte
	copy(srcMAC[:], frame[6:12])
	if srcMAC != ([6]byte{}) {
		s.learnPortMAC(portID, srcMAC, emit)
	}
	s.learnPortIPv6(portID, src, srcMAC, emit)

	if ipv6AddrIsMulticast(dst) {
		delivered := 0
		s.mu.Lock()
		targets := make([]func([]byte), 0, len(s.ports))
		for _, p := range s.ports {
			if p == nil || p.id == portID || p.emit == nil {
				continue
			}
			targets = append(targets, p.emit)
		}
		s.mu.Unlock()
		for _, targetEmit := range targets {
			targetEmit(append([]byte(nil), frame...))
			delivered++
		}
		s.trace("local_ipv6_multicast src_port=%q dst=%s delivered=%d len=%d", portID, dst, delivered, len(frame))
		return true
	}
	if !emunetIPv6LocalAddress(dst) {
		return false
	}

	s.mu.Lock()
	target := s.portByIPv6Locked(dst)
	var targetID string
	var targetEmit func([]byte)
	if target != nil {
		targetID = target.id
		targetEmit = target.emit
	}
	s.mu.Unlock()

	if targetID == "" || targetID == portID || targetEmit == nil {
		s.incDrop(emunetDropNoLocalPort)
		s.trace("local_ipv6_no_port src_port=%q dst=%s target_port=%q target_emit=%t", portID, dst, targetID, targetEmit != nil)
		return true
	}
	targetEmit(append([]byte(nil), frame...))
	s.trace("local_ipv6_deliver src_port=%q dst=%s target_port=%q len=%d", portID, dst, targetID, len(frame))
	return true
}

func emunetIPv4LANContains(ip netip.Addr) bool {
	if !ip.Is4() {
		return false
	}
	ip4 := ip.As4()
	return ip4[0] == 10 && ip4[1] == 77 && ip4[2] == 0
}

func emunetIPv6PortAddress(ip netip.Addr) bool {
	if !emunetIPv6LocalAddress(ip) {
		return false
	}
	return ip != emunetRouterIPv6 && ip != emunetRouterIPv6LinkLocal
}

func emunetIPv6LocalAddress(ip netip.Addr) bool {
	if !ip.Is6() || ipv6AddrIsUnspecified(ip) || ipv6AddrIsMulticast(ip) {
		return false
	}
	return emunetIPv6Prefix.Contains(ip) || ipv6AddrIsLinkLocalUnicast(ip)
}

func (s *tsnetVirtioStack) translateOutboundIPv4(portID string, packet []byte, tailIP netip.Addr, emit func([]byte)) []byte {
	if len(packet) < 20 || packet[0]>>4 != 4 {
		s.incDrop(emunetDropBadPacketLength)
		return nil
	}
	ihl := int(packet[0]&0x0f) * 4
	if ihl < 20 || len(packet) < ihl {
		s.incDrop(emunetDropBadHeaderLength)
		return nil
	}
	totalLen := int(binary.BigEndian.Uint16(packet[2:4]))
	if totalLen < ihl || totalLen > len(packet) {
		s.incDrop(emunetDropBadPacketLength)
		return nil
	}
	frag := binary.BigEndian.Uint16(packet[6:8])
	if frag&0x3fff != 0 {
		s.incDrop(emunetDropIPv4Fragment)
		return nil
	}
	if packet[8] <= 1 {
		s.incDrop(emunetDropTTLExpired)
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
			s.incDrop(emunetDropBadPacketLength)
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
			s.incDrop(emunetDropBadPacketLength)
			return nil
		}
		tcp := out[ihl:]
		tcpHeaderLen := int(tcp[12]>>4) * 4
		if tcpHeaderLen < 20 || len(tcp) < tcpHeaderLen {
			s.incDrop(emunetDropBadHeaderLength)
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
			s.incDrop(emunetDropUnsupportedProto)
			return nil
		}
		icmp := out[ihl:]
		guestID := binary.BigEndian.Uint16(icmp[4:6])
		ext := s.natExternalLocked(emunetNATOutKey{proto: proto, portID: portID, guestIP: guestIP, guestPort: guestID, remoteIP: remoteIP})
		binary.BigEndian.PutUint16(icmp[4:6], ext)
		icmp[2], icmp[3] = 0, 0
		binary.BigEndian.PutUint16(icmp[2:4], internetChecksum(icmp))
	default:
		s.incDrop(emunetDropUnsupportedProto)
		return nil
	}
	out[10], out[11] = 0, 0
	binary.BigEndian.PutUint16(out[10:12], ipv4HeaderChecksum(out[:ihl]))
	s.incNATOutbound(proto)
	return out
}

func (s *tsnetVirtioStack) translateOutboundIPv6(portID string, packet []byte, tailIP netip.Addr, emit func([]byte)) []byte {
	if len(packet) < 40 || packet[0]>>4 != 6 || !tailIP.Is6() {
		s.incDrop(emunetDropBadPacketLength)
		return nil
	}
	payloadLen := int(binary.BigEndian.Uint16(packet[4:6]))
	totalLen := 40 + payloadLen
	if totalLen > len(packet) {
		s.incDrop(emunetDropBadPacketLength)
		return nil
	}
	if packet[7] <= 1 {
		s.incDrop(emunetDropTTLExpired)
		return nil
	}
	guestIP := netipAddrFrom16(packet[8:24])
	remoteIP := netipAddrFrom16(packet[24:40])
	out := append([]byte(nil), packet[:totalLen]...)
	out[7]--
	tail6 := tailIP.As16()
	copy(out[8:24], tail6[:])

	proto := out[6]
	payload := out[40:totalLen]
	switch proto {
	case ipProtoUDP:
		if len(payload) < 8 {
			s.incDrop(emunetDropBadPacketLength)
			return nil
		}
		udpLen := int(binary.BigEndian.Uint16(payload[4:6]))
		if udpLen < 8 || udpLen > len(payload) {
			s.incDrop(emunetDropBadPacketLength)
			return nil
		}
		udp := payload[:udpLen]
		guestPort := binary.BigEndian.Uint16(udp[0:2])
		remotePort := binary.BigEndian.Uint16(udp[2:4])
		ext := s.natExternalLocked(emunetNATOutKey{proto: proto, portID: portID, guestIP: guestIP, guestPort: guestPort, remoteIP: remoteIP, remotePort: remotePort})
		binary.BigEndian.PutUint16(udp[0:2], ext)
		udp[6], udp[7] = 0, 0
		binary.BigEndian.PutUint16(udp[6:8], transportChecksumIPv6NonZero(out[:40], udp, proto))
	case ipProtoTCP:
		if len(payload) < 20 {
			s.incDrop(emunetDropBadPacketLength)
			return nil
		}
		tcpHeaderLen := int(payload[12]>>4) * 4
		if tcpHeaderLen < 20 || len(payload) < tcpHeaderLen {
			s.incDrop(emunetDropBadHeaderLength)
			return nil
		}
		guestPort := binary.BigEndian.Uint16(payload[0:2])
		remotePort := binary.BigEndian.Uint16(payload[2:4])
		ext := s.natExternalLocked(emunetNATOutKey{proto: proto, portID: portID, guestIP: guestIP, guestPort: guestPort, remoteIP: remoteIP, remotePort: remotePort})
		binary.BigEndian.PutUint16(payload[0:2], ext)
		payload[16], payload[17] = 0, 0
		binary.BigEndian.PutUint16(payload[16:18], transportChecksumIPv6(out[:40], payload, proto))
	case ipProtoICMPv6:
		if len(payload) < 8 || payload[0] != icmpv6EchoRequest {
			s.incDrop(emunetDropUnsupportedProto)
			return nil
		}
		guestID := binary.BigEndian.Uint16(payload[4:6])
		ext := s.natExternalLocked(emunetNATOutKey{proto: proto, portID: portID, guestIP: guestIP, guestPort: guestID, remoteIP: remoteIP})
		binary.BigEndian.PutUint16(payload[4:6], ext)
		payload[2], payload[3] = 0, 0
		binary.BigEndian.PutUint16(payload[2:4], icmpv6Checksum(out[:40], payload))
	default:
		s.incDrop(emunetDropUnsupportedProto)
		return nil
	}
	s.incNATOutbound(proto)
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
	if !ok || !tailIP.Is4() {
		s.incDrop(emunetDropNoTailIPv4)
		return "", [6]byte{}, nil, nil, false
	}
	if len(packet) < 20 || packet[0]>>4 != 4 {
		s.incDrop(emunetDropBadPacketLength)
		return "", [6]byte{}, nil, nil, false
	}
	ihl := int(packet[0]&0x0f) * 4
	totalLen := int(binary.BigEndian.Uint16(packet[2:4]))
	if ihl < 20 || totalLen < ihl || totalLen > len(packet) {
		s.incDrop(emunetDropBadPacketLength)
		return "", [6]byte{}, nil, nil, false
	}
	dst := netip.AddrFrom4([4]byte{packet[16], packet[17], packet[18], packet[19]})
	if dst != tailIP {
		s.incDrop(emunetDropNoNATMapping)
		return "", [6]byte{}, nil, nil, false
	}
	proto := packet[9]
	remoteIP := netip.AddrFrom4([4]byte{packet[12], packet[13], packet[14], packet[15]})
	var key emunetNATInKey
	switch proto {
	case ipProtoUDP:
		if totalLen < ihl+8 {
			s.incDrop(emunetDropBadPacketLength)
			return "", [6]byte{}, nil, nil, false
		}
		udp := packet[ihl:]
		key = emunetNATInKey{proto: proto, external: binary.BigEndian.Uint16(udp[2:4]), remoteIP: remoteIP, remotePort: binary.BigEndian.Uint16(udp[0:2])}
	case ipProtoTCP:
		if totalLen < ihl+20 {
			s.incDrop(emunetDropBadPacketLength)
			return "", [6]byte{}, nil, nil, false
		}
		tcp := packet[ihl:]
		key = emunetNATInKey{proto: proto, external: binary.BigEndian.Uint16(tcp[2:4]), remoteIP: remoteIP, remotePort: binary.BigEndian.Uint16(tcp[0:2])}
	case ipProtoICMP:
		if totalLen < ihl+8 || packet[ihl] != icmpEchoReply {
			s.incDrop(emunetDropUnsupportedProto)
			return "", [6]byte{}, nil, nil, false
		}
		key = emunetNATInKey{proto: proto, external: binary.BigEndian.Uint16(packet[ihl+4 : ihl+6]), remoteIP: remoteIP}
	default:
		s.incDrop(emunetDropUnsupportedProto)
		return "", [6]byte{}, nil, nil, false
	}

	now := s.nowTime()
	s.mu.Lock()
	s.cleanupExpiredNATLocked(now)
	ent := s.natByIn[key]
	if ent == nil {
		s.mu.Unlock()
		s.incDrop(emunetDropNoNATMapping)
		return "", [6]byte{}, nil, nil, false
	}
	ent.lastUsed = now
	p := s.ports[ent.portID]
	if p == nil || p.emit == nil {
		s.mu.Unlock()
		s.incDrop(emunetDropClosedEmunetPort)
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
	s.incNATInbound(proto)
	return ent.portID, guestMAC, out, emit, true
}

func (s *tsnetVirtioStack) natInboundIPv6(packet []byte) (string, [6]byte, []byte, func([]byte), bool) {
	tailIP, ok := s.tailscaleIPv6()
	if !ok || !tailIP.Is6() {
		s.incDrop(emunetDropNoTailIPv6)
		return "", [6]byte{}, nil, nil, false
	}
	if len(packet) < 40 || packet[0]>>4 != 6 {
		s.incDrop(emunetDropBadPacketLength)
		return "", [6]byte{}, nil, nil, false
	}
	payloadLen := int(binary.BigEndian.Uint16(packet[4:6]))
	totalLen := 40 + payloadLen
	if totalLen > len(packet) {
		s.incDrop(emunetDropBadPacketLength)
		return "", [6]byte{}, nil, nil, false
	}
	dst := netipAddrFrom16(packet[24:40])
	if dst != tailIP {
		s.incDrop(emunetDropNoNATMapping)
		return "", [6]byte{}, nil, nil, false
	}
	proto := packet[6]
	remoteIP := netipAddrFrom16(packet[8:24])
	payload := packet[40:totalLen]
	var key emunetNATInKey
	switch proto {
	case ipProtoUDP:
		if len(payload) < 8 {
			s.incDrop(emunetDropBadPacketLength)
			return "", [6]byte{}, nil, nil, false
		}
		udpLen := int(binary.BigEndian.Uint16(payload[4:6]))
		if udpLen < 8 || udpLen > len(payload) {
			s.incDrop(emunetDropBadPacketLength)
			return "", [6]byte{}, nil, nil, false
		}
		udp := payload[:udpLen]
		key = emunetNATInKey{proto: proto, external: binary.BigEndian.Uint16(udp[2:4]), remoteIP: remoteIP, remotePort: binary.BigEndian.Uint16(udp[0:2])}
	case ipProtoTCP:
		if len(payload) < 20 {
			s.incDrop(emunetDropBadPacketLength)
			return "", [6]byte{}, nil, nil, false
		}
		tcpHeaderLen := int(payload[12]>>4) * 4
		if tcpHeaderLen < 20 || len(payload) < tcpHeaderLen {
			s.incDrop(emunetDropBadHeaderLength)
			return "", [6]byte{}, nil, nil, false
		}
		key = emunetNATInKey{proto: proto, external: binary.BigEndian.Uint16(payload[2:4]), remoteIP: remoteIP, remotePort: binary.BigEndian.Uint16(payload[0:2])}
	case ipProtoICMPv6:
		if len(payload) < 8 || payload[0] != icmpv6EchoReply {
			s.incDrop(emunetDropUnsupportedProto)
			return "", [6]byte{}, nil, nil, false
		}
		key = emunetNATInKey{proto: proto, external: binary.BigEndian.Uint16(payload[4:6]), remoteIP: remoteIP}
	default:
		s.incDrop(emunetDropUnsupportedProto)
		return "", [6]byte{}, nil, nil, false
	}

	now := s.nowTime()
	s.mu.Lock()
	s.cleanupExpiredNATLocked(now)
	ent := s.natByIn[key]
	if ent == nil {
		s.mu.Unlock()
		s.incDrop(emunetDropNoNATMapping)
		return "", [6]byte{}, nil, nil, false
	}
	ent.lastUsed = now
	p := s.ports[ent.portID]
	if p == nil || p.emit == nil {
		s.mu.Unlock()
		s.incDrop(emunetDropClosedEmunetPort)
		return "", [6]byte{}, nil, nil, false
	}
	guestMAC := p.guestMAC
	emit := p.emit
	s.mu.Unlock()

	out := append([]byte(nil), packet[:totalLen]...)
	guest6 := ent.guestIP.As16()
	copy(out[24:40], guest6[:])
	outPayload := out[40:totalLen]
	switch proto {
	case ipProtoUDP:
		udpLen := int(binary.BigEndian.Uint16(outPayload[4:6]))
		udp := outPayload[:udpLen]
		binary.BigEndian.PutUint16(udp[2:4], ent.guestPort)
		udp[6], udp[7] = 0, 0
		binary.BigEndian.PutUint16(udp[6:8], transportChecksumIPv6NonZero(out[:40], udp, proto))
	case ipProtoTCP:
		binary.BigEndian.PutUint16(outPayload[2:4], ent.guestPort)
		outPayload[16], outPayload[17] = 0, 0
		binary.BigEndian.PutUint16(outPayload[16:18], transportChecksumIPv6(out[:40], outPayload, proto))
	case ipProtoICMPv6:
		binary.BigEndian.PutUint16(outPayload[4:6], ent.guestPort)
		outPayload[2], outPayload[3] = 0, 0
		binary.BigEndian.PutUint16(outPayload[2:4], icmpv6Checksum(out[:40], outPayload))
	}
	s.incNATInbound(proto)
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
	case ipProtoICMP, ipProtoICMPv6:
		return emunetICMPIdleTimeout
	case ipProtoTCP:
		return emunetTCPIdleTimeout
	default:
		return emunetUDPIdleTimeout
	}
}

func (s *tsnetVirtioStack) counterSnapshot() emunetCounters {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.counters
	if s.counters.Drops != nil {
		out.Drops = make(map[string]uint64, len(s.counters.Drops))
		for reason, count := range s.counters.Drops {
			out.Drops[reason] = count
		}
	}
	return out
}

func (s *tsnetVirtioStack) incDHCPReply(replyType byte) {
	s.mu.Lock()
	switch replyType {
	case dhcpOffer:
		s.counters.DHCPOffers++
	case dhcpAck:
		s.counters.DHCPAcks++
	}
	s.mu.Unlock()
}

func (s *tsnetVirtioStack) incARPReply() {
	s.mu.Lock()
	s.counters.ARPReplies++
	s.mu.Unlock()
}

func (s *tsnetVirtioStack) incGatewayICMPReply() {
	s.mu.Lock()
	s.counters.GatewayICMPReplies++
	s.mu.Unlock()
}

func (s *tsnetVirtioStack) incTailnetICMPReply() {
	s.mu.Lock()
	s.counters.TailnetICMPReplies++
	s.mu.Unlock()
}

func (s *tsnetVirtioStack) incNATOutbound(proto byte) {
	s.mu.Lock()
	switch proto {
	case ipProtoUDP:
		s.counters.NATOutboundUDP++
	case ipProtoTCP:
		s.counters.NATOutboundTCP++
	case ipProtoICMP, ipProtoICMPv6:
		s.counters.NATOutboundICMP++
	}
	s.mu.Unlock()
}

func (s *tsnetVirtioStack) incNATInbound(proto byte) {
	s.mu.Lock()
	switch proto {
	case ipProtoUDP:
		s.counters.NATInboundUDP++
	case ipProtoTCP:
		s.counters.NATInboundTCP++
	case ipProtoICMP, ipProtoICMPv6:
		s.counters.NATInboundICMP++
	}
	s.mu.Unlock()
}

func (s *tsnetVirtioStack) incDrop(reason string) {
	s.mu.Lock()
	if s.counters.Drops == nil {
		s.counters.Drops = make(map[string]uint64)
	}
	s.counters.Drops[reason]++
	count := s.counters.Drops[reason]
	s.mu.Unlock()
	if s.cfg.EmunetTrace {
		appendTsnetOpLog("emunet_trace drop reason=%q count=%d", reason, count)
	}
}

func (s *tsnetVirtioStack) trace(format string, args ...any) {
	if s == nil || !s.cfg.EmunetTrace {
		return
	}
	appendTsnetOpLog("emunet_trace %s", fmt.Sprintf(format, args...))
}

func (s *tsnetVirtioStack) traceLocked(format string, args ...any) {
	if s == nil || !s.cfg.EmunetTrace {
		return
	}
	appendTsnetOpLog("emunet_trace %s", fmt.Sprintf(format, args...))
}

func ethernetIPv4Frame(dst, src [6]byte, packet []byte) []byte {
	return ethernetFrame(dst, src, etherTypeIPv4, packet)
}

func ethernetIPv6Frame(dst, src [6]byte, packet []byte) []byte {
	return ethernetFrame(dst, src, etherTypeIPv6, packet)
}

func ethernetFrame(dst, src [6]byte, etherType uint16, packet []byte) []byte {
	frame := make([]byte, 14+len(packet))
	copy(frame[0:6], dst[:])
	copy(frame[6:12], src[:])
	binary.BigEndian.PutUint16(frame[12:14], etherType)
	copy(frame[14:], packet)
	return frame
}

func routerAdvertisementFrame(request []byte, guestMAC [6]byte) []byte {
	if len(request) < 40 {
		return nil
	}
	dstIP := netipAddrFrom16(request[8:24])
	if ipv6AddrIsUnspecified(dstIP) {
		dstIP = ipv6AllNodesMulticast
	}
	icmp := make([]byte, 16+8+8+32)
	icmp[0] = icmpv6RouterAdvertisement
	icmp[4] = 64
	binary.BigEndian.PutUint16(icmp[6:8], 1800)

	i := 16
	icmp[i] = 1 // Source link-layer address.
	icmp[i+1] = 1
	copy(icmp[i+2:i+8], emunetRouterMAC[:])

	i += 8
	icmp[i] = 5 // MTU.
	icmp[i+1] = 1
	binary.BigEndian.PutUint32(icmp[i+4:i+8], uint32(virtioNetMTU))

	i += 8
	icmp[i] = 3 // Prefix information.
	icmp[i+1] = 4
	icmp[i+2] = byte(emunetIPv6Prefix.Bits())
	icmp[i+3] = 0xc0 // On-link + autonomous address configuration.
	binary.BigEndian.PutUint32(icmp[i+4:i+8], 86400)
	binary.BigEndian.PutUint32(icmp[i+8:i+12], 14400)
	prefix := emunetIPv6Prefix.Addr().As16()
	copy(icmp[i+16:i+32], prefix[:])

	src := emunetRouterIPv6LinkLocal.As16()
	dst := dstIP.As16()
	packet := makeIPv6Packet(src, dst, ipProtoICMPv6, icmp)
	packet[7] = 255
	binary.BigEndian.PutUint16(packet[42:44], icmpv6Checksum(packet[:40], packet[40:]))

	dstMAC := guestMAC
	if ipv6AddrIsMulticast(dstIP) || dstMAC == ([6]byte{}) {
		dstMAC = ipv6MulticastMAC(dstIP)
	}
	return ethernetIPv6Frame(dstMAC, emunetRouterMAC, packet)
}

func neighborAdvertisementFrame(request []byte, guestMAC [6]byte, target netip.Addr, targetMAC [6]byte, router bool) []byte {
	if len(request) < 40 || !target.Is6() || targetMAC == ([6]byte{}) {
		return nil
	}
	dstIP := netipAddrFrom16(request[8:24])
	flags := byte(0x20) // Override.
	if router {
		flags |= 0x80
	}
	if ipv6AddrIsUnspecified(dstIP) {
		dstIP = ipv6AllNodesMulticast
	} else {
		flags |= 0x40 // Solicited.
	}

	icmp := make([]byte, 32)
	icmp[0] = icmpv6NeighborAdvertisement
	icmp[4] = flags
	target16 := target.As16()
	copy(icmp[8:24], target16[:])
	icmp[24] = 2 // Target link-layer address.
	icmp[25] = 1
	copy(icmp[26:32], targetMAC[:])

	src := target.As16()
	dst := dstIP.As16()
	packet := makeIPv6Packet(src, dst, ipProtoICMPv6, icmp)
	packet[7] = 255
	binary.BigEndian.PutUint16(packet[42:44], icmpv6Checksum(packet[:40], packet[40:]))

	dstMAC := guestMAC
	if ipv6AddrIsMulticast(dstIP) || dstMAC == ([6]byte{}) {
		dstMAC = ipv6MulticastMAC(dstIP)
	}
	return ethernetIPv6Frame(dstMAC, targetMAC, packet)
}

func ipv6LinkLocalFromMAC(mac [6]byte) netip.Addr {
	var b [16]byte
	b[0] = 0xfe
	b[1] = 0x80
	b[8] = mac[0] ^ 0x02
	b[9] = mac[1]
	b[10] = mac[2]
	b[11] = 0xff
	b[12] = 0xfe
	b[13] = mac[3]
	b[14] = mac[4]
	b[15] = mac[5]
	return netip.AddrFrom16(b)
}

func ipv6MulticastMAC(ip netip.Addr) [6]byte {
	var mac [6]byte
	mac[0] = 0x33
	mac[1] = 0x33
	if ip.Is6() {
		b := ip.As16()
		copy(mac[2:6], b[12:16])
	}
	return mac
}

func ipv6AddrIsMulticast(ip netip.Addr) bool {
	if !ip.Is6() {
		return false
	}
	b := ip.As16()
	return b[0] == 0xff
}

func ipv6AddrIsUnspecified(ip netip.Addr) bool {
	return ip == netip.IPv6Unspecified()
}

func ipv6AddrIsLinkLocalUnicast(ip netip.Addr) bool {
	if !ip.Is6() {
		return false
	}
	b := ip.As16()
	return b[0] == 0xfe && b[1]&0xc0 == 0x80
}

func makeIPv6Packet(src, dst [16]byte, nextHeader byte, payload []byte) []byte {
	ip := make([]byte, 40+len(payload))
	ip[0] = 0x60
	binary.BigEndian.PutUint16(ip[4:6], uint16(len(payload)))
	ip[6] = nextHeader
	ip[7] = 64
	copy(ip[8:24], src[:])
	copy(ip[24:40], dst[:])
	copy(ip[40:], payload)
	return ip
}

func icmpEchoReplyIPv4(packet []byte, dst netip.Addr) []byte {
	if len(packet) < 20 || packet[0]>>4 != 4 || !dst.Is4() {
		return nil
	}
	ihl := int(packet[0]&0x0f) * 4
	if ihl < 20 || len(packet) < ihl+8 || packet[9] != ipProtoICMP {
		return nil
	}
	totalLen := int(binary.BigEndian.Uint16(packet[2:4]))
	if totalLen < ihl+8 || totalLen > len(packet) {
		return nil
	}
	if frag := binary.BigEndian.Uint16(packet[6:8]); frag&0x3fff != 0 {
		return nil
	}
	packetDst := netip.AddrFrom4([4]byte{packet[16], packet[17], packet[18], packet[19]})
	if packetDst != dst || packet[ihl] != icmpEchoRequest {
		return nil
	}

	reply := append([]byte(nil), packet[:totalLen]...)
	copy(reply[12:16], packet[16:20])
	copy(reply[16:20], packet[12:16])
	reply[8] = 64
	reply[10], reply[11] = 0, 0
	binary.BigEndian.PutUint16(reply[10:12], ipv4HeaderChecksum(reply[:ihl]))
	icmp := reply[ihl:]
	icmp[0] = icmpEchoReply
	icmp[2], icmp[3] = 0, 0
	binary.BigEndian.PutUint16(icmp[2:4], internetChecksum(icmp[:totalLen-ihl]))
	return reply
}

func icmpEchoReplyIPv6(packet []byte, dst netip.Addr) []byte {
	if len(packet) < 40 || packet[0]>>4 != 6 || !dst.Is6() {
		return nil
	}
	payloadLen := int(binary.BigEndian.Uint16(packet[4:6]))
	totalLen := 40 + payloadLen
	if payloadLen < 8 || totalLen > len(packet) || packet[6] != ipProtoICMPv6 {
		return nil
	}
	packetDst := netipAddrFrom16(packet[24:40])
	if packetDst != dst || packet[40] != icmpv6EchoRequest {
		return nil
	}

	reply := append([]byte(nil), packet[:totalLen]...)
	copy(reply[8:24], packet[24:40])
	copy(reply[24:40], packet[8:24])
	reply[7] = 64
	icmp := reply[40:]
	icmp[0] = icmpv6EchoReply
	icmp[2], icmp[3] = 0, 0
	binary.BigEndian.PutUint16(icmp[2:4], icmpv6Checksum(reply[:40], icmp))
	return reply
}

func netipAddrFrom16(b []byte) netip.Addr {
	var a [16]byte
	copy(a[:], b)
	return netip.AddrFrom16(a)
}

func icmpv6Checksum(ipHeader, icmp []byte) uint16 {
	return transportChecksumIPv6(ipHeader, icmp, ipProtoICMPv6)
}

func transportChecksumIPv6(ipHeader, segment []byte, proto byte) uint16 {
	sum := uint32(0)
	for i := 8; i+1 < 40 && i+1 < len(ipHeader); i += 2 {
		sum += uint32(binary.BigEndian.Uint16(ipHeader[i:]))
	}
	n := uint32(len(segment))
	sum += (n >> 16) & 0xffff
	sum += n & 0xffff
	sum += uint32(proto)
	for i := 0; i+1 < len(segment); i += 2 {
		sum += uint32(binary.BigEndian.Uint16(segment[i:]))
	}
	if len(segment)%2 == 1 {
		sum += uint32(segment[len(segment)-1]) << 8
	}
	return foldInternetChecksum(sum)
}

func transportChecksumIPv6NonZero(ipHeader, segment []byte, proto byte) uint16 {
	sum := transportChecksumIPv6(ipHeader, segment, proto)
	if sum == 0 {
		return 0xffff
	}
	return sum
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
