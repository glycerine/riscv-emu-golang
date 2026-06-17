package riscv

import (
	"encoding/binary"
	"hash/fnv"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

type jea9LinuxStat struct {
	dev     uint64
	ino     uint64
	mode    uint32
	nlink   uint32
	uid     uint32
	gid     uint32
	rdev    uint64
	size    int64
	blksize int32
	blocks  int64
	atimeNS int64
	mtimeNS int64
	ctimeNS int64
	dtype   uint8
}

func normalizeJea9LinuxGuestPath(path string) string {
	if path == "" {
		return "/"
	}
	path = filepath.ToSlash(path)
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	path = filepath.ToSlash(filepath.Clean(path))
	if path == "." {
		return "/"
	}
	return path
}

func defaultJea9LinuxCwd(opts Jea9LinuxOptions) string {
	if opts.Cwd != "" {
		return normalizeJea9LinuxGuestPath(opts.Cwd)
	}
	if opts.AllowAllHostFiles {
		if cwd, err := os.Getwd(); err == nil && cwd != "" {
			return normalizeJea9LinuxGuestPath(cwd)
		}
	}
	return "/"
}

func joinJea9LinuxGuestPath(base, name string) string {
	if name == "" {
		return normalizeJea9LinuxGuestPath(base)
	}
	if filepath.IsAbs(name) {
		return normalizeJea9LinuxGuestPath(name)
	}
	return normalizeJea9LinuxGuestPath(filepath.ToSlash(filepath.Join(base, name)))
}

func (jos *Jea9Linux) sysGetcwd(cpu *CPU, bufAddr, size uint64) int64 {
	cwd := normalizeJea9LinuxGuestPath(jos.cwd)
	data := []byte(cwd + "\x00")
	if size == 0 || uint64(len(data)) > size {
		return jea9LinuxErrERANGE
	}
	if f := cpu.mem.WriteBytes(bufAddr, data); f != nil {
		return jea9LinuxErrEFAULT
	}
	return int64(len(data))
}

func (jos *Jea9Linux) sysChdir(cpu *CPU, pathAddr uint64) int64 {
	path, errno := jos.readLinuxAtPath(cpu, jea9LinuxATFDCWD, pathAddr)
	if errno != 0 {
		return errno
	}
	if path == "" {
		return jea9LinuxErrENOENT
	}
	st, errno := jos.statPath(path, true)
	if errno != 0 {
		return errno
	}
	if st.mode&jea9LinuxModeIFMT != jea9LinuxModeIFDIR {
		return jea9LinuxErrENOTDIR
	}
	jos.cwd = normalizeJea9LinuxGuestPath(path)
	return 0
}

func (jos *Jea9Linux) sysFstat(cpu *CPU, fdRaw, statAddr uint64) int64 {
	st, errno := jos.statFD(fdRaw)
	if errno != 0 {
		return errno
	}
	return storeJea9LinuxStat(cpu, statAddr, st)
}

func (jos *Jea9Linux) sysNewfstatat(cpu *CPU, dirfd, pathAddr, statAddr, flags uint64) int64 {
	if flags&^(jea9LinuxATSymlinkNofollow|jea9LinuxATEmptyPath) != 0 {
		return jea9LinuxErrEINVAL
	}
	path, errno := readLinuxCString(cpu, pathAddr, 4096)
	if errno != 0 {
		return errno
	}
	var st jea9LinuxStat
	if path == "" && flags&jea9LinuxATEmptyPath != 0 {
		st, errno = jos.statFD(dirfd)
	} else if path == "" {
		return jea9LinuxErrENOENT
	} else {
		path, errno = jos.resolveLinuxAtPath(dirfd, path)
		if errno == 0 {
			st, errno = jos.statPath(path, flags&jea9LinuxATSymlinkNofollow == 0)
		}
	}
	if errno != 0 {
		return errno
	}
	return storeJea9LinuxStat(cpu, statAddr, st)
}

func (jos *Jea9Linux) sysStatx(cpu *CPU, dirfd, pathAddr, flags, mask, statAddr uint64) int64 {
	if flags&^(jea9LinuxATSymlinkNofollow|jea9LinuxATEmptyPath) != 0 {
		return jea9LinuxErrEINVAL
	}
	path, errno := readLinuxCString(cpu, pathAddr, 4096)
	if errno != 0 {
		return errno
	}
	var st jea9LinuxStat
	if path == "" && flags&jea9LinuxATEmptyPath != 0 {
		st, errno = jos.statFD(dirfd)
	} else if path == "" {
		return jea9LinuxErrENOENT
	} else {
		path, errno = jos.resolveLinuxAtPath(dirfd, path)
		if errno == 0 {
			st, errno = jos.statPath(path, flags&jea9LinuxATSymlinkNofollow == 0)
		}
	}
	if errno != 0 {
		return errno
	}
	_ = mask
	return storeJea9LinuxStatx(cpu, statAddr, st)
}

func (jos *Jea9Linux) sysReadlinkat(cpu *CPU, dirfd, pathAddr, bufAddr, size uint64) int64 {
	path, errno := jos.readLinuxAtPath(cpu, dirfd, pathAddr)
	if errno != 0 {
		return errno
	}
	if path == "" {
		return jea9LinuxErrENOENT
	}
	target := ""
	switch {
	case path == "/proc/self/exe" || path == "/proc/"+uint64String(jos.pid)+"/exe":
		target = jos.execPath
		if target == "" {
			target = "/proc/self/exe"
		}
	case jos.allowAllHostFiles:
		got, err := os.Readlink(path)
		if err != nil {
			return jea9LinuxErrnoFromHost(err)
		}
		target = got
	default:
		return jea9LinuxErrEINVAL
	}
	if size == 0 {
		return 0
	}
	data := []byte(target)
	if uint64(len(data)) > size {
		data = data[:size]
	}
	if f := cpu.mem.WriteBytes(bufAddr, data); f != nil {
		return jea9LinuxErrEFAULT
	}
	return int64(len(data))
}

func (jos *Jea9Linux) sysGetdents64(cpu *CPU, fdRaw, bufAddr, n uint64) int64 {
	fd := int(int64(fdRaw))
	f, ok := jos.fds[fd]
	if !ok {
		return jea9LinuxErrEBADF
	}
	if n > uint64(int(^uint(0)>>1)) {
		return jea9LinuxErrEINVAL
	}
	if f.kind == jea9LinuxFDHostFile {
		if f.hostFile == nil {
			return jea9LinuxErrEBADF
		}
		info, err := f.hostFile.Stat()
		if err != nil {
			return jea9LinuxErrnoFromHost(err)
		}
		if !info.IsDir() {
			return jea9LinuxErrENOTDIR
		}
		if f.dirents == nil {
			names, err := f.hostFile.Readdirnames(-1)
			if err != nil && !errorsIsEOF(err) {
				return jea9LinuxErrnoFromHost(err)
			}
			f.dirPath = f.hostFile.Name()
			f.dirents = append([]string{".", ".."}, names...)
			f.direntOff = 0
		}
	} else if f.kind == jea9LinuxFDDir {
		if f.dirents == nil {
			f.dirents = jos.virtualDirEntries(f.dirPath)
			f.direntOff = 0
		}
	} else {
		return jea9LinuxErrENOTDIR
	}
	out := make([]byte, 0, int(n))
	for f.direntOff < len(f.dirents) {
		name := f.dirents[f.direntOff]
		reclen := alignJea9LinuxDirentLen(19 + len(name) + 1)
		if reclen > int(n) {
			return jea9LinuxErrEINVAL
		}
		if len(out)+reclen > int(n) {
			break
		}
		entry := make([]byte, reclen)
		childPath := joinJea9LinuxGuestPath(f.dirPath, name)
		if f.kind == jea9LinuxFDHostFile {
			childPath = filepath.Join(f.dirPath, name)
		}
		st, _ := jos.statPath(childPath, false)
		binary.LittleEndian.PutUint64(entry[0:], st.ino)
		binary.LittleEndian.PutUint64(entry[8:], uint64(f.direntOff+1))
		binary.LittleEndian.PutUint16(entry[16:], uint16(reclen))
		entry[18] = st.dtype
		copy(entry[19:], name)
		out = append(out, entry...)
		f.direntOff++
	}
	jos.fds[fd] = f
	if len(out) == 0 {
		return 0
	}
	if fault := cpu.mem.WriteBytes(bufAddr, out); fault != nil {
		return jea9LinuxErrEFAULT
	}
	return int64(len(out))
}

func (jos *Jea9Linux) sysUnlinkat(cpu *CPU, dirfd, pathAddr, flags uint64) int64 {
	if flags&^jea9LinuxATRemovedir != 0 {
		return jea9LinuxErrEINVAL
	}
	path, errno := jos.readLinuxAtPath(cpu, dirfd, pathAddr)
	if errno != 0 {
		return errno
	}
	if path == "" {
		return jea9LinuxErrENOENT
	}
	if !jos.allowAllHostFiles {
		return jea9LinuxErrENOENT
	}
	info, err := os.Lstat(path)
	if err != nil {
		return jea9LinuxErrnoFromHost(err)
	}
	isDir := info.IsDir()
	if flags&jea9LinuxATRemovedir == 0 && isDir {
		return jea9LinuxErrEISDIR
	}
	if flags&jea9LinuxATRemovedir != 0 && !isDir {
		return jea9LinuxErrENOTDIR
	}
	if err := os.Remove(path); err != nil {
		return jea9LinuxErrnoFromHost(err)
	}
	return 0
}

func (jos *Jea9Linux) sysRenameat2(cpu *CPU, oldDirfd, oldPathAddr, newDirfd, newPathAddr, flags uint64) int64 {
	if flags != 0 {
		return jea9LinuxErrEINVAL
	}
	oldPath, errno := jos.readLinuxAtPath(cpu, oldDirfd, oldPathAddr)
	if errno != 0 {
		return errno
	}
	if oldPath == "" {
		return jea9LinuxErrENOENT
	}
	newPath, errno := jos.readLinuxAtPath(cpu, newDirfd, newPathAddr)
	if errno != 0 {
		return errno
	}
	if newPath == "" {
		return jea9LinuxErrENOENT
	}
	if !jos.allowAllHostFiles {
		return jea9LinuxErrENOENT
	}
	if err := os.Rename(oldPath, newPath); err != nil {
		return jea9LinuxErrnoFromHost(err)
	}
	return 0
}

func (jos *Jea9Linux) sysFaccessat(cpu *CPU, dirfd, pathAddr, mode, flags uint64) int64 {
	if mode&^uint64(7) != 0 || flags&^(jea9LinuxATSymlinkNofollow|jea9LinuxATEaccess|jea9LinuxATEmptyPath) != 0 {
		return jea9LinuxErrEINVAL
	}
	path, errno := readLinuxCString(cpu, pathAddr, 4096)
	if errno != 0 {
		return errno
	}
	var st jea9LinuxStat
	if path == "" && flags&jea9LinuxATEmptyPath != 0 {
		st, errno = jos.statFD(dirfd)
	} else if path == "" {
		return jea9LinuxErrENOENT
	} else {
		path, errno = jos.resolveLinuxAtPath(dirfd, path)
		if errno == 0 {
			st, errno = jos.statPath(path, flags&jea9LinuxATSymlinkNofollow == 0)
		}
	}
	if errno != 0 {
		return errno
	}
	if mode == 0 {
		return 0
	}
	perm := st.mode & 0o777
	if mode&4 != 0 && perm&0o444 == 0 {
		return jea9LinuxErrEACCES
	}
	if mode&2 != 0 && perm&0o222 == 0 {
		return jea9LinuxErrEACCES
	}
	if mode&1 != 0 && perm&0o111 == 0 {
		return jea9LinuxErrEACCES
	}
	return 0
}

func (jos *Jea9Linux) sysFtruncate(fdRaw, lengthRaw uint64) int64 {
	if int64(lengthRaw) < 0 {
		return jea9LinuxErrEINVAL
	}
	fd := int(int64(fdRaw))
	f, ok := jos.fds[fd]
	if !ok {
		return jea9LinuxErrEBADF
	}
	if f.kind != jea9LinuxFDHostFile || f.hostFile == nil {
		return jea9LinuxErrEINVAL
	}
	if err := f.hostFile.Truncate(int64(lengthRaw)); err != nil {
		return jea9LinuxErrnoFromHost(err)
	}
	return 0
}

func (jos *Jea9Linux) sysFsync(fdRaw uint64) int64 {
	fd := int(int64(fdRaw))
	f, ok := jos.fds[fd]
	if !ok {
		return jea9LinuxErrEBADF
	}
	if f.kind == jea9LinuxFDHostFile && f.hostFile != nil {
		if err := f.hostFile.Sync(); err != nil {
			return jea9LinuxErrnoFromHost(err)
		}
	}
	return 0
}

func (jos *Jea9Linux) sysStatfs(cpu *CPU, pathAddr, bufAddr uint64) int64 {
	path, errno := jos.readLinuxAtPath(cpu, jea9LinuxATFDCWD, pathAddr)
	if errno != 0 {
		return errno
	}
	if path == "" {
		return jea9LinuxErrENOENT
	}
	if _, errno := jos.statPath(path, true); errno != 0 {
		return errno
	}
	return storeJea9LinuxStatfs(cpu, bufAddr)
}

func (jos *Jea9Linux) sysFstatfs(cpu *CPU, fdRaw, bufAddr uint64) int64 {
	if _, errno := jos.statFD(fdRaw); errno != 0 {
		return errno
	}
	return storeJea9LinuxStatfs(cpu, bufAddr)
}

func (jos *Jea9Linux) sysDup3(oldfdRaw, newfdRaw, flags uint64) int64 {
	if flags&^jea9LinuxFDCloexec != 0 {
		return jea9LinuxErrEINVAL
	}
	oldfd := int(int64(oldfdRaw))
	newfd := int(int64(newfdRaw))
	if oldfd < 0 || newfd < 0 {
		return jea9LinuxErrEBADF
	}
	if oldfd == newfd {
		return jea9LinuxErrEINVAL
	}
	f, ok := jos.fds[oldfd]
	if !ok {
		return jea9LinuxErrEBADF
	}
	if old, ok := jos.fds[newfd]; ok {
		if errno := closeJea9LinuxFD(old); errno != 0 {
			return errno
		}
		delete(jos.fds, newfd)
	}
	if f.kind == jea9LinuxFDHostFile && f.hostFile != nil {
		dup, err := syscall.Dup(int(f.hostFile.Fd()))
		if err != nil {
			return jea9LinuxErrnoFromHost(err)
		}
		f.hostFile = os.NewFile(uintptr(dup), f.hostFile.Name())
	}
	f.flags &^= jea9LinuxFDCloexec
	f.flags |= flags & jea9LinuxFDCloexec
	jos.fds[newfd] = f
	if newfd >= jos.nextFD {
		jos.nextFD = newfd + 1
	}
	return int64(newfd)
}

func (jos *Jea9Linux) sysPwrite64(cpu *CPU, fdRaw, bufAddr, n, offRaw uint64) int64 {
	fd := int(int64(fdRaw))
	f, ok := jos.fds[fd]
	if !ok {
		return jea9LinuxErrEBADF
	}
	if n == 0 {
		return 0
	}
	if n > uint64(int(^uint(0)>>1)) {
		return jea9LinuxErrEINVAL
	}
	off := int64(offRaw)
	if off < 0 {
		return jea9LinuxErrEINVAL
	}
	if f.kind != jea9LinuxFDHostFile || f.hostFile == nil {
		return jea9LinuxErrESPIPE
	}
	buf := make([]byte, int(n))
	if fault := cpu.mem.ReadBytes(bufAddr, buf); fault != nil {
		return jea9LinuxErrEFAULT
	}
	written, err := f.hostFile.WriteAt(buf, off)
	if written < 0 {
		return jea9LinuxErrEIO
	}
	if written > len(buf) {
		written = len(buf)
	}
	if written == 0 && err != nil {
		return jea9LinuxErrnoFromHost(err)
	}
	return int64(written)
}

func (jos *Jea9Linux) sysReadv(cpu *CPU, fdRaw, iovAddr, iovcnt uint64) int64 {
	iovs, errno := loadJea9LinuxIovecs(cpu, iovAddr, iovcnt)
	if errno != 0 {
		return errno
	}
	var total int64
	for _, iov := range iovs {
		if iov.length == 0 {
			continue
		}
		n := jos.sysRead(cpu, fdRaw, iov.base, iov.length)
		if n < 0 {
			if total > 0 {
				return total
			}
			return n
		}
		total += n
		if uint64(n) < iov.length {
			break
		}
	}
	return total
}

func (jos *Jea9Linux) sysWritev(cpu *CPU, fdRaw, iovAddr, iovcnt uint64) int64 {
	iovs, errno := loadJea9LinuxIovecs(cpu, iovAddr, iovcnt)
	if errno != 0 {
		return errno
	}
	var total int64
	for _, iov := range iovs {
		if iov.length == 0 {
			continue
		}
		n := jos.sysWrite(cpu, fdRaw, iov.base, iov.length)
		if n < 0 {
			if total > 0 {
				return total
			}
			return n
		}
		total += n
		if uint64(n) < iov.length {
			break
		}
	}
	return total
}

type jea9LinuxIovec struct {
	base   uint64
	length uint64
}

func loadJea9LinuxIovecs(cpu *CPU, addr, count uint64) ([]jea9LinuxIovec, int64) {
	if count > 1024 {
		return nil, jea9LinuxErrEINVAL
	}
	iovs := make([]jea9LinuxIovec, count)
	for i := uint64(0); i < count; i++ {
		base, f := cpu.mem.Load64(addr + i*16)
		if f != nil {
			return nil, jea9LinuxErrEFAULT
		}
		length, f := cpu.mem.Load64(addr + i*16 + 8)
		if f != nil {
			return nil, jea9LinuxErrEFAULT
		}
		if length > uint64(int(^uint(0)>>1)) {
			return nil, jea9LinuxErrEINVAL
		}
		iovs[i] = jea9LinuxIovec{base: base, length: length}
	}
	return iovs, 0
}

func (jos *Jea9Linux) statFD(fdRaw uint64) (jea9LinuxStat, int64) {
	fd := int(int64(fdRaw))
	f, ok := jos.fds[fd]
	if !ok {
		return jea9LinuxStat{}, jea9LinuxErrEBADF
	}
	now := jos.fsNowNS()
	switch f.kind {
	case jea9LinuxFDStdin, jea9LinuxFDStdout, jea9LinuxFDStderr, jea9LinuxFDRandom:
		return jea9LinuxStat{
			dev:     1,
			ino:     uint64(100 + fd),
			mode:    jea9LinuxModeIFCHR | 0o666,
			nlink:   1,
			blksize: 4096,
			atimeNS: now,
			mtimeNS: now,
			ctimeNS: now,
			dtype:   jea9LinuxDirentCHR,
		}, 0
	case jea9LinuxFDFile:
		return regularJea9LinuxStat(f.dirPath, int64(len(f.data)), 0o444, now), 0
	case jea9LinuxFDDir:
		return dirJea9LinuxStat(f.dirPath, now), 0
	case jea9LinuxFDPipeRead, jea9LinuxFDPipeWrite:
		return jea9LinuxStat{
			dev:     1,
			ino:     uint64(200 + fd),
			mode:    jea9LinuxModeIFIFO | 0o600,
			nlink:   1,
			blksize: 4096,
			atimeNS: now,
			mtimeNS: now,
			ctimeNS: now,
			dtype:   jea9LinuxDirentFIFO,
		}, 0
	case jea9LinuxFDHostFile:
		if f.hostFile == nil {
			return jea9LinuxStat{}, jea9LinuxErrEBADF
		}
		info, err := f.hostFile.Stat()
		if err != nil {
			return jea9LinuxStat{}, jea9LinuxErrnoFromHost(err)
		}
		return statFromFileInfo(f.hostFile.Name(), info), 0
	default:
		return regularJea9LinuxStat("anon", 0, 0o600, now), 0
	}
}

func (jos *Jea9Linux) statPath(path string, follow bool) (jea9LinuxStat, int64) {
	path = normalizeJea9LinuxGuestPath(path)
	now := jos.fsNowNS()
	if path == "/dev/random" || path == "/dev/urandom" {
		return jea9LinuxStat{
			dev:     1,
			ino:     inodeForJea9LinuxPath(path),
			mode:    jea9LinuxModeIFCHR | 0o666,
			nlink:   1,
			blksize: 4096,
			atimeNS: now,
			mtimeNS: now,
			ctimeNS: now,
			dtype:   jea9LinuxDirentCHR,
		}, 0
	}
	if path == "/proc/self/exe" || path == "/proc/"+uint64String(jos.pid)+"/exe" {
		if !follow {
			return symlinkJea9LinuxStat(path, now), 0
		}
		target := jos.execPath
		if target == "" {
			target = path
		}
		return regularJea9LinuxStat(target, 0, 0o555, now), 0
	}
	if data, ok := jos.files[path]; ok {
		return regularJea9LinuxStat(path, int64(len(data)), 0o444, now), 0
	}
	if jos.virtualDirExists(path) {
		return dirJea9LinuxStat(path, now), 0
	}
	if jos.allowAllHostFiles {
		var (
			info os.FileInfo
			err  error
		)
		if follow {
			info, err = os.Stat(path)
		} else {
			info, err = os.Lstat(path)
		}
		if err != nil {
			return jea9LinuxStat{}, jea9LinuxErrnoFromHost(err)
		}
		return statFromFileInfo(path, info), 0
	}
	return jea9LinuxStat{}, jea9LinuxErrENOENT
}

func regularJea9LinuxStat(path string, size int64, perm uint32, now int64) jea9LinuxStat {
	return jea9LinuxStat{
		dev:     1,
		ino:     inodeForJea9LinuxPath(path),
		mode:    jea9LinuxModeIFREG | perm,
		nlink:   1,
		size:    size,
		blksize: 4096,
		blocks:  (size + 511) / 512,
		atimeNS: now,
		mtimeNS: now,
		ctimeNS: now,
		dtype:   jea9LinuxDirentREG,
	}
}

func dirJea9LinuxStat(path string, now int64) jea9LinuxStat {
	return jea9LinuxStat{
		dev:     1,
		ino:     inodeForJea9LinuxPath(path),
		mode:    jea9LinuxModeIFDIR | 0o755,
		nlink:   2,
		blksize: 4096,
		atimeNS: now,
		mtimeNS: now,
		ctimeNS: now,
		dtype:   jea9LinuxDirentDIR,
	}
}

func symlinkJea9LinuxStat(path string, now int64) jea9LinuxStat {
	return jea9LinuxStat{
		dev:     1,
		ino:     inodeForJea9LinuxPath(path),
		mode:    jea9LinuxModeIFLNK | 0o777,
		nlink:   1,
		size:    int64(len(path)),
		blksize: 4096,
		blocks:  1,
		atimeNS: now,
		mtimeNS: now,
		ctimeNS: now,
		dtype:   jea9LinuxDirentLNK,
	}
}

func statFromFileInfo(path string, info os.FileInfo) jea9LinuxStat {
	mtime := info.ModTime().UnixNano()
	size := info.Size()
	mode := uint32(info.Mode().Perm())
	dtype := jea9LinuxDirentREG
	switch {
	case info.Mode().IsDir():
		mode |= jea9LinuxModeIFDIR
		dtype = jea9LinuxDirentDIR
	case info.Mode()&os.ModeSymlink != 0:
		mode |= jea9LinuxModeIFLNK
		dtype = jea9LinuxDirentLNK
	case info.Mode()&os.ModeCharDevice != 0:
		mode |= jea9LinuxModeIFCHR
		dtype = jea9LinuxDirentCHR
	case info.Mode()&os.ModeNamedPipe != 0:
		mode |= jea9LinuxModeIFIFO
		dtype = jea9LinuxDirentFIFO
	default:
		mode |= jea9LinuxModeIFREG
	}
	return jea9LinuxStat{
		dev:     1,
		ino:     inodeForJea9LinuxPath(path),
		mode:    mode,
		nlink:   1,
		size:    size,
		blksize: 4096,
		blocks:  (size + 511) / 512,
		atimeNS: mtime,
		mtimeNS: mtime,
		ctimeNS: mtime,
		dtype:   dtype,
	}
}

func storeJea9LinuxStat(cpu *CPU, addr uint64, st jea9LinuxStat) int64 {
	buf := make([]byte, 128)
	binary.LittleEndian.PutUint64(buf[0:], st.dev)
	binary.LittleEndian.PutUint64(buf[8:], st.ino)
	binary.LittleEndian.PutUint32(buf[16:], st.mode)
	binary.LittleEndian.PutUint32(buf[20:], st.nlink)
	binary.LittleEndian.PutUint32(buf[24:], st.uid)
	binary.LittleEndian.PutUint32(buf[28:], st.gid)
	binary.LittleEndian.PutUint64(buf[32:], st.rdev)
	binary.LittleEndian.PutUint64(buf[48:], uint64(st.size))
	binary.LittleEndian.PutUint32(buf[56:], uint32(st.blksize))
	binary.LittleEndian.PutUint64(buf[64:], uint64(st.blocks))
	putJea9LinuxTimespec(buf[72:], st.atimeNS)
	putJea9LinuxTimespec(buf[88:], st.mtimeNS)
	putJea9LinuxTimespec(buf[104:], st.ctimeNS)
	if f := cpu.mem.WriteBytes(addr, buf); f != nil {
		return jea9LinuxErrEFAULT
	}
	return 0
}

func storeJea9LinuxStatx(cpu *CPU, addr uint64, st jea9LinuxStat) int64 {
	buf := make([]byte, 256)
	binary.LittleEndian.PutUint32(buf[0:], jea9LinuxStatxBasicStats)
	binary.LittleEndian.PutUint32(buf[4:], uint32(st.blksize))
	binary.LittleEndian.PutUint32(buf[16:], st.nlink)
	binary.LittleEndian.PutUint32(buf[20:], st.uid)
	binary.LittleEndian.PutUint32(buf[24:], st.gid)
	binary.LittleEndian.PutUint16(buf[28:], uint16(st.mode))
	binary.LittleEndian.PutUint64(buf[32:], st.ino)
	binary.LittleEndian.PutUint64(buf[40:], uint64(st.size))
	binary.LittleEndian.PutUint64(buf[48:], uint64(st.blocks))
	binary.LittleEndian.PutUint64(buf[56:], uint64(0))
	putJea9LinuxStatxTimestamp(buf[64:], st.atimeNS)
	putJea9LinuxStatxTimestamp(buf[96:], st.ctimeNS)
	putJea9LinuxStatxTimestamp(buf[112:], st.mtimeNS)
	if f := cpu.mem.WriteBytes(addr, buf); f != nil {
		return jea9LinuxErrEFAULT
	}
	return 0
}

func storeJea9LinuxStatfs(cpu *CPU, addr uint64) int64 {
	buf := make([]byte, 120)
	binary.LittleEndian.PutUint64(buf[0:], uint64(0x01021994)) // tmpfs magic
	binary.LittleEndian.PutUint64(buf[8:], 4096)
	binary.LittleEndian.PutUint64(buf[16:], 1<<30)
	binary.LittleEndian.PutUint64(buf[24:], 1<<29)
	binary.LittleEndian.PutUint64(buf[32:], 1<<29)
	binary.LittleEndian.PutUint64(buf[40:], 1<<20)
	binary.LittleEndian.PutUint64(buf[48:], 1<<20)
	binary.LittleEndian.PutUint64(buf[64:], 255)
	binary.LittleEndian.PutUint64(buf[72:], 4096)
	if f := cpu.mem.WriteBytes(addr, buf); f != nil {
		return jea9LinuxErrEFAULT
	}
	return 0
}

func putJea9LinuxTimespec(buf []byte, ns int64) {
	sec := ns / 1_000_000_000
	nsec := ns % 1_000_000_000
	if nsec < 0 {
		sec--
		nsec += 1_000_000_000
	}
	binary.LittleEndian.PutUint64(buf[0:], uint64(sec))
	binary.LittleEndian.PutUint64(buf[8:], uint64(nsec))
}

func putJea9LinuxStatxTimestamp(buf []byte, ns int64) {
	sec := ns / 1_000_000_000
	nsec := ns % 1_000_000_000
	if nsec < 0 {
		sec--
		nsec += 1_000_000_000
	}
	binary.LittleEndian.PutUint64(buf[0:], uint64(sec))
	binary.LittleEndian.PutUint32(buf[8:], uint32(nsec))
}

func (jos *Jea9Linux) virtualDirExists(path string) bool {
	path = normalizeJea9LinuxGuestPath(path)
	if path == "/" {
		return true
	}
	prefix := strings.TrimSuffix(path, "/") + "/"
	for file := range jos.files {
		if strings.HasPrefix(file, prefix) {
			return true
		}
	}
	return path == "/proc" || path == "/proc/self" || path == "/dev"
}

func (jos *Jea9Linux) virtualDirEntries(path string) []string {
	path = normalizeJea9LinuxGuestPath(path)
	seen := map[string]bool{".": true, "..": true}
	for file := range jos.files {
		if child, ok := childNameInJea9LinuxDir(path, file); ok {
			seen[child] = true
		}
	}
	switch path {
	case "/":
		seen["dev"] = true
		seen["proc"] = true
	case "/dev":
		seen["random"] = true
		seen["urandom"] = true
	case "/proc":
		seen["self"] = true
		seen[uint64String(jos.pid)] = true
	case "/proc/self", "/proc/" + uint64String(jos.pid):
		seen["exe"] = true
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func childNameInJea9LinuxDir(dir, file string) (string, bool) {
	dir = strings.TrimSuffix(normalizeJea9LinuxGuestPath(dir), "/")
	file = normalizeJea9LinuxGuestPath(file)
	prefix := dir + "/"
	if dir == "/" {
		prefix = "/"
	}
	if !strings.HasPrefix(file, prefix) || file == dir {
		return "", false
	}
	rest := strings.TrimPrefix(file, prefix)
	if rest == "" {
		return "", false
	}
	if slash := strings.IndexByte(rest, '/'); slash >= 0 {
		rest = rest[:slash]
	}
	return rest, true
}

func alignJea9LinuxDirentLen(n int) int {
	return (n + 7) &^ 7
}

func inodeForJea9LinuxPath(path string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(path))
	v := h.Sum64()
	if v == 0 {
		return 1
	}
	return v
}

func closeJea9LinuxFD(f jea9LinuxFD) int64 {
	if f.kind == jea9LinuxFDHostFile && f.hostFile != nil {
		if err := f.hostFile.Close(); err != nil {
			return jea9LinuxErrnoFromHost(err)
		}
	}
	return 0
}

func (jos *Jea9Linux) closeAllFDs() {
	for fd, f := range jos.fds {
		delete(jos.fds, fd)
		_ = closeJea9LinuxFD(f)
	}
}

func uint64String(v uint64) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}

func errorsIsEOF(err error) bool {
	return err == nil || err == io.EOF
}

func (jos *Jea9Linux) fsNowNS() int64 {
	return jos.monotonicNS + jos.realtimeOffsetNS
}
