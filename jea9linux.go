package riscv

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"time"
)

type Jea9LinuxClockMode uint8

const (
	Jea9ClockIdleJump Jea9LinuxClockMode = iota
	Jea9ClockICTick
	Jea9ClockManual
)

const defaultJea9LinuxInstructionBudget = uint64(65536)

var (
	ErrJea9LinuxBudget  = errors.New("jea9linux instruction budget expired")
	ErrJea9LinuxBlocked = errors.New("jea9linux has no runnable contexts")
)

const (
	jea9LinuxErrEFAULT = int64(-14)
	jea9LinuxErrEINVAL = int64(-22)
	jea9LinuxErrEBADF  = int64(-9)
	jea9LinuxErrENOENT = int64(-2)
	jea9LinuxErrENOSYS = int64(-38)
	jea9LinuxErrEPERM  = int64(-1)
	jea9LinuxErrESRCH  = int64(-3)
	jea9LinuxErrEIO    = int64(-5)
	jea9LinuxErrEACCES = int64(-13)
	jea9LinuxErrESPIPE = int64(-29)

	jea9LinuxSysFcntl        = uint64(25)
	jea9LinuxSysOpenat       = uint64(56)
	jea9LinuxSysClose        = uint64(57)
	jea9LinuxSysLseek        = uint64(62)
	jea9LinuxSysRead         = uint64(63)
	jea9LinuxSysWrite        = uint64(64)
	jea9LinuxSysPread64      = uint64(67)
	jea9LinuxSysExit         = uint64(93)
	jea9LinuxSysExitGroup    = uint64(94)
	jea9LinuxSysNanosleep    = uint64(101)
	jea9LinuxSysClockGettime = uint64(113)
	jea9LinuxSysUname        = uint64(160)
	jea9LinuxSysGetrlimit    = uint64(163)
	jea9LinuxSysPrctl        = uint64(167)
	jea9LinuxSysGettimeofday = uint64(169)
	jea9LinuxSysGetpid       = uint64(172)
	jea9LinuxSysGettid       = uint64(178)
	jea9LinuxSysSysinfo      = uint64(179)
	jea9LinuxSysPrlimit64    = uint64(261)
	jea9LinuxSysGetrandom    = uint64(278)

	jea9LinuxClockRealtime        = uint64(0)
	jea9LinuxClockMonotonic       = uint64(1)
	jea9LinuxClockRealtimeCoarse  = uint64(5)
	jea9LinuxClockMonotonicCoarse = uint64(6)

	jea9LinuxATNull   = uint64(0)
	jea9LinuxATPHDR   = uint64(3)
	jea9LinuxATPHENT  = uint64(4)
	jea9LinuxATPHNUM  = uint64(5)
	jea9LinuxATPAGESZ = uint64(6)
	jea9LinuxATENTRY  = uint64(9)
	jea9LinuxATUID    = uint64(11)
	jea9LinuxATEUID   = uint64(12)
	jea9LinuxATGID    = uint64(13)
	jea9LinuxATEGID   = uint64(14)
	jea9LinuxATPLAT   = uint64(15)
	jea9LinuxATHWCAP  = uint64(16)
	jea9LinuxATCLKTCK = uint64(17)
	jea9LinuxATSECURE = uint64(23)
	jea9LinuxATRANDOM = uint64(25)
	jea9LinuxATHWCAP2 = uint64(26)
	jea9LinuxATEXECFN = uint64(31)
)

const (
	jea9LinuxClockTicksPerSecond = uint64(100)
	jea9LinuxPlatform            = "riscv64"

	jea9LinuxFGetFD = uint64(1)
	jea9LinuxFSetFD = uint64(2)
	jea9LinuxFGetFL = uint64(3)
	jea9LinuxFSetFL = uint64(4)

	jea9LinuxSeekSet = uint64(0)
	jea9LinuxSeekCur = uint64(1)
	jea9LinuxSeekEnd = uint64(2)

	jea9LinuxRLimitStack  = uint64(3)
	jea9LinuxRLimitNOFile = uint64(7)
	jea9LinuxRLimitAS     = uint64(9)

	jea9LinuxPRSetName = uint64(15)
	jea9LinuxPRGetName = uint64(16)
	jea9LinuxPRSetVMA  = uint64(0x53564d41)
)

type jea9LinuxFDKind uint8

const (
	jea9LinuxFDStdin jea9LinuxFDKind = iota + 1
	jea9LinuxFDStdout
	jea9LinuxFDStderr
	jea9LinuxFDRandom
	jea9LinuxFDFile
)

type jea9LinuxFD struct {
	kind  jea9LinuxFDKind
	data  []byte
	off   int64
	flags uint64
}

type Jea9LinuxOptions struct {
	EntropySeed       []byte
	ClockMode         Jea9LinuxClockMode
	MonotonicStartNS  int64
	RealtimeOffsetNS  int64
	NSPerInstruction  int64
	InstructionBudget uint64
	Stdin             io.Reader
	Stdout            io.Writer
	Stderr            io.Writer
	Files             map[string][]byte
	PID               uint64
	TID               uint64
}

type Jea9LinuxStartOptions struct {
	Args     []string
	Env      []string
	ExecPath string
	StackTop uint64
}

type Jea9Linux struct {
	clockMode         Jea9LinuxClockMode
	monotonicNS       int64
	realtimeOffsetNS  int64
	nsPerInstruction  int64
	instructionBudget uint64

	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
	fds    map[int]jea9LinuxFD
	nextFD int
	files  map[string][]byte
	pid    uint64
	tid    uint64

	rootSeed      [32]byte
	randomCounter uint64
	randomBuf     [32]byte
	randomOff     int

	budgetYields uint64
	blocked      bool
	blockedUntil int64
	threadName   string
}

type jea9LinuxAuxEntry struct {
	tag uint64
	val uint64
}

type jea9LinuxStackBuilder struct {
	cpu *CPU
	sp  uint64
}

func NewJea9Linux(opts Jea9LinuxOptions) *Jea9Linux {
	j := &Jea9Linux{
		clockMode:         opts.ClockMode,
		monotonicNS:       opts.MonotonicStartNS,
		realtimeOffsetNS:  opts.RealtimeOffsetNS,
		nsPerInstruction:  opts.NSPerInstruction,
		instructionBudget: opts.InstructionBudget,
		stdin:             opts.Stdin,
		stdout:            opts.Stdout,
		stderr:            opts.Stderr,
		fds:               make(map[int]jea9LinuxFD),
		nextFD:            3,
		files:             make(map[string][]byte),
		pid:               opts.PID,
		tid:               opts.TID,
		threadName:        "jea9linux",
	}
	if j.instructionBudget == 0 {
		j.instructionBudget = defaultJea9LinuxInstructionBudget
	}
	if j.nsPerInstruction == 0 {
		j.nsPerInstruction = 1
	}
	if j.stdout == nil {
		j.stdout = io.Discard
	}
	if j.stderr == nil {
		j.stderr = io.Discard
	}
	if j.pid == 0 {
		j.pid = 1
	}
	if j.tid == 0 {
		j.tid = j.pid
	}
	j.fds[0] = jea9LinuxFD{kind: jea9LinuxFDStdin}
	j.fds[1] = jea9LinuxFD{kind: jea9LinuxFDStdout}
	j.fds[2] = jea9LinuxFD{kind: jea9LinuxFDStderr}
	for path, data := range opts.Files {
		j.files[path] = append([]byte(nil), data...)
	}
	j.rootSeed = deriveJea9LinuxRootSeed(opts.EntropySeed)
	j.randomOff = len(j.randomBuf)
	return j
}

func deriveJea9LinuxRootSeed(seed []byte) [32]byte {
	if seed == nil {
		return sha256.Sum256([]byte("jea9linux default deterministic seed v1"))
	}
	h := sha256.New()
	_, _ = h.Write([]byte("jea9linux entropy root v1"))
	_, _ = h.Write(seed)
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

func (j *Jea9Linux) ClockMode() Jea9LinuxClockMode { return j.clockMode }

func (j *Jea9Linux) SetClockMode(mode Jea9LinuxClockMode) { j.clockMode = mode }

func (j *Jea9Linux) InstructionBudget() uint64 { return j.instructionBudget }

func (j *Jea9Linux) SetNSPerInstruction(ns int64) {
	if ns == 0 {
		ns = 1
	}
	j.nsPerInstruction = ns
}

func (j *Jea9Linux) AdvanceTime(d time.Duration) {
	j.monotonicNS += int64(d)
	j.refreshBlocked()
}

func (j *Jea9Linux) SetMonotonicNS(ns int64) {
	j.monotonicNS = ns
	j.refreshBlocked()
}

func (j *Jea9Linux) MonotonicNS() int64 { return j.monotonicNS }

func (j *Jea9Linux) BudgetYields() uint64 { return j.budgetYields }

func (j *Jea9Linux) Blocked() bool {
	j.refreshBlocked()
	return j.blocked
}

func (j *Jea9Linux) fillRandom(dst []byte) {
	for len(dst) > 0 {
		if j.randomOff >= len(j.randomBuf) {
			j.randomBuf = j.randomBlock("sys-random-v1", j.randomCounter)
			j.randomCounter++
			j.randomOff = 0
		}
		n := copy(dst, j.randomBuf[j.randomOff:])
		j.randomOff += n
		dst = dst[n:]
	}
}

func (j *Jea9Linux) randomBlock(label string, counter uint64) [32]byte {
	h := sha256.New()
	_, _ = h.Write(j.rootSeed[:])
	_, _ = h.Write([]byte(label))
	var ctr [8]byte
	binary.LittleEndian.PutUint64(ctr[:], counter)
	_, _ = h.Write(ctr[:])
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

func newJea9LinuxStackBuilder(cpu *CPU, stackTop uint64) jea9LinuxStackBuilder {
	if stackTop == 0 {
		stackTop = cpu.mem.Size() - Size1MB
	}
	return jea9LinuxStackBuilder{cpu: cpu, sp: stackTop &^ 15}
}

func (b *jea9LinuxStackBuilder) pushBytes(data []byte) (uint64, error) {
	if uint64(len(data)) > b.sp {
		return 0, errors.New("jea9linux: initial stack overflow")
	}
	b.sp -= uint64(len(data))
	if f := b.cpu.mem.WriteBytes(b.sp, data); f != nil {
		return 0, f
	}
	return b.sp, nil
}

func (b *jea9LinuxStackBuilder) pushString(s string) (uint64, error) {
	return b.pushBytes(append([]byte(s), 0))
}

func (b *jea9LinuxStackBuilder) pushStrings(strings []string) ([]uint64, error) {
	ptrs := make([]uint64, len(strings))
	for i := len(strings) - 1; i >= 0; i-- {
		p, err := b.pushString(strings[i])
		if err != nil {
			return nil, err
		}
		ptrs[i] = p
	}
	return ptrs, nil
}

func (b *jea9LinuxStackBuilder) writeInitialVector(argPtrs, envPtrs []uint64, aux []jea9LinuxAuxEntry) (uint64, error) {
	words := 1 + len(argPtrs) + 1 + len(envPtrs) + 1 + len(aux)*2 + 2
	bytes := uint64(words * 8)
	if bytes > b.sp {
		return 0, errors.New("jea9linux: initial stack overflow")
	}
	vector := (b.sp - bytes) &^ 15
	w := vector
	store := func(v uint64) error {
		if f := b.cpu.mem.Store64(w, v); f != nil {
			return f
		}
		w += 8
		return nil
	}
	if err := store(uint64(len(argPtrs))); err != nil {
		return 0, err
	}
	for _, p := range argPtrs {
		if err := store(p); err != nil {
			return 0, err
		}
	}
	if err := store(0); err != nil {
		return 0, err
	}
	for _, p := range envPtrs {
		if err := store(p); err != nil {
			return 0, err
		}
	}
	if err := store(0); err != nil {
		return 0, err
	}
	for _, entry := range aux {
		if err := store(entry.tag); err != nil {
			return 0, err
		}
		if err := store(entry.val); err != nil {
			return 0, err
		}
	}
	if err := store(jea9LinuxATNull); err != nil {
		return 0, err
	}
	if err := store(0); err != nil {
		return 0, err
	}
	return vector, nil
}

func (j *Jea9Linux) InitELFStack(cpu *CPU, ef *ELF, opts Jea9LinuxStartOptions) error {
	if ef == nil || ef.Header == nil {
		return errors.New("jea9linux: InitELFStack requires a loaded ELF with header")
	}
	args := append([]string(nil), opts.Args...)
	if len(args) == 0 {
		args = []string{""}
	}
	env := append([]string(nil), opts.Env...)
	execPath := opts.ExecPath
	if execPath == "" {
		execPath = args[0]
	}

	stack := newJea9LinuxStackBuilder(cpu, opts.StackTop)
	argPtrs, err := stack.pushStrings(args)
	if err != nil {
		return err
	}
	envPtrs, err := stack.pushStrings(env)
	if err != nil {
		return err
	}
	execfnPtr, err := stack.pushString(execPath)
	if err != nil {
		return err
	}
	platformPtr, err := stack.pushString(jea9LinuxPlatform)
	if err != nil {
		return err
	}
	auxRandom := j.randomBlock("auxv-random-v1", 0)
	randomPtr, err := stack.pushBytes(auxRandom[:16])
	if err != nil {
		return err
	}

	aux := buildJea9LinuxAuxv(ef, randomPtr, platformPtr, execfnPtr)
	vector, err := stack.writeInitialVector(argPtrs, envPtrs, aux)
	if err != nil {
		return err
	}

	cpu.SetReg(2, vector)
	cpu.SetPC(ef.Entry)
	return nil
}

func buildJea9LinuxAuxv(ef *ELF, randomPtr, platformPtr, execfnPtr uint64) []jea9LinuxAuxEntry {
	aux := []jea9LinuxAuxEntry{
		{jea9LinuxATPAGESZ, GuestPageSize},
		{jea9LinuxATPHENT, uint64(ef.Header.PhEntSize)},
		{jea9LinuxATPHNUM, uint64(ef.Header.PhNum)},
		{jea9LinuxATENTRY, ef.Entry},
		{jea9LinuxATUID, 0},
		{jea9LinuxATEUID, 0},
		{jea9LinuxATGID, 0},
		{jea9LinuxATEGID, 0},
		{jea9LinuxATPLAT, platformPtr},
		{jea9LinuxATHWCAP, 0},
		{jea9LinuxATCLKTCK, jea9LinuxClockTicksPerSecond},
		{jea9LinuxATSECURE, 0},
		{jea9LinuxATRANDOM, randomPtr},
		{jea9LinuxATHWCAP2, 0},
		{jea9LinuxATEXECFN, execfnPtr},
	}
	if phdr := elfProgramHeaderVA(ef); phdr != 0 {
		aux = append(aux, jea9LinuxAuxEntry{jea9LinuxATPHDR, phdr})
	}
	return aux
}

func elfProgramHeaderVA(ef *ELF) uint64 {
	if ef == nil || ef.Header == nil || ef.Data == nil {
		return 0
	}
	tableOff := ef.Header.PhOff
	tableEnd := tableOff + uint64(ef.Header.PhEntSize)*uint64(ef.Header.PhNum)
	for i := 0; i < int(ef.Header.PhNum); i++ {
		off := int(ef.Header.PhOff) + i*int(ef.Header.PhEntSize)
		if off+56 > len(ef.Data) {
			return 0
		}
		var ph Elf64Phdr
		if err := binary.Read(&byteReader{data: ef.Data[off:]}, binary.LittleEndian, &ph); err != nil {
			return 0
		}
		if ph.Type != ptLoad {
			continue
		}
		if tableOff >= ph.Offset && tableEnd <= ph.Offset+ph.FileSz {
			return ph.VAddr + (tableOff - ph.Offset)
		}
	}
	return 0
}

func (j *Jea9Linux) Run(cpu *CPU) error {
	before := cpu.RiscvInstrBegun()
	res, err := RunDefaultBudget(cpu, &cpu.Notes, j.instructionBudget)
	delta := cpu.RiscvInstrBegun() - before
	j.accountRetired(delta)
	if j.Blocked() {
		return ErrJea9LinuxBlocked
	}
	if err != nil {
		return err
	}
	switch res {
	case RunBudgetExpired:
		j.budgetYields++
		return ErrJea9LinuxBudget
	case RunBudgetExit:
		return nil
	default:
		return nil
	}
}

func (j *Jea9Linux) accountRetired(delta uint64) {
	if j.clockMode != Jea9ClockICTick || delta == 0 {
		return
	}
	j.monotonicNS += int64(delta) * j.nsPerInstruction
}

func (j *Jea9Linux) Handle(cpu *CPU, n Note) NoteDisposition {
	if !IsEcall(n) {
		return NoteForward
	}
	args := SyscallArgs{
		Num: cpu.Reg(17),
		A0:  cpu.Reg(10),
		A1:  cpu.Reg(11),
		A2:  cpu.Reg(12),
		A3:  cpu.Reg(13),
		A4:  cpu.Reg(14),
		A5:  cpu.Reg(15),
	}
	switch args.Num {
	case jea9LinuxSysFcntl:
		cpu.SetReg(10, uint64(j.sysFcntl(args.A0, args.A1, args.A2)))
		return NoteHandled
	case jea9LinuxSysOpenat:
		cpu.SetReg(10, uint64(j.sysOpenat(cpu, args.A0, args.A1, args.A2, args.A3)))
		return NoteHandled
	case jea9LinuxSysClose:
		cpu.SetReg(10, uint64(j.sysClose(args.A0)))
		return NoteHandled
	case jea9LinuxSysLseek:
		cpu.SetReg(10, uint64(j.sysLseek(args.A0, args.A1, args.A2)))
		return NoteHandled
	case jea9LinuxSysRead:
		cpu.SetReg(10, uint64(j.sysRead(cpu, args.A0, args.A1, args.A2)))
		return NoteHandled
	case jea9LinuxSysWrite:
		cpu.SetReg(10, uint64(j.sysWrite(cpu, args.A0, args.A1, args.A2)))
		return NoteHandled
	case jea9LinuxSysPread64:
		cpu.SetReg(10, uint64(j.sysPread64(cpu, args.A0, args.A1, args.A2, args.A3)))
		return NoteHandled
	case jea9LinuxSysExit, jea9LinuxSysExitGroup:
		cpu.ExitCode = int(int32(args.A0))
		return NoteExit
	case jea9LinuxSysClockGettime:
		cpu.SetReg(10, uint64(j.sysClockGettime(cpu, args.A0, args.A1)))
		return NoteHandled
	case jea9LinuxSysUname:
		cpu.SetReg(10, uint64(j.sysUname(cpu, args.A0)))
		return NoteHandled
	case jea9LinuxSysGetrlimit:
		cpu.SetReg(10, uint64(j.sysGetrlimit(cpu, args.A0, args.A1)))
		return NoteHandled
	case jea9LinuxSysPrctl:
		cpu.SetReg(10, uint64(j.sysPrctl(cpu, args.A0, args.A1, args.A2, args.A3, args.A4)))
		return NoteHandled
	case jea9LinuxSysGettimeofday:
		cpu.SetReg(10, uint64(j.sysGettimeofday(cpu, args.A0, args.A1)))
		return NoteHandled
	case jea9LinuxSysGetpid:
		cpu.SetReg(10, j.pid)
		return NoteHandled
	case jea9LinuxSysGettid:
		cpu.SetReg(10, j.tid)
		return NoteHandled
	case jea9LinuxSysSysinfo:
		cpu.SetReg(10, uint64(j.sysSysinfo(cpu, args.A0)))
		return NoteHandled
	case jea9LinuxSysPrlimit64:
		cpu.SetReg(10, uint64(j.sysPrlimit64(cpu, args.A0, args.A1, args.A2, args.A3)))
		return NoteHandled
	case jea9LinuxSysNanosleep:
		ret, blocked := j.sysNanosleep(cpu, args.A0, args.A1)
		if blocked {
			return NoteExit
		}
		cpu.SetReg(10, uint64(ret))
		return NoteHandled
	case jea9LinuxSysGetrandom:
		cpu.SetReg(10, uint64(j.sysGetrandom(cpu, args.A0, args.A1, args.A2)))
		return NoteHandled
	default:
		ret := jea9LinuxErrENOSYS
		cpu.SetReg(10, uint64(ret))
		return NoteHandled
	}
}

func (j *Jea9Linux) sysGetrandom(cpu *CPU, bufAddr, n, flags uint64) int64 {
	const supportedFlags = uint64(1 | 2) // GRND_NONBLOCK | GRND_RANDOM
	if flags&^supportedFlags != 0 {
		return jea9LinuxErrEINVAL
	}
	if n == 0 {
		return 0
	}
	if n > uint64(int(^uint(0)>>1)) {
		return jea9LinuxErrEINVAL
	}
	buf := make([]byte, int(n))
	j.fillRandom(buf)
	if f := cpu.mem.WriteBytes(bufAddr, buf); f != nil {
		return jea9LinuxErrEFAULT
	}
	return int64(n)
}

func (j *Jea9Linux) sysOpenat(cpu *CPU, dirfd, pathAddr, flags, mode uint64) int64 {
	_, _, _ = dirfd, flags, mode
	path, errno := readLinuxCString(cpu, pathAddr, 4096)
	if errno != 0 {
		return errno
	}
	if flags&3 != 0 {
		return jea9LinuxErrEACCES
	}
	switch path {
	case "/dev/urandom", "/dev/random":
		fd := j.nextFD
		j.nextFD++
		j.fds[fd] = jea9LinuxFD{kind: jea9LinuxFDRandom}
		return int64(fd)
	default:
		if data, ok := j.files[path]; ok {
			fd := j.nextFD
			j.nextFD++
			j.fds[fd] = jea9LinuxFD{kind: jea9LinuxFDFile, data: data}
			return int64(fd)
		}
		return jea9LinuxErrENOENT
	}
}

func (j *Jea9Linux) sysRead(cpu *CPU, fdRaw, bufAddr, n uint64) int64 {
	fd := int(int64(fdRaw))
	f, ok := j.fds[fd]
	if !ok {
		return jea9LinuxErrEBADF
	}
	if n == 0 {
		return 0
	}
	if n > uint64(int(^uint(0)>>1)) {
		return jea9LinuxErrEINVAL
	}
	switch f.kind {
	case jea9LinuxFDStdin:
		if j.stdin == nil {
			return 0
		}
		buf := make([]byte, int(n))
		nread, err := j.stdin.Read(buf)
		if nread < 0 {
			return jea9LinuxErrEIO
		}
		if nread > len(buf) {
			nread = len(buf)
		}
		if nread > 0 {
			if fault := cpu.mem.WriteBytes(bufAddr, buf[:nread]); fault != nil {
				return jea9LinuxErrEFAULT
			}
			return int64(nread)
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return jea9LinuxErrEIO
		}
		return 0
	case jea9LinuxFDRandom:
		buf := make([]byte, int(n))
		j.fillRandom(buf)
		if fault := cpu.mem.WriteBytes(bufAddr, buf); fault != nil {
			return jea9LinuxErrEFAULT
		}
		return int64(n)
	case jea9LinuxFDFile:
		count := readJea9LinuxFileRange(cpu, f, bufAddr, n, f.off)
		if count <= 0 {
			return count
		}
		f.off += count
		j.fds[fd] = f
		return count
	default:
		return jea9LinuxErrEBADF
	}
}

func (j *Jea9Linux) sysWrite(cpu *CPU, fdRaw, bufAddr, n uint64) int64 {
	fd := int(int64(fdRaw))
	f, ok := j.fds[fd]
	if !ok {
		return jea9LinuxErrEBADF
	}
	if n == 0 {
		return 0
	}
	if n > uint64(int(^uint(0)>>1)) {
		return jea9LinuxErrEINVAL
	}
	buf := make([]byte, int(n))
	if fault := cpu.mem.ReadBytes(bufAddr, buf); fault != nil {
		return jea9LinuxErrEFAULT
	}
	var w io.Writer
	switch f.kind {
	case jea9LinuxFDStdout:
		w = j.stdout
	case jea9LinuxFDStderr:
		w = j.stderr
	default:
		return jea9LinuxErrEBADF
	}
	written, err := w.Write(buf)
	if written < 0 {
		return jea9LinuxErrEIO
	}
	if written > len(buf) {
		written = len(buf)
	}
	if written == 0 && err != nil {
		return jea9LinuxErrEIO
	}
	return int64(written)
}

func (j *Jea9Linux) sysClose(fdRaw uint64) int64 {
	fd := int(int64(fdRaw))
	if _, ok := j.fds[fd]; !ok {
		return jea9LinuxErrEBADF
	}
	delete(j.fds, fd)
	return 0
}

func (j *Jea9Linux) sysFcntl(fdRaw, cmd, arg uint64) int64 {
	fd := int(int64(fdRaw))
	f, ok := j.fds[fd]
	if !ok {
		return jea9LinuxErrEBADF
	}
	switch cmd {
	case jea9LinuxFGetFD:
		return 0
	case jea9LinuxFSetFD:
		return 0
	case jea9LinuxFGetFL:
		return int64(f.flags)
	case jea9LinuxFSetFL:
		f.flags = arg
		j.fds[fd] = f
		return 0
	default:
		return jea9LinuxErrEINVAL
	}
}

func (j *Jea9Linux) sysLseek(fdRaw, offRaw, whence uint64) int64 {
	fd := int(int64(fdRaw))
	f, ok := j.fds[fd]
	if !ok {
		return jea9LinuxErrEBADF
	}
	if f.kind != jea9LinuxFDFile {
		return jea9LinuxErrESPIPE
	}
	off := int64(offRaw)
	var next int64
	switch whence {
	case jea9LinuxSeekSet:
		next = off
	case jea9LinuxSeekCur:
		next = f.off + off
	case jea9LinuxSeekEnd:
		next = int64(len(f.data)) + off
	default:
		return jea9LinuxErrEINVAL
	}
	if next < 0 {
		return jea9LinuxErrEINVAL
	}
	f.off = next
	j.fds[fd] = f
	return next
}

func (j *Jea9Linux) sysPread64(cpu *CPU, fdRaw, bufAddr, n, offRaw uint64) int64 {
	fd := int(int64(fdRaw))
	f, ok := j.fds[fd]
	if !ok {
		return jea9LinuxErrEBADF
	}
	if f.kind != jea9LinuxFDFile {
		return jea9LinuxErrESPIPE
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
	return readJea9LinuxFileRange(cpu, f, bufAddr, n, off)
}

func readJea9LinuxFileRange(cpu *CPU, f jea9LinuxFD, bufAddr, n uint64, off int64) int64 {
	if off < 0 {
		return jea9LinuxErrEINVAL
	}
	if off >= int64(len(f.data)) {
		return 0
	}
	remaining := int64(len(f.data)) - off
	count := int64(n)
	if count > remaining {
		count = remaining
	}
	if fault := cpu.mem.WriteBytes(bufAddr, f.data[off:off+count]); fault != nil {
		return jea9LinuxErrEFAULT
	}
	return count
}

func readLinuxCString(cpu *CPU, addr uint64, max int) (string, int64) {
	buf := make([]byte, 0, 64)
	for i := 0; i < max; i++ {
		b, f := cpu.mem.Load8(addr + uint64(i))
		if f != nil {
			return "", jea9LinuxErrEFAULT
		}
		if b == 0 {
			return string(buf), 0
		}
		buf = append(buf, b)
	}
	return "", jea9LinuxErrEINVAL
}

func (j *Jea9Linux) sysUname(cpu *CPU, addr uint64) int64 {
	const fieldLen = 65
	buf := make([]byte, fieldLen*6)
	storeFixedLinuxString(buf[0*fieldLen:], fieldLen, "Linux")
	storeFixedLinuxString(buf[1*fieldLen:], fieldLen, "jea9linux")
	storeFixedLinuxString(buf[2*fieldLen:], fieldLen, "6.14.0-jea9linux")
	storeFixedLinuxString(buf[3*fieldLen:], fieldLen, "#1 deterministic")
	storeFixedLinuxString(buf[4*fieldLen:], fieldLen, "riscv64")
	storeFixedLinuxString(buf[5*fieldLen:], fieldLen, "(none)")
	if f := cpu.mem.WriteBytes(addr, buf); f != nil {
		return jea9LinuxErrEFAULT
	}
	return 0
}

func storeFixedLinuxString(dst []byte, fieldLen int, s string) {
	if len(dst) > fieldLen {
		dst = dst[:fieldLen]
	}
	copy(dst, s)
}

func (j *Jea9Linux) sysGetrlimit(cpu *CPU, resource, addr uint64) int64 {
	cur, max, ok := jea9LinuxRlimit(resource)
	if !ok {
		return jea9LinuxErrEINVAL
	}
	return storeLinuxRlimit(cpu, addr, cur, max)
}

func (j *Jea9Linux) sysPrlimit64(cpu *CPU, pid, resource, newLimitAddr, oldLimitAddr uint64) int64 {
	if pid != 0 && pid != j.pid {
		return jea9LinuxErrESRCH
	}
	if newLimitAddr != 0 {
		return jea9LinuxErrEPERM
	}
	if oldLimitAddr != 0 {
		cur, max, ok := jea9LinuxRlimit(resource)
		if !ok {
			return jea9LinuxErrEINVAL
		}
		return storeLinuxRlimit(cpu, oldLimitAddr, cur, max)
	}
	_, _, ok := jea9LinuxRlimit(resource)
	if !ok {
		return jea9LinuxErrEINVAL
	}
	return 0
}

func jea9LinuxRlimit(resource uint64) (cur, max uint64, ok bool) {
	switch resource {
	case jea9LinuxRLimitStack:
		return 8 * 1024 * 1024, 8 * 1024 * 1024, true
	case jea9LinuxRLimitNOFile:
		return 1024, 1024, true
	case jea9LinuxRLimitAS:
		return ^uint64(0), ^uint64(0), true
	default:
		if resource < 16 {
			return ^uint64(0), ^uint64(0), true
		}
		return 0, 0, false
	}
}

func storeLinuxRlimit(cpu *CPU, addr, cur, max uint64) int64 {
	if f := cpu.mem.Store64(addr, cur); f != nil {
		return jea9LinuxErrEFAULT
	}
	if f := cpu.mem.Store64(addr+8, max); f != nil {
		return jea9LinuxErrEFAULT
	}
	return 0
}

func (j *Jea9Linux) sysSysinfo(cpu *CPU, addr uint64) int64 {
	buf := make([]byte, 112)
	uptime := j.monotonicNS / 1_000_000_000
	if uptime < 0 {
		uptime = 0
	}
	binary.LittleEndian.PutUint64(buf[0:], uint64(uptime))
	binary.LittleEndian.PutUint64(buf[32:], cpu.mem.Size())
	binary.LittleEndian.PutUint64(buf[40:], cpu.mem.Size())
	binary.LittleEndian.PutUint16(buf[80:], 1)
	binary.LittleEndian.PutUint32(buf[104:], 1)
	if f := cpu.mem.WriteBytes(addr, buf); f != nil {
		return jea9LinuxErrEFAULT
	}
	return 0
}

func (j *Jea9Linux) sysPrctl(cpu *CPU, option, arg2, arg3, arg4, arg5 uint64) int64 {
	_, _, _ = arg3, arg4, arg5
	switch option {
	case jea9LinuxPRSetName:
		name, errno := readLinuxThreadName(cpu, arg2)
		if errno != 0 {
			return errno
		}
		j.threadName = name
		return 0
	case jea9LinuxPRGetName:
		var buf [16]byte
		copy(buf[:], j.threadName)
		if f := cpu.mem.WriteBytes(arg2, buf[:]); f != nil {
			return jea9LinuxErrEFAULT
		}
		return 0
	case jea9LinuxPRSetVMA:
		return 0
	default:
		return jea9LinuxErrEINVAL
	}
}

func readLinuxThreadName(cpu *CPU, addr uint64) (string, int64) {
	var raw [16]byte
	for i := range raw {
		b, f := cpu.mem.Load8(addr + uint64(i))
		if f != nil {
			return "", jea9LinuxErrEFAULT
		}
		if b == 0 {
			return string(raw[:i]), 0
		}
		raw[i] = b
	}
	return string(raw[:15]), 0
}

func (j *Jea9Linux) sysClockGettime(cpu *CPU, clockID, tsAddr uint64) int64 {
	ns, ok := j.clockNow(clockID)
	if !ok {
		return jea9LinuxErrEINVAL
	}
	return storeLinuxTimespec(cpu, tsAddr, ns)
}

func (j *Jea9Linux) sysGettimeofday(cpu *CPU, tvAddr, tzAddr uint64) int64 {
	if tvAddr != 0 {
		ns := j.monotonicNS + j.realtimeOffsetNS
		sec, nsec := splitLinuxNS(ns)
		if f := cpu.mem.Store64(tvAddr, uint64(sec)); f != nil {
			return jea9LinuxErrEFAULT
		}
		if f := cpu.mem.Store64(tvAddr+8, uint64(nsec/1000)); f != nil {
			return jea9LinuxErrEFAULT
		}
	}
	if tzAddr != 0 {
		if f := cpu.mem.Store64(tzAddr, 0); f != nil {
			return jea9LinuxErrEFAULT
		}
		if f := cpu.mem.Store64(tzAddr+8, 0); f != nil {
			return jea9LinuxErrEFAULT
		}
	}
	return 0
}

func (j *Jea9Linux) sysNanosleep(cpu *CPU, reqAddr, remAddr uint64) (int64, bool) {
	_ = remAddr
	secRaw, f := cpu.mem.Load64(reqAddr)
	if f != nil {
		return jea9LinuxErrEFAULT, false
	}
	nsecRaw, f := cpu.mem.Load64(reqAddr + 8)
	if f != nil {
		return jea9LinuxErrEFAULT, false
	}
	sec := int64(secRaw)
	nsec := int64(nsecRaw)
	if sec < 0 || nsec < 0 || nsec >= 1_000_000_000 {
		return jea9LinuxErrEINVAL, false
	}
	delta := sec*1_000_000_000 + nsec
	if j.clockMode == Jea9ClockManual && delta > 0 {
		j.blocked = true
		j.blockedUntil = j.monotonicNS + delta
		return 0, true
	}
	j.monotonicNS += delta
	return 0, false
}

func (j *Jea9Linux) refreshBlocked() {
	if j.blocked && j.monotonicNS >= j.blockedUntil {
		j.blocked = false
	}
}

func (j *Jea9Linux) clockNow(clockID uint64) (int64, bool) {
	switch clockID {
	case jea9LinuxClockRealtime, jea9LinuxClockRealtimeCoarse:
		return j.monotonicNS + j.realtimeOffsetNS, true
	case jea9LinuxClockMonotonic, jea9LinuxClockMonotonicCoarse:
		return j.monotonicNS, true
	default:
		return 0, false
	}
}

func storeLinuxTimespec(cpu *CPU, addr uint64, ns int64) int64 {
	sec, nsec := splitLinuxNS(ns)
	if f := cpu.mem.Store64(addr, uint64(sec)); f != nil {
		return jea9LinuxErrEFAULT
	}
	if f := cpu.mem.Store64(addr+8, uint64(nsec)); f != nil {
		return jea9LinuxErrEFAULT
	}
	return 0
}

func splitLinuxNS(ns int64) (sec int64, nsec int64) {
	sec = ns / 1_000_000_000
	nsec = ns % 1_000_000_000
	if nsec < 0 {
		sec--
		nsec += 1_000_000_000
	}
	return sec, nsec
}

func InstallJea9Linux(cpu *CPU, j *Jea9Linux) func() {
	cpu.Notes.Push(j.Handle)
	return func() { cpu.Notes.Pop() }
}

func RunWithJea9Linux(cpu *CPU, j *Jea9Linux) (exitCode int, err error) {
	cleanup := InstallJea9Linux(cpu, j)
	defer cleanup()
	for {
		err = j.Run(cpu)
		if errors.Is(err, ErrJea9LinuxBudget) {
			continue
		}
		if err != nil {
			if ex, ok := err.(*ExitError); ok {
				return ex.Code, nil
			}
			return 0, err
		}
		return cpu.ExitCode, nil
	}
}
