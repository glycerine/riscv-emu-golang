//go:build linux

package riscv

import "golang.org/x/sys/unix"

const (
	termiosGetRequest = unix.TCGETS
	termiosSetRequest = unix.TCSETS
)
