//go:build windows
// +build windows

package riscv

import (
	"testing"

	"golang.org/x/sys/windows"
)

func TestRawConsoleInputModeKeepsCtrlCAsInputByte(t *testing.T) {
	old := uint32(windows.ENABLE_ECHO_INPUT |
		windows.ENABLE_LINE_INPUT |
		windows.ENABLE_PROCESSED_INPUT |
		windows.ENABLE_WINDOW_INPUT)
	raw := rawConsoleInputMode(old)
	if raw&windows.ENABLE_PROCESSED_INPUT != 0 {
		t.Fatal("raw console input mode kept ENABLE_PROCESSED_INPUT; Windows would intercept Ctrl-C instead of passing it to the guest")
	}
	if raw&windows.ENABLE_LINE_INPUT != 0 {
		t.Fatal("raw console input mode kept ENABLE_LINE_INPUT; Windows would line-buffer guest input")
	}
	if raw&windows.ENABLE_ECHO_INPUT != 0 {
		t.Fatal("raw console input mode kept ENABLE_ECHO_INPUT; Windows would echo guest input")
	}
	if raw&windows.ENABLE_WINDOW_INPUT == 0 {
		t.Fatal("raw console input mode dropped unrelated console input mode bits")
	}
}
