package riscv

import "errors"

// ErrEbreak is returned by CPU.Run when the guest executes EBREAK.
// Callers treat this as a clean halt rather than an error.
var ErrEbreak = errors.New("ebreak")

// ErrIllegalInstruction is returned when an unrecognised opcode is fetched.
var ErrIllegalInstruction = errors.New("illegal instruction")

// CPU is a single RV64I hart.
type CPU struct {
	pc  uint64
	x   [32]uint64 // integer registers; x[0] is always 0
	mem *GuestMemory
}

// NewCPU creates a CPU backed by the given GuestMemory.
// PC starts at 0; all registers are zero.
func NewCPU(mem *GuestMemory) *CPU {
	return &CPU{mem: mem}
}

// SetPC sets the program counter.
func (c *CPU) SetPC(addr uint64) { c.pc = addr }

// PC returns the current program counter.
func (c *CPU) PC() uint64 { return c.pc }

// SetReg sets integer register r to v. Writes to x0 are silently ignored.
func (c *CPU) SetReg(r uint8, v uint64) {
	if r != 0 {
		c.x[r] = v
	}
}

// Reg returns the value of integer register r. x0 always returns 0.
func (c *CPU) Reg(r uint8) uint64 {
	if r == 0 {
		return 0
	}
	return c.x[r]
}

// Run executes instructions until EBREAK or an error.
// Returns ErrEbreak on a clean EBREAK halt.
func (c *CPU) Run() error {
	for {
		if err := c.step(); err != nil {
			return err
		}
	}
}

// step fetches and executes one instruction, advancing PC.
func (c *CPU) step() error {
	// Fetch
	insn, f := c.mem.Fetch32(c.pc)
	if f != nil {
		return f
	}

	// Decode standard fields
	opcode := uint8(insn & 0x7F)
	rd     := uint8((insn >> 7) & 0x1F)
	funct3 := uint8((insn >> 12) & 0x07)
	rs1    := uint8((insn >> 15) & 0x1F)
	rs2    := uint8((insn >> 20) & 0x1F)

	nextPC := c.pc + 4

	switch opcode {

	case 0x03: // LOAD
		imm := int64(insn) >> 20 // sign-extended I-imm
		addr := c.Reg(rs1) + uint64(imm)
		switch funct3 {
		case 0x02: // LW — sign-extend 32→64
			v, f := c.mem.Load32(addr)
			if f != nil { return f }
			c.SetReg(rd, uint64(int64(int32(v))))
		default:
			return ErrIllegalInstruction
		}

	case 0x23: // STORE
		// S-type immediate: imm[11:5]=insn[31:25], imm[4:0]=insn[11:7]
		imm := int64((insn&0xFE000000)>>20) | int64((insn>>7)&0x1F)
		addr := c.Reg(rs1) + uint64(imm)
		switch funct3 {
		case 0x02: // SW
			if f := c.mem.Store32(addr, uint32(c.Reg(rs2))); f != nil { return f }
		default:
			return ErrIllegalInstruction
		}

	case 0x13: // OP-IMM
		imm := int64(insn) >> 20
		switch funct3 {
		case 0x00: // ADDI
			c.SetReg(rd, c.Reg(rs1)+uint64(imm))
		default:
			return ErrIllegalInstruction
		}

	case 0x73: // SYSTEM
		funct12 := insn >> 20
		switch funct12 {
		case 0x001: // EBREAK
			c.pc = nextPC
			return ErrEbreak
		default:
			return ErrIllegalInstruction
		}

	default:
		return ErrIllegalInstruction
	}

	c.pc = nextPC
	return nil
}
