package riscv

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"syscall"
	"time"
)

func (jos *Jea9Linux) sysSocket(domainRaw, typeRaw, protoRaw uint64) int64 {
	if !jos.allowAllHostFiles {
		return jea9LinuxErrEACCES
	}
	domain := int32(domainRaw)
	typ := int32(typeRaw)
	proto := int32(protoRaw)
	flags := typ & (jea9LinuxSockNonblock | jea9LinuxSockCloexec)
	baseType := typ &^ (jea9LinuxSockNonblock | jea9LinuxSockCloexec)
	if domain != jea9LinuxAFInet && domain != jea9LinuxAFInet6 {
		return jea9LinuxErrEAFNOSUPPORT
	}
	if baseType != jea9LinuxSockStream {
		return jea9LinuxErrEOPNOTSUPP
	}
	if proto != 0 && proto != jea9LinuxIPProtoTCP {
		return jea9LinuxErrEPROTONOSUP
	}
	fd := jos.allocFD(jea9LinuxFD{
		kind:           jea9LinuxFDSocket,
		flags:          uint64(flags),
		socketFamily:   domain,
		socketType:     baseType,
		socketProtocol: proto,
	})
	return int64(fd)
}

func (jos *Jea9Linux) sysSocketpair(domain, typ, proto, fdsAddr uint64) int64 {
	_, _, _, _ = domain, typ, proto, fdsAddr
	return jea9LinuxErrEOPNOTSUPP
}

func (jos *Jea9Linux) sysBind(cpu *CPU, fdRaw, sockaddrAddr, addrlen uint64) int64 {
	fd, f, errno := jos.socketFD(fdRaw)
	if errno != 0 {
		return errno
	}
	_ = fd
	if f.tcpListener != nil || f.tcpConn != nil {
		return jea9LinuxErrEINVAL
	}
	addr, errno := loadJea9LinuxTCPAddr(cpu, sockaddrAddr, addrlen)
	if errno != 0 {
		return errno
	}
	if !socketFamilyMatchesAddr(f.socketFamily, addr) {
		return jea9LinuxErrEAFNOSUPPORT
	}
	f.socketLocal = addr
	jos.fds[int(int64(fdRaw))] = f
	return 0
}

func (jos *Jea9Linux) sysListen(fdRaw, backlog uint64) int64 {
	fd, f, errno := jos.socketFD(fdRaw)
	if errno != 0 {
		return errno
	}
	_ = backlog
	if f.socketType != jea9LinuxSockStream {
		return jea9LinuxErrEOPNOTSUPP
	}
	if f.tcpConn != nil {
		return jea9LinuxErrEINVAL
	}
	if f.tcpListener != nil {
		return 0
	}
	addr := f.socketLocal
	if addr == nil {
		addr = &net.TCPAddr{IP: net.IPv4zero, Port: 0}
		if f.socketFamily == jea9LinuxAFInet6 {
			addr = &net.TCPAddr{IP: net.IPv6zero, Port: 0}
		}
	}
	network := "tcp4"
	if f.socketFamily == jea9LinuxAFInet6 {
		network = "tcp6"
	}
	ln, err := net.ListenTCP(network, addr)
	if err != nil {
		return jea9LinuxErrnoFromHost(err)
	}
	f.tcpListener = ln
	if got, ok := ln.Addr().(*net.TCPAddr); ok {
		f.socketLocal = cloneTCPAddr(got)
	}
	jos.fds[fd] = f
	return 0
}

func (jos *Jea9Linux) sysConnect(cpu *CPU, fdRaw, sockaddrAddr, addrlen uint64) int64 {
	fd, f, errno := jos.socketFD(fdRaw)
	if errno != 0 {
		return errno
	}
	if f.tcpConn != nil {
		return jea9LinuxErrEISCONN
	}
	if f.tcpListener != nil {
		return jea9LinuxErrEOPNOTSUPP
	}
	addr, errno := loadJea9LinuxTCPAddr(cpu, sockaddrAddr, addrlen)
	if errno != 0 {
		return errno
	}
	if !socketFamilyMatchesAddr(f.socketFamily, addr) {
		return jea9LinuxErrEAFNOSUPPORT
	}
	network := "tcp4"
	if f.socketFamily == jea9LinuxAFInet6 {
		network = "tcp6"
	}
	conn, err := net.DialTCP(network, f.socketLocal, addr)
	if err != nil {
		return jea9LinuxErrnoFromHost(err)
	}
	_ = conn.SetNoDelay(true)
	f.tcpConn = conn
	if got, ok := conn.LocalAddr().(*net.TCPAddr); ok {
		f.socketLocal = cloneTCPAddr(got)
	}
	if got, ok := conn.RemoteAddr().(*net.TCPAddr); ok {
		f.socketPeer = cloneTCPAddr(got)
	}
	jos.fds[fd] = f
	jos.wakeAllSocketEpollWaiters(cpu)
	return 0
}

func (jos *Jea9Linux) sysAccept4(cpu *CPU, fdRaw, sockaddrAddr, addrlenAddr, flagsRaw uint64) int64 {
	fd, f, errno := jos.socketFD(fdRaw)
	if errno != 0 {
		return errno
	}
	flags := int32(flagsRaw)
	if flags&^(jea9LinuxSockNonblock|jea9LinuxSockCloexec) != 0 {
		return jea9LinuxErrEINVAL
	}
	if f.tcpListener == nil {
		return jea9LinuxErrEINVAL
	}
	if !jos.socketEnsurePending(fd, &f) {
		return jea9LinuxErrEAGAIN
	}
	conn := f.socketPending[0]
	copy(f.socketPending, f.socketPending[1:])
	f.socketPending = f.socketPending[:len(f.socketPending)-1]
	jos.fds[fd] = f
	_ = conn.SetNoDelay(true)
	local, _ := conn.LocalAddr().(*net.TCPAddr)
	peer, _ := conn.RemoteAddr().(*net.TCPAddr)
	if sockaddrAddr != 0 && addrlenAddr != 0 && peer != nil {
		if errno := storeJea9LinuxTCPAddr(cpu, sockaddrAddr, addrlenAddr, peer); errno != 0 {
			_ = conn.Close()
			return errno
		}
	}
	newfd := jos.allocFD(jea9LinuxFD{
		kind:           jea9LinuxFDSocket,
		flags:          uint64(flags),
		socketFamily:   f.socketFamily,
		socketType:     f.socketType,
		socketProtocol: f.socketProtocol,
		tcpConn:        conn,
		socketLocal:    cloneTCPAddr(local),
		socketPeer:     cloneTCPAddr(peer),
	})
	return int64(newfd)
}

func (jos *Jea9Linux) sysGetsockname(cpu *CPU, fdRaw, sockaddrAddr, addrlenAddr uint64) int64 {
	_, f, errno := jos.socketFD(fdRaw)
	if errno != 0 {
		return errno
	}
	addr := f.socketLocal
	if f.tcpListener != nil {
		if got, ok := f.tcpListener.Addr().(*net.TCPAddr); ok {
			addr = got
		}
	}
	if f.tcpConn != nil {
		if got, ok := f.tcpConn.LocalAddr().(*net.TCPAddr); ok {
			addr = got
		}
	}
	if addr == nil {
		addr = &net.TCPAddr{IP: net.IPv4zero, Port: 0}
		if f.socketFamily == jea9LinuxAFInet6 {
			addr = &net.TCPAddr{IP: net.IPv6zero, Port: 0}
		}
	}
	return storeJea9LinuxTCPAddr(cpu, sockaddrAddr, addrlenAddr, addr)
}

func (jos *Jea9Linux) sysGetpeername(cpu *CPU, fdRaw, sockaddrAddr, addrlenAddr uint64) int64 {
	_, f, errno := jos.socketFD(fdRaw)
	if errno != 0 {
		return errno
	}
	addr := f.socketPeer
	if f.tcpConn != nil {
		if got, ok := f.tcpConn.RemoteAddr().(*net.TCPAddr); ok {
			addr = got
		}
	}
	if addr == nil {
		return jea9LinuxErrENOTCONN
	}
	return storeJea9LinuxTCPAddr(cpu, sockaddrAddr, addrlenAddr, addr)
}

func (jos *Jea9Linux) sysSetsockopt(cpu *CPU, fdRaw, levelRaw, optRaw, valAddr, vallen uint64) int64 {
	_, _, errno := jos.socketFD(fdRaw)
	if errno != 0 {
		return errno
	}
	if vallen > 0 && valAddr != 0 {
		buf := make([]byte, int(minUint64(vallen, 256)))
		if f := cpu.mem.ReadBytes(valAddr, buf); f != nil {
			return jea9LinuxErrEFAULT
		}
	}
	level := int32(levelRaw)
	opt := int32(optRaw)
	switch {
	case level == jea9LinuxSolSocket:
		return 0
	case level == jea9LinuxSolTCP && opt == jea9LinuxTCPNoDelay:
		return 0
	case level == jea9LinuxIPProtoIPv6 && opt == jea9LinuxIPv6V6Only:
		return 0
	default:
		return 0
	}
}

func (jos *Jea9Linux) sysGetsockopt(cpu *CPU, fdRaw, levelRaw, optRaw, valAddr, vallenAddr uint64) int64 {
	_, f, errno := jos.socketFD(fdRaw)
	if errno != 0 {
		return errno
	}
	if valAddr == 0 || vallenAddr == 0 {
		return jea9LinuxErrEFAULT
	}
	vallenRaw, fault := cpu.mem.Load32(vallenAddr)
	if fault != nil {
		return jea9LinuxErrEFAULT
	}
	if vallenRaw == 0 {
		return 0
	}
	level := int32(levelRaw)
	opt := int32(optRaw)
	value := int32(0)
	switch {
	case level == jea9LinuxSolSocket && opt == jea9LinuxSoError:
		value = 0
	case level == jea9LinuxSolSocket && opt == jea9LinuxSoType:
		value = f.socketType
	default:
		value = 0
	}
	var raw [4]byte
	binary.LittleEndian.PutUint32(raw[:], uint32(value))
	n := int(vallenRaw)
	if n > len(raw) {
		n = len(raw)
	}
	if fault := cpu.mem.WriteBytes(valAddr, raw[:n]); fault != nil {
		return jea9LinuxErrEFAULT
	}
	if fault := cpu.mem.Store32(vallenAddr, uint32(n)); fault != nil {
		return jea9LinuxErrEFAULT
	}
	return 0
}

func (jos *Jea9Linux) sysShutdown(fdRaw, how uint64) int64 {
	_, f, errno := jos.socketFD(fdRaw)
	if errno != 0 {
		return errno
	}
	if f.tcpConn == nil {
		return jea9LinuxErrENOTCONN
	}
	switch how {
	case 0:
		if err := f.tcpConn.CloseRead(); err != nil {
			return jea9LinuxErrnoFromHost(err)
		}
	case 1:
		if err := f.tcpConn.CloseWrite(); err != nil {
			return jea9LinuxErrnoFromHost(err)
		}
	case 2:
		if err := f.tcpConn.Close(); err != nil {
			return jea9LinuxErrnoFromHost(err)
		}
	default:
		return jea9LinuxErrEINVAL
	}
	return 0
}

func (jos *Jea9Linux) sysSendto(cpu *CPU, fdRaw, bufAddr, n, flags, toAddr, toLen uint64) int64 {
	_, _, _ = flags, toAddr, toLen
	return jos.sysSocketWrite(cpu, int(int64(fdRaw)), bufAddr, n)
}

func (jos *Jea9Linux) sysRecvfrom(cpu *CPU, fdRaw, bufAddr, n, flags, fromAddr, fromLenAddr uint64) int64 {
	_ = flags
	count := jos.sysSocketRead(cpu, int(int64(fdRaw)), bufAddr, n)
	if count >= 0 && fromAddr != 0 && fromLenAddr != 0 {
		_, f, errno := jos.socketFD(fdRaw)
		if errno != 0 {
			return errno
		}
		if f.socketPeer != nil {
			if errno := storeJea9LinuxTCPAddr(cpu, fromAddr, fromLenAddr, f.socketPeer); errno != 0 {
				return errno
			}
		}
	}
	return count
}

func (jos *Jea9Linux) sysSocketRead(cpu *CPU, fd int, bufAddr, n uint64) int64 {
	f, ok := jos.fds[fd]
	if !ok {
		return jea9LinuxErrEBADF
	}
	if f.kind != jea9LinuxFDSocket || f.tcpConn == nil {
		return jea9LinuxErrENOTCONN
	}
	if n == 0 {
		return 0
	}
	if n > uint64(int(^uint(0)>>1)) {
		return jea9LinuxErrEINVAL
	}
	out := make([]byte, int(n))
	copied := copy(out, f.socketReadBuf)
	if copied > 0 {
		f.socketReadBuf = f.socketReadBuf[copied:]
		jos.fds[fd] = f
		if fault := cpu.mem.WriteBytes(bufAddr, out[:copied]); fault != nil {
			return jea9LinuxErrEFAULT
		}
		return int64(copied)
	}
	if f.socketEOF {
		return 0
	}
	_ = f.tcpConn.SetReadDeadline(time.Now())
	nread, err := f.tcpConn.Read(out)
	_ = f.tcpConn.SetReadDeadline(time.Time{})
	if nread > 0 {
		if fault := cpu.mem.WriteBytes(bufAddr, out[:nread]); fault != nil {
			return jea9LinuxErrEFAULT
		}
		return int64(nread)
	}
	if errors.Is(err, io.EOF) {
		f.socketEOF = true
		jos.fds[fd] = f
		return 0
	}
	if socketWouldBlock(err) {
		return jea9LinuxErrEAGAIN
	}
	if err != nil {
		return jea9LinuxErrnoFromHost(err)
	}
	return 0
}

func (jos *Jea9Linux) sysSocketWrite(cpu *CPU, fd int, bufAddr, n uint64) int64 {
	f, ok := jos.fds[fd]
	if !ok {
		return jea9LinuxErrEBADF
	}
	if f.kind != jea9LinuxFDSocket || f.tcpConn == nil {
		return jea9LinuxErrENOTCONN
	}
	if n == 0 {
		return 0
	}
	if n > uint64(int(^uint(0)>>1)) {
		return jea9LinuxErrEINVAL
	}
	buf := make([]byte, int(n))
	if fault := cpu.mem.ReadBytes(bufAddr, buf); fault != nil {
		return jea9LinuxErrEFAULT
	}
	_ = f.tcpConn.SetWriteDeadline(time.Now().Add(10 * time.Millisecond))
	written, err := f.tcpConn.Write(buf)
	_ = f.tcpConn.SetWriteDeadline(time.Time{})
	if written > 0 {
		jos.wakeAllSocketEpollWaiters(cpu)
		return int64(written)
	}
	if socketWouldBlock(err) {
		return jea9LinuxErrEAGAIN
	}
	if err != nil {
		return jea9LinuxErrnoFromHost(err)
	}
	return 0
}

func (jos *Jea9Linux) socketFD(fdRaw uint64) (int, jea9LinuxFD, int64) {
	fd := int(int64(fdRaw))
	f, ok := jos.fds[fd]
	if !ok {
		return 0, jea9LinuxFD{}, jea9LinuxErrEBADF
	}
	if f.kind != jea9LinuxFDSocket {
		return 0, jea9LinuxFD{}, jea9LinuxErrENOTSOCK
	}
	return fd, f, 0
}

func loadJea9LinuxTCPAddr(cpu *CPU, addr, addrlen uint64) (*net.TCPAddr, int64) {
	if addr == 0 {
		return nil, jea9LinuxErrEFAULT
	}
	if addrlen < 2 {
		return nil, jea9LinuxErrEINVAL
	}
	familyRaw, fault := cpu.mem.Load16(addr)
	if fault != nil {
		return nil, jea9LinuxErrEFAULT
	}
	switch int32(familyRaw) {
	case jea9LinuxAFInet:
		if addrlen < 16 {
			return nil, jea9LinuxErrEINVAL
		}
		var raw [16]byte
		if fault := cpu.mem.ReadBytes(addr, raw[:]); fault != nil {
			return nil, jea9LinuxErrEFAULT
		}
		port := int(binary.BigEndian.Uint16(raw[2:4]))
		ip := net.IPv4(raw[4], raw[5], raw[6], raw[7])
		return &net.TCPAddr{IP: ip, Port: port}, 0
	case jea9LinuxAFInet6:
		if addrlen < 28 {
			return nil, jea9LinuxErrEINVAL
		}
		var raw [28]byte
		if fault := cpu.mem.ReadBytes(addr, raw[:]); fault != nil {
			return nil, jea9LinuxErrEFAULT
		}
		port := int(binary.BigEndian.Uint16(raw[2:4]))
		ip := append(net.IP(nil), raw[8:24]...)
		scope := int(binary.LittleEndian.Uint32(raw[24:28]))
		return &net.TCPAddr{IP: ip, Port: port, Zone: zoneForScopeID(scope)}, 0
	default:
		return nil, jea9LinuxErrEAFNOSUPPORT
	}
}

func storeJea9LinuxTCPAddr(cpu *CPU, addr, addrlenAddr uint64, tcp *net.TCPAddr) int64 {
	if addr == 0 || addrlenAddr == 0 {
		return jea9LinuxErrEFAULT
	}
	addrlenRaw, fault := cpu.mem.Load32(addrlenAddr)
	if fault != nil {
		return jea9LinuxErrEFAULT
	}
	ip4 := tcp.IP.To4()
	var raw []byte
	if ip4 != nil {
		buf := make([]byte, 16)
		binary.LittleEndian.PutUint16(buf[0:2], uint16(jea9LinuxAFInet))
		binary.BigEndian.PutUint16(buf[2:4], uint16(tcp.Port))
		copy(buf[4:8], ip4)
		raw = buf
	} else {
		buf := make([]byte, 28)
		binary.LittleEndian.PutUint16(buf[0:2], uint16(jea9LinuxAFInet6))
		binary.BigEndian.PutUint16(buf[2:4], uint16(tcp.Port))
		ip16 := tcp.IP.To16()
		if ip16 == nil {
			ip16 = net.IPv6zero
		}
		copy(buf[8:24], ip16)
		raw = buf
	}
	n := int(addrlenRaw)
	if n > len(raw) {
		n = len(raw)
	}
	if n > 0 {
		if fault := cpu.mem.WriteBytes(addr, raw[:n]); fault != nil {
			return jea9LinuxErrEFAULT
		}
	}
	if fault := cpu.mem.Store32(addrlenAddr, uint32(len(raw))); fault != nil {
		return jea9LinuxErrEFAULT
	}
	return 0
}

func socketFamilyMatchesAddr(family int32, addr *net.TCPAddr) bool {
	if addr == nil {
		return false
	}
	if family == jea9LinuxAFInet {
		return addr.IP.To4() != nil
	}
	if family == jea9LinuxAFInet6 {
		return addr.IP.To4() == nil
	}
	return false
}

func (jos *Jea9Linux) socketEnsurePending(fd int, f *jea9LinuxFD) bool {
	if len(f.socketPending) > 0 {
		return true
	}
	if f.tcpListener == nil {
		return false
	}
	_ = f.tcpListener.SetDeadline(time.Now())
	conn, err := f.tcpListener.AcceptTCP()
	_ = f.tcpListener.SetDeadline(time.Time{})
	if err != nil {
		return false
	}
	f.socketPending = append(f.socketPending, conn)
	jos.fds[fd] = *f
	return true
}

func (jos *Jea9Linux) socketPollReadable(fd int, f *jea9LinuxFD) bool {
	if f.socketEOF || len(f.socketReadBuf) > 0 {
		return true
	}
	if f.tcpConn == nil {
		return false
	}
	var b [1]byte
	_ = f.tcpConn.SetReadDeadline(time.Now())
	n, err := f.tcpConn.Read(b[:])
	_ = f.tcpConn.SetReadDeadline(time.Time{})
	if n > 0 {
		f.socketReadBuf = append(f.socketReadBuf, b[:n]...)
		jos.fds[fd] = *f
		return true
	}
	if errors.Is(err, io.EOF) {
		f.socketEOF = true
		jos.fds[fd] = *f
		return true
	}
	return false
}

func (jos *Jea9Linux) wakeAllSocketEpollWaiters(cpu *CPU) {
	for fd, f := range jos.fds {
		if f.kind == jea9LinuxFDSocket {
			jos.wakeEpollWaitersForFD(cpu, fd)
		}
	}
}

func socketWouldBlock(err error) bool {
	if err == nil {
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	return errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EWOULDBLOCK)
}

func cloneTCPAddr(addr *net.TCPAddr) *net.TCPAddr {
	if addr == nil {
		return nil
	}
	out := *addr
	out.IP = append(net.IP(nil), addr.IP...)
	return &out
}

func zoneForScopeID(scope int) string {
	if scope == 0 {
		return ""
	}
	if ifi, err := net.InterfaceByIndex(scope); err == nil {
		return ifi.Name
	}
	return ""
}

func minUint64(a, b uint64) uint64 {
	if a < b {
		return a
	}
	return b
}
