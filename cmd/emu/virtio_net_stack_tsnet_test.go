//go:build tsnet

package main

import (
	"encoding/binary"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestTsnetVirtioStackDHCPDiscoverAndRequest(t *testing.T) {
	guestIP := netip.MustParseAddr("100.64.12.34")
	guestMAC := [6]byte{0x02, 0x72, 0x69, 0x73, 0x00, 0x01}
	stack := &tsnetVirtioStack{
		hostMAC: [6]byte{0x02, 0x72, 0x69, 0x73, 0xff, 0x01},
	}
	stack.setTailIPv4(guestIP)

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

func TestTsnetOpLogDefaultsToHostTailemuDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if got, want := tsnetOpLogPath(), filepath.Join(home, ".tailemu", "oplog.txt"); got != want {
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
