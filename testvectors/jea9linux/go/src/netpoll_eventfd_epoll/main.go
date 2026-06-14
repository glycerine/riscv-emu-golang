package main

import (
	"encoding/binary"
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

const (
	sysEventfd2     = 19
	sysEpollCreate1 = 20
	sysEpollCtl     = 21
	sysEpollPwait   = 22

	epollCtlAdd = 1
	epollIn     = 1
)

func main() {
	efd, _, errno := syscall.RawSyscall(sysEventfd2, 1, 0, 0)
	if errno != 0 || efd < 3 {
		os.Exit(1)
	}
	epfd, _, errno := syscall.RawSyscall(sysEpollCreate1, 0, 0, 0)
	if errno != 0 || epfd < 3 {
		os.Exit(2)
	}

	var ev [12]byte
	binary.LittleEndian.PutUint32(ev[0:4], epollIn)
	binary.LittleEndian.PutUint64(ev[4:12], 0x12345678)
	_, _, errno = syscall.RawSyscall6(
		sysEpollCtl,
		epfd,
		epollCtlAdd,
		efd,
		uintptr(unsafe.Pointer(&ev[0])),
		0,
		0,
	)
	if errno != 0 {
		os.Exit(3)
	}

	var out [12]byte
	n, _, errno := syscall.RawSyscall6(
		sysEpollPwait,
		epfd,
		uintptr(unsafe.Pointer(&out[0])),
		1,
		0,
		0,
		0,
	)
	if errno != 0 || n != 1 {
		os.Exit(4)
	}
	if binary.LittleEndian.Uint32(out[0:4]) != epollIn ||
		binary.LittleEndian.Uint64(out[4:12]) != 0x12345678 {
		os.Exit(5)
	}
	fmt.Print("eventfd_epoll_ready\n")
}
