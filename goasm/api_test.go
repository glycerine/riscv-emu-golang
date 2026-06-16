package goasm_test

import (
	"bytes"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"unsafe"

	"github.com/glycerine/riscv-emu-golang/goasm"
	"github.com/glycerine/riscv-emu-golang/goasm/obj"
	"github.com/glycerine/riscv-emu-golang/goasm/obj/arm64"
	"github.com/glycerine/riscv-emu-golang/goasm/obj/x86"
)

// ─── helpers ─────────────────────────────────────────────────────────────────

func amd64Ctx(t *testing.T) *goasm.Ctx {
	t.Helper()
	c := goasm.New(goasm.AMD64)
	c.Append(c.NewATEXT())
	return c
}

func arm64Ctx(t *testing.T) *goasm.Ctx {
	t.Helper()
	c := goasm.New(goasm.ARM64)
	c.Append(c.NewATEXT())
	return c
}

// immReg builds a Prog with a register destination and an immediate source.
func immReg(c *goasm.Ctx, as obj.As, imm int64, dstreg int16) *obj.Prog {
	p := c.NewProg()
	p.As = as
	p.From.Type = obj.TYPE_CONST
	p.From.Offset = imm
	p.To.Type = obj.TYPE_REG
	p.To.Reg = dstreg
	return p
}

// regReg builds a Prog with register source and register destination.
func regReg(c *goasm.Ctx, as obj.As, srcreg, dstreg int16) *obj.Prog {
	p := c.NewProg()
	p.As = as
	p.From.Type = obj.TYPE_REG
	p.From.Reg = srcreg
	p.To.Type = obj.TYPE_REG
	p.To.Reg = dstreg
	return p
}

// memLoad builds a load: dst = mem[base+disp]
func memLoad(c *goasm.Ctx, as obj.As, base int16, disp int64, dstreg int16) *obj.Prog {
	p := c.NewProg()
	p.As = as
	p.From.Type = obj.TYPE_MEM
	p.From.Reg = base
	p.From.Offset = disp
	p.From.Name = obj.NAME_NONE
	p.To.Type = obj.TYPE_REG
	p.To.Reg = dstreg
	return p
}

// memStore builds a store: mem[base+disp] = imm
func memStoreImm(c *goasm.Ctx, as obj.As, imm int64, base int16, disp int64) *obj.Prog {
	p := c.NewProg()
	p.As = as
	p.From.Type = obj.TYPE_CONST
	p.From.Offset = imm
	p.To.Type = obj.TYPE_MEM
	p.To.Reg = base
	p.To.Offset = disp
	p.To.Name = obj.NAME_NONE
	return p
}

func TestAssembleInto_AliasesDestinationAMD64(t *testing.T) {
	c := amd64Ctx(t)
	c.Append(immReg(c, x86.AMOVQ, 0x12345678, x86.REG_AX))
	c.Append(c.NewRET())
	want, err := c.Assemble()
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}

	c = amd64Ctx(t)
	c.Append(immReg(c, x86.AMOVQ, 0x12345678, x86.REG_AX))
	c.Append(c.NewRET())
	buf := make([]byte, len(want)+16)
	got, err := c.AssembleInto(buf)
	if err != nil {
		t.Fatalf("AssembleInto: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("AssembleInto produced empty output")
	}
	if &got[0] != &buf[0] {
		t.Fatal("AssembleInto output does not alias destination")
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("AssembleInto bytes mismatch:\n got % X\nwant % X", got, want)
	}
}

func TestAssembleInto_SmallBufferErrorsAMD64(t *testing.T) {
	c := amd64Ctx(t)
	c.Append(immReg(c, x86.AMOVQ, 0x12345678, x86.REG_AX))
	c.Append(c.NewRET())
	_, err := c.AssembleInto(make([]byte, 0, 1))
	if err == nil {
		t.Fatal("AssembleInto succeeded with too-small buffer")
	}
	if !strings.Contains(err.Error(), "fixed output buffer too small") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestAssembleInto_SmallBufferErrorsARM64(t *testing.T) {
	c := arm64Ctx(t)
	p := c.NewProg()
	p.As = arm64.AMOVD
	p.From.Type = obj.TYPE_CONST
	p.From.Offset = 0x1234
	p.To.Type = obj.TYPE_REG
	p.To.Reg = arm64.REG_R0
	c.Append(p)
	c.Append(c.NewRET())
	_, err := c.AssembleInto(make([]byte, 0, 1))
	if err == nil {
		t.Fatal("AssembleInto succeeded with too-small buffer")
	}
	if !strings.Contains(err.Error(), "fixed output buffer too small") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func hexFmt(b []byte) string {
	if len(b) == 0 {
		return "<empty>"
	}
	return fmt.Sprintf("% X", b)
}

// assertBytes verifies got starts with want, and that any trailing
// bytes (alignment padding the encoder may append, notably arm64) are
// all zeros. This is stricter than the previous TrimRight approach,
// which silently accepted any non-zero junk after a leading zero.
func assertBytes(t *testing.T, got, want []byte) {
	t.Helper()
	if len(got) < len(want) {
		t.Errorf("output shorter than expected: got %d bytes %s, want %d bytes %s",
			len(got), hexFmt(got), len(want), hexFmt(want))
		return
	}
	if !bytes.Equal(got[:len(want)], want) {
		t.Errorf("bytes mismatch:\n  got  %s\n  want %s",
			hexFmt(got[:len(want)]), hexFmt(want))
		return
	}
	for i, b := range got[len(want):] {
		if b != 0 {
			t.Errorf("non-zero trailing byte at offset %d: 0x%02X (full output: %s)",
				len(want)+i, b, hexFmt(got))
			return
		}
	}
}

// ─── AMD64 byte tests ─────────────────────────────────────────────────────────
// Expected bytes verified via:
//   GOARCH=amd64 GOOS=linux go tool asm -o /tmp/t.o /tmp/t.s
//   GOARCH=amd64 GOOS=linux go tool objdump /tmp/t.o

func TestAMD64_MOVQ_const_AX_RET(t *testing.T) {
	c := amd64Ctx(t)
	c.Append(immReg(c, x86.AMOVQ, 0x42, x86.REG_AX))
	c.Append(c.NewRET())
	got, err := c.Assemble()
	if err != nil {
		t.Fatal(err)
	}
	// MOVQ $0x42, AX = 48 C7 C0 42 00 00 00; RET = C3
	want := []byte{0x48, 0xC7, 0xC0, 0x42, 0x00, 0x00, 0x00, 0xC3}
	assertBytes(t, got, want)
}

func TestAMD64_ADDQ_RR(t *testing.T) {
	c := amd64Ctx(t)
	c.Append(immReg(c, x86.AMOVQ, 3, x86.REG_AX))
	c.Append(immReg(c, x86.AMOVQ, 4, x86.REG_BX))
	c.Append(regReg(c, x86.AADDQ, x86.REG_BX, x86.REG_AX))
	c.Append(c.NewRET())
	got, err := c.Assemble()
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{
		0x48, 0xC7, 0xC0, 0x03, 0x00, 0x00, 0x00, // MOVQ $3, AX
		0x48, 0xC7, 0xC3, 0x04, 0x00, 0x00, 0x00, // MOVQ $4, BX
		0x48, 0x01, 0xD8, // ADDQ BX, AX
		0xC3, // RET
	}
	assertBytes(t, got, want)
}

func TestAMD64_SUBQ_imm(t *testing.T) {
	c := amd64Ctx(t)
	c.Append(immReg(c, x86.AMOVQ, 10, x86.REG_AX))
	p := c.NewProg()
	p.As = x86.ASUBQ
	p.From.Type = obj.TYPE_CONST
	p.From.Offset = 3
	p.To.Type = obj.TYPE_REG
	p.To.Reg = x86.REG_AX
	c.Append(p)
	c.Append(c.NewRET())
	got, err := c.Assemble()
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{
		0x48, 0xC7, 0xC0, 0x0A, 0x00, 0x00, 0x00, // MOVQ $10, AX
		0x48, 0x83, 0xE8, 0x03, // SUBQ $3, AX
		0xC3, // RET
	}
	assertBytes(t, got, want)
}

func TestAMD64_XORQ_zero_idiom(t *testing.T) {
	c := amd64Ctx(t)
	c.Append(regReg(c, x86.AXORQ, x86.REG_AX, x86.REG_AX))
	c.Append(c.NewRET())
	got, err := c.Assemble()
	if err != nil {
		t.Fatal(err)
	}
	// XORQ AX, AX → 48 31 C0; RET = C3
	want := []byte{0x48, 0x31, 0xC0, 0xC3}
	assertBytes(t, got, want)
}

func TestAMD64_ANDQ_RR(t *testing.T) {
	c := amd64Ctx(t)
	c.Append(immReg(c, x86.AMOVQ, 0xFF, x86.REG_AX))
	c.Append(immReg(c, x86.AMOVQ, 0x0F, x86.REG_BX))
	c.Append(regReg(c, x86.AANDQ, x86.REG_BX, x86.REG_AX))
	c.Append(c.NewRET())
	got, err := c.Assemble()
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{
		0x48, 0xC7, 0xC0, 0xFF, 0x00, 0x00, 0x00, // MOVQ $0xFF, AX
		0x48, 0xC7, 0xC3, 0x0F, 0x00, 0x00, 0x00, // MOVQ $0x0F, BX
		0x48, 0x21, 0xD8, // ANDQ BX, AX
		0xC3, // RET
	}
	assertBytes(t, got, want)
}

func TestAMD64_ORQ_RR(t *testing.T) {
	c := amd64Ctx(t)
	c.Append(immReg(c, x86.AMOVQ, 0xF0, x86.REG_AX))
	c.Append(immReg(c, x86.AMOVQ, 0x0F, x86.REG_BX))
	c.Append(regReg(c, x86.AORQ, x86.REG_BX, x86.REG_AX))
	c.Append(c.NewRET())
	got, err := c.Assemble()
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{
		0x48, 0xC7, 0xC0, 0xF0, 0x00, 0x00, 0x00, // MOVQ $0xF0, AX
		0x48, 0xC7, 0xC3, 0x0F, 0x00, 0x00, 0x00, // MOVQ $0x0F, BX
		0x48, 0x09, 0xD8, // ORQ BX, AX
		0xC3, // RET
	}
	assertBytes(t, got, want)
}

func TestAMD64_SHLQ_imm(t *testing.T) {
	c := amd64Ctx(t)
	c.Append(immReg(c, x86.AMOVQ, 1, x86.REG_AX))
	p := c.NewProg()
	p.As = x86.ASHLQ
	p.From.Type = obj.TYPE_CONST
	p.From.Offset = 3
	p.To.Type = obj.TYPE_REG
	p.To.Reg = x86.REG_AX
	c.Append(p)
	c.Append(c.NewRET())
	got, err := c.Assemble()
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{
		0x48, 0xC7, 0xC0, 0x01, 0x00, 0x00, 0x00, // MOVQ $1, AX
		0x48, 0xC1, 0xE0, 0x03, // SHLQ $3, AX
		0xC3, // RET
	}
	assertBytes(t, got, want)
}

func TestAMD64_IMULQ_RR(t *testing.T) {
	c := amd64Ctx(t)
	c.Append(immReg(c, x86.AMOVQ, 6, x86.REG_AX))
	c.Append(immReg(c, x86.AMOVQ, 7, x86.REG_BX))
	c.Append(regReg(c, x86.AIMULQ, x86.REG_BX, x86.REG_AX))
	c.Append(c.NewRET())
	got, err := c.Assemble()
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{
		0x48, 0xC7, 0xC0, 0x06, 0x00, 0x00, 0x00, // MOVQ $6, AX
		0x48, 0xC7, 0xC3, 0x07, 0x00, 0x00, 0x00, // MOVQ $7, BX
		0x48, 0x0F, 0xAF, 0xC3, // IMULQ BX, AX
		0xC3, // RET
	}
	assertBytes(t, got, want)
}

func TestAMD64_NEGQ(t *testing.T) {
	c := amd64Ctx(t)
	c.Append(immReg(c, x86.AMOVQ, 5, x86.REG_AX))
	p := c.NewProg()
	p.As = x86.ANEGQ
	p.To.Type = obj.TYPE_REG
	p.To.Reg = x86.REG_AX
	c.Append(p)
	c.Append(c.NewRET())
	got, err := c.Assemble()
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{
		0x48, 0xC7, 0xC0, 0x05, 0x00, 0x00, 0x00, // MOVQ $5, AX
		0x48, 0xF7, 0xD8, // NEGQ AX
		0xC3, // RET
	}
	assertBytes(t, got, want)
}

func TestAMD64_MOVQ_R12_REX(t *testing.T) {
	// Tests REX.R encoding for registers R8-R15.
	c := amd64Ctx(t)
	c.Append(immReg(c, x86.AMOVQ, 99, x86.REG_R12))
	c.Append(regReg(c, x86.AMOVQ, x86.REG_R12, x86.REG_AX))
	c.Append(c.NewRET())
	got, err := c.Assemble()
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{
		0x49, 0xC7, 0xC4, 0x63, 0x00, 0x00, 0x00, // MOVQ $99, R12  (REX.WB)
		0x4C, 0x89, 0xE0, // MOVQ R12, AX   (REX.WR)
		0xC3, // RET
	}
	assertBytes(t, got, want)
}

func TestAMD64_JMP_forward(t *testing.T) {
	// JMP past a dead MOVQ; then MOVQ $42, AX; RET.
	c := amd64Ctx(t)

	// placeholder for branch; target set after emitting targetProg
	jmp := c.NewProg()
	jmp.As = obj.AJMP
	jmp.To.Type = obj.TYPE_BRANCH
	c.Append(jmp)

	// dead instruction — should be skipped
	c.Append(immReg(c, x86.AMOVQ, 1, x86.REG_AX))

	// branch target
	targetProg := immReg(c, x86.AMOVQ, 42, x86.REG_AX)
	jmp.To.SetTarget(targetProg)
	c.Append(targetProg)

	c.Append(c.NewRET())

	got, err := c.Assemble()
	if err != nil {
		t.Fatal(err)
	}
	// JMP +7 (short): EB 07
	// MOVQ $1, AX (dead): 48 C7 C0 01 00 00 00
	// MOVQ $42, AX:       48 C7 C0 2A 00 00 00
	// RET: C3
	want := []byte{
		0xEB, 0x07,
		0x48, 0xC7, 0xC0, 0x01, 0x00, 0x00, 0x00,
		0x48, 0xC7, 0xC0, 0x2A, 0x00, 0x00, 0x00,
		0xC3,
	}
	assertBytes(t, got, want)
}

func TestAMD64_MOVQ_load_store(t *testing.T) {
	// MOVQ $99, 0(DI); MOVQ 0(DI), AX; RET
	c := amd64Ctx(t)
	c.Append(memStoreImm(c, x86.AMOVQ, 99, x86.REG_DI, 0))
	c.Append(memLoad(c, x86.AMOVQ, x86.REG_DI, 0, x86.REG_AX))
	c.Append(c.NewRET())
	got, err := c.Assemble()
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{
		0x48, 0xC7, 0x07, 0x63, 0x00, 0x00, 0x00, // MOVQ $99, 0(DI)
		0x48, 0x8B, 0x07, // MOVQ 0(DI), AX
		0xC3, // RET
	}
	assertBytes(t, got, want)
}

// TestAMD64_MOVABS_const — Issue 11: 64-bit immediate that does not fit
// in sign-extended 32 bits forces the 10-byte MOVABS encoding (REX.W +
// opcode B8+r + imm64). Required for RISC-V LUI+ADDI sequences whose
// combined value overflows int32.
func TestAMD64_MOVABS_const(t *testing.T) {
	c := amd64Ctx(t)
	c.Append(immReg(c, x86.AMOVQ, 0x123456789ABCDEF, x86.REG_AX))
	c.Append(c.NewRET())
	got, err := c.Assemble()
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{
		0x48, 0xB8, 0xEF, 0xCD, 0xAB, 0x89, 0x67, 0x45, 0x23, 0x01, // MOVQ $0x123456789ABCDEF, AX
		0xC3, // RET
	}
	assertBytes(t, got, want)
}

// TestAMD64_MOVQ_load_disp32 — Issue 11: large memory displacement
// (>127) forces ModR/M disp32 form. Mirrors RV LD with 32-bit offsets.
func TestAMD64_MOVQ_load_disp32(t *testing.T) {
	c := amd64Ctx(t)
	c.Append(memLoad(c, x86.AMOVQ, x86.REG_BX, 0x10000, x86.REG_AX))
	c.Append(c.NewRET())
	got, err := c.Assemble()
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{
		0x48, 0x8B, 0x83, 0x00, 0x00, 0x01, 0x00, // MOVQ 0x10000(BX), AX
		0xC3, // RET
	}
	assertBytes(t, got, want)
}

// TestAMD64_MOVQ_store_reg — Issue 11: register→memory store.
// memStoreImm covers the const→memory case; this covers reg→memory,
// which is what the JIT will emit for RV SD.
func TestAMD64_MOVQ_store_reg(t *testing.T) {
	c := amd64Ctx(t)
	p := c.NewProg()
	p.As = x86.AMOVQ
	p.From.Type = obj.TYPE_REG
	p.From.Reg = x86.REG_AX
	p.To.Type = obj.TYPE_MEM
	p.To.Reg = x86.REG_BX
	p.To.Offset = 8
	c.Append(p)
	c.Append(c.NewRET())
	got, err := c.Assemble()
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{
		0x48, 0x89, 0x43, 0x08, // MOVQ AX, 8(BX)
		0xC3, // RET
	}
	assertBytes(t, got, want)
}

// TestAMD64_SHLQ_CL — Issue 11: variable-amount shift uses CL implicitly
// (D3 /4). Required for RV SLL / SRL where the shift amount is in a
// register, not an immediate.
func TestAMD64_SHLQ_CL(t *testing.T) {
	c := amd64Ctx(t)
	c.Append(regReg(c, x86.ASHLQ, x86.REG_CX, x86.REG_AX))
	c.Append(c.NewRET())
	got, err := c.Assemble()
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{
		0x48, 0xD3, 0xE0, // SHLQ CL, AX
		0xC3, // RET
	}
	assertBytes(t, got, want)
}

// TestAMD64_MULQ — Issue 11: single-operand unsigned multiply (F7 /4).
// MULQ BX computes RDX:RAX = RAX * RBX. Required for RV MULHU.
// MULQ is NOT in unaryDst (unlike NEGQ), so the operand goes in From.
func TestAMD64_MULQ(t *testing.T) {
	c := amd64Ctx(t)
	c.Append(immReg(c, x86.AMOVQ, 7, x86.REG_AX))
	c.Append(immReg(c, x86.AMOVQ, 6, x86.REG_BX))
	p := c.NewProg()
	p.As = x86.AMULQ
	p.From.Type = obj.TYPE_REG
	p.From.Reg = x86.REG_BX
	c.Append(p)
	c.Append(c.NewRET())
	got, err := c.Assemble()
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{
		0x48, 0xC7, 0xC0, 0x07, 0x00, 0x00, 0x00, // MOVQ $7, AX
		0x48, 0xC7, 0xC3, 0x06, 0x00, 0x00, 0x00, // MOVQ $6, BX
		0x48, 0xF7, 0xE3, // MULQ BX
		0xC3, // RET
	}
	assertBytes(t, got, want)
}

// TestAMD64_IDIVQ — Issue 11: signed divide RDX:RAX by r/m64 (F7 /7).
// Required for RV DIV. Test sets up DX:AX = 0:42, divides by 7.
// Like MULQ, IDIVQ's single operand goes in From (not in unaryDst).
func TestAMD64_IDIVQ(t *testing.T) {
	c := amd64Ctx(t)
	c.Append(immReg(c, x86.AMOVQ, 0, x86.REG_DX))
	c.Append(immReg(c, x86.AMOVQ, 42, x86.REG_AX))
	c.Append(immReg(c, x86.AMOVQ, 7, x86.REG_BX))
	p := c.NewProg()
	p.As = x86.AIDIVQ
	p.From.Type = obj.TYPE_REG
	p.From.Reg = x86.REG_BX
	c.Append(p)
	c.Append(c.NewRET())
	got, err := c.Assemble()
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{
		0x48, 0xC7, 0xC2, 0x00, 0x00, 0x00, 0x00, // MOVQ $0, DX
		0x48, 0xC7, 0xC0, 0x2A, 0x00, 0x00, 0x00, // MOVQ $42, AX
		0x48, 0xC7, 0xC3, 0x07, 0x00, 0x00, 0x00, // MOVQ $7, BX
		0x48, 0xF7, 0xFB, // IDIVQ BX
		0xC3, // RET
	}
	assertBytes(t, got, want)
}

// TestAMD64_JEQ_forward — Issue 11: short-form conditional jump
// (74 imm8). Locks in the condition encoding for RV BEQ / BNE.
func TestAMD64_JEQ_forward(t *testing.T) {
	c := amd64Ctx(t)
	c.Append(regReg(c, x86.ACMPQ, x86.REG_AX, x86.REG_BX))
	jeq := c.NewProg()
	jeq.As = x86.AJEQ
	jeq.To.Type = obj.TYPE_BRANCH
	c.Append(jeq)
	c.Append(immReg(c, x86.AMOVQ, 1, x86.REG_AX))
	done := c.NewRET()
	jeq.To.SetTarget(done)
	c.Append(done)
	got, err := c.Assemble()
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{
		0x48, 0x39, 0xD8, // CMPQ AX, BX (reg=BX, rm=AX)
		0x74, 0x07, // JE +7 (skip MOVQ)
		0x48, 0xC7, 0xC0, 0x01, 0x00, 0x00, 0x00, // MOVQ $1, AX
		0xC3, // RET
	}
	assertBytes(t, got, want)
}

// TestAMD64_JNE_backward — Issue 11: backward conditional jump.
// Pattern is the canonical loop body the RV JIT will emit.
func TestAMD64_JNE_backward(t *testing.T) {
	c := amd64Ctx(t)
	target := immReg(c, x86.AMOVQ, 0, x86.REG_AX)
	c.Append(target)
	c.Append(regReg(c, x86.ACMPQ, x86.REG_AX, x86.REG_BX))
	jne := c.NewProg()
	jne.As = x86.AJNE
	jne.To.Type = obj.TYPE_BRANCH
	jne.To.SetTarget(target)
	c.Append(jne)
	c.Append(c.NewRET())
	got, err := c.Assemble()
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{
		0x48, 0xC7, 0xC0, 0x00, 0x00, 0x00, 0x00, // MOVQ $0, AX (target)
		0x48, 0x39, 0xD8, // CMPQ AX, BX
		0x75, 0xF4, // JNE -12 (back to target)
		0xC3, // RET
	}
	assertBytes(t, got, want)
}

// TestAMD64_MOVL_zeroext — Issue 11: 32-bit MOV implicitly zeros the
// upper 32 bits. Required to model RV ADDIW / SLLIW correctly.
func TestAMD64_MOVL_zeroext(t *testing.T) {
	c := amd64Ctx(t)
	c.Append(regReg(c, x86.AMOVL, x86.REG_AX, x86.REG_AX))
	c.Append(c.NewRET())
	got, err := c.Assemble()
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{
		0x89, 0xC0, // MOVL AX, AX (zero-extends EAX into RAX)
		0xC3, // RET
	}
	assertBytes(t, got, want)
}

// TestAMD64_MOVBQSX — Issue 11: byte → 64-bit sign-extending move.
// Required for RV LB.
func TestAMD64_MOVBQSX(t *testing.T) {
	c := amd64Ctx(t)
	c.Append(regReg(c, x86.AMOVBQSX, x86.REG_AL, x86.REG_AX))
	c.Append(c.NewRET())
	got, err := c.Assemble()
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{
		0x48, 0x0F, 0xBE, 0xC0, // MOVBQSX AL, AX
		0xC3, // RET
	}
	assertBytes(t, got, want)
}

// ─── AMD64 SSE / SSE2 byte tests ──────────────────────────────────────────────

// TestAMD64_MOVSD_load — Issue 11: scalar f64 load. Required for RV FLD.
func TestAMD64_MOVSD_load(t *testing.T) {
	c := amd64Ctx(t)
	c.Append(memLoad(c, x86.AMOVSD, x86.REG_BX, 0, x86.REG_X0))
	c.Append(c.NewRET())
	got, err := c.Assemble()
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{
		0xF2, 0x0F, 0x10, 0x03, // MOVSD 0(BX), X0
		0xC3, // RET
	}
	assertBytes(t, got, want)
}

// TestAMD64_ADDSD — Issue 11: scalar f64 add. Required for RV FADD.D.
func TestAMD64_ADDSD(t *testing.T) {
	c := amd64Ctx(t)
	c.Append(regReg(c, x86.AADDSD, x86.REG_X1, x86.REG_X0))
	c.Append(c.NewRET())
	got, err := c.Assemble()
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{
		0xF2, 0x0F, 0x58, 0xC1, // ADDSD X1, X0
		0xC3, // RET
	}
	assertBytes(t, got, want)
}

// TestAMD64_MULSD — Issue 11: scalar f64 multiply. Required for RV FMUL.D.
func TestAMD64_MULSD(t *testing.T) {
	c := amd64Ctx(t)
	c.Append(regReg(c, x86.AMULSD, x86.REG_X1, x86.REG_X0))
	c.Append(c.NewRET())
	got, err := c.Assemble()
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{
		0xF2, 0x0F, 0x59, 0xC1, // MULSD X1, X0
		0xC3, // RET
	}
	assertBytes(t, got, want)
}

// TestAMD64_SQRTSD — Issue 11: scalar f64 sqrt. Required for RV FSQRT.D.
func TestAMD64_SQRTSD(t *testing.T) {
	c := amd64Ctx(t)
	c.Append(regReg(c, x86.ASQRTSD, x86.REG_X0, x86.REG_X0))
	c.Append(c.NewRET())
	got, err := c.Assemble()
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{
		0xF2, 0x0F, 0x51, 0xC0, // SQRTSD X0, X0
		0xC3, // RET
	}
	assertBytes(t, got, want)
}

// ─── ARM64 byte tests ─────────────────────────────────────────────────────────
// Expected bytes verified via:
//   GOARCH=arm64 GOOS=linux go tool asm -o /tmp/t.o /tmp/t.s
//   GOARCH=arm64 GOOS=linux go tool objdump /tmp/t.o
// ARM64 is little-endian; each 32-bit instruction is stored LSB-first.

func TestARM64_MOVD_const_R0_RET(t *testing.T) {
	c := arm64Ctx(t)

	p := c.NewProg()
	p.As = arm64.AMOVD
	p.From.Type = obj.TYPE_CONST
	p.From.Offset = 0x42
	p.To.Type = obj.TYPE_REG
	p.To.Reg = arm64.REG_R0
	c.Append(p)
	c.Append(c.NewRET())

	got, err := c.Assemble()
	if err != nil {
		t.Fatal(err)
	}
	// MOVD $66, R0 = D2 80 08 40 (LE)
	// RET          = C0 03 5F D6 (LE)
	want := []byte{0x40, 0x08, 0x80, 0xD2, 0xC0, 0x03, 0x5F, 0xD6}
	assertBytes(t, got, want)
}

func TestARM64_ADD_3reg(t *testing.T) {
	c := arm64Ctx(t)

	movd := func(imm int64, reg int16) *obj.Prog {
		p := c.NewProg()
		p.As = arm64.AMOVD
		p.From.Type = obj.TYPE_CONST
		p.From.Offset = imm
		p.To.Type = obj.TYPE_REG
		p.To.Reg = reg
		return p
	}
	c.Append(movd(3, arm64.REG_R1))
	c.Append(movd(4, arm64.REG_R2))

	// ADD R1, R2, R3
	add := c.NewProg()
	add.As = arm64.AADD
	add.From.Type = obj.TYPE_REG
	add.From.Reg = arm64.REG_R1
	add.Reg = arm64.REG_R2
	add.To.Type = obj.TYPE_REG
	add.To.Reg = arm64.REG_R3
	c.Append(add)

	// MOVD R3, R0
	mv := c.NewProg()
	mv.As = arm64.AMOVD
	mv.From.Type = obj.TYPE_REG
	mv.From.Reg = arm64.REG_R3
	mv.To.Type = obj.TYPE_REG
	mv.To.Reg = arm64.REG_R0
	c.Append(mv)

	c.Append(c.NewRET())

	got, err := c.Assemble()
	if err != nil {
		t.Fatal(err)
	}
	// ORR $3, ZR, R1  = E1 07 40 B2
	// ORR $4, ZR, R2  = E2 03 7E B2
	// ADD R1, R2, R3  = 43 00 01 8B
	// MOVD R3, R0     = E0 03 03 AA
	// RET             = C0 03 5F D6
	want := []byte{
		0xE1, 0x07, 0x40, 0xB2,
		0xE2, 0x03, 0x7E, 0xB2,
		0x43, 0x00, 0x01, 0x8B,
		0xE0, 0x03, 0x03, 0xAA,
		0xC0, 0x03, 0x5F, 0xD6,
	}
	assertBytes(t, got, want)
}

func TestARM64_LSL_imm(t *testing.T) {
	c := arm64Ctx(t)

	// MOVD $1, R0
	p := c.NewProg()
	p.As = arm64.AMOVD
	p.From.Type = obj.TYPE_CONST
	p.From.Offset = 1
	p.To.Type = obj.TYPE_REG
	p.To.Reg = arm64.REG_R0
	c.Append(p)

	// LSL $3, R0, R0
	lsl := c.NewProg()
	lsl.As = arm64.ALSL
	lsl.From.Type = obj.TYPE_CONST
	lsl.From.Offset = 3
	lsl.Reg = arm64.REG_R0
	lsl.To.Type = obj.TYPE_REG
	lsl.To.Reg = arm64.REG_R0
	c.Append(lsl)

	c.Append(c.NewRET())

	got, err := c.Assemble()
	if err != nil {
		t.Fatal(err)
	}
	// ORR $1, ZR, R0  = E0 03 40 B2
	// LSL $3, R0, R0  = 00 F0 7D D3
	// RET             = C0 03 5F D6
	want := []byte{
		0xE0, 0x03, 0x40, 0xB2,
		0x00, 0xF0, 0x7D, 0xD3,
		0xC0, 0x03, 0x5F, 0xD6,
	}
	assertBytes(t, got, want)
}

// TestARM64_LDR_post — Issue 11: 64-bit load with positive 8-byte
// offset. Mirrors RV LD encoding on an arm64 host.
func TestARM64_LDR_post(t *testing.T) {
	c := arm64Ctx(t)
	c.Append(memLoad(c, arm64.AMOVD, arm64.REG_R0, 8, arm64.REG_R1))
	c.Append(c.NewRET())
	got, err := c.Assemble()
	if err != nil {
		t.Fatal(err)
	}
	// MOVD 8(R0), R1 = 01 04 40 F9
	// RET            = C0 03 5F D6
	want := []byte{
		0x01, 0x04, 0x40, 0xF9,
		0xC0, 0x03, 0x5F, 0xD6,
	}
	assertBytes(t, got, want)
}

// TestARM64_STR — Issue 11: 64-bit store with positive 8-byte offset.
// Mirrors RV SD encoding on an arm64 host.
func TestARM64_STR(t *testing.T) {
	c := arm64Ctx(t)
	p := c.NewProg()
	p.As = arm64.AMOVD
	p.From.Type = obj.TYPE_REG
	p.From.Reg = arm64.REG_R1
	p.To.Type = obj.TYPE_MEM
	p.To.Reg = arm64.REG_R0
	p.To.Offset = 8
	c.Append(p)
	c.Append(c.NewRET())
	got, err := c.Assemble()
	if err != nil {
		t.Fatal(err)
	}
	// MOVD R1, 8(R0) = 01 04 00 F9
	// RET            = C0 03 5F D6
	want := []byte{
		0x01, 0x04, 0x00, 0xF9,
		0xC0, 0x03, 0x5F, 0xD6,
	}
	assertBytes(t, got, want)
}

// TestARM64_CBZ_forward — Issue 11: branch-if-zero with a forward
// target. Mirrors RV BEQ X, X0, label.
func TestARM64_CBZ_forward(t *testing.T) {
	c := arm64Ctx(t)
	cbz := c.NewProg()
	cbz.As = arm64.ACBZ
	cbz.From.Type = obj.TYPE_REG
	cbz.From.Reg = arm64.REG_R0
	cbz.To.Type = obj.TYPE_BRANCH
	c.Append(cbz)

	// MOVD $1, R1 (skipped on R0 == 0)
	mid := c.NewProg()
	mid.As = arm64.AMOVD
	mid.From.Type = obj.TYPE_CONST
	mid.From.Offset = 1
	mid.To.Type = obj.TYPE_REG
	mid.To.Reg = arm64.REG_R1
	c.Append(mid)

	done := c.NewRET()
	cbz.To.SetTarget(done)
	c.Append(done)

	got, err := c.Assemble()
	if err != nil {
		t.Fatal(err)
	}
	// CBZ R0, +2 instr  = 40 00 00 B4   (offset 2 instructions = 8 bytes)
	// ORR $1, ZR, R1    = E1 03 40 B2
	// RET               = C0 03 5F D6
	want := []byte{
		0x40, 0x00, 0x00, 0xB4,
		0xE1, 0x03, 0x40, 0xB2,
		0xC0, 0x03, 0x5F, 0xD6,
	}
	assertBytes(t, got, want)
}

// TestARM64_FADDD — Issue 11: scalar f64 add. Mirrors RV FADD.D.
// In Go's arm64 assembly the form is FADDD Fm, Fn, Fd: From=Fm,
// Reg=Fn, To=Fd.
func TestARM64_FADDD(t *testing.T) {
	c := arm64Ctx(t)
	p := c.NewProg()
	p.As = arm64.AFADDD
	p.From.Type = obj.TYPE_REG
	p.From.Reg = arm64.REG_F1
	p.Reg = arm64.REG_F2
	p.To.Type = obj.TYPE_REG
	p.To.Reg = arm64.REG_F0
	c.Append(p)
	c.Append(c.NewRET())
	got, err := c.Assemble()
	if err != nil {
		t.Fatal(err)
	}
	// FADDD F1, F2, F0 = 40 28 61 1E
	// RET              = C0 03 5F D6
	want := []byte{
		0x40, 0x28, 0x61, 0x1E,
		0xC0, 0x03, 0x5F, 0xD6,
	}
	assertBytes(t, got, want)
}

// ─── Smoke test: assemble + execute ──────────────────────────────────────────
//
// On the host arch, assemble MOVQ $42, AX; RET (amd64) or MOVD $42, R0; RET
// (arm64), mmap the bytes as executable, call via unsafe, verify return value.

func TestSmoke_Assemble_and_Execute(t *testing.T) {
	// On Apple Silicon (darwin/arm64), W^X enforcement requires MAP_JIT
	// plus pthread_jit_write_protect_np toggling. The simple anonymous
	// PROT_WRITE|PROT_EXEC mmap below is rejected. Defer real darwin/arm64
	// execution to the dedicated internal/jitcall path.
	if runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" {
		t.Skip("smoke execute: darwin/arm64 requires MAP_JIT; tracked via internal/jitcall")
	}

	var (
		code []byte
		err  error
	)

	switch runtime.GOARCH {
	case "amd64":
		c := goasm.New(goasm.AMD64)
		c.Append(c.NewATEXT())
		c.Append(immReg(c, x86.AMOVQ, 42, x86.REG_AX))
		c.Append(c.NewRET())
		code, err = c.Assemble()

	case "arm64":
		c := goasm.New(goasm.ARM64)
		c.Append(c.NewATEXT())
		p := c.NewProg()
		p.As = arm64.AMOVD
		p.From.Type = obj.TYPE_CONST
		p.From.Offset = 42
		p.To.Type = obj.TYPE_REG
		p.To.Reg = arm64.REG_R0
		c.Append(p)
		c.Append(c.NewRET())
		code, err = c.Assemble()

	default:
		t.Skipf("smoke execution test not implemented for GOARCH=%s", runtime.GOARCH)
		return
	}

	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if len(code) == 0 {
		t.Fatal("Assemble returned empty bytes")
	}

	// Map an anonymous executable page.
	pageSize := syscall.Getpagesize()
	mapSize := ((len(code) + pageSize - 1) / pageSize) * pageSize
	mem, err := syscall.Mmap(
		-1, 0, mapSize,
		syscall.PROT_READ|syscall.PROT_WRITE|syscall.PROT_EXEC,
		syscall.MAP_ANON|syscall.MAP_PRIVATE,
	)
	if err != nil {
		t.Fatalf("mmap: %v", err)
	}
	defer syscall.Munmap(mem) //nolint:errcheck

	copy(mem, code)

	// Call the JIT function.
	// In Go's ABI, a function value is a pointer to a funcval struct whose
	// first field is the code pointer. We point funcval.fn at mem[0].
	codePtr := uintptr(unsafe.Pointer(&mem[0]))
	result := callAt(codePtr)

	if result != 42 {
		t.Errorf("expected 42, got %d", result)
	}
}

// callAt calls the function at the given code address and returns its int64
// result.
//
// A Go function value is a pointer to a funcval struct whose first field
// is the code pointer (uintptr). Calling fn() compiles to:
//
//	MOV DX, fn  ; DX = funcval pointer
//	CALL [DX]   ; jump to *(funcval+0) = code pointer
//
// So fn must hold the address of a uintptr that holds addr. We give addr
// itself a stable address (&addr) and reinterpret the **uintptr as
// *fnType. The compiler emits CALL [&addr] = CALL addr.
func callAt(addr uintptr) int64 {
	type fnType func() int64
	fp := &addr
	fn := *(*fnType)(unsafe.Pointer(&fp))
	return fn()
}

// ─── HostArch convenience test ────────────────────────────────────────────────

func TestHostArch(t *testing.T) {
	got := goasm.HostArch()
	switch runtime.GOARCH {
	case "amd64":
		if got != goasm.AMD64 {
			t.Errorf("HostArch()=%d, want AMD64=%d", got, goasm.AMD64)
		}
	case "arm64":
		if got != goasm.ARM64 {
			t.Errorf("HostArch()=%d, want ARM64=%d", got, goasm.ARM64)
		}
	default:
		t.Skipf("HostArch not tested for GOARCH=%s", runtime.GOARCH)
	}
}

// ─── Reset test ───────────────────────────────────────────────────────────────

func TestCtx_Reset(t *testing.T) {
	c := goasm.New(goasm.AMD64)
	c.Append(c.NewATEXT())
	c.Append(immReg(c, x86.AMOVQ, 1, x86.REG_AX))
	c.Append(c.NewRET())
	b1, err := c.Assemble()
	if err != nil {
		t.Fatal(err)
	}

	c.Reset()
	c.Append(c.NewATEXT())
	c.Append(immReg(c, x86.AMOVQ, 2, x86.REG_AX))
	c.Append(c.NewRET())
	b2, err := c.Assemble()
	if err != nil {
		t.Fatal(err)
	}

	if bytes.Equal(b1, b2) {
		t.Error("Reset should produce different bytes for different immediates")
	}
	// b1 should encode $1 and b2 should encode $2 (byte offset 3)
	if len(b1) >= 4 && b1[3] != 0x01 {
		t.Errorf("first block immediate: got 0x%02X want 0x01", b1[3])
	}
	if len(b2) >= 4 && b2[3] != 0x02 {
		t.Errorf("second block immediate: got 0x%02X want 0x02", b2[3])
	}
}

// ─── Error-path & lifecycle tests ─────────────────────────────────────────────

// TestErr_FirstProgNotATEXT — Issue 5: Assemble must reject a Prog list
// whose first entry is not ATEXT.
func TestErr_FirstProgNotATEXT(t *testing.T) {
	c := goasm.New(goasm.AMD64)
	// intentionally skip NewATEXT
	c.Append(immReg(c, x86.AMOVQ, 0x42, x86.REG_AX))
	c.Append(c.NewRET())

	_, err := c.Assemble()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "ATEXT") {
		t.Errorf("expected error mentioning ATEXT, got: %v", err)
	}
}

// TestErr_DoubleAssemble — Issue 4: a second Assemble on the same Ctx
// without Reset must return a clear error rather than the confusing
// "symbol redeclared" diag the encoder would otherwise emit.
func TestErr_DoubleAssemble(t *testing.T) {
	c := amd64Ctx(t)
	c.Append(immReg(c, x86.AMOVQ, 1, x86.REG_AX))
	c.Append(c.NewRET())
	if _, err := c.Assemble(); err != nil {
		t.Fatalf("first Assemble: %v", err)
	}

	_, err := c.Assemble()
	if err == nil {
		t.Fatal("expected error from second Assemble, got nil")
	}
	if !strings.Contains(err.Error(), "Reset") {
		t.Errorf("expected error mentioning Reset, got: %v", err)
	}
}

// TestErr_FatalRecovers — Issue 1: a malformed CALL (no target, no Sym)
// drives obj/x86/asm6.go through ctxt.Diag → ctxt.DiagFlush → log.Fatalf.
// With our DiagFlush installed as a panicking function and a deferred
// recover in Assemble, this returns a normal error instead of killing
// the process.
func TestErr_FatalRecovers(t *testing.T) {
	c := amd64Ctx(t)
	bad := c.NewProg()
	bad.As = obj.ACALL
	bad.To.Type = obj.TYPE_BRANCH
	// leave bad.To.Sym nil and no SetTarget — hits "call without target"
	c.Append(bad)
	c.Append(c.NewRET())

	_, err := c.Assemble()
	if err == nil {
		t.Fatal("expected error from malformed CALL, got nil")
	}
	if !strings.Contains(err.Error(), "call without target") {
		t.Errorf("expected 'call without target' in error, got: %v", err)
	}
}

// TestErr_EmptyProgList — preexisting check, but we lock it in here so a
// future refactor doesn't drop it.
func TestErr_EmptyProgList(t *testing.T) {
	c := goasm.New(goasm.AMD64)
	_, err := c.Assemble()
	if err == nil {
		t.Fatal("expected error from empty prog list, got nil")
	}
	if !strings.Contains(err.Error(), "empty prog list") {
		t.Errorf("expected 'empty prog list' in error, got: %v", err)
	}
}

// ─── DefaultFlags / Issue 3 ──────────────────────────────────────────────────

// TestDefaultFlags_NoMorestack — Issue 3: with default Flags
// (NOSPLIT|NOFRAME), an ATEXT + ACALL + RET sequence must NOT produce a
// relocation against runtime.morestack* (which would jump to garbage at
// run time, since our LSym hash has no such symbol).
func TestDefaultFlags_NoMorestack(t *testing.T) {
	c := amd64Ctx(t)
	call := c.NewProg()
	call.As = obj.ACALL
	call.To.Type = obj.TYPE_MEM
	call.To.Name = obj.NAME_EXTERN
	call.To.Sym = c.Ctxt().Lookup("some_target")
	c.Append(call)
	c.Append(c.NewRET())

	if _, err := c.Assemble(); err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	for _, r := range c.Sym().R {
		if r.Sym == nil {
			continue
		}
		if strings.Contains(r.Sym.Name, "morestack") {
			t.Errorf("unexpected morestack relocation in output: %s", r.Sym.Name)
		}
	}
}

// TestFlagsOverride_StackCheck — Issue 3: clearing Flags re-enables the
// auto-prologue and stacksplit, which in turn produces the
// runtime.morestack relocation. This proves the override knob works.
func TestFlagsOverride_StackCheck(t *testing.T) {
	c := goasm.New(goasm.AMD64)
	c.Flags = 0 // opt back into default Go function prologue
	c.Append(c.NewATEXT())
	call := c.NewProg()
	call.As = obj.ACALL
	call.To.Type = obj.TYPE_MEM
	call.To.Name = obj.NAME_EXTERN
	call.To.Sym = c.Ctxt().Lookup("some_target")
	c.Append(call)
	c.Append(c.NewRET())

	if _, err := c.Assemble(); err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	found := false
	for _, r := range c.Sym().R {
		if r.Sym != nil && strings.Contains(r.Sym.Name, "morestack") {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected morestack relocation when Flags=0, found none")
	}
}

// ─── Concurrency / Issue 7 ───────────────────────────────────────────────────

// TestConcurrentNew exercises the sync.Once-protected LinkArch.Init.
// Without the Once guard, concurrent first-time New() calls would race
// on the package globals (ycover, optab, oprange) inside instinit /
// buildop. Run under -race; must report no data race.
func TestConcurrentNew(t *testing.T) {
	const goroutines = 16
	var wg sync.WaitGroup
	wg.Add(goroutines)
	errs := make(chan error, goroutines)
	for i := 0; i < goroutines; i++ {
		i := i
		go func() {
			defer wg.Done()
			arch := goasm.AMD64
			if i%2 == 0 {
				arch = goasm.ARM64
			}
			c := goasm.New(arch)
			c.Append(c.NewATEXT())
			if arch == goasm.AMD64 {
				c.Append(immReg(c, x86.AMOVQ, int64(i), x86.REG_AX))
			} else {
				p := c.NewProg()
				p.As = arm64.AMOVD
				p.From.Type = obj.TYPE_CONST
				p.From.Offset = int64(i)
				p.To.Type = obj.TYPE_REG
				p.To.Reg = arm64.REG_R0
				c.Append(p)
			}
			c.Append(c.NewRET())
			if _, err := c.Assemble(); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent Assemble: %v", err)
	}
}

// TestArchTablesInitOnce — Issue 22: a second goasm.New for the same
// arch (and a Reset cycle) must not re-run instinit / buildop in a way
// that yields "phase error in optab" diags. We assert: no errors, and
// identical bytes from three independent encodings of the same prog
// list.
func TestArchTablesInitOnce(t *testing.T) {
	encode := func() []byte {
		c := goasm.New(goasm.AMD64)
		c.Append(c.NewATEXT())
		c.Append(immReg(c, x86.AMOVQ, 1, x86.REG_AX))
		c.Append(c.NewRET())
		b, err := c.Assemble()
		if err != nil {
			t.Fatalf("Assemble: %v", err)
		}
		return b
	}

	a := encode()
	b := encode()

	c := goasm.New(goasm.AMD64)
	c.Append(c.NewATEXT())
	c.Append(immReg(c, x86.AMOVQ, 1, x86.REG_AX))
	c.Append(c.NewRET())
	if _, err := c.Assemble(); err != nil {
		t.Fatalf("third Assemble: %v", err)
	}
	c.Reset()
	c.Append(c.NewATEXT())
	c.Append(immReg(c, x86.AMOVQ, 1, x86.REG_AX))
	c.Append(c.NewRET())
	d, err := c.Assemble()
	if err != nil {
		t.Fatalf("post-reset Assemble: %v", err)
	}

	if !bytes.Equal(a, b) || !bytes.Equal(a, d) {
		t.Errorf("inconsistent bytes across encodings:\n  a=%X\n  b=%X\n  d=%X", a, b, d)
	}
}

// ─── assertBytes self-test ────────────────────────────────────────────────────

// TestAssertBytes_RejectsTrailingJunk verifies the helper now flags
// non-zero trailing bytes (the previous TrimRight version silently let
// any post-zero junk through).
func TestAssertBytes_RejectsTrailingJunk(t *testing.T) {
	want := []byte{0x90}
	got := []byte{0x90, 0x00, 0x01} // a non-zero byte after a zero
	fake := &testing.T{}
	assertBytes(fake, got, want)
	if !fake.Failed() {
		t.Error("assertBytes accepted non-zero trailing byte; expected failure")
	}
}
