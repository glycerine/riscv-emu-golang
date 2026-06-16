// Package goasm provides a thin API over Go's extracted obj instruction
// encoders. The Ctx (Assemble) path supports amd64 and arm64 host
// architectures; register-name re-exports cover all 9 backends shipped
// by Go (amd64, arm64, arm, loong64, mips, ppc64, riscv, s390x, wasm) so
// callers can construct cross-arch Progs by hand if they extend the
// New() switch in api.go.
//
// Lifecycle:
//
//	New() -> Append* -> Assemble() -> ( Reset() -> Append* -> Assemble() )*
//
// Each Ctx assembles exactly one text symbol named "jit_block". To
// produce multiple independent symbols, use multiple Ctx instances.
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
	"bufio"
	"fmt"
	"io"
	"runtime"
	"strings"
	"sync"

	"github.com/glycerine/riscv-emu-golang/goasm/obj"
	"github.com/glycerine/riscv-emu-golang/goasm/obj/arm64"
	"github.com/glycerine/riscv-emu-golang/goasm/obj/x86"
	"github.com/glycerine/riscv-emu-golang/goasm/objabi"
	"github.com/glycerine/riscv-emu-golang/goasm/src"
)

// Arch selects the target ISA.
type Arch int

const (
	AMD64 Arch = iota
	ARM64
)

// DefaultFlags is the bitmask passed to obj.InitTextSym for every Ctx
// unless overridden via Ctx.Flags. NOSPLIT|NOFRAME is correct for the
// trampoline-style JIT blocks goasm is designed to produce: such blocks
// own no Go stack frame and must not insert a runtime.morestack call
// (which would reference an unresolved symbol in our Link context and
// jump to garbage at run time).
const DefaultFlags = obj.NOSPLIT | obj.NOFRAME

// Ctx holds per-JIT-block assembler state.
// Create with New; reuse across blocks by calling Reset.
type Ctx struct {
	// Flags is the bitmask of obj.{NOSPLIT,NOFRAME,WRAPPER,...} attached
	// to the text symbol via InitTextSym at Assemble time. Defaults to
	// DefaultFlags. Set to 0 (or any custom value) before calling
	// Assemble to override.
	Flags int

	arch      Arch
	ctxt      *obj.Link
	sym       *obj.LSym
	firstProg *obj.Prog // the ATEXT prog (first appended)
	last      *obj.Prog // last appended Prog
	errors    []string
}

// goasmFatal is the value panicked from DiagFlush. Assemble's deferred
// recover converts it to a returned error so the host process is not
// killed by the log.Fatalf calls that follow DiagFlush in extracted
// obj/* code. A distinct type lets us re-panic anything else (real
// bugs) without swallowing it.
type goasmFatal struct {
	diags []string
}

func (e *goasmFatal) Error() string {
	if len(e.diags) == 0 {
		return "goasm: assembler raised fatal (no diagnostic recorded)"
	}
	return "goasm: assembler raised fatal: " + strings.Join(e.diags, "; ")
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
	// DiagFlush is invoked by the extracted obj/* code immediately
	// before log.Fatalf in unrecoverable error paths. We panic with a
	// typed value so Assemble's deferred recover converts the fatal
	// path into a normal error return — the log.Fatalf line itself is
	// left in the extracted source for reference, but the panic unwinds
	// the goroutine before it executes.
	c.ctxt.DiagFlush = func() {
		snapshot := append([]string(nil), c.errors...)
		panic(&goasmFatal{diags: snapshot})
	}
	// Bso would be dereferenced by ctxt.Logf in some of the same fatal
	// paths. Wire it to a discard writer so we don't nil-deref before
	// reaching DiagFlush.
	c.ctxt.Bso = bufio.NewWriter(io.Discard)
	// IsAsm tells the encoder we are emitting hand-written assembly:
	// disables 32-byte jump padding (which would silently insert NOPs)
	// and skips the auto-SPWRITE log.Fatalf path triggered by any Prog
	// that writes SP without setting Spadj.
	c.ctxt.IsAsm = true

	c.Flags = DefaultFlags
	c.sym = c.ctxt.LookupInit("jit_block", func(s *obj.LSym) {
		s.Type = objabi.STEXT
	})
	c.firstProg = nil
	c.last = nil
}

// Reset clears state so the Ctx can be reused for a new block.
// The underlying Link and its Prog arena slabs are retained — no new
// heap allocations as long as the next block fits in the same slabs.
func (c *Ctx) Reset() {
	c.errors = nil
	c.ctxt.ResetProgs()
	// Remove the old symbol and create a fresh one so Assemble's
	// "already called" guard (checks Func()==nil) passes cleanly.
	c.ctxt.DeleteSym("jit_block")
	c.sym = c.ctxt.LookupInit("jit_block", func(s *obj.LSym) {
		s.Type = objabi.STEXT
	})
	c.firstProg = nil
	c.last = nil
}

// NewProg allocates an uninitialized Prog linked to this context.
// (obj.Link.NewProg already wires p.Ctxt — no re-assignment needed.)
func (c *Ctx) NewProg() *obj.Prog { return c.ctxt.NewProg() }

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
	p.To.Offset = 0     // frame size 0; trampoline owns the frame
	p.To.Val = int32(0) // arg size
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

// Assemble encodes the Prog chain to native machine-code bytes and returns
// a detached heap-owned byte slice.
// Returns an error if any diagnostic was emitted during assembly or if the
// encoder hit a fatal path (which we convert from log.Fatalf to a normal
// error via DiagFlush+recover).
//
// One-shot: a Ctx may be assembled exactly once. To assemble another block,
// call Reset first.
func (c *Ctx) Assemble() (out []byte, err error) {
	out, err = c.AssembleView()
	if err != nil {
		return nil, err
	}
	detached := make([]byte, len(out))
	copy(detached, out)
	return detached, nil
}

// AssembleView encodes the Prog chain to native machine-code bytes and returns
// goasm-owned storage. The returned slice is valid until the Ctx is Reset or
// discarded. Callers that need to keep the bytes should use Assemble instead.
func (c *Ctx) AssembleView() (out []byte, err error) {
	return c.assembleInto(nil)
}

// AssembleInto encodes the Prog chain directly into dst and returns the used
// prefix. The returned slice aliases dst. If dst is too small, AssembleInto
// returns an error instead of allocating replacement storage.
func (c *Ctx) AssembleInto(dst []byte) (out []byte, err error) {
	if cap(dst) == 0 {
		return nil, fmt.Errorf("goasm: AssembleInto requires non-empty output capacity")
	}
	return c.assembleInto(dst[:0])
}

func (c *Ctx) assembleInto(dst []byte) (out []byte, err error) {
	if c.firstProg == nil {
		return nil, fmt.Errorf("goasm: empty prog list")
	}
	if c.firstProg.As != obj.ATEXT {
		return nil, fmt.Errorf("goasm: first Prog must be ATEXT (use Ctx.NewATEXT); got %v", c.firstProg.As)
	}
	if c.sym.Func() != nil {
		return nil, fmt.Errorf("goasm: Assemble already called on this Ctx; call Reset before re-assembling")
	}
	c.last.Link = nil // terminate the chain

	// Convert the encoder's fatal paths into a returned error. Any
	// non-goasmFatal panic is a real bug — re-raise so it surfaces.
	defer func() {
		if r := recover(); r != nil {
			switch x := r.(type) {
			case *goasmFatal:
				err = x
				out = nil
				return
			case *obj.FixedBufferTooSmall:
				err = x
				out = nil
				return
			}
			panic(r)
		}
	}()

	// InitTextSym initialises FuncInfo; Preprocess requires it.
	// Use src.NoXPos since we have no source position to report.
	// Flags default to NOSPLIT|NOFRAME (see DefaultFlags) so the
	// encoder does not inject runtime.morestack references for blocks
	// containing CALL instructions.
	c.ctxt.InitTextSym(c.sym, c.Flags, src.NoXPos)

	// Attach the prog chain to the symbol. The Go assembler (cmd/asm/asm.go)
	// does this immediately after InitTextSym: sym.Func().Text = atextProg.
	// Without this, Preprocess sees Func().Text == nil and returns immediately,
	// leaving sym.P empty.
	c.sym.Func().Text = c.firstProg

	if dst != nil {
		c.sym.P = dst[:0]
		c.sym.FixedP = true
	}

	// Run the stripped assembly pipeline (no DWARF, no PCLN).
	obj.AssembleBlock(c.ctxt, c.sym, c.ctxt.NewProg)

	if len(c.errors) > 0 {
		return nil, fmt.Errorf("goasm: assembly errors: %v", c.errors)
	}
	return c.sym.P, nil
}

// DumpProgs returns a human-readable listing of all appended Progs.
// Skips the ATEXT pseudo-instruction (which may not be fully initialized).
// NOP instructions that serve as branch targets are replaced with a
// label line showing their byte offset (e.g. "L_0x2e:"); other NOPs
// are omitted entirely. This matches what the assembler emits — it
// eliminates NOPs, so showing them would misalign with machine code.
func (c *Ctx) DumpProgs() string {
	// First pass: collect NOP Progs that are branch targets.
	nopTargets := map[*obj.Prog]bool{}
	for p := c.firstProg; p != nil; p = p.Link {
		if p.To.Type == obj.TYPE_BRANCH {
			if t := p.To.Target(); t != nil && t.As == obj.ANOP {
				nopTargets[t] = true
			}
		}
	}

	var sb strings.Builder
	for p := c.firstProg; p != nil; p = p.Link {
		if p.As == obj.ATEXT {
			continue
		}
		if p.As == obj.ANOP {
			if nopTargets[p] {
				fmt.Fprintf(&sb, " nopTarget ANOP p.Pc 0x%x (decimal %v):\n", p.Pc, int64(p.Pc))
			}
			continue
		}
		fmt.Fprintf(&sb, "[%5d]  %s\n", p.Pc, p.InstructionString())
	}
	return sb.String()
}

// Sym returns the text LSym (for setting up ATEXT manually if needed).
func (c *Ctx) Sym() *obj.LSym { return c.sym }

// Ctxt returns the raw Link context for advanced use.
func (c *Ctx) Ctxt() *obj.Link { return c.ctxt }

// First returns the head of the Prog chain (the ATEXT Prog), or nil
// if nothing has been appended yet. Walk via p.Link. Read-only access
// intended; to modify the chain use Peephole.
func (c *Ctx) First() *obj.Prog { return c.firstProg }

// Peephole walks adjacent Prog pairs (prev, curr) in the chain and
// invokes f. When f returns true, curr is unlinked from the chain and
// the next iteration compares the same prev against curr's successor.
// Mutations to prev or curr performed by f (via pointer) persist.
// Returns the number of Progs removed.
//
// Callers are responsible for ensuring that deleted Progs are not
// referenced outside the chain (e.g., as branch targets via
// p.To.Target, or as external pointers to MOVABS/label Progs). The
// ATEXT header Prog is never passed as curr.
//
// Safe to call only before Assemble.
func (c *Ctx) Peephole(f func(prev, curr *obj.Prog) bool) int {
	if c.firstProg == nil {
		return 0
	}
	removed := 0
	prev := c.firstProg
	for curr := prev.Link; curr != nil; {
		if f(prev, curr) {
			prev.Link = curr.Link
			if curr == c.last {
				c.last = prev
			}
			removed++
			curr = prev.Link
			continue
		}
		prev = curr
		curr = curr.Link
	}
	return removed
}

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

// initOnce serializes the first invocation of LinkArch.Init for each
// arch. Init bodies write to package-level globals (ycover, optab,
// oprange, …) without locking; concurrent first-time newLinkCtx calls
// would race on those writes. After the first run each Init body has a
// fast-path early return, so subsequent calls are safe — but Once still
// guarantees the visibility of the initialized tables to all
// goroutines.
var initOnce sync.Map // map[Arch]*sync.Once

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
	// Note: obj.Linknew already initialises Headtype from
	// buildcfg.GOOS (which falls back to runtime.GOOS when the GOOS env
	// var is unset). We override here so cross-tooling (e.g., a host
	// process with GOOS env var set to a different target) still gets
	// the host-correct headtype.
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
	// Init initialises architecture-specific instruction encoding tables
	// (e.g. x86.instinit for amd64). The compiler calls this from main;
	// we must call it explicitly here. Serialise via sync.Once per
	// arch so concurrent first-time calls don't race on the global
	// optab/ycover/oprange writes inside Init.
	if la.Init != nil {
		oncev, _ := initOnce.LoadOrStore(arch, new(sync.Once))
		oncev.(*sync.Once).Do(func() { la.Init(ctxt) })
	}
	return ctxt
}
