//go:build windows
// +build windows

package riscv

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"golang.org/x/sys/windows"
)

var procSetConsoleCtrlHandler = windows.NewLazySystemDLL("kernel32.dll").NewProc("SetConsoleCtrlHandler")

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
	restore := &windowsTerminalRestore{fd: fd, oldMode: oldMode}
	restore.callback = windows.NewCallback(func(ctrlType uint32) uintptr {
		if windowsConsoleCtrlShouldRestore(ctrlType) {
			_ = restore.restoreMode()
		}
		return 0
	})
	if err := setConsoleCtrlHandler(restore.callback, true); err != nil {
		_ = restore.restoreMode()
		return nil, false, fmt.Errorf("enable raw terminal ctrl handler: %w", err)
	}
	return restore.restore, true, nil
}

func rawConsoleInputMode(oldMode uint32) uint32 {
	raw := oldMode
	raw &^= windows.ENABLE_ECHO_INPUT |
		windows.ENABLE_LINE_INPUT |
		windows.ENABLE_PROCESSED_INPUT
	raw |= windows.ENABLE_VIRTUAL_TERMINAL_INPUT
	return raw
}

type windowsTerminalRestore struct {
	fd       windows.Handle
	oldMode  uint32
	callback uintptr

	modeOnce       sync.Once
	modeErr        error
	unregisterOnce sync.Once
	unregisterErr  error
}

func (r *windowsTerminalRestore) restore() error {
	modeErr := r.restoreMode()
	unregisterErr := r.unregister()
	if modeErr != nil {
		return modeErr
	}
	return unregisterErr
}

func (r *windowsTerminalRestore) restoreMode() error {
	r.modeOnce.Do(func() {
		if err := windows.SetConsoleMode(r.fd, r.oldMode); err != nil {
			r.modeErr = fmt.Errorf("restore terminal: %w", err)
		}
	})
	return r.modeErr
}

func (r *windowsTerminalRestore) unregister() error {
	r.unregisterOnce.Do(func() {
		if r.callback == 0 {
			return
		}
		if err := setConsoleCtrlHandler(r.callback, false); err != nil {
			r.unregisterErr = fmt.Errorf("restore terminal ctrl handler: %w", err)
		}
	})
	return r.unregisterErr
}

func windowsConsoleCtrlShouldRestore(ctrlType uint32) bool {
	switch ctrlType {
	case windows.CTRL_C_EVENT,
		windows.CTRL_BREAK_EVENT,
		windows.CTRL_CLOSE_EVENT,
		windows.CTRL_LOGOFF_EVENT,
		windows.CTRL_SHUTDOWN_EVENT:
		return true
	default:
		return false
	}
}

func setConsoleCtrlHandler(handler uintptr, add bool) error {
	var addArg uintptr
	if add {
		addArg = 1
	}
	r1, _, err := procSetConsoleCtrlHandler.Call(handler, addArg)
	if r1 == 0 {
		return err
	}
	return nil
}
