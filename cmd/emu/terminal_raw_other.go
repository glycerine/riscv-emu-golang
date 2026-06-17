//go:build !(linux || darwin || freebsd || netbsd || openbsd)

package main

import "io"

func enableRawTerminal(stdin io.Reader) (func() error, bool, error) {
	return nil, false, nil
}
