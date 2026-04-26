package ir

import (
	"fmt"
	"os"
)

// VIZJIT_DIR is a debug facility for viewing the assembly output.
// It is a directory path where disassembly will be dumped.
var VIZJIT_DIR string

func init() {
	home := os.Getenv("HOME")
	VIZJIT_DIR = home + "/go/src/github.com/glycerine/riscv-emu-golang/debug_vizjit_dir"

	off := os.Getenv("GOCPU_VIZJIT_OFF")
	if off != "" {
		VIZJIT_DIR = ""
		return
	}
	viz := os.Getenv("GOCPU_VIZJIT")
	if viz != "" {
		VIZJIT_DIR = viz
		fmt.Fprintf(os.Stderr, "env var GOCPU_VIZJIT was set: writing disassembly to dir: '%v'\n", viz)
	}
}

// InlineSyscall gates lowerSyscall's inline fast path. When true, a
// successful dispatcher return (RAX==0) chains directly to the
// post-ECALL block via the existing chain-exit machinery; a non-zero
// return falls through to the cold-path sret write + RET (today's
// behavior). When false, lowerSyscall unconditionally takes the
// cold path after the dispatcher CALL (bit-identical to pre-Step-5).
// Set from the root package's SetInlineEcallEnabled.
var InlineSyscall bool

// VReg is a virtual register index. 0 is reserved for "discard" (sink writes,
// zero reads — mirrors RISC-V's x0). Emitter allocates fresh VRegs via Tmp()
// or uses fixed IDs 1..31 for guest x1..x31, and 32..63 for f0..f31.
type VReg uint16

const (
	VRegZero      VReg = 0  // discard / x0
	VRegTempStart VReg = 64 // first allocatable temporary
)

// String returns a human-readable name for the VReg.
func (v VReg) String() string {
	switch {
	case v == VRegZero:
		return "v0"
	case v < 32:
		return fmt.Sprintf("x%d", v)
	case v < 64:
		return fmt.Sprintf("f%d", v-32)
	default:
		return fmt.Sprintf("t%d", v)
	}
}

// Type distinguishes operand sizes and classes.
type Type uint8

const (
	I8  Type = iota // 1-byte integer
	I16             // 2-byte integer
	I32             // 4-byte integer
	I64             // 8-byte integer
	F32             // 4-byte float
	F64             // 8-byte float
)

// Size returns the byte width of the type.
func (t Type) Size() int {
	switch t {
	case I8:
		return 1
	case I16:
		return 2
	case I32, F32:
		return 4
	case I64, F64:
		return 8
	default:
		panic(fmt.Sprintf("ir.Type.Size: unknown type %d", t))
	}
}

// String returns the type name.
func (t Type) String() string {
	switch t {
	case I8:
		return "i8"
	case I16:
		return "i16"
	case I32:
		return "i32"
	case I64:
		return "i64"
	case F32:
		return "f32"
	case F64:
		return "f64"
	default:
		return fmt.Sprintf("type(%d)", t)
	}
}

// Pred is a comparison predicate for IRBranch, IRSet, and IRFCmp.
type Pred uint8

const (
	EQ  Pred = iota // equal
	NE              // not equal
	LT              // signed less than
	LE              // signed less or equal
	GT              // signed greater than
	GE              // signed greater or equal
	LTU             // unsigned less than
	LEU             // unsigned less or equal
	GTU             // unsigned greater than
	GEU             // unsigned greater or equal
)

// String returns the predicate name.
func (p Pred) String() string {
	switch p {
	case EQ:
		return "eq"
	case NE:
		return "ne"
	case LT:
		return "lt"
	case LE:
		return "le"
	case GT:
		return "gt"
	case GE:
		return "ge"
	case LTU:
		return "ltu"
	case LEU:
		return "leu"
	case GTU:
		return "gtu"
	case GEU:
		return "geu"
	default:
		return fmt.Sprintf("pred(%d)", p)
	}
}

// Label identifies a jump target within a block.
type Label int64

// IROp enumerates the IR operations.
type IROp uint8

const (
	IROpInvalid IROp = iota // sentinel for uninitialized Instr

	// Memory ops
	IRLoad   // Dst = load[T](A + Imm)
	IRStore  // store[T](A + Imm, B)       — Dst unused
	IRLoadX  // Dst = load[T](A + B*Scale)
	IRStoreX // store[T](A + B*Scale, Dst)  — repurposes Dst as value

	// Integer arithmetic
	IRAdd    // Dst = A + B
	IRAddImm // Dst = A + Imm
	IRSub    // Dst = A - B
	IRSubImm // Dst = A - Imm
	IRMul    // Dst = A * B
	IRDivS   // Dst = (int64)A / (int64)B
	IRDivU   // Dst = (uint64)A / (uint64)B
	IRRem    // Dst = (int64)A % (int64)B
	IRRemU   // Dst = (uint64)A % (uint64)B
	IRMulHS  // Dst = signed high-64 of 128-bit A*B
	IRMulHU  // Dst = unsigned high-64 of 128-bit A*B
	IRMulHSU // Dst = signed×unsigned high-64
	IRNeg    // Dst = -A

	// Shifts
	IRShl    // Dst = A << B
	IRShlImm // Dst = A << Imm
	IRShr    // Dst = A >> B  (logical)
	IRShrImm // Dst = A >> Imm (logical)
	IRSar    // Dst = A >> B  (arithmetic)
	IRSarImm // Dst = A >> Imm (arithmetic)

	// Bitwise
	IRAnd    // Dst = A & B
	IRAndImm // Dst = A & Imm
	IROr     // Dst = A | B
	IROrImm  // Dst = A | Imm
	IRXor    // Dst = A ^ B
	IRXorImm // Dst = A ^ Imm
	IRNot    // Dst = ~A

	// Bit manipulation
	IRClz      // Dst = count leading zeros of A (type T: I32 or I64)
	IRCtz      // Dst = count trailing zeros of A (type T: I32 or I64)
	IRPopcount // Dst = population count of A (type T: I32 or I64)
	IRBswap    // Dst = byte-reverse of A (64-bit)

	// Comparison (produces 0/1 in Dst)
	IRSet    // Dst = (A pred B) ? 1 : 0
	IRSetImm // Dst = (A pred Imm) ? 1 : 0

	// Data movement
	IRMov   // Dst = A
	IRConst // Dst = Imm
	IRSext  // Dst = sign-extend A from T
	IRZext  // Dst = zero-extend A from T

	// Control flow
	IRLabel     // marks target; Imm = label ID
	IRBranch    // if (A pred B) goto label(Imm)
	IRBranchImm // if (A pred Imm2) goto label(Imm)
	IRJump      // goto label(Imm)
	IRCall      // call external symbol; Imm = CTab index
	IRRet       // return {pc=Imm, status=Imm2, faultAddr=A}
	IRRetDyn    // return {pc=A, status=Imm, faultAddr=B}  — dynamic PC from VReg
	IRChainExit // chain exit: {targetPC=Imm, exitIdx=Imm2}. WriteBackAll must precede.

	IRJalrIC // JALR site "inline cache" (vestigial name, and was never instruction count). Now better described as: JALR indirect jump via decoder-cache lookup (the old 2-slot IC is deprecated). {targetVReg=A, siteIdx=Imm}. WriteBackAll must precede.

	IRSyscall // ECALL fast path. Imm=pc+4 (resume), Imm2=CTab index for dispatcher sym.
	// Calls the SysV-ABI dispatcher with (xBase, memBase, memMask), writes sret with
	// Status=RAX (0=jitOK, 1=jitEcall), and returns. Terminator. WriteBackAll must precede.

	IRMisalignLoad  // Dst = byte-by-byte load(addr=A, width=T). Lowerer inlines using [RBP+520/528] for memBase/memMask.
	IRMisalignStore // byte-by-byte store(addr=A, value=B, width=T). Lowerer inlines using [RBP+520/528].

	// Floating point
	IRFAdd      // Dst = A + B       (FP, type T)
	IRFSub      // Dst = A - B       (FP)
	IRFMul      // Dst = A * B       (FP)
	IRFDiv      // Dst = A / B       (FP)
	IRFSqrt     // Dst = sqrt(A)     (FP)
	IRFma       // Dst = A*B + C     (FP, fused single-rounding, §11.6)
	IRFmsub     // Dst = A*B - C     (FP, fused)
	IRFnmadd    // Dst = -(A*B + C)  (FP, fused, RISC-V FNMADD)
	IRFnmsub    // Dst = -(A*B - C) = -A*B + C (FP, fused, RISC-V FNMSUB)
	IRFCmp      // Dst = (A pred B) ? 1 : 0  (FP compare)
	IRFNeg      // Dst = -A          (FP)
	IRFAbs      // Dst = |A|         (FP)
	IRFCvtToI   // Dst(int) = convert(A(FP))      T=dst type, U=src FP type
	IRFCvtToU   // Dst(uint) = convert(A(FP))
	IRFCvtFromI // Dst(FP) = convert(A(int))       T=dst FP type, U=src int type
	IRFCvtFromU // Dst(FP) = convert(A(uint))
	IRFCvtFF    // Dst = convert(A)  F32↔F64       T=dst, U=src

	// Pseudo-ops
	IRMarkLive  // declares A live here (allocator hint)
	IRMarkDead  // declares A dead here (allocator hint)
	IRWriteback // writes dirty vregs back to x[] array

	// Count of IR ops (not a valid op).
	irOpCount
)

// String returns the op mnemonic.
func (op IROp) String() string {
	if int(op) < len(irOpNames) {
		return irOpNames[op]
	}
	return fmt.Sprintf("op(%d)", op)
}

var irOpNames = [...]string{
	IROpInvalid: "invalid",
	IRLoad:      "load",
	IRStore:     "store",
	IRLoadX:     "loadx",
	IRStoreX:    "storex",
	IRAdd:       "add",
	IRAddImm:    "add_imm",
	IRSub:       "sub",
	IRSubImm:    "sub_imm",
	IRMul:       "mul",
	IRDivS:      "divs",
	IRDivU:      "divu",
	IRRem:       "rem",
	IRRemU:      "remu",
	IRMulHS:     "mulhs",
	IRMulHU:     "mulhu",
	IRMulHSU:    "mulhsu",
	IRNeg:       "neg",
	IRClz:       "clz",
	IRCtz:       "ctz",
	IRPopcount:  "popcnt",
	IRBswap:     "bswap",
	IRShl:       "shl",
	IRShlImm:    "shl_imm",
	IRShr:       "shr",
	IRShrImm:    "shr_imm",
	IRSar:       "sar",
	IRSarImm:    "sar_imm",
	IRAnd:       "and",
	IRAndImm:    "and_imm",
	IROr:        "or",
	IROrImm:     "or_imm",
	IRXor:       "xor",
	IRXorImm:    "xor_imm",
	IRNot:       "not",
	IRSet:       "set",
	IRSetImm:    "set_imm",
	IRMov:       "mov",
	IRConst:     "const",
	IRSext:      "sext",
	IRZext:      "zext",
	IRLabel:     "label",
	IRBranch:    "branch",
	IRBranchImm: "branch_imm",
	IRJump:      "jump",
	IRCall:      "call",
	IRRet:       "ret",
	IRRetDyn:    "ret_dyn",
	IRChainExit: "chain_exit",
	IRJalrIC:    "jalr_ic",
	IRSyscall:   "syscall",
	IRFAdd:      "fadd",
	IRFSub:      "fsub",
	IRFMul:      "fmul",
	IRFDiv:      "fdiv",
	IRFSqrt:     "fsqrt",
	IRFma:       "fma",
	IRFmsub:     "fmsub",
	IRFnmadd:    "fnmadd",
	IRFnmsub:    "fnmsub",
	IRFCmp:      "fcmp",
	IRFNeg:      "fneg",
	IRFAbs:      "fabs",
	IRFCvtToI:   "fcvt_to_i",
	IRFCvtToU:   "fcvt_to_u",
	IRFCvtFromI: "fcvt_from_i",
	IRFCvtFromU: "fcvt_from_u",
	IRFCvtFF:    "fcvt_ff",
	IRMarkLive:  "mark_live",
	IRMarkDead:  "mark_dead",
	IRWriteback: "writeback",
}

// IRInstr is one IR operation. Fixed-size struct (no slices) for cache locality.
type IRInstr struct {
	Op    IROp
	T     Type  // operand type
	U     Type  // secondary type (for conversions)
	Pred  Pred  // comparison predicate (IRBranch, IRSet, IRFCmp)
	Scale uint8 // 1/2/4/8 for IRLoadX/IRStoreX
	Dst   VReg
	A     VReg
	B     VReg // also: value for IRStore, index for IRLoadX/IRStoreX
	C     VReg // third source for ternary ops (IRFma: Dst = A*B + C)
	Imm   int64
	Imm2  int64 // for IRBranchImm (compare value), IRRet (status)
}

// String returns a human-readable disassembly of the instruction.
func (ins IRInstr) String() string {
	switch ins.Op {
	case IRConst:
		return fmt.Sprintf("%s %s = %d", ins.Op, ins.Dst, ins.Imm)
	case IRMov, IRNeg, IRNot, IRSext, IRZext, IRFNeg, IRFAbs, IRFSqrt:
		return fmt.Sprintf("%s.%s %s = %s", ins.Op, ins.T, ins.Dst, ins.A)
	case IRAddImm, IRSubImm, IRShlImm, IRShrImm, IRSarImm,
		IRAndImm, IROrImm, IRXorImm:
		return fmt.Sprintf("%s.%s %s = %s, %d", ins.Op, ins.T, ins.Dst, ins.A, ins.Imm)
	case IRSet, IRFCmp:
		return fmt.Sprintf("%s.%s.%s %s = %s, %s", ins.Op, ins.T, ins.Pred, ins.Dst, ins.A, ins.B)
	case IRSetImm:
		return fmt.Sprintf("%s.%s.%s %s = %s, %d", ins.Op, ins.T, ins.Pred, ins.Dst, ins.A, ins.Imm)
	case IRLoad:
		return fmt.Sprintf("%s.%s %s = [%s + %d]", ins.Op, ins.T, ins.Dst, ins.A, ins.Imm)
	case IRStore:
		return fmt.Sprintf("%s.%s [%s + %d] = %s", ins.Op, ins.T, ins.A, ins.Imm, ins.B)
	case IRLabel:
		return fmt.Sprintf("L%d:", ins.Imm)
	case IRBranch:
		return fmt.Sprintf("%s.%s %s, %s -> L%d", ins.Op, ins.Pred, ins.A, ins.B, ins.Imm)
	case IRBranchImm:
		return fmt.Sprintf("%s.%s %s, %d -> L%d", ins.Op, ins.Pred, ins.A, ins.Imm2, ins.Imm)
	case IRJump:
		return fmt.Sprintf("%s -> L%d", ins.Op, ins.Imm)
	case IRCall:
		return fmt.Sprintf("%s [%d]", ins.Op, ins.Imm)
	case IRRet:
		return fmt.Sprintf("%s pc=%d status=%d fault=%s", ins.Op, ins.Imm, ins.Imm2, ins.A)
	case IRRetDyn:
		return fmt.Sprintf("%s pc=%s status=%d fault=%s", ins.Op, ins.A, ins.Imm, ins.B)
	case IRJalrIC:
		return fmt.Sprintf("%s target=%s site=%d", ins.Op, ins.A, ins.Imm)
	case IRSyscall:
		return fmt.Sprintf("%s resumePC=0x%x ctab=%d", ins.Op, uint64(ins.Imm), ins.Imm2)
	case IRChainExit:
		return fmt.Sprintf("%s targetPC=0x%x exitIdx=%d", ins.Op, uint64(ins.Imm), ins.Imm2)
	default:
		if ins.B != VRegZero {
			return fmt.Sprintf("%s.%s %s = %s, %s", ins.Op, ins.T, ins.Dst, ins.A, ins.B)
		}
		return fmt.Sprintf("%s.%s %s = %s", ins.Op, ins.T, ins.Dst, ins.A)
	}
}

// Block holds the IR for a single JIT block.
type Block struct {
	Instrs    []IRInstr
	Labels    map[Label]int // label ID -> index in Instrs where IRLabel sits
	NextLabel Label         // fresh label allocator
	CTab      []CSym        // external call symbols
	VRegLive  []VRegLiveness

	maxVreg VReg // uint16
}

func (b *Block) appendIns(ins IRInstr) {
	b.Instrs = append(b.Instrs, ins)
	if ins.Dst > b.maxVreg {
		b.maxVreg = ins.Dst
	}
	if ins.A > b.maxVreg {
		b.maxVreg = ins.A
	}
	if ins.B > b.maxVreg {
		b.maxVreg = ins.B
	}
}

// NewBlock allocates a Block with an initialized Labels map.
func NewBlock() *Block {
	return &Block{
		Labels: make(map[Label]int),
	}
}

// CSym describes an external C ABI symbol.
type CSym struct {
	Name string
	Addr uintptr
}

// VRegLiveness records the start and end instruction index for a VReg's live range.
type VRegLiveness struct {
	Start, End int
}

// typeWidth converts a Type to its byte width.
func typeWidth(t Type) int {
	switch t {
	case I8:
		return 1
	case I16:
		return 2
	case I32, F32:
		return 4
	case I64, F64:
		return 8
	default:
		return 8
	}
}

// WidthToType converts a byte width (1, 2, 4, 8) to the corresponding integer Type.
func WidthToType(width int) Type {
	switch width {
	case 1:
		return I8
	case 2:
		return I16
	case 4:
		return I32
	case 8:
		return I64
	default:
		panic(fmt.Sprintf("ir.WidthToType: unsupported width %d", width))
	}
}
