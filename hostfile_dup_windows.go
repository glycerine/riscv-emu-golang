//go:build windows

package riscv

import (
	"os"

	"golang.org/x/sys/windows"
)

func dupHostFile(file *os.File) (*os.File, error) {
	var dup windows.Handle
	err := windows.DuplicateHandle(
		windows.CurrentProcess(),
		windows.Handle(file.Fd()),
		windows.CurrentProcess(),
		&dup,
		0,
		false,
		windows.DUPLICATE_SAME_ACCESS,
	)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(dup), file.Name()), nil
}
