package riscv

import (
	"encoding/binary"
	"errors"
	"io"
	"os"
)

const (
	hostIOMagic   = uint32(0x314f4948) // "HIO1"
	hostIOVersion = uint32(1)

	hostIORegMagic   = uint64(0x00)
	hostIORegVersion = uint64(0x04)
	hostIORegStatus  = uint64(0x08)
	hostIORegErrno   = uint64(0x0c)
	hostIORegCmdAddr = uint64(0x10)
	hostIORegCmdSize = uint64(0x18)
	hostIORegSubmit  = uint64(0x20)
	hostIORegResult  = uint64(0x28)

	hostIOStatusReady      = uint32(0)
	hostIOStatusOK         = uint32(1)
	hostIOStatusErr        = uint32(2)
	hostIOStatusBadCommand = uint32(3)

	hostIOCmdSize = 96

	hostIOOpNop       = uint32(0)
	hostIOOpOpen      = uint32(1)
	hostIOOpCreate    = uint32(2)
	hostIOOpClose     = uint32(3)
	hostIOOpRead      = uint32(4)
	hostIOOpWrite     = uint32(5)
	hostIOOpSeek      = uint32(6)
	hostIOOpMkdir     = uint32(7)
	hostIOOpMkdirAll  = uint32(8)
	hostIOOpRemove    = uint32(9)
	hostIOOpRemoveAll = uint32(10)
	hostIOOpRename    = uint32(11)
	hostIOOpReadFile  = uint32(12)
	hostIOOpWriteFile = uint32(13)
	hostIOOpStat      = uint32(14)
	hostIOOpFstat     = uint32(15)
	hostIOOpTruncate  = uint32(16)
	hostIOOpFtruncate = uint32(17)
	hostIOOpSync      = uint32(18)
	hostIOOpReadDir   = uint32(19)
	hostIOOpLstat     = uint32(20)
	hostIOOpReadlink  = uint32(21)
	hostIOOpSymlink   = uint32(22)
	hostIOOpChmod     = uint32(23)

	hostIOOpenAccessMask = uint32(0x3)
	hostIOOpenReadOnly   = uint32(0)
	hostIOOpenWriteOnly  = uint32(1)
	hostIOOpenReadWrite  = uint32(2)
	hostIOOpenCreate     = uint32(1 << 8)
	hostIOOpenExcl       = uint32(1 << 9)
	hostIOOpenTrunc      = uint32(1 << 10)
	hostIOOpenAppend     = uint32(1 << 11)
	hostIOOpenSync       = uint32(1 << 12)

	hostIOStatSize         = 32
	hostIODirentHeaderSize = 32
	hostIOMaxPath          = 64 << 10
	hostIOMaxCopy          = 64 << 20
)

// hostio errno values are part of the guest Linux ABI. Keep them as Linux
// numbers even when the emulator is running on a non-Linux host.
const (
	hostIOErrEPERM        = uint32(1)
	hostIOErrENOENT       = uint32(2)
	hostIOErrEIO          = uint32(5)
	hostIOErrEBADF        = uint32(9)
	hostIOErrENOMEM       = uint32(12)
	hostIOErrEACCES       = uint32(13)
	hostIOErrEFAULT       = uint32(14)
	hostIOErrEEXIST       = uint32(17)
	hostIOErrENOTDIR      = uint32(20)
	hostIOErrEINVAL       = uint32(22)
	hostIOErrEFBIG        = uint32(27)
	hostIOErrENOSPC       = uint32(28)
	hostIOErrESPIPE       = uint32(29)
	hostIOErrEROFS        = uint32(30)
	hostIOErrEPIPE        = uint32(32)
	hostIOErrENAMETOOLONG = uint32(36)
	hostIOErrENOSYS       = uint32(38)
	hostIOErrENOTEMPTY    = uint32(39)
	hostIOErrELOOP        = uint32(40)
	hostIOErrEMFILE       = uint32(24)
	hostIOErrEXDEV        = uint32(18)
	hostIOErrEOPNOTSUPP   = uint32(95)
	hostIOErrENOBUFS      = uint32(105)
)

type hostIODevice struct {
	mem        *GuestMemory
	cmdAddr    uint64
	cmdSize    uint64
	status     uint32
	errno      uint32
	result     int64
	nextHandle uint64
	files      map[uint64]*os.File
}

type hostIOCommand struct {
	Op       uint32
	Flags    uint32
	Path     uint64
	PathLen  uint64
	Path2    uint64
	Path2Len uint64
	Buf      uint64
	Len      uint64
	Offset   uint64
	Mode     uint64
	Handle   uint64
	Result   int64
	Errno    uint32
	Status   uint32
}

func newHostIODevice(mem *GuestMemory) *hostIODevice {
	return &hostIODevice{
		mem:        mem,
		cmdSize:    hostIOCmdSize,
		nextHandle: 1,
		files:      make(map[uint64]*os.File),
	}
}

func (d *hostIODevice) Close() {
	for h, f := range d.files {
		_ = f.Close()
		delete(d.files, h)
	}
}

func (d *hostIODevice) Load(off, width uint64) uint64 {
	switch off {
	case hostIORegMagic:
		return uint64(hostIOMagic)
	case hostIORegVersion:
		return uint64(hostIOVersion)
	case hostIORegStatus:
		return uint64(d.status)
	case hostIORegErrno:
		return uint64(d.errno)
	case hostIORegCmdAddr:
		return d.cmdAddr
	case hostIORegCmdSize:
		return d.cmdSize
	case hostIORegResult:
		return uint64(d.result)
	case hostIORegCmdAddr + 4:
		return d.cmdAddr >> 32
	case hostIORegCmdSize + 4:
		return d.cmdSize >> 32
	case hostIORegResult + 4:
		return uint64(d.result) >> 32
	default:
		return 0
	}
}

func (d *hostIODevice) Store(off, width, value uint64) *MemFault {
	switch off {
	case hostIORegStatus:
		if value == 0 {
			d.setStatus(hostIOStatusReady, 0, 0)
		}
	case hostIORegCmdAddr:
		d.cmdAddr = writeHostIOReg(d.cmdAddr, width, value, false)
	case hostIORegCmdAddr + 4:
		d.cmdAddr = writeHostIOReg(d.cmdAddr, width, value, true)
	case hostIORegCmdSize:
		d.cmdSize = writeHostIOReg(d.cmdSize, width, value, false)
	case hostIORegCmdSize + 4:
		d.cmdSize = writeHostIOReg(d.cmdSize, width, value, true)
	case hostIORegSubmit:
		if value != 0 {
			return d.submit()
		}
	}
	return nil
}

func writeHostIOReg(old, width, value uint64, high bool) uint64 {
	if width == 8 && !high {
		return value
	}
	if width != 4 {
		return old
	}
	if high {
		return (old & uint64(0xffffffff)) | uint64(uint32(value))<<32
	}
	return (old &^ uint64(0xffffffff)) | uint64(uint32(value))
}

func (d *hostIODevice) submit() *MemFault {
	cmd, fault, ok := d.readCommand()
	if fault != nil {
		d.setStatus(hostIOStatusBadCommand, hostIOErrEFAULT, -1)
		return fault
	}
	if !ok {
		d.setStatus(hostIOStatusBadCommand, hostIOErrEINVAL, -1)
		return nil
	}
	result, errno := d.execute(cmd)
	status := hostIOStatusOK
	if errno != 0 {
		status = hostIOStatusErr
	}
	cmd.Result = result
	cmd.Errno = errno
	cmd.Status = status
	d.setStatus(status, errno, result)
	return d.writeCommand(cmd)
}

func (d *hostIODevice) readCommand() (*hostIOCommand, *MemFault, bool) {
	if d.cmdSize == 0 {
		d.cmdSize = hostIOCmdSize
	}
	if d.cmdSize < hostIOCmdSize || d.cmdSize > biosHostIOSize {
		return nil, nil, false
	}
	var raw [hostIOCmdSize]byte
	if fault := d.mem.ReadBytes(d.cmdAddr, raw[:]); fault != nil {
		return nil, fault, false
	}
	cmd := &hostIOCommand{
		Op:       binary.LittleEndian.Uint32(raw[0:]),
		Flags:    binary.LittleEndian.Uint32(raw[4:]),
		Path:     binary.LittleEndian.Uint64(raw[8:]),
		PathLen:  binary.LittleEndian.Uint64(raw[16:]),
		Path2:    binary.LittleEndian.Uint64(raw[24:]),
		Path2Len: binary.LittleEndian.Uint64(raw[32:]),
		Buf:      binary.LittleEndian.Uint64(raw[40:]),
		Len:      binary.LittleEndian.Uint64(raw[48:]),
		Offset:   binary.LittleEndian.Uint64(raw[56:]),
		Mode:     binary.LittleEndian.Uint64(raw[64:]),
		Handle:   binary.LittleEndian.Uint64(raw[72:]),
		Result:   int64(binary.LittleEndian.Uint64(raw[80:])),
		Errno:    binary.LittleEndian.Uint32(raw[88:]),
		Status:   binary.LittleEndian.Uint32(raw[92:]),
	}
	return cmd, nil, true
}

func (d *hostIODevice) writeCommand(cmd *hostIOCommand) *MemFault {
	var raw [hostIOCmdSize]byte
	binary.LittleEndian.PutUint32(raw[0:], cmd.Op)
	binary.LittleEndian.PutUint32(raw[4:], cmd.Flags)
	binary.LittleEndian.PutUint64(raw[8:], cmd.Path)
	binary.LittleEndian.PutUint64(raw[16:], cmd.PathLen)
	binary.LittleEndian.PutUint64(raw[24:], cmd.Path2)
	binary.LittleEndian.PutUint64(raw[32:], cmd.Path2Len)
	binary.LittleEndian.PutUint64(raw[40:], cmd.Buf)
	binary.LittleEndian.PutUint64(raw[48:], cmd.Len)
	binary.LittleEndian.PutUint64(raw[56:], cmd.Offset)
	binary.LittleEndian.PutUint64(raw[64:], cmd.Mode)
	binary.LittleEndian.PutUint64(raw[72:], cmd.Handle)
	binary.LittleEndian.PutUint64(raw[80:], uint64(cmd.Result))
	binary.LittleEndian.PutUint32(raw[88:], cmd.Errno)
	binary.LittleEndian.PutUint32(raw[92:], cmd.Status)
	return d.mem.WriteBytes(d.cmdAddr, raw[:])
}

func (d *hostIODevice) execute(cmd *hostIOCommand) (int64, uint32) {
	switch cmd.Op {
	case hostIOOpNop:
		return 0, 0
	case hostIOOpOpen:
		path, errno := d.readPath(cmd.Path, cmd.PathLen)
		if errno != 0 {
			return -1, errno
		}
		f, err := os.OpenFile(path, hostIOOpenFlags(cmd.Flags), os.FileMode(cmd.Mode))
		if err != nil {
			return -1, hostIOErrno(err)
		}
		return int64(d.registerFile(f)), 0
	case hostIOOpCreate:
		path, errno := d.readPath(cmd.Path, cmd.PathLen)
		if errno != 0 {
			return -1, errno
		}
		f, err := os.Create(path)
		if err != nil {
			return -1, hostIOErrno(err)
		}
		return int64(d.registerFile(f)), 0
	case hostIOOpClose:
		f, errno := d.file(cmd.Handle)
		if errno != 0 {
			return -1, errno
		}
		delete(d.files, cmd.Handle)
		if err := f.Close(); err != nil {
			return -1, hostIOErrno(err)
		}
		return 0, 0
	case hostIOOpRead:
		f, errno := d.file(cmd.Handle)
		if errno != 0 {
			return -1, errno
		}
		if cmd.Len > hostIOMaxCopy {
			return -1, hostIOErrENOMEM
		}
		buf := make([]byte, int(cmd.Len))
		n, err := f.Read(buf)
		if n > 0 {
			if fault := d.mem.WriteBytes(cmd.Buf, buf[:n]); fault != nil {
				return -1, hostIOErrEFAULT
			}
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return int64(n), hostIOErrno(err)
		}
		return int64(n), 0
	case hostIOOpWrite:
		f, errno := d.file(cmd.Handle)
		if errno != 0 {
			return -1, errno
		}
		buf, errno := d.readGuestBuffer(cmd.Buf, cmd.Len)
		if errno != 0 {
			return -1, errno
		}
		n, err := f.Write(buf)
		if err != nil {
			return int64(n), hostIOErrno(err)
		}
		return int64(n), 0
	case hostIOOpSeek:
		f, errno := d.file(cmd.Handle)
		if errno != 0 {
			return -1, errno
		}
		pos, err := f.Seek(int64(cmd.Offset), int(cmd.Flags))
		if err != nil {
			return -1, hostIOErrno(err)
		}
		return pos, 0
	case hostIOOpMkdir:
		return d.pathModeOp(cmd, os.Mkdir)
	case hostIOOpMkdirAll:
		return d.pathModeOp(cmd, os.MkdirAll)
	case hostIOOpRemove:
		return d.pathOnlyOp(cmd, os.Remove)
	case hostIOOpRemoveAll:
		return d.pathOnlyOp(cmd, os.RemoveAll)
	case hostIOOpRename:
		oldPath, errno := d.readPath(cmd.Path, cmd.PathLen)
		if errno != 0 {
			return -1, errno
		}
		newPath, errno := d.readPath(cmd.Path2, cmd.Path2Len)
		if errno != 0 {
			return -1, errno
		}
		if err := os.Rename(oldPath, newPath); err != nil {
			return -1, hostIOErrno(err)
		}
		return 0, 0
	case hostIOOpReadFile:
		path, errno := d.readPath(cmd.Path, cmd.PathLen)
		if errno != 0 {
			return -1, errno
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return -1, hostIOErrno(err)
		}
		if uint64(len(data)) > cmd.Len {
			return int64(len(data)), hostIOErrENOBUFS
		}
		if fault := d.mem.WriteBytes(cmd.Buf, data); fault != nil {
			return -1, hostIOErrEFAULT
		}
		return int64(len(data)), 0
	case hostIOOpWriteFile:
		path, errno := d.readPath(cmd.Path, cmd.PathLen)
		if errno != 0 {
			return -1, errno
		}
		buf, errno := d.readGuestBuffer(cmd.Buf, cmd.Len)
		if errno != 0 {
			return -1, errno
		}
		if err := os.WriteFile(path, buf, os.FileMode(cmd.Mode)); err != nil {
			return -1, hostIOErrno(err)
		}
		return int64(len(buf)), 0
	case hostIOOpStat:
		path, errno := d.readPath(cmd.Path, cmd.PathLen)
		if errno != 0 {
			return -1, errno
		}
		info, err := os.Stat(path)
		if err != nil {
			return -1, hostIOErrno(err)
		}
		return d.writeStat(cmd.Buf, cmd.Len, info)
	case hostIOOpLstat:
		path, errno := d.readPath(cmd.Path, cmd.PathLen)
		if errno != 0 {
			return -1, errno
		}
		info, err := os.Lstat(path)
		if err != nil {
			return -1, hostIOErrno(err)
		}
		return d.writeStat(cmd.Buf, cmd.Len, info)
	case hostIOOpFstat:
		f, errno := d.file(cmd.Handle)
		if errno != 0 {
			return -1, errno
		}
		info, err := f.Stat()
		if err != nil {
			return -1, hostIOErrno(err)
		}
		return d.writeStat(cmd.Buf, cmd.Len, info)
	case hostIOOpTruncate:
		path, errno := d.readPath(cmd.Path, cmd.PathLen)
		if errno != 0 {
			return -1, errno
		}
		if err := os.Truncate(path, int64(cmd.Offset)); err != nil {
			return -1, hostIOErrno(err)
		}
		return 0, 0
	case hostIOOpFtruncate:
		f, errno := d.file(cmd.Handle)
		if errno != 0 {
			return -1, errno
		}
		if err := f.Truncate(int64(cmd.Offset)); err != nil {
			return -1, hostIOErrno(err)
		}
		return 0, 0
	case hostIOOpSync:
		f, errno := d.file(cmd.Handle)
		if errno != 0 {
			return -1, errno
		}
		if err := f.Sync(); err != nil {
			return -1, hostIOErrno(err)
		}
		return 0, 0
	case hostIOOpReadDir:
		path, errno := d.readPath(cmd.Path, cmd.PathLen)
		if errno != 0 {
			return -1, errno
		}
		entries, err := os.ReadDir(path)
		if err != nil {
			return -1, hostIOErrno(err)
		}
		return d.writeDirents(cmd.Buf, cmd.Len, entries)
	case hostIOOpReadlink:
		path, errno := d.readPath(cmd.Path, cmd.PathLen)
		if errno != 0 {
			return -1, errno
		}
		target, err := os.Readlink(path)
		if err != nil {
			return -1, hostIOErrno(err)
		}
		need := uint64(len(target)) + 1
		if need > cmd.Len {
			return int64(need), hostIOErrENOBUFS
		}
		if fault := d.mem.WriteBytes(cmd.Buf, []byte(target)); fault != nil {
			return -1, hostIOErrEFAULT
		}
		if fault := d.mem.WriteBytes(cmd.Buf+uint64(len(target)), []byte{0}); fault != nil {
			return -1, hostIOErrEFAULT
		}
		return int64(len(target)), 0
	case hostIOOpSymlink:
		target, errno := d.readPath(cmd.Path, cmd.PathLen)
		if errno != 0 {
			return -1, errno
		}
		link, errno := d.readPath(cmd.Path2, cmd.Path2Len)
		if errno != 0 {
			return -1, errno
		}
		if err := os.Symlink(target, link); err != nil {
			return -1, hostIOErrno(err)
		}
		return 0, 0
	case hostIOOpChmod:
		return d.pathModeOp(cmd, os.Chmod)
	default:
		return -1, hostIOErrENOSYS
	}
}

func (d *hostIODevice) setStatus(status, errno uint32, result int64) {
	d.status = status
	d.errno = errno
	d.result = result
}

func (d *hostIODevice) registerFile(f *os.File) uint64 {
	h := d.nextHandle
	d.nextHandle++
	if d.nextHandle == 0 {
		d.nextHandle = 1
	}
	d.files[h] = f
	return h
}

func (d *hostIODevice) file(handle uint64) (*os.File, uint32) {
	f := d.files[handle]
	if f == nil {
		return nil, hostIOErrEBADF
	}
	return f, 0
}

func (d *hostIODevice) pathOnlyOp(cmd *hostIOCommand, fn func(string) error) (int64, uint32) {
	path, errno := d.readPath(cmd.Path, cmd.PathLen)
	if errno != 0 {
		return -1, errno
	}
	if err := fn(path); err != nil {
		return -1, hostIOErrno(err)
	}
	return 0, 0
}

func (d *hostIODevice) pathModeOp(cmd *hostIOCommand, fn func(string, os.FileMode) error) (int64, uint32) {
	path, errno := d.readPath(cmd.Path, cmd.PathLen)
	if errno != 0 {
		return -1, errno
	}
	if err := fn(path, os.FileMode(cmd.Mode)); err != nil {
		return -1, hostIOErrno(err)
	}
	return 0, 0
}

func (d *hostIODevice) readPath(addr, length uint64) (string, uint32) {
	if length == 0 || length > hostIOMaxPath {
		return "", hostIOErrENAMETOOLONG
	}
	buf := make([]byte, int(length))
	if fault := d.mem.ReadBytes(addr, buf); fault != nil {
		return "", hostIOErrEFAULT
	}
	return hostIONormalizePath(string(buf)), 0
}

func (d *hostIODevice) readGuestBuffer(addr, length uint64) ([]byte, uint32) {
	if length > hostIOMaxCopy {
		return nil, hostIOErrENOMEM
	}
	if length > uint64(int(^uint(0)>>1)) {
		return nil, hostIOErrENOMEM
	}
	buf := make([]byte, int(length))
	if fault := d.mem.ReadBytes(addr, buf); fault != nil {
		return nil, hostIOErrEFAULT
	}
	return buf, 0
}

func (d *hostIODevice) writeStat(addr, length uint64, info os.FileInfo) (int64, uint32) {
	if length < hostIOStatSize {
		return hostIOStatSize, hostIOErrENOBUFS
	}
	var raw [hostIOStatSize]byte
	binary.LittleEndian.PutUint64(raw[0:], uint64(info.Size()))
	binary.LittleEndian.PutUint64(raw[8:], uint64(info.Mode()))
	binary.LittleEndian.PutUint64(raw[16:], uint64(info.ModTime().UnixNano()))
	if info.IsDir() {
		binary.LittleEndian.PutUint64(raw[24:], 1)
	}
	if fault := d.mem.WriteBytes(addr, raw[:]); fault != nil {
		return -1, hostIOErrEFAULT
	}
	return hostIOStatSize, 0
}

func (d *hostIODevice) writeDirents(addr, length uint64, entries []os.DirEntry) (int64, uint32) {
	if length > hostIOMaxCopy {
		return -1, hostIOErrENOMEM
	}
	if length > uint64(int(^uint(0)>>1)) {
		return -1, hostIOErrENOMEM
	}
	out := make([]byte, 0, int(length))
	for _, entry := range entries {
		name := entry.Name()
		need := hostIODirentHeaderSize + len(name)
		if need > int(length)-len(out) {
			return int64(len(out) + need), hostIOErrENOBUFS
		}
		info, err := entry.Info()
		if err != nil {
			return -1, hostIOErrno(err)
		}
		var header [hostIODirentHeaderSize]byte
		binary.LittleEndian.PutUint64(header[0:], uint64(info.Size()))
		binary.LittleEndian.PutUint64(header[8:], uint64(info.Mode()))
		binary.LittleEndian.PutUint64(header[16:], uint64(info.ModTime().UnixNano()))
		binary.LittleEndian.PutUint32(header[24:], uint32(len(name)))
		if info.IsDir() {
			binary.LittleEndian.PutUint32(header[28:], 1)
		}
		out = append(out, header[:]...)
		out = append(out, name...)
	}
	if fault := d.mem.WriteBytes(addr, out); fault != nil {
		return -1, hostIOErrEFAULT
	}
	return int64(len(out)), 0
}

func hostIOOpenFlags(flags uint32) int {
	var out int
	switch flags & hostIOOpenAccessMask {
	case hostIOOpenWriteOnly:
		out |= os.O_WRONLY
	case hostIOOpenReadWrite:
		out |= os.O_RDWR
	default:
		out |= os.O_RDONLY
	}
	if flags&hostIOOpenCreate != 0 {
		out |= os.O_CREATE
	}
	if flags&hostIOOpenExcl != 0 {
		out |= os.O_EXCL
	}
	if flags&hostIOOpenTrunc != 0 {
		out |= os.O_TRUNC
	}
	if flags&hostIOOpenAppend != 0 {
		out |= os.O_APPEND
	}
	if flags&hostIOOpenSync != 0 {
		out |= os.O_SYNC
	}
	return out
}

func hostIOErrno(err error) uint32 {
	if err == nil || errors.Is(err, io.EOF) {
		return 0
	}
	if errno, ok := hostIOPlatformErrno(err); ok {
		return errno
	}
	switch {
	case errors.Is(err, os.ErrNotExist):
		return hostIOErrENOENT
	case errors.Is(err, os.ErrPermission):
		return hostIOErrEACCES
	case errors.Is(err, os.ErrExist):
		return hostIOErrEEXIST
	case errors.Is(err, os.ErrInvalid):
		return hostIOErrEINVAL
	}
	return hostIOErrEIO
}
