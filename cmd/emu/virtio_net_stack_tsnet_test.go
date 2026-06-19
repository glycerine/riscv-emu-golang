//go:build tsnet

package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	emunetpkg "github.com/glycerine/riscv-emu-golang/emunet"
)

func TestTsnetVirtioStackEmunetDHCPDiscoverAndRequest(t *testing.T) {
	guestIP := netip.MustParseAddr("10.77.0.2")
	guestMAC := [6]byte{0x02, 0x72, 0x69, 0x73, 0x00, 0x01}
	stack := &tsnetVirtioStack{
		hostMAC: emunetRouterMAC,
	}

	stack.InjectInboundPacket(dhcpTestFrame(t, dhcpDiscover, 0x12345678, guestMAC))
	offer := onlyPendingEthernetFrame(t, stack)
	assertDHCPReply(t, offer, dhcpOffer, guestIP)
	assertDHCPOptionIP(t, offer, dhcpOptionServer, netip.MustParseAddr("10.77.0.1"))
	assertDHCPOptionIP(t, offer, dhcpOptionRouter, netip.MustParseAddr("10.77.0.1"))
	assertDHCPOptionIP(t, offer, dhcpOptionDNS, netip.MustParseAddr("100.100.100.100"))
	assertDHCPOptionBytes(t, offer, dhcpOptionSubnet, []byte{255, 255, 255, 0})
	assertDHCPOptionUint16(t, offer, dhcpOptionMTU, virtioNetMTU)

	stack.mu.Lock()
	stack.pending = nil
	stack.mu.Unlock()

	stack.InjectInboundPacket(dhcpTestFrame(t, dhcpRequest, 0x12345679, guestMAC))
	ack := onlyPendingEthernetFrame(t, stack)
	assertDHCPReply(t, ack, dhcpAck, guestIP)
}

func TestTsnetVirtioStackEmunetDHCPAllocatesDistinctPortLeases(t *testing.T) {
	stack := &tsnetVirtioStack{hostMAC: emunetRouterMAC}
	macA := [6]byte{0x02, 0, 0, 0, 1, 0x01}
	macB := [6]byte{0x02, 0, 0, 0, 1, 0x02}

	stack.handleGuestFrameForPort("port-a", dhcpTestFrame(t, dhcpDiscover, 0x11111111, macA), stack.injectGuestEthernet)
	offerA := onlyPendingEthernetFrame(t, stack)
	assertDHCPReply(t, offerA, dhcpOffer, netip.MustParseAddr("10.77.0.2"))

	stack.mu.Lock()
	stack.pending = nil
	stack.mu.Unlock()

	stack.handleGuestFrameForPort("port-b", dhcpTestFrame(t, dhcpDiscover, 0x22222222, macB), stack.injectGuestEthernet)
	offerB := onlyPendingEthernetFrame(t, stack)
	assertDHCPReply(t, offerB, dhcpOffer, netip.MustParseAddr("10.77.0.3"))
}

func TestTsnetVirtioStackEmunetDHCPSafelyConsumesMalformedPackets(t *testing.T) {
	stack := &tsnetVirtioStack{hostMAC: emunetRouterMAC}
	frame := dhcpTestFrame(t, dhcpDiscover, 0x33333333, [6]byte{0x02, 0, 0, 0, 1, 0x03})
	frame = frame[:14+20+8+32]

	stack.InjectInboundPacket(frame)
	stack.mu.Lock()
	pending := len(stack.pending)
	stack.mu.Unlock()
	if pending != 0 {
		t.Fatalf("malformed DHCP produced %d pending frames, want 0", pending)
	}
}

func TestTsnetVirtioStackDirectTailnetDHCPDiscover(t *testing.T) {
	guestIP := netip.MustParseAddr("100.64.12.34")
	guestMAC := [6]byte{0x02, 0x72, 0x69, 0x73, 0x00, 0x01}
	stack := &tsnetVirtioStack{
		hostMAC:            emunetRouterMAC,
		directTailnetGuest: true,
	}
	stack.setTailIPv4(guestIP)

	stack.InjectInboundPacket(dhcpTestFrame(t, dhcpDiscover, 0x12345678, guestMAC))
	offer := onlyPendingEthernetFrame(t, stack)
	assertDHCPReply(t, offer, dhcpOffer, guestIP)
}

func TestTsnetVirtioStackEmunetARPForGateway(t *testing.T) {
	guestMAC := [6]byte{0x02, 0, 0, 0, 1, 0x10}
	stack := &tsnetVirtioStack{hostMAC: emunetRouterMAC}

	stack.InjectInboundPacket(arpRequestFrame(guestMAC, [4]byte{10, 77, 0, 2}, [4]byte{10, 77, 0, 1}))
	reply := onlyPendingEthernetFrame(t, stack)
	if got := binary.BigEndian.Uint16(reply[12:14]); got != etherTypeARP {
		t.Fatalf("ether type = %#x, want ARP", got)
	}
	if !bytes.Equal(reply[0:6], guestMAC[:]) {
		t.Fatalf("ARP dst MAC = %x, want %x", reply[0:6], guestMAC)
	}
	if !bytes.Equal(reply[6:12], emunetRouterMAC[:]) {
		t.Fatalf("ARP src MAC = %x, want router %x", reply[6:12], emunetRouterMAC)
	}
	if got := binary.BigEndian.Uint16(reply[20:22]); got != 2 {
		t.Fatalf("ARP op = %d, want reply", got)
	}
	if !bytes.Equal(reply[22:28], emunetRouterMAC[:]) {
		t.Fatalf("ARP sender MAC = %x, want router %x", reply[22:28], emunetRouterMAC)
	}
	if got := [4]byte{reply[28], reply[29], reply[30], reply[31]}; got != [4]byte{10, 77, 0, 1} {
		t.Fatalf("ARP sender IP = %v, want 10.77.0.1", got)
	}
}

func TestTsnetVirtioStackEmunetARPForUnrelatedIPDoesNotReply(t *testing.T) {
	guestMAC := [6]byte{0x02, 0, 0, 0, 1, 0x20}
	stack := &tsnetVirtioStack{hostMAC: emunetRouterMAC}

	stack.InjectInboundPacket(arpRequestFrame(guestMAC, [4]byte{10, 77, 0, 2}, [4]byte{8, 8, 8, 8}))
	stack.mu.Lock()
	pending := len(stack.pending)
	stack.mu.Unlock()
	if pending != 0 {
		t.Fatalf("ARP for unrelated IP produced %d replies, want 0", pending)
	}
}

func TestTsnetVirtioStackEmunetGatewayPing(t *testing.T) {
	guestMAC := [6]byte{0x02, 0, 0, 0, 1, 0x11}
	stack := &tsnetVirtioStack{hostMAC: emunetRouterMAC}

	stack.InjectInboundPacket(icmpEchoFrame(guestMAC, [4]byte{10, 77, 0, 2}, [4]byte{10, 77, 0, 1}, 0x1234, 1))
	reply := onlyPendingEthernetFrame(t, stack)
	if got := binary.BigEndian.Uint16(reply[12:14]); got != etherTypeIPv4 {
		t.Fatalf("ether type = %#x, want IPv4", got)
	}
	if !bytes.Equal(reply[0:6], guestMAC[:]) {
		t.Fatalf("reply dst MAC = %x, want %x", reply[0:6], guestMAC)
	}
	ip := reply[14:]
	ihl := int(ip[0]&0x0f) * 4
	if ip[9] != ipProtoICMP || ip[ihl] != icmpEchoReply {
		t.Fatalf("reply protocol/type = %d/%d, want ICMP echo reply", ip[9], ip[ihl])
	}
	if got := [4]byte{ip[12], ip[13], ip[14], ip[15]}; got != [4]byte{10, 77, 0, 1} {
		t.Fatalf("reply src IP = %v, want gateway", got)
	}
	if got := [4]byte{ip[16], ip[17], ip[18], ip[19]}; got != [4]byte{10, 77, 0, 2} {
		t.Fatalf("reply dst IP = %v, want guest", got)
	}
}

func TestTsnetVirtioStackEmunetIgnoresNonIPv4Traffic(t *testing.T) {
	stack := &tsnetVirtioStack{hostMAC: emunetRouterMAC}
	frame := make([]byte, 14+40)
	binary.BigEndian.PutUint16(frame[12:14], etherTypeIPv6)
	frame[14] = 0x60

	stack.InjectInboundPacket(frame)
	stack.mu.Lock()
	pending := len(stack.pending)
	stack.mu.Unlock()
	if pending != 0 {
		t.Fatalf("IPv6 frame produced %d pending frames, want 0", pending)
	}
}

func TestTsnetVirtioStackEmunetUDPNATRoundTrip(t *testing.T) {
	stack := &tsnetVirtioStack{hostMAC: emunetRouterMAC}
	tailIP := netip.MustParseAddr("100.64.12.34")
	stack.setTailIPv4(tailIP)
	guestMAC := [6]byte{0x02, 0, 0, 0, 1, 0x12}
	portID := "follower-a"
	stack.learnPortMAC(portID, guestMAC, func([]byte) {})

	out := stack.translateOutboundIPv4(portID, udpIPv4Packet([4]byte{10, 77, 0, 2}, [4]byte{8, 8, 8, 8}, 1234, 53, []byte("hello")), tailIP, func([]byte) {})
	if len(out) == 0 {
		t.Fatal("NAT outbound dropped UDP packet")
	}
	assertIPv4ChecksumValid(t, out)
	assertTransportChecksumValid(t, out, ipProtoUDP)
	if got := out[8]; got != 63 {
		t.Fatalf("NAT UDP TTL = %d, want 63", got)
	}
	if got := [4]byte{out[12], out[13], out[14], out[15]}; got != tailIP.As4() {
		t.Fatalf("NAT src IP = %v, want %s", got, tailIP)
	}
	ext := binary.BigEndian.Uint16(out[20:22])
	if ext < 40000 || ext > 60999 {
		t.Fatalf("NAT source port = %d, want allocated range", ext)
	}

	reply := udpIPv4Packet([4]byte{8, 8, 8, 8}, tailIP.As4(), 53, ext, []byte("world"))
	gotPortID, gotMAC, guestPkt, _, ok := stack.natInbound(reply)
	if !ok {
		t.Fatal("NAT inbound did not match UDP reply")
	}
	if gotPortID != portID {
		t.Fatalf("NAT port ID = %q, want %q", gotPortID, portID)
	}
	if gotMAC != guestMAC {
		t.Fatalf("NAT guest MAC = %x, want %x", gotMAC, guestMAC)
	}
	if got := [4]byte{guestPkt[16], guestPkt[17], guestPkt[18], guestPkt[19]}; got != [4]byte{10, 77, 0, 2} {
		t.Fatalf("NAT dst IP = %v, want guest", got)
	}
	assertIPv4ChecksumValid(t, guestPkt)
	assertTransportChecksumValid(t, guestPkt, ipProtoUDP)
	if got := binary.BigEndian.Uint16(guestPkt[22:24]); got != 1234 {
		t.Fatalf("NAT dst port = %d, want guest port 1234", got)
	}
}

func TestTsnetVirtioStackEmunetUDPNATPreservesZeroChecksum(t *testing.T) {
	stack := &tsnetVirtioStack{hostMAC: emunetRouterMAC}
	tailIP := netip.MustParseAddr("100.64.12.34")
	packet := udpIPv4Packet([4]byte{10, 77, 0, 2}, [4]byte{8, 8, 8, 8}, 1234, 53, []byte("hello"))
	packet[26], packet[27] = 0, 0

	out := stack.translateOutboundIPv4("zero-udp", packet, tailIP, func([]byte) {})
	if len(out) == 0 {
		t.Fatal("NAT outbound dropped UDP packet")
	}
	if got := binary.BigEndian.Uint16(out[26:28]); got != 0 {
		t.Fatalf("NAT UDP checksum = %#x, want zero preserved", got)
	}
}

func TestTsnetVirtioStackEmunetTCPNATRoundTrip(t *testing.T) {
	stack := &tsnetVirtioStack{hostMAC: emunetRouterMAC}
	tailIP := netip.MustParseAddr("100.64.12.34")
	stack.setTailIPv4(tailIP)
	guestMAC := [6]byte{0x02, 0, 0, 0, 1, 0x15}
	portID := "tcp-follower"
	stack.learnPortMAC(portID, guestMAC, func([]byte) {})

	out := stack.translateOutboundIPv4(portID, tcpIPv4Packet([4]byte{10, 77, 0, 2}, [4]byte{100, 100, 100, 100}, 1234, 443, 0x02, nil), tailIP, func([]byte) {})
	if len(out) == 0 {
		t.Fatal("NAT outbound dropped TCP packet")
	}
	assertIPv4ChecksumValid(t, out)
	assertTransportChecksumValid(t, out, ipProtoTCP)
	if got := out[8]; got != 63 {
		t.Fatalf("NAT TCP TTL = %d, want 63", got)
	}
	if got := [4]byte{out[12], out[13], out[14], out[15]}; got != tailIP.As4() {
		t.Fatalf("NAT TCP src IP = %v, want %s", got, tailIP)
	}
	ext := binary.BigEndian.Uint16(out[20:22])
	if ext < 40000 || ext > 60999 {
		t.Fatalf("NAT TCP source port = %d, want allocated range", ext)
	}

	reply := tcpIPv4Packet([4]byte{100, 100, 100, 100}, tailIP.As4(), 443, ext, 0x12, nil)
	gotPortID, gotMAC, guestPkt, _, ok := stack.natInbound(reply)
	if !ok {
		t.Fatal("NAT inbound did not match TCP reply")
	}
	if gotPortID != portID {
		t.Fatalf("NAT TCP port ID = %q, want %q", gotPortID, portID)
	}
	if gotMAC != guestMAC {
		t.Fatalf("NAT TCP guest MAC = %x, want %x", gotMAC, guestMAC)
	}
	assertIPv4ChecksumValid(t, guestPkt)
	assertTransportChecksumValid(t, guestPkt, ipProtoTCP)
	if got := [4]byte{guestPkt[16], guestPkt[17], guestPkt[18], guestPkt[19]}; got != [4]byte{10, 77, 0, 2} {
		t.Fatalf("NAT TCP dst IP = %v, want guest", got)
	}
	if got := binary.BigEndian.Uint16(guestPkt[22:24]); got != 1234 {
		t.Fatalf("NAT TCP dst port = %d, want guest port 1234", got)
	}
}

func TestTsnetVirtioStackEmunetICMPNATRoundTrip(t *testing.T) {
	stack := &tsnetVirtioStack{hostMAC: emunetRouterMAC}
	tailIP := netip.MustParseAddr("100.64.12.34")
	stack.setTailIPv4(tailIP)
	guestMAC := [6]byte{0x02, 0, 0, 0, 1, 0x16}
	portID := "icmp-follower"
	stack.learnPortMAC(portID, guestMAC, func([]byte) {})

	out := stack.translateOutboundIPv4(portID, icmpIPv4Packet([4]byte{10, 77, 0, 2}, [4]byte{8, 8, 8, 8}, icmpEchoRequest, 0x1234, 1), tailIP, func([]byte) {})
	if len(out) == 0 {
		t.Fatal("NAT outbound dropped ICMP echo packet")
	}
	assertIPv4ChecksumValid(t, out)
	assertICMPChecksumValid(t, out)
	ext := binary.BigEndian.Uint16(out[24:26])
	if ext == 0x1234 {
		t.Fatalf("NAT ICMP identifier was not rewritten")
	}

	reply := icmpIPv4Packet([4]byte{8, 8, 8, 8}, tailIP.As4(), icmpEchoReply, ext, 1)
	gotPortID, gotMAC, guestPkt, _, ok := stack.natInbound(reply)
	if !ok {
		t.Fatal("NAT inbound did not match ICMP echo reply")
	}
	if gotPortID != portID {
		t.Fatalf("NAT ICMP port ID = %q, want %q", gotPortID, portID)
	}
	if gotMAC != guestMAC {
		t.Fatalf("NAT ICMP guest MAC = %x, want %x", gotMAC, guestMAC)
	}
	assertIPv4ChecksumValid(t, guestPkt)
	assertICMPChecksumValid(t, guestPkt)
	if got := binary.BigEndian.Uint16(guestPkt[24:26]); got != 0x1234 {
		t.Fatalf("NAT ICMP restored id = %#x, want 0x1234", got)
	}
}

func TestTsnetVirtioStackEmunetNATDistinguishesSameGuestPortOnDifferentPorts(t *testing.T) {
	stack := &tsnetVirtioStack{hostMAC: emunetRouterMAC}
	tailIP := netip.MustParseAddr("100.64.12.34")
	stack.setTailIPv4(tailIP)
	stack.learnPortMAC("guest-a", [6]byte{0x02, 0, 0, 0, 1, 0x17}, func([]byte) {})
	stack.learnPortMAC("guest-b", [6]byte{0x02, 0, 0, 0, 1, 0x18}, func([]byte) {})

	packet := udpIPv4Packet([4]byte{10, 77, 0, 2}, [4]byte{8, 8, 8, 8}, 1234, 53, []byte("hello"))
	outA := stack.translateOutboundIPv4("guest-a", packet, tailIP, func([]byte) {})
	outB := stack.translateOutboundIPv4("guest-b", packet, tailIP, func([]byte) {})
	if len(outA) == 0 || len(outB) == 0 {
		t.Fatalf("NAT dropped duplicate-port packets: lenA=%d lenB=%d", len(outA), len(outB))
	}
	extA := binary.BigEndian.Uint16(outA[20:22])
	extB := binary.BigEndian.Uint16(outB[20:22])
	if extA == extB {
		t.Fatalf("duplicate guest ports got same external NAT port %d", extA)
	}
}

func TestTsnetVirtioStackEmunetNATReusesExistingMappingForSameFlow(t *testing.T) {
	stack := &tsnetVirtioStack{hostMAC: emunetRouterMAC}
	tailIP := netip.MustParseAddr("100.64.12.34")
	packet := udpIPv4Packet([4]byte{10, 77, 0, 2}, [4]byte{8, 8, 8, 8}, 1234, 53, []byte("hello"))
	outA := stack.translateOutboundIPv4("guest-a", packet, tailIP, func([]byte) {})
	outB := stack.translateOutboundIPv4("guest-a", packet, tailIP, func([]byte) {})
	if len(outA) == 0 || len(outB) == 0 {
		t.Fatalf("NAT dropped repeated flow packets: lenA=%d lenB=%d", len(outA), len(outB))
	}
	if extA, extB := binary.BigEndian.Uint16(outA[20:22]), binary.BigEndian.Uint16(outB[20:22]); extA != extB {
		t.Fatalf("same flow got different external ports %d and %d", extA, extB)
	}
}

func TestTsnetVirtioStackEmunetNATAllocatorSkipsUsedExternalPorts(t *testing.T) {
	stack := &tsnetVirtioStack{hostMAC: emunetRouterMAC, nextNATID: 40000}
	tailIP := netip.MustParseAddr("100.64.12.34")
	packetA := udpIPv4Packet([4]byte{10, 77, 0, 2}, [4]byte{8, 8, 8, 8}, 1234, 53, []byte("a"))
	packetB := udpIPv4Packet([4]byte{10, 77, 0, 3}, [4]byte{8, 8, 8, 8}, 1234, 53, []byte("b"))
	outA := stack.translateOutboundIPv4("guest-a", packetA, tailIP, func([]byte) {})
	outB := stack.translateOutboundIPv4("guest-b", packetB, tailIP, func([]byte) {})
	if len(outA) == 0 || len(outB) == 0 {
		t.Fatalf("NAT dropped allocator test packets: lenA=%d lenB=%d", len(outA), len(outB))
	}
	if extA, extB := binary.BigEndian.Uint16(outA[20:22]), binary.BigEndian.Uint16(outB[20:22]); extA != 40000 || extB != 40001 {
		t.Fatalf("allocator externals = %d/%d, want 40000/40001", extA, extB)
	}
}

func TestTsnetVirtioStackEmunetNATDropsFragmentsTTLAndUnmatchedInbound(t *testing.T) {
	stack := &tsnetVirtioStack{hostMAC: emunetRouterMAC}
	tailIP := netip.MustParseAddr("100.64.12.34")
	stack.setTailIPv4(tailIP)
	packet := udpIPv4Packet([4]byte{10, 77, 0, 2}, [4]byte{8, 8, 8, 8}, 1234, 53, []byte("hello"))

	fragment := append([]byte(nil), packet...)
	binary.BigEndian.PutUint16(fragment[6:8], 0x2000)
	if out := stack.translateOutboundIPv4("guest-a", fragment, tailIP, func([]byte) {}); len(out) != 0 {
		t.Fatalf("fragmented packet translated to %d bytes, want drop", len(out))
	}

	ttlExpired := append([]byte(nil), packet...)
	ttlExpired[8] = 1
	if out := stack.translateOutboundIPv4("guest-a", ttlExpired, tailIP, func([]byte) {}); len(out) != 0 {
		t.Fatalf("TTL-expired packet translated to %d bytes, want drop", len(out))
	}

	reply := udpIPv4Packet([4]byte{8, 8, 8, 8}, tailIP.As4(), 53, 40000, []byte("world"))
	if _, _, _, _, ok := stack.natInbound(reply); ok {
		t.Fatalf("unmatched inbound packet unexpectedly matched NAT state")
	}
}

func TestTsnetVirtioStackEmunetNATDropsBadLengthsAndUnsupportedProtocol(t *testing.T) {
	stack := &tsnetVirtioStack{hostMAC: emunetRouterMAC}
	tailIP := netip.MustParseAddr("100.64.12.34")
	packet := udpIPv4Packet([4]byte{10, 77, 0, 2}, [4]byte{8, 8, 8, 8}, 1234, 53, []byte("hello"))

	tooShort := packet[:19]
	if out := stack.translateOutboundIPv4("guest-a", tooShort, tailIP, func([]byte) {}); len(out) != 0 {
		t.Fatalf("short IPv4 packet translated to %d bytes, want drop", len(out))
	}

	badIHL := append([]byte(nil), packet...)
	badIHL[0] = 0x44
	if out := stack.translateOutboundIPv4("guest-a", badIHL, tailIP, func([]byte) {}); len(out) != 0 {
		t.Fatalf("bad-IHL packet translated to %d bytes, want drop", len(out))
	}

	badTotal := append([]byte(nil), packet...)
	binary.BigEndian.PutUint16(badTotal[2:4], uint16(len(packet)+1))
	if out := stack.translateOutboundIPv4("guest-a", badTotal, tailIP, func([]byte) {}); len(out) != 0 {
		t.Fatalf("bad-total-length packet translated to %d bytes, want drop", len(out))
	}

	unsupported := ipv4Packet([4]byte{10, 77, 0, 2}, [4]byte{8, 8, 8, 8}, 99, []byte("payload"))
	if out := stack.translateOutboundIPv4("guest-a", unsupported, tailIP, func([]byte) {}); len(out) != 0 {
		t.Fatalf("unsupported-protocol packet translated to %d bytes, want drop", len(out))
	}
}

func TestTsnetVirtioStackEmunetNATExpiresIdleMappings(t *testing.T) {
	now := time.Unix(1000, 0)
	stack := &tsnetVirtioStack{
		hostMAC: emunetRouterMAC,
		now:     func() time.Time { return now },
	}
	tailIP := netip.MustParseAddr("100.64.12.34")
	stack.setTailIPv4(tailIP)
	guestMAC := [6]byte{0x02, 0, 0, 0, 1, 0x19}
	portID := "expiring-udp"
	stack.learnPortMAC(portID, guestMAC, func([]byte) {})
	out := stack.translateOutboundIPv4(portID, udpIPv4Packet([4]byte{10, 77, 0, 2}, [4]byte{8, 8, 8, 8}, 1234, 53, []byte("hello")), tailIP, func([]byte) {})
	if len(out) == 0 {
		t.Fatal("NAT outbound dropped UDP packet")
	}
	ext := binary.BigEndian.Uint16(out[20:22])

	now = now.Add(emunetUDPIdleTimeout + time.Nanosecond)
	if removed := stack.cleanupExpiredNAT(); removed != 1 {
		t.Fatalf("expired NAT mappings removed = %d, want 1", removed)
	}
	reply := udpIPv4Packet([4]byte{8, 8, 8, 8}, tailIP.As4(), 53, ext, []byte("world"))
	if _, _, _, _, ok := stack.natInbound(reply); ok {
		t.Fatalf("expired NAT mapping accepted inbound reply")
	}
}

func TestTsnetVirtioStackEmunetNATInboundRefreshesIdleTimeout(t *testing.T) {
	now := time.Unix(2000, 0)
	stack := &tsnetVirtioStack{
		hostMAC: emunetRouterMAC,
		now:     func() time.Time { return now },
	}
	tailIP := netip.MustParseAddr("100.64.12.34")
	stack.setTailIPv4(tailIP)
	guestMAC := [6]byte{0x02, 0, 0, 0, 1, 0x1a}
	portID := "refreshing-udp"
	stack.learnPortMAC(portID, guestMAC, func([]byte) {})
	out := stack.translateOutboundIPv4(portID, udpIPv4Packet([4]byte{10, 77, 0, 2}, [4]byte{8, 8, 8, 8}, 1234, 53, []byte("hello")), tailIP, func([]byte) {})
	if len(out) == 0 {
		t.Fatal("NAT outbound dropped UDP packet")
	}
	ext := binary.BigEndian.Uint16(out[20:22])
	reply := udpIPv4Packet([4]byte{8, 8, 8, 8}, tailIP.As4(), 53, ext, []byte("world"))

	now = now.Add(emunetUDPIdleTimeout - time.Second)
	if _, _, _, _, ok := stack.natInbound(reply); !ok {
		t.Fatalf("active NAT mapping did not accept inbound reply")
	}
	now = now.Add(emunetUDPIdleTimeout - time.Second)
	if removed := stack.cleanupExpiredNAT(); removed != 0 {
		t.Fatalf("refreshed NAT mappings removed = %d, want 0", removed)
	}
	if _, _, _, _, ok := stack.natInbound(reply); !ok {
		t.Fatalf("refreshed NAT mapping did not accept later inbound reply")
	}
}

func TestEmunetNATIdleTimeoutsAreProtocolSpecific(t *testing.T) {
	if got := emunetNATIdleTimeout(ipProtoICMP); got != 30*time.Second {
		t.Fatalf("ICMP NAT timeout = %s, want 30s", got)
	}
	if got := emunetNATIdleTimeout(ipProtoUDP); got != 2*time.Minute {
		t.Fatalf("UDP NAT timeout = %s, want 2m", got)
	}
	if got := emunetNATIdleTimeout(ipProtoTCP); got != 10*time.Minute {
		t.Fatalf("TCP NAT timeout = %s, want 10m", got)
	}
}

func TestTsnetVirtioStackRemoveEmunetPortRemovesLeaseAndNAT(t *testing.T) {
	stack := &tsnetVirtioStack{hostMAC: emunetRouterMAC}
	tailIP := netip.MustParseAddr("100.64.12.34")
	stack.setTailIPv4(tailIP)
	guestMAC := [6]byte{0x02, 0, 0, 0, 1, 0x13}
	portID := "follower-to-remove"
	stack.learnPortMAC(portID, guestMAC, func([]byte) {})
	out := stack.translateOutboundIPv4(portID, udpIPv4Packet([4]byte{10, 77, 0, 2}, [4]byte{8, 8, 8, 8}, 1234, 53, []byte("hello")), tailIP, func([]byte) {})
	if len(out) == 0 {
		t.Fatal("NAT outbound dropped UDP packet")
	}

	stack.removeEmunetPort(portID)

	stack.mu.Lock()
	defer stack.mu.Unlock()
	if _, ok := stack.ports[portID]; ok {
		t.Fatalf("port %q still exists after remove", portID)
	}
	for key := range stack.natByOut {
		if key.portID == portID {
			t.Fatalf("outbound NAT key for removed port still exists: %#v", key)
		}
	}
	if len(stack.natByIn) != 0 {
		t.Fatalf("inbound NAT mappings = %d, want 0 after removing only port", len(stack.natByIn))
	}
}

func TestTsnetVirtioStackEmunetCountersRecordSuccessesAndDrops(t *testing.T) {
	stack := &tsnetVirtioStack{hostMAC: emunetRouterMAC}
	tailIP := netip.MustParseAddr("100.64.12.34")
	stack.setTailIPv4(tailIP)
	guestMAC := [6]byte{0x02, 0, 0, 0, 1, 0x1b}
	stack.learnPortMAC("counter-port", guestMAC, func([]byte) {})

	stack.InjectInboundPacket(dhcpTestFrame(t, dhcpDiscover, 0x77777777, guestMAC))
	_ = onlyPendingEthernetFrame(t, stack)
	stack.mu.Lock()
	stack.pending = nil
	stack.mu.Unlock()
	stack.InjectInboundPacket(dhcpTestFrame(t, dhcpRequest, 0x88888888, guestMAC))
	_ = onlyPendingEthernetFrame(t, stack)
	stack.mu.Lock()
	stack.pending = nil
	stack.mu.Unlock()
	stack.InjectInboundPacket(arpRequestFrame(guestMAC, [4]byte{10, 77, 0, 2}, [4]byte{10, 77, 0, 1}))
	_ = onlyPendingEthernetFrame(t, stack)
	stack.mu.Lock()
	stack.pending = nil
	stack.mu.Unlock()
	stack.InjectInboundPacket(icmpEchoFrame(guestMAC, [4]byte{10, 77, 0, 2}, [4]byte{10, 77, 0, 1}, 0x1234, 1))
	_ = onlyPendingEthernetFrame(t, stack)

	udpOut := stack.translateOutboundIPv4("counter-port", udpIPv4Packet([4]byte{10, 77, 0, 2}, [4]byte{8, 8, 8, 8}, 1234, 53, []byte("udp")), tailIP, func([]byte) {})
	tcpOut := stack.translateOutboundIPv4("counter-port", tcpIPv4Packet([4]byte{10, 77, 0, 2}, [4]byte{8, 8, 4, 4}, 2345, 443, 0x02, nil), tailIP, func([]byte) {})
	icmpOut := stack.translateOutboundIPv4("counter-port", icmpIPv4Packet([4]byte{10, 77, 0, 2}, [4]byte{1, 1, 1, 1}, icmpEchoRequest, 0x3456, 1), tailIP, func([]byte) {})
	if len(udpOut) == 0 || len(tcpOut) == 0 || len(icmpOut) == 0 {
		t.Fatalf("NAT success counter setup dropped packet lengths udp=%d tcp=%d icmp=%d", len(udpOut), len(tcpOut), len(icmpOut))
	}
	_, _, _, _, _ = stack.natInbound(udpIPv4Packet([4]byte{8, 8, 8, 8}, tailIP.As4(), 53, binary.BigEndian.Uint16(udpOut[20:22]), []byte("reply")))
	_, _, _, _, _ = stack.natInbound(tcpIPv4Packet([4]byte{8, 8, 4, 4}, tailIP.As4(), 443, binary.BigEndian.Uint16(tcpOut[20:22]), 0x12, nil))
	_, _, _, _, _ = stack.natInbound(icmpIPv4Packet([4]byte{1, 1, 1, 1}, tailIP.As4(), icmpEchoReply, binary.BigEndian.Uint16(icmpOut[24:26]), 1))

	fragment := udpIPv4Packet([4]byte{10, 77, 0, 2}, [4]byte{9, 9, 9, 9}, 3456, 53, []byte("drop"))
	binary.BigEndian.PutUint16(fragment[6:8], 0x2000)
	_ = stack.translateOutboundIPv4("counter-port", fragment, tailIP, func([]byte) {})
	_ = stack.translateOutboundIPv4("counter-port", ipv4Packet([4]byte{10, 77, 0, 2}, [4]byte{9, 9, 9, 9}, 99, []byte("drop")), tailIP, func([]byte) {})
	_, _, _, _, _ = stack.natInbound(udpIPv4Packet([4]byte{9, 9, 9, 9}, tailIP.As4(), 53, 49999, []byte("drop")))

	counters := stack.counterSnapshot()
	if counters.DHCPOffers != 1 || counters.DHCPAcks != 1 || counters.ARPReplies != 1 || counters.GatewayICMPReplies != 1 {
		t.Fatalf("LAN counters = offers:%d acks:%d arp:%d gwicmp:%d, want all 1",
			counters.DHCPOffers, counters.DHCPAcks, counters.ARPReplies, counters.GatewayICMPReplies)
	}
	if counters.NATOutboundUDP != 1 || counters.NATOutboundTCP != 1 || counters.NATOutboundICMP != 1 {
		t.Fatalf("NAT outbound counters = udp:%d tcp:%d icmp:%d, want all 1",
			counters.NATOutboundUDP, counters.NATOutboundTCP, counters.NATOutboundICMP)
	}
	if counters.NATInboundUDP != 1 || counters.NATInboundTCP != 1 || counters.NATInboundICMP != 1 {
		t.Fatalf("NAT inbound counters = udp:%d tcp:%d icmp:%d, want all 1",
			counters.NATInboundUDP, counters.NATInboundTCP, counters.NATInboundICMP)
	}
	if counters.Drops[emunetDropIPv4Fragment] != 1 ||
		counters.Drops[emunetDropUnsupportedProto] != 1 ||
		counters.Drops[emunetDropNoNATMapping] != 1 {
		t.Fatalf("drop counters = %#v, want fragment/unsupported/no mapping each 1", counters.Drops)
	}
	counters.Drops[emunetDropIPv4Fragment] = 999
	if got := stack.counterSnapshot().Drops[emunetDropIPv4Fragment]; got != 1 {
		t.Fatalf("counter snapshot was not isolated: got fragment count %d, want 1", got)
	}
}

func TestTsnetVirtioStackEmunetTraceLogsDrops(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("RISCV_EMU_EMUNET_TRACE", "1")
	stack := &tsnetVirtioStack{hostMAC: emunetRouterMAC}
	tailIP := netip.MustParseAddr("100.64.12.34")
	fragment := udpIPv4Packet([4]byte{10, 77, 0, 2}, [4]byte{8, 8, 8, 8}, 1234, 53, []byte("hello"))
	binary.BigEndian.PutUint16(fragment[6:8], 0x2000)

	_ = stack.translateOutboundIPv4("trace-port", fragment, tailIP, func([]byte) {})

	got, err := os.ReadFile(tsnetOpLogPath())
	if err != nil {
		t.Fatalf("read trace oplog: %v", err)
	}
	text := string(got)
	if !strings.Contains(text, "emunet_trace drop") || !strings.Contains(text, emunetDropIPv4Fragment) {
		t.Fatalf("trace oplog missing emunet drop line: %q", text)
	}
}

func TestTsnetVirtioStackEmunetCounterSnapshotsDuringTraffic(t *testing.T) {
	stack := &tsnetVirtioStack{hostMAC: emunetRouterMAC}
	tailIP := netip.MustParseAddr("100.64.12.34")
	stack.setTailIPv4(tailIP)
	workersDone := make(chan struct{})
	errCh := make(chan string, 1)

	var wg sync.WaitGroup
	for worker := range 8 {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := range 100 {
				portID := fmt.Sprintf("worker-%d", worker)
				src := [4]byte{10, 77, byte(worker), byte(i + 2)}
				dst := [4]byte{8, 8, 8, byte(worker + 1)}
				packet := udpIPv4Packet(src, dst, uint16(1000+i), 53, []byte("traffic"))
				if out := stack.translateOutboundIPv4(portID, packet, tailIP, func([]byte) {}); len(out) == 0 {
					select {
					case errCh <- fmt.Sprintf("worker %d packet %d dropped", worker, i):
					default:
					}
					return
				}
				_ = stack.counterSnapshot()
			}
		}(worker)
	}
	go func() {
		wg.Wait()
		close(workersDone)
	}()

	for {
		select {
		case msg := <-errCh:
			t.Fatal(msg)
		case <-workersDone:
			if got := stack.counterSnapshot().NATOutboundUDP; got == 0 {
				t.Fatalf("NATOutboundUDP = 0, want concurrent traffic to increment it")
			}
			return
		default:
			_ = stack.counterSnapshot()
			time.Sleep(time.Millisecond)
		}
	}
}

func TestTsnetDirDefaultsToHostPersistentStateDir(t *testing.T) {
	t.Setenv("RISCV_EMU_TSNET_DIR", "")
	t.Setenv("HOME", "/tmp/riscv-emu-home")
	want := "/tmp/riscv-emu-home/.emunet/riscv-emu"
	if got := tsnetDir(); got != want {
		t.Fatalf("tsnetDir default = %q, want %q", got, want)
	}

	t.Setenv("RISCV_EMU_TSNET_DIR", "/tmp/emutail-test")
	if got := tsnetDir(); got != "/tmp/emutail-test" {
		t.Fatalf("tsnetDir override = %q, want env value", got)
	}
}

func TestTsnetOpLogDefaultsToPerProcessEmunetStateDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if got, want := tsnetOpLogPath(), filepath.Join(home, ".local", "state", "emunet", fmt.Sprintf("oplog.%d", os.Getpid())); got != want {
		t.Fatalf("tsnetOpLogPath = %q, want %q", got, want)
	}
	appendTsnetOpLog("test_event value=%d", 42)
	appendTsnetOpLog("second_event")

	got, err := os.ReadFile(tsnetOpLogPath())
	if err != nil {
		t.Fatalf("read oplog: %v", err)
	}
	text := string(got)
	if !strings.Contains(text, "test_event value=42") {
		t.Fatalf("oplog missing first event: %q", text)
	}
	if !strings.Contains(text, "second_event") {
		t.Fatalf("oplog missing second event: %q", text)
	}
	for _, line := range strings.Split(strings.TrimSpace(text), "\n") {
		if !regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}(Z|[+-]\d{2}:\d{2}) `).MatchString(line) {
			t.Fatalf("oplog line has wrong timestamp format: %q", line)
		}
	}
}

func TestTsnetUserLogfAppendsUserVisibleLinesToOpLog(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	tsnetUserLogf("auth url: %s\nstatus: %s", "https://login.tailscale.com/a/example", "waiting")

	got, err := os.ReadFile(tsnetOpLogPath())
	if err != nil {
		t.Fatalf("read oplog: %v", err)
	}
	text := string(got)
	if !strings.Contains(text, "tsnet_user auth url: https://login.tailscale.com/a/example") {
		t.Fatalf("oplog missing auth URL line: %q", text)
	}
	if !strings.Contains(text, "tsnet_user status: waiting") {
		t.Fatalf("oplog missing second user log line: %q", text)
	}
}

func TestUpdateEmunetLeaderOpLogLinkPointsToCurrentOpLog(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	appendTsnetOpLog("leader_ready")
	link := updateEmunetLeaderOpLogLink()
	wantLink := filepath.Join(home, ".emunet", "oplog.leader.lnk")
	if link != wantLink {
		t.Fatalf("leader oplog link = %q, want %q", link, wantLink)
	}
	target, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("read leader oplog symlink: %v", err)
	}
	if want := tsnetOpLogPath(); target != want {
		t.Fatalf("leader oplog symlink target = %q, want %q", target, want)
	}
	got, err := os.ReadFile(link)
	if err != nil {
		t.Fatalf("read leader oplog through symlink: %v", err)
	}
	if !strings.Contains(string(got), "leader_ready") {
		t.Fatalf("leader oplog symlink did not resolve to current oplog: %q", got)
	}
}

func TestUpdateEmunetLeaderOpLogLinkReplacesRegularFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	link := emunetLeaderOpLogLinkPath()
	if err := os.MkdirAll(filepath.Dir(link), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(link, []byte("keep me\n"), 0600); err != nil {
		t.Fatal(err)
	}

	updateEmunetLeaderOpLogLink()
	target, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("read leader oplog symlink: %v", err)
	}
	if want := tsnetOpLogPath(); target != want {
		t.Fatalf("leader oplog symlink target = %q, want %q", target, want)
	}
}

func TestWriteEmunetLeaderPIDFileReplacesStaleLeaderFiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := emunetDir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, "leader.123")
	other := filepath.Join(dir, "tailscaled.state")
	if err := os.WriteFile(stale, []byte("123\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(other, []byte("keep\n"), 0600); err != nil {
		t.Fatal(err)
	}

	path := writeEmunetLeaderPIDFile()
	want := filepath.Join(dir, fmt.Sprintf("leader.%d", os.Getpid()))
	if path != want {
		t.Fatalf("leader pid path = %q, want %q", path, want)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != fmt.Sprintf("%d\n", os.Getpid()) {
		t.Fatalf("leader pid file = %q, want current pid", got)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale leader file still exists or unexpected stat error: %v", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatalf("non-leader file should remain: %v", err)
	}
}

func TestEmunetFollowerDoesNotStartTsnetBeforePromotion(t *testing.T) {
	t.Setenv("RPC25519_SERVER_DATA_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	leader, dnsSrv, addr := startTestEmunetLeaderDNS(t, ctx)
	defer leader.Close()
	defer dnsSrv.Close()
	t.Setenv("RISCV_EMU_EMUNET_ADDR", addr)

	starts := installFakeEmunetLeaderHook(t, 20*time.Millisecond)

	stackIf, err := newEmunetVirtioStack(EmuConfig{})
	if err != nil {
		t.Fatal(err)
	}
	stack := stackIf.(*emunetVirtioStack)
	defer stack.Close()

	time.Sleep(3 * emunetWatchDogInterval)
	if got := starts.Load(); got != 0 {
		t.Fatalf("leader tsnet starts = %d, want 0 while follower DNS owner is alive", got)
	}
	stack.mu.Lock()
	role := stack.role
	core := stack.leaderCore
	leaderCkt := stack.leaderCkt
	stack.mu.Unlock()
	if role != "follower" {
		t.Fatalf("role = %q, want follower", role)
	}
	if core != nil {
		t.Fatalf("follower unexpectedly has leader core: %#v", core)
	}
	if leaderCkt == nil {
		t.Fatalf("follower did not connect to leader circuit")
	}
}

func TestEmunetFollowerWatchDogPromotesAfterRendezvousFreed(t *testing.T) {
	t.Setenv("RPC25519_SERVER_DATA_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	leader, dnsSrv, addr := startTestEmunetLeaderDNS(t, ctx)
	defer leader.Close()
	t.Setenv("RISCV_EMU_EMUNET_ADDR", addr)

	starts := installFakeEmunetLeaderHook(t, 10*time.Millisecond)

	stackIf, err := newEmunetVirtioStack(EmuConfig{})
	if err != nil {
		t.Fatal(err)
	}
	stack := stackIf.(*emunetVirtioStack)
	defer stack.Close()

	if got := starts.Load(); got != 0 {
		t.Fatalf("leader tsnet starts before failover = %d, want 0", got)
	}
	if err := dnsSrv.Close(); err != nil {
		t.Fatalf("close leader DNS: %v", err)
	}

	waitForTestCondition(t, time.Second, func() bool {
		stack.mu.Lock()
		defer stack.mu.Unlock()
		return stack.role == "leader" && stack.leaderCore != nil && stack.dns != nil && stack.leaderCkt == nil
	})
	if got := starts.Load(); got != 1 {
		t.Fatalf("leader tsnet starts after failover = %d, want 1", got)
	}

	dnsCtx, dnsCancel := context.WithTimeout(context.Background(), time.Second)
	defer dnsCancel()
	dns, err := emunetpkg.LookupDNS(dnsCtx, addr)
	if err != nil {
		t.Fatal(err)
	}
	if dns.LeaderURL != stack.node.PeerURL() {
		t.Fatalf("promoted DNS leader URL = %q, want %q", dns.LeaderURL, stack.node.PeerURL())
	}
}

func TestEmunetFollowerWatchDogRacePromotesOneAndReconnectsLosers(t *testing.T) {
	t.Setenv("RPC25519_SERVER_DATA_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	leader, dnsSrv, addr := startTestEmunetLeaderDNS(t, ctx)
	t.Setenv("RISCV_EMU_EMUNET_ADDR", addr)
	starts := installFakeEmunetLeaderHook(t, 10*time.Millisecond)

	stacks := make([]*emunetVirtioStack, 0, 3)
	for range 3 {
		stackIf, err := newEmunetVirtioStack(EmuConfig{})
		if err != nil {
			t.Fatal(err)
		}
		stack := stackIf.(*emunetVirtioStack)
		stacks = append(stacks, stack)
		defer stack.Close()
	}

	if err := leader.Close(); err != nil {
		t.Fatalf("close old leader node: %v", err)
	}
	if err := dnsSrv.Close(); err != nil {
		t.Fatalf("close old leader DNS: %v", err)
	}

	waitForTestCondition(t, 3*time.Second, func() bool {
		promoted := promotedTestStack(stacks)
		if promoted == nil || starts.Load() != 1 {
			return false
		}
		needed := make(map[string]struct{}, len(stacks)-1)
		for _, stack := range stacks {
			if stack != promoted {
				needed[stack.node.PeerURL()] = struct{}{}
			}
		}
		promoted.mu.Lock()
		defer promoted.mu.Unlock()
		for url := range needed {
			if _, ok := promoted.followerURLs[url]; !ok {
				return false
			}
		}
		return true
	})

	if got := promotedTestStackCount(stacks); got != 1 {
		t.Fatalf("promoted leader count = %d, want 1", got)
	}
	if got := starts.Load(); got != 1 {
		t.Fatalf("leader tsnet starts = %d, want 1", got)
	}
}

func TestEmunetFollowerCloseStopsWatchDogPromotion(t *testing.T) {
	t.Setenv("RPC25519_SERVER_DATA_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	leader, dnsSrv, addr := startTestEmunetLeaderDNS(t, ctx)
	defer leader.Close()
	t.Setenv("RISCV_EMU_EMUNET_ADDR", addr)
	starts := installFakeEmunetLeaderHook(t, 10*time.Millisecond)

	stackIf, err := newEmunetVirtioStack(EmuConfig{})
	if err != nil {
		t.Fatal(err)
	}
	stack := stackIf.(*emunetVirtioStack)
	if err := stack.Close(); err != nil {
		t.Fatalf("close follower stack: %v", err)
	}
	if err := dnsSrv.Close(); err != nil {
		t.Fatalf("close leader DNS: %v", err)
	}

	time.Sleep(5 * emunetWatchDogInterval)
	if got := starts.Load(); got != 0 {
		t.Fatalf("leader tsnet starts after closed follower = %d, want 0", got)
	}
}

func TestEmunetLeaderForgetsFollowerCircuitAndPort(t *testing.T) {
	peerURL := "tcp://127.0.0.1:30002/emunet/follower-a"
	var ckt emunetpkg.Circuit
	core := &tsnetVirtioStack{hostMAC: emunetRouterMAC}
	core.learnPortMAC(peerURL, [6]byte{0x02, 0, 0, 0, 1, 0x14}, func([]byte) {})
	core.setTailIPv4(netip.MustParseAddr("100.64.12.34"))
	if out := core.translateOutboundIPv4(peerURL, udpIPv4Packet([4]byte{10, 77, 0, 2}, [4]byte{8, 8, 8, 8}, 1234, 53, []byte("hello")), netip.MustParseAddr("100.64.12.34"), func([]byte) {}); len(out) == 0 {
		t.Fatal("NAT outbound dropped UDP packet")
	}
	stack := &emunetVirtioStack{
		role:              "leader",
		leaderCore:        core,
		followerURLs:      map[string]struct{}{peerURL: {}},
		followerByCircuit: map[*emunetpkg.Circuit]string{&ckt: peerURL},
	}

	stack.forgetFollowerCircuit(&ckt, errors.New("closed"))

	stack.mu.Lock()
	_, stillFollower := stack.followerURLs[peerURL]
	_, stillCircuit := stack.followerByCircuit[&ckt]
	stack.mu.Unlock()
	if stillFollower {
		t.Fatalf("follower URL %q still published after circuit removal", peerURL)
	}
	if stillCircuit {
		t.Fatalf("follower circuit still tracked after removal")
	}
	core.mu.Lock()
	defer core.mu.Unlock()
	if _, ok := core.ports[peerURL]; ok {
		t.Fatalf("core port %q still exists after follower removal", peerURL)
	}
	if len(core.natByOut) != 0 || len(core.natByIn) != 0 {
		t.Fatalf("NAT mappings remain after follower removal: out=%d in=%d", len(core.natByOut), len(core.natByIn))
	}
}

func TestEmunetFollowerDHCPOverCircuit(t *testing.T) {
	t.Setenv("RPC25519_SERVER_DATA_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("RISCV_EMU_EMUNET_ADDR", reserveTestEmunetAddr(t))
	starts, _ := installFakeEmunetLeaderCoreHook(t, 20*time.Millisecond, nil)

	leaderIf, err := newEmunetVirtioStack(EmuConfig{})
	if err != nil {
		t.Fatal(err)
	}
	leader := leaderIf.(*emunetVirtioStack)
	defer leader.Close()
	followerIf, err := newEmunetVirtioStack(EmuConfig{})
	if err != nil {
		t.Fatal(err)
	}
	follower := followerIf.(*emunetVirtioStack)
	defer follower.Close()

	if got := starts.Load(); got != 1 {
		t.Fatalf("leader starts = %d, want 1", got)
	}
	guestMAC := [6]byte{0x02, 0, 0, 0, 2, 0x01}
	follower.InjectInboundPacket(dhcpTestFrame(t, dhcpDiscover, 0x44444444, guestMAC))
	reply := waitPendingEmunetFrame(t, follower, time.Second)
	assertDHCPReply(t, reply, dhcpOffer, netip.MustParseAddr("10.77.0.2"))
	assertDHCPOptionIP(t, reply, dhcpOptionRouter, netip.MustParseAddr("10.77.0.1"))

	leader.mu.Lock()
	_, known := leader.followerURLs[follower.node.PeerURL()]
	leader.mu.Unlock()
	if !known {
		t.Fatalf("leader did not publish follower URL %q", follower.node.PeerURL())
	}
}

func TestEmunetLeaderRoutesNATReplyOnlyToOwningFollower(t *testing.T) {
	t.Setenv("RPC25519_SERVER_DATA_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("RISCV_EMU_EMUNET_ADDR", reserveTestEmunetAddr(t))
	tailIP := netip.MustParseAddr("100.64.12.34")
	_, cores := installFakeEmunetLeaderCoreHook(t, 20*time.Millisecond, func(core *tsnetVirtioStack) {
		core.setTailIPv4(tailIP)
	})

	leaderIf, err := newEmunetVirtioStack(EmuConfig{})
	if err != nil {
		t.Fatal(err)
	}
	leader := leaderIf.(*emunetVirtioStack)
	defer leader.Close()
	core := recvLeaderCore(t, cores)

	followerAIf, err := newEmunetVirtioStack(EmuConfig{})
	if err != nil {
		t.Fatal(err)
	}
	followerA := followerAIf.(*emunetVirtioStack)
	defer followerA.Close()
	followerBIf, err := newEmunetVirtioStack(EmuConfig{})
	if err != nil {
		t.Fatal(err)
	}
	followerB := followerBIf.(*emunetVirtioStack)
	defer followerB.Close()

	macA := [6]byte{0x02, 0, 0, 0, 2, 0x02}
	macB := [6]byte{0x02, 0, 0, 0, 2, 0x03}
	followerA.InjectInboundPacket(dhcpTestFrame(t, dhcpDiscover, 0x55555555, macA))
	assertDHCPReply(t, waitPendingEmunetFrame(t, followerA, time.Second), dhcpOffer, netip.MustParseAddr("10.77.0.2"))
	clearEmunetPending(followerA)
	followerB.InjectInboundPacket(dhcpTestFrame(t, dhcpDiscover, 0x66666666, macB))
	assertDHCPReply(t, waitPendingEmunetFrame(t, followerB, time.Second), dhcpOffer, netip.MustParseAddr("10.77.0.3"))
	clearEmunetPending(followerB)

	followerA.InjectInboundPacket(ethernetIPv4Frame(emunetRouterMAC, macA, udpIPv4Packet([4]byte{10, 77, 0, 2}, [4]byte{8, 8, 8, 8}, 1234, 53, []byte("hello"))))
	ext := waitForNATExternal(t, core, followerA.node.PeerURL(), ipProtoUDP, 1234, [4]byte{8, 8, 8, 8}, 53)
	core.handleTsnetPacket(udpIPv4Packet([4]byte{8, 8, 8, 8}, tailIP.As4(), 53, ext, []byte("world")))

	reply := waitPendingEmunetFrame(t, followerA, time.Second)
	if !bytes.Equal(reply[0:6], macA[:]) {
		t.Fatalf("NAT reply dst MAC = %x, want follower A %x", reply[0:6], macA)
	}
	ip := reply[14:]
	if got := binary.BigEndian.Uint16(ip[22:24]); got != 1234 {
		t.Fatalf("NAT reply dst port = %d, want 1234", got)
	}
	followerB.mu.Lock()
	pendingB := len(followerB.pending)
	followerB.mu.Unlock()
	if pendingB != 0 {
		t.Fatalf("follower B received %d frames for follower A NAT reply", pendingB)
	}
}

func TestEmunetLeaderDropsUnmatchedTUNPacket(t *testing.T) {
	t.Setenv("RPC25519_SERVER_DATA_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("RISCV_EMU_EMUNET_ADDR", reserveTestEmunetAddr(t))
	tailIP := netip.MustParseAddr("100.64.12.34")
	_, cores := installFakeEmunetLeaderCoreHook(t, 20*time.Millisecond, func(core *tsnetVirtioStack) {
		core.setTailIPv4(tailIP)
	})

	leaderIf, err := newEmunetVirtioStack(EmuConfig{})
	if err != nil {
		t.Fatal(err)
	}
	leader := leaderIf.(*emunetVirtioStack)
	defer leader.Close()
	core := recvLeaderCore(t, cores)
	followerIf, err := newEmunetVirtioStack(EmuConfig{})
	if err != nil {
		t.Fatal(err)
	}
	follower := followerIf.(*emunetVirtioStack)
	defer follower.Close()

	core.handleTsnetPacket(udpIPv4Packet([4]byte{8, 8, 8, 8}, tailIP.As4(), 53, 40000, []byte("world")))
	time.Sleep(3 * emunetWatchDogInterval)
	follower.mu.Lock()
	pending := len(follower.pending)
	follower.mu.Unlock()
	if pending != 0 {
		t.Fatalf("unmatched TUN packet delivered %d follower frames, want 0", pending)
	}
}

func installFakeEmunetLeaderHook(t *testing.T, interval time.Duration) *atomic.Int32 {
	t.Helper()
	starts, _ := installFakeEmunetLeaderCoreHook(t, interval, nil)
	return starts
}

func installFakeEmunetLeaderCoreHook(t *testing.T, interval time.Duration, configure func(*tsnetVirtioStack)) (*atomic.Int32, <-chan *tsnetVirtioStack) {
	t.Helper()
	starts := new(atomic.Int32)
	cores := make(chan *tsnetVirtioStack, 8)
	oldHook := newEmunetLeaderTsnetVirtioStackHook
	oldInterval := emunetWatchDogInterval
	newEmunetLeaderTsnetVirtioStackHook = func(EmuConfig) (*tsnetVirtioStack, error) {
		starts.Add(1)
		core := &tsnetVirtioStack{hostMAC: emunetRouterMAC}
		core.tun = newVirtioNetMemoryTUN(core.handleTsnetPacket)
		if configure != nil {
			configure(core)
		}
		cores <- core
		return core, nil
	}
	emunetWatchDogInterval = interval
	t.Cleanup(func() {
		newEmunetLeaderTsnetVirtioStackHook = oldHook
		emunetWatchDogInterval = oldInterval
	})
	return starts, cores
}

func promotedTestStack(stacks []*emunetVirtioStack) *emunetVirtioStack {
	var promoted *emunetVirtioStack
	for _, stack := range stacks {
		stack.mu.Lock()
		isLeader := stack.role == "leader" && stack.leaderCore != nil && stack.dns != nil
		stack.mu.Unlock()
		if !isLeader {
			continue
		}
		if promoted != nil {
			return nil
		}
		promoted = stack
	}
	return promoted
}

func promotedTestStackCount(stacks []*emunetVirtioStack) int {
	count := 0
	for _, stack := range stacks {
		stack.mu.Lock()
		if stack.role == "leader" && stack.leaderCore != nil && stack.dns != nil {
			count++
		}
		stack.mu.Unlock()
	}
	return count
}

func reserveTestEmunetAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

func recvLeaderCore(t *testing.T, cores <-chan *tsnetVirtioStack) *tsnetVirtioStack {
	t.Helper()
	select {
	case core := <-cores:
		return core
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for fake leader core")
		return nil
	}
}

func waitPendingEmunetFrame(t *testing.T, stack *emunetVirtioStack, timeout time.Duration) []byte {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		stack.mu.Lock()
		if len(stack.pending) != 0 {
			frame := append([]byte(nil), stack.pending[0]...)
			copy(stack.pending, stack.pending[1:])
			stack.pending = stack.pending[:len(stack.pending)-1]
			stack.mu.Unlock()
			return frame
		}
		stack.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	stack.mu.Lock()
	pending := len(stack.pending)
	stack.mu.Unlock()
	t.Fatalf("timed out waiting for pending emunet frame; pending=%d", pending)
	return nil
}

func clearEmunetPending(stack *emunetVirtioStack) {
	stack.mu.Lock()
	stack.pending = nil
	stack.mu.Unlock()
}

func waitForNATExternal(t *testing.T, core *tsnetVirtioStack, portID string, proto byte, guestPort uint16, remoteIP [4]byte, remotePort uint16) uint16 {
	t.Helper()
	wantRemote := netip.AddrFrom4(remoteIP)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		core.mu.Lock()
		for key, ent := range core.natByOut {
			if key.portID == portID && key.proto == proto && key.guestPort == guestPort && key.remoteIP == wantRemote && key.remotePort == remotePort {
				ext := ent.external
				core.mu.Unlock()
				return ext
			}
		}
		core.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for NAT mapping portID=%q proto=%d guestPort=%d remote=%s:%d", portID, proto, guestPort, wantRemote, remotePort)
	return 0
}

func startTestEmunetLeaderDNS(t *testing.T, ctx context.Context) (*emunetpkg.Node, *emunetpkg.DNSServer, string) {
	t.Helper()
	leader, err := emunetpkg.StartNode(ctx, emunetpkg.NodeOptions{PeerName: "emunet-test-leader"})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := emunetpkg.ListenRendezvous(ctx, "127.0.0.1:0")
	if err != nil {
		leader.Close()
		t.Fatal(err)
	}
	dnsSrv := emunetpkg.StartDNSServer(ctx, ln, func() emunetpkg.EmunetDNS {
		return emunetpkg.EmunetDNS{LeaderURL: leader.PeerURL()}
	})
	return leader, dnsSrv, ln.Addr().String()
}

func waitForTestCondition(t *testing.T, timeout time.Duration, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	if fn() {
		return
	}
	t.Fatalf("condition was not true within %s", timeout)
}

func dhcpTestFrame(t *testing.T, msgType byte, xid uint32, mac [6]byte) []byte {
	t.Helper()
	const dhcpLen = 300
	dhcp := make([]byte, dhcpLen)
	dhcp[0] = 1 // BOOTREQUEST
	dhcp[1] = 1 // Ethernet
	dhcp[2] = 6
	binary.BigEndian.PutUint32(dhcp[4:8], xid)
	binary.BigEndian.PutUint16(dhcp[10:12], 0x8000)
	copy(dhcp[28:34], mac[:])
	copy(dhcp[236:240], []byte{99, 130, 83, 99})
	copy(dhcp[240:], []byte{dhcpOptionMessage, 1, msgType, dhcpOptionEnd})

	udpLen := 8 + dhcpLen
	ipLen := 20 + udpLen
	frame := make([]byte, 14+ipLen)
	for i := 0; i < 6; i++ {
		frame[i] = 0xff
	}
	copy(frame[6:12], mac[:])
	binary.BigEndian.PutUint16(frame[12:14], etherTypeIPv4)

	ip := frame[14:]
	ip[0] = 0x45
	ip[8] = 64
	ip[9] = ipProtoUDP
	binary.BigEndian.PutUint16(ip[2:4], uint16(ipLen))
	copy(ip[16:20], []byte{255, 255, 255, 255})
	binary.BigEndian.PutUint16(ip[10:12], ipv4HeaderChecksum(ip[:20]))

	udp := ip[20:]
	binary.BigEndian.PutUint16(udp[0:2], bootpClientPort)
	binary.BigEndian.PutUint16(udp[2:4], bootpServerPort)
	binary.BigEndian.PutUint16(udp[4:6], uint16(udpLen))
	copy(udp[8:], dhcp)
	return frame
}

func onlyPendingEthernetFrame(t *testing.T, stack *tsnetVirtioStack) []byte {
	t.Helper()
	stack.mu.Lock()
	defer stack.mu.Unlock()
	if len(stack.pending) != 1 {
		t.Fatalf("pending frames = %d, want 1", len(stack.pending))
	}
	return append([]byte(nil), stack.pending[0]...)
}

func assertDHCPReply(t *testing.T, frame []byte, wantType byte, wantIP netip.Addr) {
	t.Helper()
	if len(frame) < 14+20+8+240 {
		t.Fatalf("DHCP reply length = %d, too short", len(frame))
	}
	if got := binary.BigEndian.Uint16(frame[12:14]); got != etherTypeIPv4 {
		t.Fatalf("ether type = %#x, want IPv4", got)
	}
	ip := frame[14:]
	ihl := int(ip[0]&0x0f) * 4
	udp := ip[ihl:]
	if got := binary.BigEndian.Uint16(udp[0:2]); got != bootpServerPort {
		t.Fatalf("UDP source port = %d, want DHCP server", got)
	}
	if got := binary.BigEndian.Uint16(udp[2:4]); got != bootpClientPort {
		t.Fatalf("UDP dest port = %d, want DHCP client", got)
	}
	dhcp := udp[8:]
	if dhcp[0] != 2 {
		t.Fatalf("BOOTP op = %d, want BOOTREPLY", dhcp[0])
	}
	gotIP := netip.AddrFrom4([4]byte{dhcp[16], dhcp[17], dhcp[18], dhcp[19]})
	if gotIP != wantIP {
		t.Fatalf("yiaddr = %s, want %s", gotIP, wantIP)
	}
	gotType, ok := dhcpMessageType(dhcp)
	if !ok {
		t.Fatal("DHCP message type option missing")
	}
	if gotType != wantType {
		t.Fatalf("DHCP message type = %d, want %d", gotType, wantType)
	}
}

func assertDHCPOptionIP(t *testing.T, frame []byte, code byte, want netip.Addr) {
	t.Helper()
	got := dhcpOptionValue(t, frame, code)
	if len(got) != 4 {
		t.Fatalf("DHCP option %d length = %d, want 4", code, len(got))
	}
	if gotIP := netip.AddrFrom4([4]byte{got[0], got[1], got[2], got[3]}); gotIP != want {
		t.Fatalf("DHCP option %d IP = %s, want %s", code, gotIP, want)
	}
}

func assertDHCPOptionBytes(t *testing.T, frame []byte, code byte, want []byte) {
	t.Helper()
	got := dhcpOptionValue(t, frame, code)
	if !bytes.Equal(got, want) {
		t.Fatalf("DHCP option %d = %v, want %v", code, got, want)
	}
}

func assertDHCPOptionUint16(t *testing.T, frame []byte, code byte, want uint16) {
	t.Helper()
	got := dhcpOptionValue(t, frame, code)
	if len(got) != 2 {
		t.Fatalf("DHCP option %d length = %d, want 2", code, len(got))
	}
	if gotU16 := binary.BigEndian.Uint16(got); gotU16 != want {
		t.Fatalf("DHCP option %d uint16 = %d, want %d", code, gotU16, want)
	}
}

func dhcpOptionValue(t *testing.T, frame []byte, wantCode byte) []byte {
	t.Helper()
	dhcp := dhcpPayload(t, frame)
	for i := 240; i < len(dhcp); {
		code := dhcp[i]
		i++
		switch code {
		case 0:
			continue
		case dhcpOptionEnd:
			t.Fatalf("DHCP option %d not found", wantCode)
		}
		if i >= len(dhcp) {
			t.Fatalf("DHCP option %d has no length byte", code)
		}
		n := int(dhcp[i])
		i++
		if i+n > len(dhcp) {
			t.Fatalf("DHCP option %d length %d overruns payload", code, n)
		}
		if code == wantCode {
			return append([]byte(nil), dhcp[i:i+n]...)
		}
		i += n
	}
	t.Fatalf("DHCP option %d not found before payload end", wantCode)
	return nil
}

func dhcpPayload(t *testing.T, frame []byte) []byte {
	t.Helper()
	if len(frame) < 14+20+8+240 {
		t.Fatalf("DHCP frame length = %d, too short", len(frame))
	}
	ip := frame[14:]
	ihl := int(ip[0]&0x0f) * 4
	if ihl < 20 || len(ip) < ihl+8+240 {
		t.Fatalf("bad DHCP IPv4 header length %d for IP len %d", ihl, len(ip))
	}
	return ip[ihl+8:]
}

func arpRequestFrame(mac [6]byte, senderIP, targetIP [4]byte) []byte {
	frame := make([]byte, 42)
	for i := 0; i < 6; i++ {
		frame[i] = 0xff
	}
	copy(frame[6:12], mac[:])
	binary.BigEndian.PutUint16(frame[12:14], etherTypeARP)
	binary.BigEndian.PutUint16(frame[14:16], 1)
	binary.BigEndian.PutUint16(frame[16:18], etherTypeIPv4)
	frame[18] = 6
	frame[19] = 4
	binary.BigEndian.PutUint16(frame[20:22], 1)
	copy(frame[22:28], mac[:])
	copy(frame[28:32], senderIP[:])
	copy(frame[38:42], targetIP[:])
	return frame
}

func icmpEchoFrame(mac [6]byte, src, dst [4]byte, id, seq uint16) []byte {
	icmp := make([]byte, 8+4)
	icmp[0] = icmpEchoRequest
	binary.BigEndian.PutUint16(icmp[4:6], id)
	binary.BigEndian.PutUint16(icmp[6:8], seq)
	copy(icmp[8:], []byte("ping"))
	binary.BigEndian.PutUint16(icmp[2:4], internetChecksum(icmp))
	ip := ipv4Packet(src, dst, ipProtoICMP, icmp)
	frame := make([]byte, 14+len(ip))
	copy(frame[0:6], emunetRouterMAC[:])
	copy(frame[6:12], mac[:])
	binary.BigEndian.PutUint16(frame[12:14], etherTypeIPv4)
	copy(frame[14:], ip)
	return frame
}

func udpIPv4Packet(src, dst [4]byte, srcPort, dstPort uint16, payload []byte) []byte {
	udp := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint16(udp[0:2], srcPort)
	binary.BigEndian.PutUint16(udp[2:4], dstPort)
	binary.BigEndian.PutUint16(udp[4:6], uint16(len(udp)))
	copy(udp[8:], payload)
	ip := ipv4Packet(src, dst, ipProtoUDP, udp)
	binary.BigEndian.PutUint16(ip[20+6:20+8], transportChecksum(ip[:20], ip[20:], ipProtoUDP))
	return ip
}

func tcpIPv4Packet(src, dst [4]byte, srcPort, dstPort uint16, flags byte, payload []byte) []byte {
	tcp := make([]byte, 20+len(payload))
	binary.BigEndian.PutUint16(tcp[0:2], srcPort)
	binary.BigEndian.PutUint16(tcp[2:4], dstPort)
	tcp[12] = 5 << 4
	tcp[13] = flags
	binary.BigEndian.PutUint16(tcp[14:16], 65535)
	copy(tcp[20:], payload)
	ip := ipv4Packet(src, dst, ipProtoTCP, tcp)
	binary.BigEndian.PutUint16(ip[20+16:20+18], transportChecksum(ip[:20], ip[20:], ipProtoTCP))
	return ip
}

func icmpIPv4Packet(src, dst [4]byte, typ byte, id, seq uint16) []byte {
	icmp := make([]byte, 8+4)
	icmp[0] = typ
	binary.BigEndian.PutUint16(icmp[4:6], id)
	binary.BigEndian.PutUint16(icmp[6:8], seq)
	copy(icmp[8:], []byte("ping"))
	binary.BigEndian.PutUint16(icmp[2:4], internetChecksum(icmp))
	return ipv4Packet(src, dst, ipProtoICMP, icmp)
}

func ipv4Packet(src, dst [4]byte, proto byte, payload []byte) []byte {
	ip := make([]byte, 20+len(payload))
	ip[0] = 0x45
	ip[8] = 64
	ip[9] = proto
	binary.BigEndian.PutUint16(ip[2:4], uint16(len(ip)))
	copy(ip[12:16], src[:])
	copy(ip[16:20], dst[:])
	copy(ip[20:], payload)
	binary.BigEndian.PutUint16(ip[10:12], ipv4HeaderChecksum(ip[:20]))
	return ip
}

func assertIPv4ChecksumValid(t *testing.T, packet []byte) {
	t.Helper()
	if len(packet) < 20 {
		t.Fatalf("packet length = %d, too short for IPv4", len(packet))
	}
	ihl := int(packet[0]&0x0f) * 4
	if ihl < 20 || len(packet) < ihl {
		t.Fatalf("bad IPv4 header length %d for packet length %d", ihl, len(packet))
	}
	if got := internetChecksum(packet[:ihl]); got != 0 {
		t.Fatalf("IPv4 checksum validation = %#x, want 0", got)
	}
}

func assertTransportChecksumValid(t *testing.T, packet []byte, proto byte) {
	t.Helper()
	ihl := int(packet[0]&0x0f) * 4
	totalLen := int(binary.BigEndian.Uint16(packet[2:4]))
	if totalLen < ihl || totalLen > len(packet) {
		t.Fatalf("bad IPv4 total length %d for packet length %d", totalLen, len(packet))
	}
	if got := transportChecksum(packet[:ihl], packet[ihl:totalLen], proto); got != 0 {
		t.Fatalf("transport checksum validation = %#x, want 0", got)
	}
}

func assertICMPChecksumValid(t *testing.T, packet []byte) {
	t.Helper()
	ihl := int(packet[0]&0x0f) * 4
	totalLen := int(binary.BigEndian.Uint16(packet[2:4]))
	if totalLen < ihl || totalLen > len(packet) {
		t.Fatalf("bad IPv4 total length %d for packet length %d", totalLen, len(packet))
	}
	if got := internetChecksum(packet[ihl:totalLen]); got != 0 {
		t.Fatalf("ICMP checksum validation = %#x, want 0", got)
	}
}
