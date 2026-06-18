//go:build tsnet

package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	emunetpkg "github.com/glycerine/riscv-emu-golang/emunet"
	"github.com/tailscale/wireguard-go/tun"
	"tailscale.com/tsnet"
)

const (
	defaultTailemuSubdir    = ".tailemu"
	defaultTsnetStateSubdir = "riscv-emu"
	tsnetOpLogName          = "oplog.txt"

	etherTypeIPv4 = uint16(0x0800)
	etherTypeARP  = uint16(0x0806)
	etherTypeIPv6 = uint16(0x86dd)

	ipProtoUDP        = byte(17)
	bootpServerPort   = uint16(67)
	bootpClientPort   = uint16(68)
	dhcpOptionMessage = byte(53)
	dhcpOptionSubnet  = byte(1)
	dhcpOptionRouter  = byte(3)
	dhcpOptionDNS     = byte(6)
	dhcpOptionMTU     = byte(26)
	dhcpOptionLease   = byte(51)
	dhcpOptionServer  = byte(54)
	dhcpOptionEnd     = byte(255)

	dhcpDiscover = byte(1)
	dhcpOffer    = byte(2)
	dhcpRequest  = byte(3)
	dhcpAck      = byte(5)
)

type emunetVirtioStack struct {
	mu sync.Mutex

	node   *emunetpkg.Node
	cancel context.CancelFunc
	role   string

	leaderCore *tsnetVirtioStack
	dns        *emunetpkg.DNSServer
	leaderCkt  *emunetpkg.Circuit

	dev     *virtioNetDevice
	pending [][]byte

	followerURLs map[string]struct{}
}

type tsnetVirtioStack struct {
	srv *tsnet.Server
	tun *virtioNetMemoryTUN

	mu       sync.Mutex
	cancel   context.CancelFunc
	stateDir string
	dev      *virtioNetDevice
	pending  [][]byte
	hostMAC  [6]byte
	guestMAC [6]byte
	tailIPv4 netip.Addr
}

func newVirtioNetPacketStack(cfg EmuConfig) (virtioNetPacketStack, error) {
	if tsnetEnvBool("RISCV_EMU_EMUNET_DISABLE") {
		return newDirectTsnetVirtioStack(cfg)
	}
	return newEmunetVirtioStack(cfg)
}

func newDirectTsnetVirtioStack(cfg EmuConfig) (*tsnetVirtioStack, error) {
	stack := &tsnetVirtioStack{
		hostMAC: [6]byte{0x02, 0x72, 0x69, 0x73, 0xff, 0x01},
	}
	stack.tun = newVirtioNetMemoryTUN(stack.handleTsnetPacket)
	stateDir := tsnetDir()
	hostname := tsnetHostname()
	ephemeral := tsnetEnvBool("RISCV_EMU_TSNET_EPHEMERAL")
	stack.stateDir = stateDir
	appendTsnetOpLog("start state_dir=%q state_file=%q hostname=%q ephemeral=%t authkey_set=%t",
		stateDir, filepath.Join(stateDir, "tailscaled.state"), hostname, ephemeral, os.Getenv("TS_AUTHKEY") != "")
	stack.srv = &tsnet.Server{
		Dir:       stateDir,
		Hostname:  hostname,
		AuthKey:   os.Getenv("TS_AUTHKEY"),
		Ephemeral: ephemeral,
		Tun:       stack.tun,
		UserLogf: func(format string, args ...any) {
			fmt.Fprintf(os.Stderr, "tsnet: "+format+"\n", args...)
		},
	}
	if err := stack.srv.Start(); err != nil {
		appendTsnetOpLog("start_error state_dir=%q error=%q", stateDir, err)
		stack.tun.Close()
		return nil, err
	}
	appendTsnetOpLog("started state_dir=%q", stateDir)
	ctx, cancel := context.WithCancel(context.Background())
	stack.cancel = cancel
	if ip := tsnetEnvAddr("RISCV_EMU_TSNET_GUEST_IPV4"); ip.Is4() {
		stack.setTailIPv4(ip)
		appendTsnetOpLog("guest_ipv4_override ip=%s", ip)
	}
	go stack.waitTsnetUp(ctx)
	return stack, nil
}

func newEmunetVirtioStack(cfg EmuConfig) (virtioNetPacketStack, error) {
	ctx, cancel := context.WithCancel(context.Background())
	node, err := emunetpkg.StartNode(ctx, emunetpkg.NodeOptions{})
	if err != nil {
		cancel()
		return nil, err
	}
	stack := &emunetVirtioStack{
		node:         node,
		cancel:       cancel,
		followerURLs: make(map[string]struct{}),
	}
	go stack.readEmunetEvents(ctx)

	ln, listenErr := emunetpkg.ListenRendezvous(ctx, emunetpkg.AddrFromEnv())
	if listenErr == nil {
		stack.role = "leader"
		appendTsnetOpLog("emunet_election role=leader addr=%q peer_url=%q", ln.Addr().String(), node.PeerURL())
		core, err := newDirectTsnetVirtioStack(cfg)
		if err != nil {
			_ = ln.Close()
			_ = stack.Close()
			return nil, err
		}
		stack.leaderCore = core
		stack.dns = emunetpkg.StartDNSServer(ctx, ln, stack.dnsSnapshot)
		appendTsnetOpLog("emunet_dns_start addr=%q leader_url=%q", ln.Addr().String(), node.PeerURL())
		return stack, nil
	}

	dns, lookupErr := emunetpkg.LookupDNS(ctx, emunetpkg.AddrFromEnv())
	if lookupErr != nil {
		_ = stack.Close()
		return nil, errors.Join(listenErr, lookupErr)
	}
	stack.role = "follower"
	appendTsnetOpLog("emunet_election role=follower addr=%q leader_url=%q follower_count=%d peer_url=%q",
		emunetpkg.AddrFromEnv(), dns.LeaderURL, len(dns.KnownFollowerURLs), node.PeerURL())
	hello := emunetpkg.NewMessage(emunetpkg.MessageKindHello)
	hello.PeerURL = node.PeerURL()
	ckt, err := node.Connect(ctx, dns.LeaderURL, &hello)
	if err != nil {
		_ = stack.Close()
		return nil, err
	}
	stack.leaderCkt = ckt
	appendTsnetOpLog("emunet_circuit_connected role=follower leader_url=%q", dns.LeaderURL)
	return stack, nil
}

func (s *emunetVirtioStack) dnsSnapshot() emunetpkg.EmunetDNS {
	s.mu.Lock()
	defer s.mu.Unlock()
	followers := make([]string, 0, len(s.followerURLs))
	for url := range s.followerURLs {
		followers = append(followers, url)
	}
	return emunetpkg.EmunetDNS{
		LeaderURL:         s.node.PeerURL(),
		KnownFollowerURLs: followers,
	}
}

func (s *emunetVirtioStack) attachVirtioNet(dev *virtioNetDevice) {
	s.mu.Lock()
	s.dev = dev
	pending := s.pending
	s.pending = nil
	core := s.leaderCore
	s.mu.Unlock()

	if core != nil {
		core.attachVirtioNet(dev)
		return
	}
	for _, frame := range pending {
		dev.InjectGuestFrame(frame)
	}
}

func (s *emunetVirtioStack) InjectInboundPacket(frame []byte) {
	s.mu.Lock()
	core := s.leaderCore
	ckt := s.leaderCkt
	node := s.node
	peerURL := ""
	var mac []byte
	if s.dev != nil {
		mac = append([]byte(nil), s.dev.mac[:]...)
	}
	if node != nil {
		peerURL = node.PeerURL()
	}
	s.mu.Unlock()

	if core != nil {
		core.InjectInboundPacket(frame)
		return
	}
	if node == nil || ckt == nil {
		return
	}
	msg := emunetpkg.NewMessage(emunetpkg.MessageKindEthernetFrame)
	msg.PeerURL = peerURL
	msg.MAC = mac
	_ = node.SendFrame(ckt, msg, frame)
}

func (s *emunetVirtioStack) Close() error {
	var err error
	if s.cancel != nil {
		s.cancel()
	}
	if s.leaderCkt != nil {
		s.leaderCkt.Close(nil)
	}
	if s.dns != nil {
		err = errors.Join(err, s.dns.Close())
	}
	if s.leaderCore != nil {
		err = errors.Join(err, s.leaderCore.Close())
	}
	if s.node != nil {
		err = errors.Join(err, s.node.Close())
	}
	return err
}

func (s *emunetVirtioStack) readEmunetEvents(ctx context.Context) {
	for {
		select {
		case ev := <-s.node.Events():
			if ev.Err != nil {
				appendTsnetOpLog("emunet_circuit_error error=%q", ev.Err)
				continue
			}
			s.handleEmunetMessage(ev)
		case <-ctx.Done():
			return
		}
	}
}

func (s *emunetVirtioStack) handleEmunetMessage(ev emunetpkg.Event) {
	switch ev.Message.Kind {
	case emunetpkg.MessageKindHello:
		if ev.Message.PeerURL != "" {
			s.mu.Lock()
			s.followerURLs[ev.Message.PeerURL] = struct{}{}
			s.mu.Unlock()
			appendTsnetOpLog("emunet_client_connected peer_url=%q", ev.Message.PeerURL)
		}
	case emunetpkg.MessageKindEthernetFrame:
		s.mu.Lock()
		core := s.leaderCore
		node := s.node
		s.mu.Unlock()
		if core != nil {
			core.handleGuestFrame(ev.Message.Frame, func(reply []byte) {
				msg := emunetpkg.NewMessage(emunetpkg.MessageKindEthernetFrame)
				msg.PeerURL = node.PeerURL()
				_ = node.SendFrame(ev.Circuit, msg, reply)
			})
			return
		}
		s.injectGuestEthernet(ev.Message.Frame)
	}
}

func (s *emunetVirtioStack) injectGuestEthernet(frame []byte) {
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
	s.handleGuestFrame(frame, s.injectGuestEthernet)
}

func (s *tsnetVirtioStack) handleGuestFrame(frame []byte, emit func([]byte)) {
	if len(frame) < 14 {
		return
	}
	etherType := binary.BigEndian.Uint16(frame[12:14])
	switch etherType {
	case etherTypeIPv4:
		if s.handleDHCP(frame, emit) {
			return
		}
		s.tun.InjectIPPacket(frame[14:])
	case etherTypeIPv6:
		s.tun.InjectIPPacket(frame[14:])
	case etherTypeARP:
		if reply := s.arpReply(frame); len(reply) != 0 {
			emit(reply)
		}
	}
}

func (s *tsnetVirtioStack) Close() error {
	var err error
	if s.cancel != nil {
		s.cancel()
	}
	if s.srv != nil {
		err = errors.Join(err, s.srv.Close())
	}
	if s.tun != nil {
		err = errors.Join(err, s.tun.Close())
	}
	return err
}

func (s *tsnetVirtioStack) waitTsnetUp(ctx context.Context) {
	if s.srv == nil {
		return
	}
	status, err := s.srv.Up(ctx)
	if err != nil {
		if ctx.Err() == nil {
			fmt.Fprintf(os.Stderr, "tsnet: up: %v\n", err)
			appendTsnetOpLog("up_error error=%q", err)
		}
		return
	}
	appendTsnetOpLog("authorized ips=%v state_dir=%q", status.TailscaleIPs, s.stateDir)
	for _, ip := range status.TailscaleIPs {
		if ip.Is4() {
			s.setTailIPv4(ip)
			appendTsnetOpLog("guest_ipv4_ready ip=%s", ip)
			return
		}
	}
	appendTsnetOpLog("authorized_no_ipv4 ips=%v", status.TailscaleIPs)
}

func (s *tsnetVirtioStack) setTailIPv4(ip netip.Addr) {
	if !ip.Is4() {
		return
	}
	s.mu.Lock()
	s.tailIPv4 = ip
	s.mu.Unlock()
}

func (s *tsnetVirtioStack) tailscaleIPv4() (netip.Addr, bool) {
	s.mu.Lock()
	ip := s.tailIPv4
	s.mu.Unlock()
	if ip.Is4() {
		return ip, true
	}
	if s.srv != nil {
		ip, _ = s.srv.TailscaleIPs()
		if ip.Is4() {
			s.setTailIPv4(ip)
			return ip, true
		}
	}
	return netip.Addr{}, false
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

func (s *tsnetVirtioStack) handleDHCP(frame []byte, emit func([]byte)) bool {
	if len(frame) < 14+20 {
		return false
	}
	ip := frame[14:]
	if ip[0]>>4 != 4 {
		return false
	}
	ihl := int(ip[0]&0x0f) * 4
	if ihl < 20 || len(ip) < ihl+8 || ip[9] != ipProtoUDP {
		return false
	}
	udp := ip[ihl:]
	srcPort := binary.BigEndian.Uint16(udp[0:2])
	dstPort := binary.BigEndian.Uint16(udp[2:4])
	if srcPort != bootpClientPort || dstPort != bootpServerPort {
		return false
	}
	if len(udp) < 8+240 {
		return true
	}
	dhcp := udp[8:]
	msgType, ok := dhcpMessageType(dhcp)
	if !ok {
		return true
	}

	var replyType byte
	switch msgType {
	case dhcpDiscover:
		replyType = dhcpOffer
	case dhcpRequest:
		replyType = dhcpAck
	default:
		return true
	}
	guestIP, ok := s.tailscaleIPv4()
	if !ok {
		return true
	}
	reply := s.dhcpReply(dhcp, replyType, guestIP)
	if len(reply) != 0 {
		emit(reply)
	}
	return true
}

func (s *tsnetVirtioStack) dhcpReply(reqDHCP []byte, replyType byte, guestIP netip.Addr) []byte {
	if len(reqDHCP) < 240 || !guestIP.Is4() {
		return nil
	}
	var guestMAC [6]byte
	copy(guestMAC[:], reqDHCP[28:34])
	s.mu.Lock()
	s.guestMAC = guestMAC
	hostMAC := s.hostMAC
	s.mu.Unlock()

	serverIP := tsnetDHCPServerIPv4()
	dnsIP := tsnetDNSIPv4()
	guest4 := guestIP.As4()
	server4 := serverIP.As4()
	dns4 := dnsIP.As4()

	options := make([]byte, 0, 64)
	options = append(options, dhcpOptionMessage, 1, replyType)
	options = append(options, dhcpOptionServer, 4)
	options = append(options, server4[:]...)
	options = append(options, dhcpOptionLease, 4, 0x00, 0x01, 0x51, 0x80) // 86400 seconds.
	options = append(options, dhcpOptionSubnet, 4, 255, 0, 0, 0)
	options = append(options, dhcpOptionRouter, 4)
	options = append(options, server4[:]...)
	options = append(options, dhcpOptionDNS, 4)
	options = append(options, dns4[:]...)
	options = append(options, dhcpOptionMTU, 2, byte(uint16(virtioNetMTU)>>8), byte(uint16(virtioNetMTU)&0xff))
	options = append(options, dhcpOptionEnd)

	dhcpLen := 240 + len(options)
	if dhcpLen < 300 {
		dhcpLen = 300
	}
	dhcp := make([]byte, dhcpLen)
	dhcp[0] = 2 // BOOTREPLY
	copy(dhcp[1:4], reqDHCP[1:4])
	copy(dhcp[4:12], reqDHCP[4:12])
	copy(dhcp[16:20], guest4[:])
	copy(dhcp[20:24], server4[:])
	copy(dhcp[28:44], reqDHCP[28:44])
	copy(dhcp[236:240], []byte{99, 130, 83, 99})
	copy(dhcp[240:], options)

	udpLen := 8 + len(dhcp)
	ipLen := 20 + udpLen
	frame := make([]byte, 14+ipLen)
	for i := 0; i < 6; i++ {
		frame[i] = 0xff
	}
	copy(frame[6:12], hostMAC[:])
	binary.BigEndian.PutUint16(frame[12:14], etherTypeIPv4)

	ip := frame[14:]
	ip[0] = 0x45
	ip[8] = 64
	ip[9] = ipProtoUDP
	binary.BigEndian.PutUint16(ip[2:4], uint16(ipLen))
	copy(ip[12:16], server4[:])
	copy(ip[16:20], []byte{255, 255, 255, 255})
	binary.BigEndian.PutUint16(ip[10:12], ipv4HeaderChecksum(ip[:20]))

	udp := ip[20:]
	binary.BigEndian.PutUint16(udp[0:2], bootpServerPort)
	binary.BigEndian.PutUint16(udp[2:4], bootpClientPort)
	binary.BigEndian.PutUint16(udp[4:6], uint16(udpLen))
	copy(udp[8:], dhcp)
	return frame
}

func dhcpMessageType(dhcp []byte) (byte, bool) {
	if len(dhcp) < 240 || dhcp[236] != 99 || dhcp[237] != 130 || dhcp[238] != 83 || dhcp[239] != 99 {
		return 0, false
	}
	for i := 240; i < len(dhcp); {
		code := dhcp[i]
		i++
		switch code {
		case 0:
			continue
		case dhcpOptionEnd:
			return 0, false
		}
		if i >= len(dhcp) {
			return 0, false
		}
		n := int(dhcp[i])
		i++
		if i+n > len(dhcp) {
			return 0, false
		}
		if code == dhcpOptionMessage && n == 1 {
			return dhcp[i], true
		}
		i += n
	}
	return 0, false
}

func ipv4HeaderChecksum(h []byte) uint16 {
	sum := uint32(0)
	for i := 0; i+1 < len(h); i += 2 {
		sum += uint32(binary.BigEndian.Uint16(h[i:]))
	}
	for sum > 0xffff {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	return ^uint16(sum)
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
	return filepath.Join(tailemuDir(), defaultTsnetStateSubdir)
}

func tailemuDir() string {
	if home := os.Getenv("HOME"); home != "" {
		return filepath.Join(home, defaultTailemuSubdir)
	}
	return filepath.Join(os.TempDir(), defaultTailemuSubdir)
}

func tsnetOpLogPath() string {
	return filepath.Join(tailemuDir(), tsnetOpLogName)
}

func appendTsnetOpLog(format string, args ...any) {
	path := tsnetOpLogPath()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		fmt.Fprintf(os.Stderr, "tsnet: oplog mkdir: %v\n", err)
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		fmt.Fprintf(os.Stderr, "tsnet: oplog open: %v\n", err)
		return
	}
	defer f.Close()
	msg := fmt.Sprintf(format, args...)
	_, _ = fmt.Fprintf(f, "%s %s\n", time.Now().Format(rfc3339MsecTz0), msg)
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

func tsnetEnvAddr(name string) netip.Addr {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return netip.Addr{}
	}
	ip, err := netip.ParseAddr(v)
	if err != nil {
		return netip.Addr{}
	}
	return ip
}

func tsnetDHCPServerIPv4() netip.Addr {
	if ip := tsnetEnvAddr("RISCV_EMU_TSNET_DHCP_SERVER_IPV4"); ip.Is4() {
		return ip
	}
	return netip.MustParseAddr("100.100.100.100")
}

func tsnetDNSIPv4() netip.Addr {
	if ip := tsnetEnvAddr("RISCV_EMU_TSNET_DNS_IPV4"); ip.Is4() {
		return ip
	}
	return tsnetDHCPServerIPv4()
}
