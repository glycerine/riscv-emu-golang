//go:build !windows

package riscv

import (
	"os"
	"syscall"
)

func dupHostFile(file *os.File) (*os.File, error) {
	dup, err := syscall.Dup(int(file.Fd()))
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(dup), file.Name()), nil
}
