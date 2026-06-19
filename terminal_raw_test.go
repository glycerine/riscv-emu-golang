//go:build linux || darwin || freebsd || netbsd || openbsd

package riscv

import (
	"testing"

	"golang.org/x/sys/unix"
)

func TestRawTerminalStatePreservesOutputProcessing(t *testing.T) {
	var old unix.Termios
	old.Oflag = unix.OPOST

	raw := rawTerminalState(old)
	if raw.Oflag&unix.OPOST == 0 {
		t.Fatal("raw terminal state cleared OPOST; serial console display output needs host output processing preserved")
	}
}

func TestRawTerminalStateKeepsCtrlCAsInputByte(t *testing.T) {
	var old unix.Termios
	old.Lflag = unix.ECHO | unix.ICANON | unix.IEXTEN | unix.ISIG

	raw := rawTerminalState(old)
	if raw.Lflag&unix.ISIG != 0 {
		t.Fatal("raw terminal state kept ISIG; host terminal would intercept Ctrl-C instead of passing it to the guest")
	}
	if raw.Lflag&unix.ICANON != 0 {
		t.Fatal("raw terminal state kept ICANON; host terminal would line-buffer guest input")
	}
}
