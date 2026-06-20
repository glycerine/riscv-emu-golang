package riscv

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	emunetpkg "github.com/glycerine/riscv-emu-golang/emunet"
	"github.com/tailscale/wireguard-go/tun"
	"tailscale.com/ipn"
	"tailscale.com/tsnet"
)

const (
	defaultTsnetStateSubdir = "riscv-emu"

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

	node      *emunetpkg.Node
	cancel    context.CancelFunc
	role      string
	promoting bool

	leaderCore *tsnetVirtioStack
	dns        *emunetpkg.DNSServer
	leaderCkt  *emunetpkg.Circuit
	leaderURL  string

	dev               *virtioNetDevice
	pending           [][]byte
	followerByCircuit map[*emunetpkg.Circuit]string

	followerURLs map[string]struct{}
}

type tsnetVirtioStack struct {
	srv *tsnet.Server
	tun *virtioNetMemoryTUN
	cfg EmuConfig

	mu                 sync.Mutex
	cancel             context.CancelFunc
	stateDir           string
	dev                *virtioNetDevice
	pending            [][]byte
	hostMAC            [6]byte
	guestMAC           [6]byte
	tailIPv4           netip.Addr
	tailIPv6           netip.Addr
	directTailnetGuest bool

	ports     map[string]*emunetPort
	nextLease byte
	natByOut  map[emunetNATOutKey]*emunetNATEntry
	natByIn   map[emunetNATInKey]*emunetNATEntry
	nextNATID uint16
	now       func() time.Time
	counters  emunetCounters
}

var (
	newEmunetLeaderTsnetVirtioStackHook = newEmunetLeaderTsnetVirtioStack
	emunetWatchDogInterval              = 250 * time.Millisecond
)

func newVirtioNetPacketStack(cfg EmuConfig) (virtioNetPacketStack, error) {
	if cfg.NetDirectTailnet {
		return newTsnetVirtioStack(cfg, true)
	}
	return newEmunetVirtioStack(cfg)
}

func newDirectTsnetVirtioStack(cfg EmuConfig) (*tsnetVirtioStack, error) {
	return newTsnetVirtioStack(cfg, true)
}

func newEmunetLeaderTsnetVirtioStack(cfg EmuConfig) (*tsnetVirtioStack, error) {
	return newTsnetVirtioStack(cfg, false)
}

func newTsnetVirtioStack(cfg EmuConfig, directTailnetGuest bool) (*tsnetVirtioStack, error) {
	stack := &tsnetVirtioStack{
		cfg:                cfg,
		hostMAC:            emunetRouterMAC,
		directTailnetGuest: directTailnetGuest,
	}
	stack.tun = newVirtioNetMemoryTUN(stack.handleTsnetPacket)
	stateDir := tsnetDir(cfg)
	hostname := tsnetHostname(cfg)
	ephemeral := cfg.TsnetEphemeral
	stack.stateDir = stateDir
	appendTsnetOpLog("start state_dir=%q state_file=%q hostname=%q ephemeral=%t authkey_set=%t",
		stateDir, filepath.Join(stateDir, "tailscaled.state"), hostname, ephemeral, cfg.TsnetAuthKey != "")
	stack.srv = &tsnet.Server{
		Dir:       stateDir,
		Hostname:  hostname,
		AuthKey:   cfg.TsnetAuthKey,
		Ephemeral: ephemeral,
		Tun:       stack.tun,
		UserLogf:  tsnetUserLogf,
	}
	if err := stack.srv.Start(); err != nil {
		appendTsnetOpLog("start_error state_dir=%q error=%q", stateDir, err)
		stack.tun.Close()
		return nil, err
	}
	appendTsnetOpLog("started state_dir=%q", stateDir)
	ctx, cancel := context.WithCancel(context.Background())
	stack.cancel = cancel
	if ip := parseTsnetAddr(cfg.TsnetGuestIPv4); ip.Is4() {
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
		node:              node,
		cancel:            cancel,
		followerURLs:      make(map[string]struct{}),
		followerByCircuit: make(map[*emunetpkg.Circuit]string),
	}
	go stack.readEmunetEvents(ctx)

	addr := emunetAddr(cfg)
	ln, listenErr := emunetpkg.ListenRendezvous(ctx, addr)
	if listenErr == nil {
		if err := stack.promoteToLeader(ctx, cfg, ln, "initial"); err != nil {
			_ = ln.Close()
			_ = stack.Close()
			return nil, err
		}
		return stack, nil
	}

	dns, lookupErr := emunetpkg.LookupDNS(ctx, addr)
	if lookupErr != nil {
		_ = stack.Close()
		return nil, errors.Join(listenErr, lookupErr)
	}
	appendTsnetOpLog("emunet_election role=follower addr=%q leader_url=%q follower_count=%d peer_url=%q",
		addr, dns.LeaderURL, len(dns.KnownFollowerURLs), node.PeerURL())
	if err := stack.connectToLeader(ctx, dns); err != nil {
		_ = stack.Close()
		return nil, err
	}
	stack.startFollowerWatchDog(ctx, cfg)
	return stack, nil
}

func (s *emunetVirtioStack) promoteToLeader(ctx context.Context, cfg EmuConfig, ln net.Listener, reason string) error {
	s.mu.Lock()
	if s.role == "leader" || s.promoting {
		s.mu.Unlock()
		_ = ln.Close()
		return nil
	}
	s.promoting = true
	s.mu.Unlock()

	appendTsnetOpLog("emunet_watchDog_promote_start reason=%q addr=%q peer_url=%q", reason, ln.Addr().String(), s.node.PeerURL())
	core, err := newEmunetLeaderTsnetVirtioStackHook(cfg)
	if err != nil {
		s.mu.Lock()
		s.promoting = false
		s.mu.Unlock()
		appendTsnetOpLog("emunet_watchDog_promote_error reason=%q addr=%q error=%q", reason, ln.Addr().String(), err)
		return err
	}
	dns := emunetpkg.StartDNSServer(ctx, ln, s.dnsSnapshot)

	s.mu.Lock()
	oldCkt := s.leaderCkt
	dev := s.dev
	s.leaderCkt = nil
	s.leaderURL = s.node.PeerURL()
	s.leaderCore = core
	s.dns = dns
	s.role = "leader"
	s.promoting = false
	s.mu.Unlock()

	if oldCkt != nil {
		oldCkt.Close(nil)
	}
	if dev != nil {
		core.attachVirtioNet(dev)
	}
	writeEmunetLeaderPIDFile()
	updateEmunetLeaderOpLogLink()
	appendTsnetOpLog("emunet_election role=leader reason=%q addr=%q peer_url=%q", reason, ln.Addr().String(), s.node.PeerURL())
	appendTsnetOpLog("emunet_dns_start addr=%q leader_url=%q", ln.Addr().String(), s.node.PeerURL())
	return nil
}

func (s *emunetVirtioStack) connectToLeader(ctx context.Context, dns emunetpkg.EmunetDNS) error {
	if dns.LeaderURL == "" {
		return errors.New("emunet dns response missing leader URL")
	}
	hello := emunetpkg.NewMessage(emunetpkg.MessageKindHello)
	hello.PeerURL = s.node.PeerURL()
	ckt, err := s.node.Connect(ctx, dns.LeaderURL, &hello)
	if err != nil {
		return err
	}

	s.mu.Lock()
	if s.role == "leader" {
		s.mu.Unlock()
		ckt.Close(nil)
		return nil
	}
	oldCkt := s.leaderCkt
	s.leaderCkt = ckt
	s.leaderURL = dns.LeaderURL
	s.role = "follower"
	s.mu.Unlock()

	if oldCkt != nil {
		oldCkt.Close(nil)
	}
	appendTsnetOpLog("emunet_circuit_connected role=follower leader_url=%q", dns.LeaderURL)
	return nil
}

func (s *emunetVirtioStack) startFollowerWatchDog(ctx context.Context, cfg EmuConfig) {
	appendTsnetOpLog("emunet_watchDog_start role=follower addr=%q interval=%s", emunetAddr(cfg), emunetWatchDogInterval)
	go s.runFollowerWatchDog(ctx, cfg)
}

func (s *emunetVirtioStack) runFollowerWatchDog(ctx context.Context, cfg EmuConfig) {
	ticker := time.NewTicker(emunetWatchDogInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if !s.isFollower() {
			return
		}
		addr := emunetAddr(cfg)
		ln, err := emunetpkg.ListenRendezvous(ctx, addr)
		if err == nil {
			if err := s.promoteToLeader(ctx, cfg, ln, "watchDog"); err != nil {
				_ = ln.Close()
				continue
			}
			return
		}
		if !s.isFollower() {
			continue
		}
		dns, lookupErr := emunetpkg.LookupDNS(ctx, addr)
		if lookupErr != nil {
			if s.needsLeaderReconnect() {
				appendTsnetOpLog("emunet_watchDog_reconnect_error addr=%q error=%q", addr, lookupErr)
			}
			continue
		}
		if !s.shouldConnectToDNSLeader(dns.LeaderURL) {
			continue
		}
		if err := s.connectToLeader(ctx, dns); err != nil {
			appendTsnetOpLog("emunet_watchDog_reconnect_error leader_url=%q error=%q", dns.LeaderURL, err)
			continue
		}
		appendTsnetOpLog("emunet_watchDog_reconnected leader_url=%q", dns.LeaderURL)
	}
}

func (s *emunetVirtioStack) isFollower() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.role == "follower"
}

func (s *emunetVirtioStack) needsLeaderReconnect() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.role == "follower" && s.leaderCkt == nil
}

func (s *emunetVirtioStack) noteLeaderCircuitLost(ckt *emunetpkg.Circuit, cause error) {
	s.mu.Lock()
	if s.role != "follower" || s.leaderCkt == nil || (ckt != nil && s.leaderCkt != ckt) {
		s.mu.Unlock()
		return
	}
	s.leaderCkt = nil
	s.leaderURL = ""
	s.mu.Unlock()
	appendTsnetOpLog("emunet_watchDog_leader_circuit_lost error=%q", cause)
}

func (s *emunetVirtioStack) shouldConnectToDNSLeader(leaderURL string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.role == "follower" && leaderURL != "" && (s.leaderCkt == nil || s.leaderURL != leaderURL)
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
	if err := node.SendFrame(ckt, msg, frame); err != nil {
		s.noteLeaderCircuitLost(ckt, err)
	}
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
				appendTsnetOpLog("emunet_circuit_error role=%q error=%q", s.roleSnapshot(), ev.Err)
				s.handleEmunetCircuitError(ev.Circuit, ev.Err)
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
			s.rememberFollowerCircuit(ev.Circuit, ev.Message.PeerURL)
			appendTsnetOpLog("emunet_client_connected role=%q peer_url=%q", s.roleSnapshot(), ev.Message.PeerURL)
		}
	case emunetpkg.MessageKindEthernetFrame:
		s.mu.Lock()
		core := s.leaderCore
		node := s.node
		s.mu.Unlock()
		if core != nil {
			portID := ev.Message.PeerURL
			if portID == "" && ev.Circuit != nil {
				portID = ev.Circuit.RemoteCircuitURL()
			}
			core.handleGuestFrameForPort(portID, ev.Message.Frame, func(reply []byte) {
				msg := emunetpkg.NewMessage(emunetpkg.MessageKindEthernetFrame)
				msg.PeerURL = node.PeerURL()
				_ = node.SendFrame(ev.Circuit, msg, reply)
			})
			return
		}
		s.injectGuestEthernet(ev.Message.Frame)
	}
}

func (s *emunetVirtioStack) handleEmunetCircuitError(ckt *emunetpkg.Circuit, cause error) {
	if s.roleSnapshot() == "leader" {
		s.forgetFollowerCircuit(ckt, cause)
		return
	}
	s.noteLeaderCircuitLost(ckt, cause)
}

func (s *emunetVirtioStack) rememberFollowerCircuit(ckt *emunetpkg.Circuit, peerURL string) {
	s.mu.Lock()
	if s.followerURLs == nil {
		s.followerURLs = make(map[string]struct{})
	}
	if s.followerByCircuit == nil {
		s.followerByCircuit = make(map[*emunetpkg.Circuit]string)
	}
	s.followerURLs[peerURL] = struct{}{}
	if ckt != nil {
		s.followerByCircuit[ckt] = peerURL
	}
	s.mu.Unlock()
}

func (s *emunetVirtioStack) forgetFollowerCircuit(ckt *emunetpkg.Circuit, cause error) {
	s.mu.Lock()
	peerURL := ""
	if ckt != nil && s.followerByCircuit != nil {
		peerURL = s.followerByCircuit[ckt]
		delete(s.followerByCircuit, ckt)
	}
	if peerURL != "" {
		delete(s.followerURLs, peerURL)
	}
	core := s.leaderCore
	s.mu.Unlock()

	if peerURL == "" {
		return
	}
	if core != nil {
		core.removeEmunetPort(peerURL)
	}
	appendTsnetOpLog("emunet_client_disconnected peer_url=%q error=%q", peerURL, cause)
}

func (s *emunetVirtioStack) roleSnapshot() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.role
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
	s.handleGuestFrameForPort(emunetLocalPortID, frame, s.injectGuestEthernet)
}

func (s *tsnetVirtioStack) handleGuestFrameForPort(portID string, frame []byte, emit func([]byte)) {
	if len(frame) < 14 {
		s.incDrop(emunetDropMalformedEthernet)
		return
	}
	etherType := binary.BigEndian.Uint16(frame[12:14])
	switch etherType {
	case etherTypeIPv4:
		if s.handleDHCP(portID, frame, emit) {
			return
		}
		if s.directTailnetGuest {
			s.tun.InjectIPPacket(frame[14:])
			return
		}
		if reply := s.gatewayICMPEchoReply(portID, frame, emit); len(reply) != 0 {
			emit(reply)
			return
		}
		if reply := s.tsnetICMPEchoReplyForGuest(portID, frame, emit); len(reply) != 0 {
			emit(reply)
			return
		}
		if s.deliverLocalIPv4(portID, frame, emit) {
			return
		}
		if pkt := s.natOutbound(portID, frame[14:], emit); len(pkt) != 0 {
			s.tun.InjectIPPacket(pkt)
		}
	case etherTypeIPv6:
		if s.directTailnetGuest {
			s.tun.InjectIPPacket(frame[14:])
			return
		}
		if reply := s.tsnetICMPEchoReplyForGuest(portID, frame, emit); len(reply) != 0 {
			emit(reply)
			return
		}
		s.incDrop(emunetDropUnsupportedEth)
	case etherTypeARP:
		if s.directTailnetGuest {
			if reply := s.arpReply(frame); len(reply) != 0 {
				emit(reply)
			}
			return
		}
		if reply := s.arpReplyForPort(portID, frame, emit); len(reply) != 0 {
			emit(reply)
		}
	default:
		s.incDrop(emunetDropUnsupportedEth)
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
			writeTerminalStatusf("tsnet: up: %v", err)
			appendTsnetOpLog("up_error error=%q", err)
		}
		return
	}
	if !s.directTailnetGuest {
		s.configureEmunetLeaderTsnetPrefs(ctx)
	}
	appendTsnetOpLog("authorized ips=%v state_dir=%q", status.TailscaleIPs, s.stateDir)
	have4 := false
	have6 := false
	for _, ip := range status.TailscaleIPs {
		if ip.Is4() {
			s.setTailIPv4(ip)
			appendTsnetOpLog("guest_ipv4_ready ip=%s", ip)
			have4 = true
		}
		if ip.Is6() {
			s.setTailIPv6(ip)
			appendTsnetOpLog("guest_ipv6_ready ip=%s", ip)
			have6 = true
		}
	}
	if !have4 {
		appendTsnetOpLog("authorized_no_ipv4 ips=%v", status.TailscaleIPs)
	}
	if !have6 {
		appendTsnetOpLog("authorized_no_ipv6 ips=%v", status.TailscaleIPs)
	}
}

func (s *tsnetVirtioStack) configureEmunetLeaderTsnetPrefs(ctx context.Context) {
	lc, err := s.srv.LocalClient()
	if err != nil {
		appendTsnetOpLog("prefs_error routeall=true auto_exit_node=%q error=%q", ipn.AnyExitNode, err)
		return
	}
	prefs, err := lc.EditPrefs(ctx, emunetLeaderTsnetPrefs())
	if err != nil {
		appendTsnetOpLog("prefs_error routeall=true auto_exit_node=%q error=%q", ipn.AnyExitNode, err)
		return
	}
	appendTsnetOpLog("prefs routeall=%t auto_exit_node=%q exit_node_id=%q exit_node_ip=%s", prefs.RouteAll, prefs.AutoExitNode, prefs.ExitNodeID, prefs.ExitNodeIP)
}

func emunetLeaderTsnetPrefs() *ipn.MaskedPrefs {
	return &ipn.MaskedPrefs{
		RouteAllSet:     true,
		AutoExitNodeSet: true,
		Prefs: ipn.Prefs{
			RouteAll:     true,
			AutoExitNode: ipn.AnyExitNode,
		},
	}
}

func (s *tsnetVirtioStack) setTailIPv4(ip netip.Addr) {
	if !ip.Is4() {
		return
	}
	s.mu.Lock()
	s.tailIPv4 = ip
	s.mu.Unlock()
}

func (s *tsnetVirtioStack) setTailIPv6(ip netip.Addr) {
	if !ip.Is6() {
		return
	}
	s.mu.Lock()
	s.tailIPv6 = ip
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
		ip4, ip6 := s.srv.TailscaleIPs()
		if ip6.Is6() {
			s.setTailIPv6(ip6)
		}
		if ip4.Is4() {
			s.setTailIPv4(ip4)
			return ip4, true
		}
	}
	return netip.Addr{}, false
}

func (s *tsnetVirtioStack) tailscaleIPv6() (netip.Addr, bool) {
	s.mu.Lock()
	ip := s.tailIPv6
	s.mu.Unlock()
	if ip.Is6() {
		return ip, true
	}
	if s.srv != nil {
		ip4, ip6 := s.srv.TailscaleIPs()
		if ip4.Is4() {
			s.setTailIPv4(ip4)
		}
		if ip6.Is6() {
			s.setTailIPv6(ip6)
			return ip6, true
		}
	}
	return netip.Addr{}, false
}

func (s *tsnetVirtioStack) handleTsnetPacket(pkt []byte) {
	if len(pkt) == 0 {
		return
	}
	if !s.directTailnetGuest {
		if reply := s.tsnetICMPEchoReplyForTailnet(pkt); len(reply) != 0 {
			s.tun.InjectIPPacket(reply)
			return
		}
		_, guestMAC, guestPkt, emit, ok := s.natInbound(pkt)
		if !ok {
			return
		}
		emit(ethernetIPv4Frame(guestMAC, s.hostMAC, guestPkt))
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

func (s *tsnetVirtioStack) handleDHCP(portID string, frame []byte, emit func([]byte)) bool {
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
	var guestIP netip.Addr
	var serverIP netip.Addr
	var dnsIP netip.Addr
	subnet := [4]byte{255, 0, 0, 0}
	if s.directTailnetGuest {
		var ok bool
		guestIP, ok = s.tailscaleIPv4()
		if !ok {
			return true
		}
		serverIP = tsnetDHCPServerIPv4(s.cfg)
		dnsIP = tsnetDNSIPv4(s.cfg)
	} else {
		var guestMAC [6]byte
		copy(guestMAC[:], dhcp[28:34])
		s.learnPortMAC(portID, guestMAC, emit)
		guestIP = s.portLease(portID, emit)
		s.trace("dhcp port=%q msg=%d lease=%s mac=%x", portID, msgType, guestIP, guestMAC)
		serverIP = emunetRouterIPv4
		dnsIP = emunetDNSIPv4
		subnet = [4]byte{255, 255, 255, 0}
	}
	reply := s.dhcpReply(dhcp, replyType, guestIP, serverIP, dnsIP, subnet)
	if len(reply) != 0 {
		emit(reply)
		s.incDHCPReply(replyType)
	}
	return true
}

func (s *tsnetVirtioStack) dhcpReply(reqDHCP []byte, replyType byte, guestIP, serverIP, dnsIP netip.Addr, subnet [4]byte) []byte {
	if len(reqDHCP) < 240 || !guestIP.Is4() || !serverIP.Is4() || !dnsIP.Is4() {
		return nil
	}
	var guestMAC [6]byte
	copy(guestMAC[:], reqDHCP[28:34])
	s.mu.Lock()
	s.guestMAC = guestMAC
	hostMAC := s.hostMAC
	s.mu.Unlock()

	guest4 := guestIP.As4()
	server4 := serverIP.As4()
	dns4 := dnsIP.As4()

	options := make([]byte, 0, 64)
	options = append(options, dhcpOptionMessage, 1, replyType)
	options = append(options, dhcpOptionServer, 4)
	options = append(options, server4[:]...)
	options = append(options, dhcpOptionLease, 4, 0x00, 0x01, 0x51, 0x80) // 86400 seconds.
	options = append(options, dhcpOptionSubnet, 4)
	options = append(options, subnet[:]...)
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

func emunetAddr(cfg EmuConfig) string {
	if cfg.EmunetAddr != "" {
		return cfg.EmunetAddr
	}
	return emunetpkg.DefaultAddr
}

func tsnetDir(cfg EmuConfig) string {
	if cfg.TsnetDir != "" {
		return cfg.TsnetDir
	}
	return filepath.Join(emunetDir(), defaultTsnetStateSubdir)
}

func tsnetOpLogPath() string {
	name := fmt.Sprintf("oplog.%d", os.Getpid())
	return filepath.Join(emunetStateDir(), name)
}

func emunetLeaderOpLogLinkPath() string {
	return filepath.Join(emunetDir(), "oplog.leader.lnk")
}

func writeEmunetLeaderPIDFile() string {
	dir := emunetDir()
	name := fmt.Sprintf("leader.%d", os.Getpid())
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(dir, 0700); err != nil {
		appendTsnetOpLog("emunet_leader_pidfile_error path=%q error=%q", path, err)
		return path
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		appendTsnetOpLog("emunet_leader_pidfile_readdir_error dir=%q error=%q", dir, err)
	} else {
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasPrefix(entry.Name(), "leader.") || entry.Name() == name {
				continue
			}
			stale := filepath.Join(dir, entry.Name())
			if err := os.Remove(stale); err != nil {
				appendTsnetOpLog("emunet_leader_pidfile_remove_error path=%q error=%q", stale, err)
			}
		}
	}
	if err := os.WriteFile(path, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0600); err != nil {
		appendTsnetOpLog("emunet_leader_pidfile_error path=%q error=%q", path, err)
		return path
	}
	appendTsnetOpLog("emunet_leader_pidfile path=%q", path)
	return path
}

func updateEmunetLeaderOpLogLink() string {
	link := emunetLeaderOpLogLinkPath()
	target := tsnetOpLogPath()
	if err := os.MkdirAll(filepath.Dir(link), 0700); err != nil {
		appendTsnetOpLog("emunet_leader_oplog_link_error path=%q target=%q error=%q", link, target, err)
		return link
	}

	tmp := filepath.Join(filepath.Dir(link), fmt.Sprintf(".oplog.leader.lnk.%d.tmp", os.Getpid()))
	_ = os.Remove(tmp)
	if err := os.Symlink(target, tmp); err != nil {
		appendTsnetOpLog("emunet_leader_oplog_link_error path=%q target=%q error=%q", link, target, err)
		return link
	}
	if err := os.Remove(link); err != nil && !os.IsNotExist(err) {
		_ = os.Remove(tmp)
		appendTsnetOpLog("emunet_leader_oplog_link_error path=%q target=%q error=%q", link, target, err)
		return link
	}
	if err := os.Rename(tmp, link); err != nil {
		_ = os.Remove(tmp)
		appendTsnetOpLog("emunet_leader_oplog_link_error path=%q target=%q error=%q", link, target, err)
		return link
	}
	appendTsnetOpLog("emunet_leader_oplog_link path=%q target=%q", link, target)
	return link
}

func tsnetUserLogf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	for _, line := range strings.Split(strings.TrimRight(msg, "\n"), "\n") {
		appendTsnetOpLog("tsnet_user %s", line)
	}
	writeTerminalStatusf("tsnet: %s", msg)
}

func appendTsnetOpLog(format string, args ...any) {
	path := tsnetOpLogPath()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		writeTerminalStatusf("tsnet: oplog mkdir: %v", err)
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		writeTerminalStatusf("tsnet: oplog open: %v", err)
		return
	}
	defer f.Close()
	msg := fmt.Sprintf(format, args...)
	_, _ = fmt.Fprintf(f, "%s %s\n", time.Now().Format(rfc3339MsecTz0), msg)
}

func tsnetHostname(cfg EmuConfig) string {
	if cfg.TsnetHostname != "" {
		return cfg.TsnetHostname
	}
	return "riscv-emu"
}

func parseTsnetAddr(raw string) netip.Addr {
	v := strings.TrimSpace(raw)
	if v == "" {
		return netip.Addr{}
	}
	ip, err := netip.ParseAddr(v)
	if err != nil {
		return netip.Addr{}
	}
	return ip
}

func tsnetDHCPServerIPv4(cfg EmuConfig) netip.Addr {
	if ip := parseTsnetAddr(cfg.TsnetDHCPServerIPv4); ip.Is4() {
		return ip
	}
	return netip.MustParseAddr("100.100.100.100")
}

func tsnetDNSIPv4(cfg EmuConfig) netip.Addr {
	if ip := parseTsnetAddr(cfg.TsnetDNSIPv4); ip.Is4() {
		return ip
	}
	return tsnetDHCPServerIPv4(cfg)
}
