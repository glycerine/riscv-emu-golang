//go:build darwin || freebsd || netbsd || openbsd

package main

import "golang.org/x/sys/unix"

const (
	termiosGetRequest = unix.TIOCGETA
	termiosSetRequest = unix.TIOCSETA
)
