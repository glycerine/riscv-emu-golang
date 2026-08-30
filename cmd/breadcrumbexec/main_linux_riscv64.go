//go:build linux && riscv64

package main

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

const (
	breadcrumbBase                  = uintptr(0x10002000)
	breadcrumbSize                  = 0x1000
	breadcrumbRegControl            = uintptr(0x10)
	breadcrumbRegTargetPID          = uintptr(0x68)
	breadcrumbControlArmNextASReset = uint32(7)
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: breadcrumbexec program [args...]")
		os.Exit(2)
	}

	target, err := syscall.BytePtrFromString(os.Args[1])
	if err != nil {
		fmt.Fprintf(os.Stderr, "breadcrumbexec: target path: %v\n", err)
		os.Exit(2)
	}
	argv, err := syscall.SlicePtrFromStrings(os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "breadcrumbexec: argv: %v\n", err)
		os.Exit(2)
	}
	envv, err := syscall.SlicePtrFromStrings(os.Environ())
	if err != nil {
		fmt.Fprintf(os.Stderr, "breadcrumbexec: env: %v\n", err)
		os.Exit(2)
	}

	fd, err := syscall.Open("/dev/mem", syscall.O_RDWR|syscall.O_SYNC|syscall.O_CLOEXEC, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "breadcrumbexec: open /dev/mem: %v\n", err)
		os.Exit(1)
	}
	page, err := syscall.Mmap(fd, int64(breadcrumbBase), breadcrumbSize, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		fmt.Fprintf(os.Stderr, "breadcrumbexec: mmap breadcrumb MMIO: %v\n", err)
		os.Exit(1)
	}

	*(*uint32)(unsafe.Pointer(&page[breadcrumbRegTargetPID])) = uint32(os.Getpid())
	*(*uint32)(unsafe.Pointer(&page[breadcrumbRegControl])) = breadcrumbControlArmNextASReset

	_, _, errno := syscall.RawSyscall(
		syscall.SYS_EXECVE,
		uintptr(unsafe.Pointer(target)),
		uintptr(unsafe.Pointer(&argv[0])),
		uintptr(unsafe.Pointer(&envv[0])),
	)
	fmt.Fprintf(os.Stderr, "breadcrumbexec: execve %s: %v\n", os.Args[1], errno)
	os.Exit(1)
}
