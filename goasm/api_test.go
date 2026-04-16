package goasm_test

import (
	"bytes"
	"fmt"
	"runtime"
	"syscall"
	"testing"
	"unsafe"

	"riscv/goasm"
	"riscv/goasm/obj"
	"riscv/goasm/obj/arm64"
	"riscv/goasm/obj/x86"
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
		0x48, 0x01, 0xD8,                          // ADDQ BX, AX
		0xC3,                                      // RET
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
		0x48, 0x83, 0xE8, 0x03,                    // SUBQ $3, AX
		0xC3,                                      // RET
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
		0x48, 0x21, 0xD8,                          // ANDQ BX, AX
		0xC3,                                      // RET
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
		0x48, 0x09, 0xD8,                          // ORQ BX, AX
		0xC3,                                      // RET
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
		0x48, 0xC1, 0xE0, 0x03,                    // SHLQ $3, AX
		0xC3,                                      // RET
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
		0x48, 0x0F, 0xAF, 0xC3,                    // IMULQ BX, AX
		0xC3,                                      // RET
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
		0x48, 0xF7, 0xD8,                          // NEGQ AX
		0xC3,                                      // RET
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
		0x4C, 0x89, 0xE0,                          // MOVQ R12, AX   (REX.WR)
		0xC3,                                      // RET
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
		0x48, 0x8B, 0x07,                          // MOVQ 0(DI), AX
		0xC3,                                      // RET
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
