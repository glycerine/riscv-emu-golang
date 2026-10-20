package riscv

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/tailscale/wireguard-go/tun"
)

func TestTsnetMemoryTUNReadPacketSpacing(t *testing.T) {
	device := newVirtioNetMemoryTUN(nil)
	defer device.Close()
	pkt := bytes.Repeat([]byte{0x45}, int(virtioNetMTU))
	device.InjectIPPacket(pkt)

	slab := bytes.Repeat([]byte{0xaa}, len(pkt)+2*tun.ReadPacketSpacing)
	packets := make([]tun.ReadPacket, device.BatchSize())
	n, err := device.Read(slab, packets)
	if err != nil || n != 1 {
		t.Fatalf("Read = (%d, %v), want (1, nil)", n, err)
	}
	p := packets[0]
	if p.Offset != tun.ReadPacketSpacing || p.Size != len(pkt) {
		t.Fatalf("packet = %+v, want offset %d, size %d", p, tun.ReadPacketSpacing, len(pkt))
	}
	if !bytes.Equal(slab[p.Offset:p.Offset+p.Size], pkt) {
		t.Fatal("Read changed the packet contents")
	}
	padding := bytes.Repeat([]byte{0xaa}, tun.ReadPacketSpacing)
	if !bytes.Equal(slab[:p.Offset], padding) || !bytes.Equal(slab[p.Offset+p.Size:], padding) {
		t.Fatal("Read overwrote the reserved packet spacing")
	}
}

func TestTsnetMemoryTUNReadShortBuffers(t *testing.T) {
	pkt := []byte{0x45, 0, 0, 4}
	for _, tc := range []struct {
		name    string
		slabLen int
		entries int
	}{
		{"no_descriptors", len(pkt) + 2*tun.ReadPacketSpacing, 0},
		{"no_slab", 0, 1},
		{"short_packet_space", len(pkt) + 2*tun.ReadPacketSpacing - 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			device := newVirtioNetMemoryTUN(nil)
			defer device.Close()
			device.InjectIPPacket(pkt)
			slab := bytes.Repeat([]byte{0xaa}, tc.slabLen)
			before := bytes.Clone(slab)
			n, err := device.Read(slab, make([]tun.ReadPacket, tc.entries))
			if n != 0 || !errors.Is(err, tun.ErrTooManySegments) {
				t.Fatalf("Read = (%d, %v), want (0, ErrTooManySegments)", n, err)
			}
			if !bytes.Equal(slab, before) {
				t.Fatal("Read copied a partial packet into a short buffer")
			}
		})
	}
}

func TestTsnetMemoryTUNReadClosed(t *testing.T) {
	device := newVirtioNetMemoryTUN(nil)
	device.Close()
	n, err := device.Read(make([]byte, 2*tun.ReadPacketSpacing), make([]tun.ReadPacket, 1))
	if n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("Read = (%d, %v), want (0, EOF)", n, err)
	}
}
