package riscv

import (
	"encoding/binary"
	"unsafe"
)

const (
	RAX = 0
	RCX = 1
	RDX = 2
	RBX = 3
	RSP = 4
	RBP = 5
	RSI = 6
	RDI = 7
	R8  = 8
	R9  = 9
	R10 = 10
	R11 = 11
	R12 = 12
	R13 = 13
	R14 = 14
	R15 = 15
)

type CodeBuilder struct {
	buf []byte
	off int
}

func NewCodeBuilder() (*CodeBuilder, error) {
	buf, err := mmapExec(4096)
	if err != nil {
		return nil, err
	}
	return &CodeBuilder{buf: buf}, nil
}

func (c *CodeBuilder) Free() error {
	return munmapExec(c.buf)
}

func (c *CodeBuilder) Addr() uintptr {
	return uintptr(unsafe.Pointer(&c.buf[0]))
}

func (c *CodeBuilder) Len() int { return c.off }

func (c *CodeBuilder) Reset() { c.off = 0 }

func (c *CodeBuilder) emit(b ...byte) {
	copy(c.buf[c.off:], b)
	c.off += len(b)
}

func (c *CodeBuilder) imm32(v int32) {
	binary.LittleEndian.PutUint32(c.buf[c.off:], uint32(v))
	c.off += 4
}

func (c *CodeBuilder) imm64(v uint64) {
	binary.LittleEndian.PutUint64(c.buf[c.off:], v)
	c.off += 8
}

func (c *CodeBuilder) Movabs(reg int, imm uint64) {
	if reg >= 8 {
		c.emit(0x49)
	} else {
		c.emit(0x48)
	}
	c.emit(0xB8 + byte(reg&7))
	c.imm64(imm)
}

// StoreToRBP emits MOV [RBP+disp], srcReg (64-bit).
// RBP with mod=00 is RIP-relative, so we always use mod=01 or mod=10.
func (c *CodeBuilder) StoreToRBP(srcReg, disp int) {
	rex := byte(0x48)
	if srcReg >= 8 {
		rex |= 0x04
	}
	c.emit(rex, 0x89)
	regBits := byte(srcReg&7) << 3
	if disp >= -128 && disp <= 127 {
		c.emit(0x45|regBits, byte(int8(disp)))
	} else {
		c.emit(0x85 | regBits)
		c.imm32(int32(disp))
	}
}

// LoadFromRBP emits MOV dstReg, [RBP+disp] (64-bit).
func (c *CodeBuilder) LoadFromRBP(dstReg, disp int) {
	rex := byte(0x48)
	if dstReg >= 8 {
		rex |= 0x04
	}
	c.emit(rex, 0x8B)
	regBits := byte(dstReg&7) << 3
	if disp >= -128 && disp <= 127 {
		c.emit(0x45|regBits, byte(int8(disp)))
	} else {
		c.emit(0x85 | regBits)
		c.imm32(int32(disp))
	}
}

func (c *CodeBuilder) AddReg(dst, src int) {
	rex := byte(0x48)
	if src >= 8 {
		rex |= 0x04
	}
	if dst >= 8 {
		rex |= 0x01
	}
	c.emit(rex, 0x01, 0xC0|byte(src&7)<<3|byte(dst&7))
}

func (c *CodeBuilder) SubReg(dst, src int) {
	rex := byte(0x48)
	if src >= 8 {
		rex |= 0x04
	}
	if dst >= 8 {
		rex |= 0x01
	}
	c.emit(rex, 0x29, 0xC0|byte(src&7)<<3|byte(dst&7))
}

func (c *CodeBuilder) Callback(goFunc uintptr) {
	c.Movabs(R11, uint64(gocallAddr))
	c.emit(0x4C, 0x8D, 0x15, 0x11, 0x00, 0x00, 0x00) // LEA R10,[RIP+17]
	c.emit(0x4C, 0x89, 0x14, 0x24)                     // MOV [RSP],R10
	c.Movabs(R10, uint64(goFunc))
	c.emit(0x41, 0xFF, 0xE3) // JMP R11
}

func (c *CodeBuilder) Exit() {
	c.emit(0x48, 0x8B, 0x5C, 0x24, 0x08) // MOV RBX, [RSP+0x08]
	c.emit(0x4C, 0x8B, 0x64, 0x24, 0x18) // MOV R12, [RSP+0x18]
	c.emit(0x4C, 0x8B, 0x6C, 0x24, 0x20) // MOV R13, [RSP+0x20]
	c.emit(0x4C, 0x8B, 0x7C, 0x24, 0x28) // MOV R15, [RSP+0x28]
	c.emit(0x48, 0x81, 0xC4, 0xF8, 0xFF, 0x00, 0x00) // ADD RSP, 0xFFF8
	c.emit(0x5D) // POP RBP
	c.emit(0xC3) // RET
}

func (c *CodeBuilder) MovRegReg(dst, src int) {
	rex := byte(0x48)
	if src >= 8 {
		rex |= 0x04
	}
	if dst >= 8 {
		rex |= 0x01
	}
	c.emit(rex, 0x89, 0xC0|byte(src&7)<<3|byte(dst&7))
}
