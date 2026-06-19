package main

import (
	"fmt"
	"io"
	"os"
	"strings"
)

func terminalStatusText(format string, args ...any) string {
	msg := fmt.Sprintf(format, args...)
	msg = strings.ReplaceAll(msg, "\r\n", "\n")
	msg = strings.ReplaceAll(msg, "\r", "\n")
	msg = strings.TrimRight(msg, "\n")
	if msg == "" {
		return "\r\n"
	}
	return strings.ReplaceAll(msg, "\n", "\r\n") + "\r\n"
}

func writeTerminalStatusf(format string, args ...any) {
	_, _ = io.WriteString(os.Stderr, terminalStatusText(format, args...))
}
