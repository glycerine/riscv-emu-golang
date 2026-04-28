//go:build unix

package riscv

import "syscall"

func mmapExec(size int) ([]byte, error) {
	return syscall.Mmap(-1, 0, size,
		syscall.PROT_READ|syscall.PROT_WRITE|syscall.PROT_EXEC,
		syscall.MAP_PRIVATE|syscall.MAP_ANON)
}

func munmapExec(b []byte) error {
	return syscall.Munmap(b)
}
