//go:build linux

package main

import "golang.org/x/sys/unix"

const (
	termiosGetRequest = unix.TCGETS
	termiosSetRequest = unix.TCSETS
)
