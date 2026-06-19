package emunet

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

const (
	DefaultAddr = "127.0.0.1:7557"
	DNSHello    = "HELLO EMUNET\n"
)

type DNSProvider func() EmunetDNS

type DNSServer struct {
	ln     net.Listener
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func ListenRendezvous(ctx context.Context, addr string) (net.Listener, error) {
	if addr == "" {
		addr = DefaultAddr
	}
	if err := validateLoopbackAddr(addr); err != nil {
		return nil, err
	}
	var lc net.ListenConfig
	return lc.Listen(ctx, "tcp", addr)
}

func StartDNSServer(ctx context.Context, ln net.Listener, provider DNSProvider) *DNSServer {
	ctx, cancel := context.WithCancel(ctx)
	s := &DNSServer{
		ln:     ln,
		cancel: cancel,
	}
	s.wg.Add(1)
	go s.serve(ctx, provider)
	return s
}

func (s *DNSServer) Close() error {
	if s == nil {
		return nil
	}
	s.cancel()
	err := s.ln.Close()
	s.wg.Wait()
	return err
}

func (s *DNSServer) serve(ctx context.Context, provider DNSProvider) {
	defer s.wg.Done()
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
				continue
			}
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			_ = serveDNSConn(ctx, conn, provider)
		}()
	}
}

func serveDNSConn(ctx context.Context, conn net.Conn, provider DNSProvider) error {
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return err
	}
	if line != DNSHello {
		return fmt.Errorf("bad emunet dns hello %q", line)
	}
	dns := EmunetDNS{}
	if provider != nil {
		dns = provider()
	}
	payload, err := MarshalDNS(dns)
	if err != nil {
		return err
	}
	_, err = conn.Write(payload)
	return err
}

func LookupDNS(ctx context.Context, addr string) (EmunetDNS, error) {
	if addr == "" {
		addr = DefaultAddr
	}
	if err := validateLoopbackAddr(addr); err != nil {
		return EmunetDNS{}, err
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return EmunetDNS{}, err
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	if _, err := io.WriteString(conn, DNSHello); err != nil {
		return EmunetDNS{}, err
	}
	payload, err := io.ReadAll(conn)
	if err != nil {
		return EmunetDNS{}, err
	}
	if len(payload) == 0 {
		return EmunetDNS{}, errors.New("empty emunet dns response")
	}
	return UnmarshalDNS(payload)
}

func validateLoopbackAddr(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("emunet address %q: %w", addr, err)
	}
	if host == "" {
		return fmt.Errorf("emunet address %q is not loopback", addr)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("emunet address %q is not loopback", addr)
	}
	return nil
}
