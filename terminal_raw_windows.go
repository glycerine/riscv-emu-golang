//go:build windows
// +build windows

package riscv

import (
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/windows"
)

func enableRawTerminal(stdin io.Reader) (func() error, bool, error) {
	file, ok := stdin.(*os.File)
	if !ok {
		return nil, false, nil
	}
	fd := windows.Handle(file.Fd())
	var oldMode uint32
	if err := windows.GetConsoleMode(fd, &oldMode); err != nil {
		if errors.Is(err, windows.ERROR_INVALID_HANDLE) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("enable raw terminal: %w", err)
	}
	raw := rawConsoleInputMode(oldMode)
	if err := windows.SetConsoleMode(fd, raw); err != nil {
		return nil, false, fmt.Errorf("enable raw terminal: %w", err)
	}
	restore := func() error {
		if err := windows.SetConsoleMode(fd, oldMode); err != nil {
			return fmt.Errorf("restore terminal: %w", err)
		}
		return nil
	}
	return restore, true, nil
}

func rawConsoleInputMode(oldMode uint32) uint32 {
	raw := oldMode
	raw &^= windows.ENABLE_ECHO_INPUT |
		windows.ENABLE_LINE_INPUT |
		windows.ENABLE_PROCESSED_INPUT
	raw |= windows.ENABLE_VIRTUAL_TERMINAL_INPUT
	return raw
}
