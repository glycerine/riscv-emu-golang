//go:build tsnet

package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/tailscale/wireguard-go/tun"
	"tailscale.com/tsnet"
)

const (
	etherTypeIPv4 = uint16(0x0800)
	etherTypeARP  = uint16(0x0806)
	etherTypeIPv6 = uint16(0x86dd)
)

type tsnetVirtioStack struct {
	srv *tsnet.Server
	tun *virtioNetMemoryTUN

	mu       sync.Mutex
	dev      *virtioNetDevice
	pending  [][]byte
	hostMAC  [6]byte
	guestMAC [6]byte
}

func newVirtioNetPacketStack(cfg EmuConfig) (virtioNetPacketStack, error) {
	stack := &tsnetVirtioStack{
		hostMAC: [6]byte{0x02, 0x72, 0x69, 0x73, 0xff, 0x01},
	}
	stack.tun = newVirtioNetMemoryTUN(stack.handleTsnetPacket)
	stack.srv = &tsnet.Server{
		Dir:       tsnetDir(),
		Hostname:  tsnetHostname(),
		AuthKey:   os.Getenv("TS_AUTHKEY"),
		Ephemeral: tsnetEnvBool("RISCV_EMU_TSNET_EPHEMERAL"),
		Tun:       stack.tun,
		UserLogf: func(format string, args ...any) {
			fmt.Fprintf(os.Stderr, "tsnet: "+format+"\n", args...)
		},
	}
	if err := stack.srv.Start(); err != nil {
		stack.tun.Close()
		return nil, err
	}
	return stack, nil
}

func (s *tsnetVirtioStack) attachVirtioNet(dev *virtioNetDevice) {
	s.mu.Lock()
	s.dev = dev
	s.guestMAC = dev.mac
	pending := s.pending
	s.pending = nil
	s.mu.Unlock()

	for _, frame := range pending {
		dev.InjectGuestFrame(frame)
	}
}

func (s *tsnetVirtioStack) InjectInboundPacket(frame []byte) {
	if len(frame) < 14 {
		return
	}
	etherType := binary.BigEndian.Uint16(frame[12:14])
	switch etherType {
	case etherTypeIPv4, etherTypeIPv6:
		s.tun.InjectIPPacket(frame[14:])
	case etherTypeARP:
		if reply := s.arpReply(frame); len(reply) != 0 {
			s.injectGuestEthernet(reply)
		}
	}
}

func (s *tsnetVirtioStack) Close() error {
	var err error
	if s.srv != nil {
		err = errors.Join(err, s.srv.Close())
	}
	if s.tun != nil {
		err = errors.Join(err, s.tun.Close())
	}
	return err
}

func (s *tsnetVirtioStack) handleTsnetPacket(pkt []byte) {
	if len(pkt) == 0 {
		return
	}
	etherType := uint16(0)
	switch pkt[0] >> 4 {
	case 4:
		etherType = etherTypeIPv4
	case 6:
		etherType = etherTypeIPv6
	default:
		return
	}
	frame := make([]byte, 14+len(pkt))
	s.mu.Lock()
	copy(frame[0:6], s.guestMAC[:])
	copy(frame[6:12], s.hostMAC[:])
	s.mu.Unlock()
	binary.BigEndian.PutUint16(frame[12:14], etherType)
	copy(frame[14:], pkt)
	s.injectGuestEthernet(frame)
}

func (s *tsnetVirtioStack) injectGuestEthernet(frame []byte) {
	s.mu.Lock()
	dev := s.dev
	if dev == nil {
		s.pending = append(s.pending, append([]byte(nil), frame...))
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	dev.InjectGuestFrame(frame)
}

func (s *tsnetVirtioStack) arpReply(req []byte) []byte {
	if len(req) < 42 || binary.BigEndian.Uint16(req[14:16]) != 1 ||
		binary.BigEndian.Uint16(req[16:18]) != etherTypeIPv4 ||
		req[18] != 6 || req[19] != 4 ||
		binary.BigEndian.Uint16(req[20:22]) != 1 {
		return nil
	}
	var guestMAC [6]byte
	copy(guestMAC[:], req[22:28])
	s.mu.Lock()
	s.guestMAC = guestMAC
	hostMAC := s.hostMAC
	s.mu.Unlock()

	reply := make([]byte, 42)
	copy(reply[0:6], req[6:12])
	copy(reply[6:12], hostMAC[:])
	binary.BigEndian.PutUint16(reply[12:14], etherTypeARP)
	copy(reply[14:20], req[14:20])
	binary.BigEndian.PutUint16(reply[20:22], 2)
	copy(reply[22:28], hostMAC[:])
	copy(reply[28:32], req[38:42])
	copy(reply[32:38], req[22:28])
	copy(reply[38:42], req[28:32])
	return reply
}

type virtioNetMemoryTUN struct {
	outbound chan []byte
	events   chan tun.Event
	closed   chan struct{}
	close    sync.Once
	onWrite  func([]byte)
}

func newVirtioNetMemoryTUN(onWrite func([]byte)) *virtioNetMemoryTUN {
	t := &virtioNetMemoryTUN{
		outbound: make(chan []byte, 1024),
		events:   make(chan tun.Event, 1),
		closed:   make(chan struct{}),
		onWrite:  onWrite,
	}
	t.events <- tun.EventUp
	return t
}

func (t *virtioNetMemoryTUN) InjectIPPacket(pkt []byte) {
	if len(pkt) == 0 {
		return
	}
	cloned := append([]byte(nil), pkt...)
	select {
	case <-t.closed:
	case t.outbound <- cloned:
	default:
	}
}

func (t *virtioNetMemoryTUN) File() *os.File { return nil }

func (t *virtioNetMemoryTUN) Read(bufs [][]byte, sizes []int, offset int) (int, error) {
	select {
	case <-t.closed:
		return 0, io.EOF
	case pkt, ok := <-t.outbound:
		if !ok {
			return 0, io.EOF
		}
		if len(bufs) == 0 || len(sizes) == 0 {
			return 0, nil
		}
		sizes[0] = copy(bufs[0][offset:], pkt)
		return 1, nil
	}
}

func (t *virtioNetMemoryTUN) Write(bufs [][]byte, offset int) (int, error) {
	for _, buf := range bufs {
		if len(buf) <= offset {
			continue
		}
		pkt := append([]byte(nil), buf[offset:]...)
		select {
		case <-t.closed:
			return 0, io.ErrClosedPipe
		default:
		}
		if t.onWrite != nil {
			t.onWrite(pkt)
		}
	}
	return len(bufs), nil
}

func (t *virtioNetMemoryTUN) Flush() error { return nil }

func (t *virtioNetMemoryTUN) MTU() (int, error) { return int(virtioNetMTU), nil }

func (t *virtioNetMemoryTUN) Name() (string, error) { return "riscv-emu-tsnet", nil }

func (t *virtioNetMemoryTUN) Events() <-chan tun.Event { return t.events }

func (t *virtioNetMemoryTUN) BatchSize() int { return 1 }

func (t *virtioNetMemoryTUN) Close() error {
	t.close.Do(func() {
		close(t.closed)
		close(t.outbound)
	})
	return nil
}

func tsnetDir() string {
	if v := os.Getenv("RISCV_EMU_TSNET_DIR"); v != "" {
		return v
	}
	if cfg, err := os.UserConfigDir(); err == nil {
		return filepath.Join(cfg, "riscv-emu-golang-tsnet")
	}
	return filepath.Join(os.TempDir(), "riscv-emu-golang-tsnet")
}

func tsnetHostname() string {
	if v := os.Getenv("RISCV_EMU_TSNET_HOSTNAME"); v != "" {
		return v
	}
	return "riscv-emu"
}

func tsnetEnvBool(name string) bool {
	v := strings.TrimSpace(strings.ToLower(os.Getenv(name)))
	if v == "" {
		return false
	}
	ok, err := strconv.ParseBool(v)
	return err == nil && ok
}
