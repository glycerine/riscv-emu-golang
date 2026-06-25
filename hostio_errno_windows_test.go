//go:build windows

package riscv

import (
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestHostIOWindowsErrnoMapping(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want uint32
	}{
		{name: "invalid-name", err: windows.ERROR_INVALID_NAME, want: hostIOErrEINVAL},
		{name: "path-not-found", err: windows.ERROR_PATH_NOT_FOUND, want: hostIOErrENOENT},
		{name: "access-denied", err: windows.ERROR_ACCESS_DENIED, want: hostIOErrEACCES},
		{name: "disk-full", err: windows.ERROR_DISK_FULL, want: hostIOErrENOSPC},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hostIOErrno(tt.err); got != tt.want {
				t.Fatalf("hostIOErrno(%v) = %d, want %d", tt.err, got, tt.want)
			}
		})
	}
}

func TestHostIOWindowsNormalizePath(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{in: `/C:/Users/gsg`, want: `C:/Users/gsg`},
		{in: `//C:/Users/gsg`, want: `C:/Users/gsg`},
		{in: `///C:/Users/gsg`, want: `C:/Users/gsg`},
		{in: `//`, want: `/`},
	}
	for _, tt := range tests {
		if got := filepath.ToSlash(hostIONormalizePath(tt.in)); got != tt.want {
			t.Fatalf("hostIONormalizePath(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
