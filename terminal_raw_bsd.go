//go:build darwin || freebsd || netbsd || openbsd

package riscv

import "golang.org/x/sys/unix"

const (
	termiosGetRequest = unix.TIOCGETA
	termiosSetRequest = unix.TIOCSETA
)
