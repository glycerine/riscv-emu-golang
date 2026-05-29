package bench

import (
	"fmt"
	"syscall"
	"testing"
	"unsafe"

	"github.com/glycerine/riscv-emu-golang/goasm"
	"github.com/glycerine/riscv-emu-golang/goasm/obj"
	"github.com/glycerine/riscv-emu-golang/goasm/obj/x86"
	"github.com/glycerine/riscv-emu-golang/internal/jitcall"
)

// BenchmarkAddImm_RSP vs BenchmarkAddImm_RBP:
//
// Two JIT functions that are IDENTICAL except for the base register
// in the inner ADDQ instruction:
//
//	RSP version: ADDQ $1, 0(RSP)   ← 6.7x slower
//	RBP version: ADDQ $1, 0(RBP)   ← fast
//
// Both operate on the same physical memory (RBP is set to RSP).
// The encoding is correct for both. The difference is purely
// microarchitectural: Intel's "stack engine" penalizes non-PUSH/POP
// use of RSP as a memory base register.

const benchAddN = 1000

func buildAddLoop(base int16) []byte {
	ctx := goasm.New(goasm.AMD64)
	ctx.Append(ctx.NewATEXT())

	emitRI := func(op obj.As, imm int64, reg int16) {
		p := ctx.NewProg()
		p.As = op
		p.From.Type = obj.TYPE_CONST
		p.From.Offset = imm
		p.To.Type = obj.TYPE_REG
		p.To.Reg = reg
		ctx.Append(p)
	}
	emitMI := func(op obj.As, imm int64, base int16, off int64) {
		p := ctx.NewProg()
		p.As = op
		p.From.Type = obj.TYPE_CONST
		p.From.Offset = imm
		p.To.Type = obj.TYPE_MEM
		p.To.Reg = base
		p.To.Offset = off
		ctx.Append(p)
	}
	emitRM := func(op obj.As, base int16, off int64, dst int16) {
		p := ctx.NewProg()
		p.As = op
		p.From.Type = obj.TYPE_MEM
		p.From.Reg = base
		p.From.Offset = off
		p.To.Type = obj.TYPE_REG
		p.To.Reg = dst
		ctx.Append(p)
	}

	// Prologue: allocate 16 bytes of scratch on stack.
	emitRI(x86.ASUBQ, 16, goasm.REG_AMD64_SP)

	// Point RBP at the scratch area (same memory as RSP+0).
	if base == goasm.REG_AMD64_BP {
		emitRM(x86.ALEAQ, goasm.REG_AMD64_SP, 0, goasm.REG_AMD64_BP)
	}

	// Initialize: MOV $0, [base+0]
	emitMI(x86.AMOVQ, 0, base, 0)

	// Hot loop body: ADDQ $1, [base+0] × N
	for i := 0; i < benchAddN; i++ {
		emitMI(x86.AADDQ, 1, base, 0)
	}

	// Epilogue: restore RSP, return.
	emitRI(x86.AADDQ, 16, goasm.REG_AMD64_SP)

	p := ctx.NewProg()
	p.As = obj.ARET
	ctx.Append(p)

	code, err := ctx.Assemble()
	if err != nil {
		panic(err)
	}
	return code
}

func benchAddImm(b *testing.B, base int16) {
	code := buildAddLoop(base)
	ps := syscall.Getpagesize()
	sz := ((len(code) + ps - 1) / ps) * ps
	mem, err := syscall.Mmap(-1, 0, sz,
		syscall.PROT_READ|syscall.PROT_WRITE|syscall.PROT_EXEC,
		syscall.MAP_ANON|syscall.MAP_PRIVATE)
	if err != nil {
		b.Fatal(err)
	}
	defer syscall.Munmap(mem)
	copy(mem, code)

	fn := uintptr(unsafe.Pointer(&mem[0]))
	var x [32]uint64
	var f [32]uint64
	var fcsr uint32

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		jitcall.Call(fn, &x, &f, &fcsr, 0, 0)
	}
	b.StopTimer()
	b.ReportMetric(float64(benchAddN), "adds/op")
	nsPerAdd := float64(b.Elapsed().Nanoseconds()) / float64(b.N) / float64(benchAddN)
	b.ReportMetric(nsPerAdd, "ns/add")
}

func BenchmarkAddImm_RSP(b *testing.B) { benchAddImm(b, goasm.REG_AMD64_SP) }
func BenchmarkAddImm_RBP(b *testing.B) { benchAddImm(b, goasm.REG_AMD64_BP) }

func TestAddImm_RSP_vs_RBP(t *testing.T) {
	for _, tc := range []struct {
		name string
		base int16
	}{
		{"RSP", goasm.REG_AMD64_SP},
		{"RBP", goasm.REG_AMD64_BP},
	} {
		code := buildAddLoop(tc.base)
		t.Logf("%s: %d bytes of code", tc.name, len(code))
		t.Logf("%s: first 12 bytes: % 02x", tc.name, code[:min(12, len(code))])
	}

	rspCode := buildAddLoop(goasm.REG_AMD64_SP)
	rbpCode := buildAddLoop(goasm.REG_AMD64_BP)
	diff := len(rspCode) - len(rbpCode)
	t.Logf("size diff: RSP=%d RBP=%d delta=%d (RSP needs SIB byte per ADDQ)",
		len(rspCode), len(rbpCode), diff)
	expected := benchAddN + 1 // +1 for the MOV init which also differs
	if diff != expected {
		t.Logf("expected delta=%d (one SIB byte per mem op)", expected)
	}
	fmt.Printf("\nRun benchmarks with:\n")
	fmt.Printf("  go test -bench='BenchmarkAddImm' -benchtime=10000x -count=5 ./bench/\n\n")
}
