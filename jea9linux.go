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

var ErrJea9LinuxBudget = errors.New("jea9linux instruction budget expired")

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

	rootSeed      [32]byte
	randomCounter uint64
	randomBuf     [32]byte
	randomOff     int

	budgetYields uint64
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
}

func (j *Jea9Linux) SetMonotonicNS(ns int64) { j.monotonicNS = ns }

func (j *Jea9Linux) MonotonicNS() int64 { return j.monotonicNS }

func (j *Jea9Linux) BudgetYields() uint64 { return j.budgetYields }

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

func (j *Jea9Linux) Run(cpu *CPU) error {
	before := cpu.RiscvInstrBegun()
	res, err := RunDefaultBudget(cpu, &cpu.Notes, j.instructionBudget)
	delta := cpu.RiscvInstrBegun() - before
	j.accountRetired(delta)
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
	case 93, 94:
		cpu.ExitCode = int(int32(args.A0))
		return NoteExit
	default:
		cpu.SetReg(10, ^uint64(37)) // -ENOSYS
		return NoteHandled
	}
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
