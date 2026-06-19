//go:build !(linux || darwin || freebsd || netbsd || openbsd)

package riscv

import "io"

func enableRawTerminal(stdin io.Reader) (func() error, bool, error) {
	return nil, false, nil
}
