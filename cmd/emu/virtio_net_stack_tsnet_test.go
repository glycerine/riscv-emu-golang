//go:build tsnet

package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strings"
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

	stack.mu.Lock()
	stack.pending = nil
	stack.mu.Unlock()

	stack.InjectInboundPacket(dhcpTestFrame(t, dhcpRequest, 0x12345679, guestMAC))
	ack := onlyPendingEthernetFrame(t, stack)
	assertDHCPReply(t, ack, dhcpAck, guestIP)
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
	if got := binary.BigEndian.Uint16(guestPkt[22:24]); got != 1234 {
		t.Fatalf("NAT dst port = %d, want guest port 1234", got)
	}
}

func TestTsnetDirDefaultsToHostPersistentStateDir(t *testing.T) {
	t.Setenv("RISCV_EMU_TSNET_DIR", "")
	t.Setenv("HOME", "/tmp/riscv-emu-home")
	want := "/tmp/riscv-emu-home/.tailemu/riscv-emu"
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

func TestWriteEmunetLeaderPIDFileReplacesStaleLeaderFiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := tailemuDir()
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

	var starts atomic.Int32
	oldHook := newEmunetLeaderTsnetVirtioStackHook
	oldInterval := emunetWatchDogInterval
	newEmunetLeaderTsnetVirtioStackHook = func(EmuConfig) (*tsnetVirtioStack, error) {
		starts.Add(1)
		return &tsnetVirtioStack{hostMAC: emunetRouterMAC}, nil
	}
	emunetWatchDogInterval = 20 * time.Millisecond
	t.Cleanup(func() {
		newEmunetLeaderTsnetVirtioStackHook = oldHook
		emunetWatchDogInterval = oldInterval
	})

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

	var starts atomic.Int32
	oldHook := newEmunetLeaderTsnetVirtioStackHook
	oldInterval := emunetWatchDogInterval
	newEmunetLeaderTsnetVirtioStackHook = func(EmuConfig) (*tsnetVirtioStack, error) {
		starts.Add(1)
		return &tsnetVirtioStack{hostMAC: emunetRouterMAC}, nil
	}
	emunetWatchDogInterval = 10 * time.Millisecond
	t.Cleanup(func() {
		newEmunetLeaderTsnetVirtioStackHook = oldHook
		emunetWatchDogInterval = oldInterval
	})

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
