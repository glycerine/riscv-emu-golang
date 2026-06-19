//go:build linux || darwin || freebsd || netbsd || openbsd

package riscv

import (
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

func enableRawTerminal(stdin io.Reader) (func() error, bool, error) {
	file, ok := stdin.(*os.File)
	if !ok {
		return nil, false, nil
	}
	fd := int(file.Fd())
	oldState, err := unix.IoctlGetTermios(fd, termiosGetRequest)
	if err != nil {
		if errors.Is(err, unix.ENOTTY) || errors.Is(err, unix.EINVAL) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("enable raw terminal: %w", err)
	}
	raw := rawTerminalState(*oldState)
	if err := unix.IoctlSetTermios(fd, termiosSetRequest, &raw); err != nil {
		return nil, false, fmt.Errorf("enable raw terminal: %w", err)
	}
	restore := func() error {
		if err := unix.IoctlSetTermios(fd, termiosSetRequest, oldState); err != nil {
			return fmt.Errorf("restore terminal: %w", err)
		}
		return nil
	}
	return restore, true, nil
}

func rawTerminalState(old unix.Termios) unix.Termios {
	raw := old
	raw.Iflag &^= unix.BRKINT | unix.ICRNL | unix.INPCK | unix.ISTRIP | unix.IXON
	raw.Cflag &^= unix.CSIZE | unix.PARENB
	raw.Cflag |= unix.CS8
	raw.Lflag &^= unix.ECHO | unix.ICANON | unix.IEXTEN | unix.ISIG
	raw.Cc[unix.VMIN] = 1
	raw.Cc[unix.VTIME] = 0
	return raw
}
