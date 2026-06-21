//go:build !windows

package riscv

import (
	"net"
	"syscall"
)

func socketPlatformWouldBlock(err error) bool {
	return false
}

func socketNonblockingRead(conn *net.TCPConn, buf []byte) (int, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0, err
	}
	var n int
	var opErr error
	if err := raw.Control(func(fd uintptr) {
		n, opErr = syscall.Read(int(fd), buf)
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
		n, opErr = syscall.Write(int(fd), buf)
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
		n, _, opErr = syscall.Recvfrom(int(fd), b[:], syscall.MSG_PEEK)
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
