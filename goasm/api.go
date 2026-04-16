// Package goasm provides a thin API over Go's extracted obj instruction
// encoders for amd64 and arm64. It is used by the riscv JIT backend to
// emit machine code without cgo or an external assembler.
//
// Typical usage:
//
//	c := goasm.New(goasm.AMD64)
//	c.Append(c.NewATEXT())
//	p := c.NewProg()
//	p.As = x86.AMOVQ
//	... set p.From / p.To ...
//	c.Append(p)
//	c.Append(c.NewRET())
//	bytes, err := c.Assemble()
package goasm

import (
	"fmt"
	"runtime"

	"riscv/goasm/obj"
	"riscv/goasm/obj/arm64"
	"riscv/goasm/obj/x86"
	"riscv/goasm/objabi"
	"riscv/goasm/src"
)

// Arch selects the target ISA.
type Arch int

const (
	AMD64 Arch = iota
	ARM64
)

// Ctx holds per-JIT-block assembler state.
// Create with New; reuse across blocks by calling Reset.
type Ctx struct {
	arch      Arch
	ctxt      *obj.Link
	sym       *obj.LSym
	firstProg *obj.Prog // the ATEXT prog (first appended)
	last      *obj.Prog // last appended Prog
	errors    []string
}

// New creates a fresh Ctx for the given architecture.
// The first Prog appended must be an ATEXT pseudo-instruction;
// use NewATEXT() to build it.
func New(arch Arch) *Ctx {
	c := &Ctx{arch: arch}
	c.init()
	return c
}

func (c *Ctx) init() {
	c.ctxt = newLinkCtx(c.arch)
	c.ctxt.DiagFunc = func(msg string, args ...any) {
		c.errors = append(c.errors, fmt.Sprintf(msg, args...))
	}
	c.sym = c.ctxt.LookupInit("jit_block", func(s *obj.LSym) {
		s.Type = objabi.STEXT
	})
	c.firstProg = nil
	c.last = nil
}

// Reset clears state so the Ctx can be reused for a new block.
func (c *Ctx) Reset() {
	c.errors = nil
	c.init()
}

// NewProg allocates an uninitialized Prog linked to this context.
func (c *Ctx) NewProg() *obj.Prog {
	p := c.ctxt.NewProg()
	p.Ctxt = c.ctxt
	return p
}

// NewATEXT builds the ATEXT pseudo-instruction that must be the first
// Prog in every function. InitTextSym + Preprocess use it to set up
// FuncInfo and sym.Func().Text.
func (c *Ctx) NewATEXT() *obj.Prog {
	p := c.NewProg()
	p.As = obj.ATEXT
	p.From.Type = obj.TYPE_MEM
	p.From.Sym = c.sym
	p.From.Name = obj.NAME_EXTERN
	p.To.Type = obj.TYPE_TEXTSIZE
	p.To.Offset = 0       // frame size 0; trampoline owns the frame
	p.To.Val = int32(0)   // arg size
	return p
}

// NewRET builds a RET instruction.
func (c *Ctx) NewRET() *obj.Prog {
	p := c.NewProg()
	p.As = obj.ARET
	return p
}

// Append adds p to the Prog chain.
// The first prog appended must be the ATEXT prog (from NewATEXT).
func (c *Ctx) Append(p *obj.Prog) {
	if c.last == nil {
		c.firstProg = p
	} else {
		c.last.Link = p
	}
	c.last = p
}

// Assemble encodes the Prog chain to native machine-code bytes.
// Returns an error if any diagnostic was emitted during assembly.
func (c *Ctx) Assemble() ([]byte, error) {
	if c.firstProg == nil {
		return nil, fmt.Errorf("goasm: empty prog list")
	}
	c.last.Link = nil // terminate the chain

	// InitTextSym initialises FuncInfo; Preprocess requires it.
	// Use src.NoXPos since we have no source position to report.
	c.ctxt.InitTextSym(c.sym, 0, src.NoXPos)

	// Run the stripped assembly pipeline (no DWARF, no PCLN).
	obj.AssembleBlock(c.ctxt, c.sym, c.ctxt.NewProg)

	if len(c.errors) > 0 {
		return nil, fmt.Errorf("goasm: assembly errors: %v", c.errors)
	}
	out := make([]byte, len(c.sym.P))
	copy(out, c.sym.P)
	return out, nil
}

// Sym returns the text LSym (for setting up ATEXT manually if needed).
func (c *Ctx) Sym() *obj.LSym { return c.sym }

// Ctxt returns the raw Link context for advanced use.
func (c *Ctx) Ctxt() *obj.Link { return c.ctxt }

// HostArch returns the Arch matching the current build host (GOARCH).
func HostArch() Arch {
	switch runtime.GOARCH {
	case "amd64":
		return AMD64
	case "arm64":
		return ARM64
	default:
		panic("goasm: unsupported host GOARCH: " + runtime.GOARCH)
	}
}

func newLinkCtx(arch Arch) *obj.Link {
	var la *obj.LinkArch
	switch arch {
	case AMD64:
		la = &x86.Linkamd64
	case ARM64:
		la = &arm64.Linkarm64
	default:
		panic(fmt.Sprintf("goasm: unsupported arch %d", arch))
	}
	ctxt := obj.Linknew(la)
	ctxt.Flag_optimize = false
	// Headtype must reflect the host OS so Preprocess makes the right
	// platform decisions (TLS rewriting, frame-pointer conventions, etc.).
	switch runtime.GOOS {
	case "darwin", "ios":
		ctxt.Headtype = objabi.Hdarwin
	case "linux":
		ctxt.Headtype = objabi.Hlinux
	case "windows":
		ctxt.Headtype = objabi.Hwindows
	default:
		ctxt.Headtype = objabi.Hlinux // safe default
	}
	return ctxt
}
