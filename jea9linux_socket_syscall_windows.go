//go:build windows

package riscv

import (
	"errors"
	"fmt"
	"net"

	"golang.org/x/sys/windows"
)

func socketPlatformWouldBlock(err error) bool {
	return errors.Is(err, windows.WSAEWOULDBLOCK)
}

func socketNonblockingRead(conn *net.TCPConn, buf []byte) (int, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0, err
	}
	var n int
	var opErr error
	if err := raw.Control(func(fd uintptr) {
		n, opErr = socketWSARecv(windows.Handle(fd), buf, 0)
	}); err != nil {
		return n, err
	}
	return n, opErr
}

func socketNonblockingWrite(conn *net.TCPConn, buf []byte) (int, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0, err
	}
	var n int
	var opErr error
	if err := raw.Control(func(fd uintptr) {
		n, opErr = socketWSASend(windows.Handle(fd), buf)
	}); err != nil {
		return n, err
	}
	return n, opErr
}

func socketPeekReadable(conn *net.TCPConn) (bool, bool, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return false, false, err
	}
	var n int
	var opErr error
	var b [1]byte
	if err := raw.Control(func(fd uintptr) {
		n, opErr = socketWSARecv(windows.Handle(fd), b[:], windows.MSG_PEEK)
	}); err != nil {
		return false, false, err
	}
	if opErr != nil {
		if socketWouldBlock(opErr) {
			return false, false, nil
		}
		return false, false, opErr
	}
	if n == 0 {
		return true, true, nil
	}
	return true, false, nil
}

func socketWSARecv(fd windows.Handle, buf []byte, flags uint32) (int, error) {
	if len(buf) == 0 {
		return 0, nil
	}
	if len(buf) > int(^uint32(0)) {
		return 0, fmt.Errorf("socket read too large: %d", len(buf))
	}
	wsabuf := windows.WSABuf{Len: uint32(len(buf)), Buf: &buf[0]}
	var recvd uint32
	err := windows.WSARecv(fd, &wsabuf, 1, &recvd, &flags, nil, nil)
	if err != nil {
		return int(recvd), err
	}
	return int(recvd), nil
}

func socketWSASend(fd windows.Handle, buf []byte) (int, error) {
	if len(buf) == 0 {
		return 0, nil
	}
	if len(buf) > int(^uint32(0)) {
		return 0, fmt.Errorf("socket write too large: %d", len(buf))
	}
	wsabuf := windows.WSABuf{Len: uint32(len(buf)), Buf: &buf[0]}
	var sent uint32
	err := windows.WSASend(fd, &wsabuf, 1, &sent, 0, nil, nil)
	if err != nil {
		return int(sent), err
	}
	return int(sent), nil
}
