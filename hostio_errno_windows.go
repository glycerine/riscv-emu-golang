//go:build windows

package riscv

import (
	"errors"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"
)

func hostIONormalizePath(path string) string {
	path = strings.ReplaceAll(path, `\`, `/`)
	if n := leadingSlashes(path); n > 0 && hasWindowsDrivePrefix(path[n:]) {
		path = path[n:]
	} else if n > 2 {
		path = "/" + strings.TrimLeft(path, "/")
	}
	if path == "//" {
		path = "/"
	}
	if path == "" {
		path = "."
	}
	return filepath.Clean(filepath.FromSlash(path))
}

func leadingSlashes(path string) int {
	n := 0
	for n < len(path) && path[n] == '/' {
		n++
	}
	return n
}

func hasWindowsDrivePrefix(path string) bool {
	if len(path) < 2 || path[1] != ':' {
		return false
	}
	c := path[0]
	return ('A' <= c && c <= 'Z') || ('a' <= c && c <= 'z')
}

func hostIOPlatformErrno(err error) (uint32, bool) {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return 0, false
	}
	switch errno {
	case windows.ERROR_FILE_NOT_FOUND,
		windows.ERROR_PATH_NOT_FOUND,
		windows.ERROR_BAD_NETPATH,
		windows.ERROR_BAD_NET_NAME:
		return hostIOErrENOENT, true
	case windows.ERROR_ACCESS_DENIED,
		windows.ERROR_SHARING_VIOLATION,
		windows.ERROR_LOCK_VIOLATION:
		return hostIOErrEACCES, true
	case windows.ERROR_ALREADY_EXISTS,
		windows.ERROR_FILE_EXISTS:
		return hostIOErrEEXIST, true
	case windows.ERROR_INVALID_NAME,
		windows.ERROR_BAD_PATHNAME,
		windows.ERROR_DIRECTORY,
		windows.ERROR_NEGATIVE_SEEK:
		return hostIOErrEINVAL, true
	case windows.ERROR_NOT_ENOUGH_MEMORY,
		windows.ERROR_OUTOFMEMORY:
		return hostIOErrENOMEM, true
	case windows.ERROR_INVALID_HANDLE:
		return hostIOErrEBADF, true
	case windows.ERROR_TOO_MANY_OPEN_FILES:
		return hostIOErrEMFILE, true
	case windows.ERROR_FILENAME_EXCED_RANGE:
		return hostIOErrENAMETOOLONG, true
	case windows.ERROR_DIR_NOT_EMPTY:
		return hostIOErrENOTEMPTY, true
	case windows.ERROR_DISK_FULL,
		windows.ERROR_HANDLE_DISK_FULL:
		return hostIOErrENOSPC, true
	case windows.ERROR_WRITE_PROTECT:
		return hostIOErrEROFS, true
	case windows.ERROR_BROKEN_PIPE:
		return hostIOErrEPIPE, true
	case windows.ERROR_NOT_SAME_DEVICE:
		return hostIOErrEXDEV, true
	}
	return hostIOErrEIO, true
}
