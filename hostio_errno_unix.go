//go:build !windows

package riscv

import (
	"errors"
	"syscall"
)

func hostIONormalizePath(path string) string {
	return path
}

func hostIOPlatformErrno(err error) (uint32, bool) {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return 0, false
	}
	switch errno {
	case syscall.EPERM:
		return hostIOErrEPERM, true
	case syscall.ENOENT:
		return hostIOErrENOENT, true
	case syscall.EIO:
		return hostIOErrEIO, true
	case syscall.EBADF:
		return hostIOErrEBADF, true
	case syscall.ENOMEM:
		return hostIOErrENOMEM, true
	case syscall.EACCES:
		return hostIOErrEACCES, true
	case syscall.EFAULT:
		return hostIOErrEFAULT, true
	case syscall.EEXIST:
		return hostIOErrEEXIST, true
	case syscall.ENOTDIR:
		return hostIOErrENOTDIR, true
	case syscall.EINVAL:
		return hostIOErrEINVAL, true
	case syscall.EFBIG:
		return hostIOErrEFBIG, true
	case syscall.ENOSPC:
		return hostIOErrENOSPC, true
	case syscall.ESPIPE:
		return hostIOErrESPIPE, true
	case syscall.EROFS:
		return hostIOErrEROFS, true
	case syscall.EPIPE:
		return hostIOErrEPIPE, true
	case syscall.ENAMETOOLONG:
		return hostIOErrENAMETOOLONG, true
	case syscall.ENOSYS:
		return hostIOErrENOSYS, true
	case syscall.ENOTEMPTY:
		return hostIOErrENOTEMPTY, true
	case syscall.ELOOP:
		return hostIOErrELOOP, true
	case syscall.EMFILE:
		return hostIOErrEMFILE, true
	case syscall.EXDEV:
		return hostIOErrEXDEV, true
	case syscall.ENOBUFS:
		return hostIOErrENOBUFS, true
	}
	return uint32(errno), true
}
