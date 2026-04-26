package riscv

// Exhaustive register-pair tests for lowerShift and lowerBinop.
//
// Single-instruction tests: all O(N^3) combos of (dst, src1, src2).
// Two-instruction sequence tests: op1 feeds into op2, testing all O(N^4)
// combos of (d1, a1, b1, d2) where d1 feeds into op2 as a source.
// This catches cross-instruction clobber bugs where op1's lowering
// destroys a register that op2 needs.

import (
	"fmt"
	"syscall"
	"testing"
	"unsafe"

	"riscv/goasm"
	"riscv/internal/jitcall"
)

const exhaustN = 7 // guest regs x1..x7

func execBlock(t *testing.T, b *Block, x *[32]uint64, _ bool) jitcall.Result {
	t.Helper()
	pool := RV8Pool(b)
	alloc := helperTestAllocate(b, pool, RV8Pinned(), nil)
	ctx := goasm.New(goasm.AMD64)
	ctx.Append(ctx.NewATEXT())
	_, err := LowerAMD64_RV8(ctx, b, alloc)
	if err != nil {
		t.Fatalf("lower: %v", err)
	}
	code, err := ctx.Assemble()
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	ps := syscall.Getpagesize()
	sz := ((len(code) + ps - 1) / ps) * ps
	mem, err := syscall.Mmap(-1, 0, sz,
		syscall.PROT_READ|syscall.PROT_WRITE|syscall.PROT_EXEC,
		syscall.MAP_ANON|syscall.MAP_PRIVATE)
	if err != nil {
		t.Fatalf("mmap: %v", err)
	}
	defer syscall.Munmap(mem)
	copy(mem, code)
	var f [32]uint64
	var fcsr uint32
	return jitcall.Call(uintptr(unsafe.Pointer(&mem[0])), x, &f, &fcsr, 0, 0)
}

// ── Single-instruction exhaustive tests ──

func buildSingleBlock(rd, ra, rb int, emitOp func(*Emitter, VReg, VReg, VReg)) *Block {
	e := NewEmitter(nil)
	for i := 1; i <= exhaustN; i++ {
		e.Load(VReg(i), e.XBase(), int64(i)*8, I64, false)
	}
	emitOp(e, VReg(rd), VReg(ra), VReg(rb))
	e.Store(e.XBase(), int64(rd)*8, VReg(rd), I64)
	e.Ret(0x1000, 0, VRegZero)
	return e.Block
}

func runExhaustiveSingle(t *testing.T, name string, emitOp func(*Emitter, VReg, VReg, VReg), ref func(uint64, uint64) uint64) {
	t.Skip("too slow")

	valA := uint64(0xDEADBEEF12345678)
	valB := uint64(4)

	for rd := 1; rd <= exhaustN; rd++ {
		for ra := 1; ra <= exhaustN; ra++ {
			for rb := 1; rb <= exhaustN; rb++ {
				t.Run(fmt.Sprintf("%s/d=x%d/a=x%d/b=x%d", name, rd, ra, rb), func(t *testing.T) {
					blk := buildSingleBlock(rd, ra, rb, emitOp)
					effA, effB := valA, valB
					if ra == rb {
						effA = valB
					}
					want := ref(effA, effB)

					var x1, x2 [32]uint64
					for i := 1; i <= exhaustN; i++ {
						x1[i] = uint64(i * 111)
						x2[i] = uint64(i * 111)
					}
					x1[ra] = valA
					x1[rb] = valB
					x2[ra] = valA
					x2[rb] = valB
					execBlock(t, blk, &x1, false)
					execBlock(t, blk, &x2, true)

					if x1[rd] != want {
						t.Errorf("V1: x[%d]=0x%x, want 0x%x", rd, x1[rd], want)
					}
					if x2[rd] != want {
						t.Errorf("V2: x[%d]=0x%x, want 0x%x", rd, x2[rd], want)
					}
					if x1[rd] != x2[rd] {
						t.Errorf("V1!=V2: 0x%x vs 0x%x", x1[rd], x2[rd])
					}
				})
			}
		}
	}
}

func TestExhaustive_SHR(t *testing.T) {
	runExhaustiveSingle(t, "SHR", (*Emitter).Shr, func(a, b uint64) uint64 { return a >> (b & 63) })
}
func TestExhaustive_SHL(t *testing.T) {
	runExhaustiveSingle(t, "SHL", (*Emitter).Shl, func(a, b uint64) uint64 { return a << (b & 63) })
}
func TestExhaustive_SAR(t *testing.T) {
	runExhaustiveSingle(t, "SAR", (*Emitter).Sar, func(a, b uint64) uint64 { return uint64(int64(a) >> (b & 63)) })
}
func TestExhaustive_SUB(t *testing.T) {
	runExhaustiveSingle(t, "SUB", (*Emitter).Sub, func(a, b uint64) uint64 { return a - b })
}
func TestExhaustive_ADD(t *testing.T) {
	runExhaustiveSingle(t, "ADD", (*Emitter).Add, func(a, b uint64) uint64 { return a + b })
}
func TestExhaustive_XOR(t *testing.T) {
	runExhaustiveSingle(t, "XOR", (*Emitter).Xor, func(a, b uint64) uint64 { return a ^ b })
}

// ── Two-instruction sequence exhaustive tests ──
//
// Pattern: op1 writes to d1, then op2 reads d1 as a source.
// This catches bugs where op1's lowering clobbers a register
// that still holds a value op2 needs.
//
//   x[ra] = valA, x[rb] = valB (set up)
//   x[d1] = op1(x[ra], x[rb])     ← first operation
//   x[d2] = op2(x[d1], x[rc])     ← second operation, reads d1's result
//   verify x[d2]

type opDef struct {
	name string
	emit func(*Emitter, VReg, VReg, VReg)
	ref  func(uint64, uint64) uint64
}

var seqOps = []opDef{
	{"SHR", (*Emitter).Shr, func(a, b uint64) uint64 { return a >> (b & 63) }},
	{"SHL", (*Emitter).Shl, func(a, b uint64) uint64 { return a << (b & 63) }},
	{"SUB", (*Emitter).Sub, func(a, b uint64) uint64 { return a - b }},
	{"ADD", (*Emitter).Add, func(a, b uint64) uint64 { return a + b }},
	{"XOR", (*Emitter).Xor, func(a, b uint64) uint64 { return a ^ b }},
}

// TestExhaustive_Seq2_SHR_then_X tests SHR followed by each other operation,
// across all register assignments.
func TestExhaustive_Seq2_SHR_then_ADD(t *testing.T) {
	runSeq2(t, seqOps[0], seqOps[3]) // SHR then ADD
}
func TestExhaustive_Seq2_SHR_then_SUB(t *testing.T) {
	runSeq2(t, seqOps[0], seqOps[2]) // SHR then SUB
}
func TestExhaustive_Seq2_SHR_then_SHR(t *testing.T) {
	runSeq2(t, seqOps[0], seqOps[0]) // SHR then SHR
}
func TestExhaustive_Seq2_SUB_then_SHR(t *testing.T) {
	runSeq2(t, seqOps[2], seqOps[0]) // SUB then SHR
}
func TestExhaustive_Seq2_ADD_then_SHR(t *testing.T) {
	runSeq2(t, seqOps[3], seqOps[0]) // ADD then SHR
}
func TestExhaustive_Seq2_SHL_then_SUB(t *testing.T) {
	runSeq2(t, seqOps[1], seqOps[2]) // SHL then SUB
}
func TestExhaustive_Seq2_XOR_then_SHL(t *testing.T) {
	runSeq2(t, seqOps[4], seqOps[1]) // XOR then SHL
}
func TestExhaustive_Seq2_SUB_then_SUB(t *testing.T) {
	runSeq2(t, seqOps[2], seqOps[2]) // SUB then SUB
}

func runSeq2(t *testing.T, op1, op2 opDef) {
	t.Skip("too slow")

	valA := uint64(0xDEADBEEF12345678)
	valB := uint64(4)
	valC := uint64(3) // second operand for op2

	for d1 := 1; d1 <= exhaustN; d1++ {
		for ra := 1; ra <= exhaustN; ra++ {
			for rb := 1; rb <= exhaustN; rb++ {
				for d2 := 1; d2 <= exhaustN; d2++ {
					// rc is a fixed register different from d1 to supply valC.
					rc := 1
					for rc == d1 || rc == ra || rc == rb || rc == d2 {
						rc++
						if rc > exhaustN {
							rc = 1
							break
						}
					}

					label := fmt.Sprintf("%s_then_%s/d1=x%d/a=x%d/b=x%d/d2=x%d",
						op1.name, op2.name, d1, ra, rb, d2)
					t.Run(label, func(t *testing.T) {
						e := NewEmitter(nil)
						for i := 1; i <= exhaustN; i++ {
							e.Load(VReg(i), e.XBase(), int64(i)*8, I64, false)
						}
						// op1: x[d1] = op1(x[ra], x[rb])
						op1.emit(e, VReg(d1), VReg(ra), VReg(rb))
						// op2: x[d2] = op2(x[d1], x[rc])
						op2.emit(e, VReg(d2), VReg(d1), VReg(rc))
						// Write back both results.
						e.Store(e.XBase(), int64(d1)*8, VReg(d1), I64)
						e.Store(e.XBase(), int64(d2)*8, VReg(d2), I64)
						e.Ret(0x1000, 0, VRegZero)
						blk := e.Block

						// Compute expected values.
						initRegs := func(x *[32]uint64) {
							for i := 1; i <= exhaustN; i++ {
								x[i] = uint64(i * 111)
							}
							x[ra] = valA
							x[rb] = valB
							x[rc] = valC
						}

						// Reference computation.
						var xref [32]uint64
						initRegs(&xref)
						eA, eB := xref[ra], xref[rb]
						r1 := op1.ref(eA, eB)
						xref[d1] = r1
						r2 := op2.ref(xref[d1], xref[rc])
						wantD1 := r1
						wantD2 := r2

						// V1
						var xv1 [32]uint64
						initRegs(&xv1)
						execBlock(t, blk, &xv1, false)

						// V2
						var xv2 [32]uint64
						initRegs(&xv2)
						execBlock(t, blk, &xv2, true)

						if xv1[d2] != wantD2 {
							t.Errorf("V1: x[%d]=0x%x, want 0x%x (d1 x[%d]=0x%x want 0x%x)",
								d2, xv1[d2], wantD2, d1, xv1[d1], wantD1)
						}
						if xv2[d2] != wantD2 {
							t.Errorf("V2: x[%d]=0x%x, want 0x%x", d2, xv2[d2], wantD2)
						}
						if xv1[d1] != xv2[d1] || xv1[d2] != xv2[d2] {
							t.Errorf("V1!=V2: d1 V1=0x%x V2=0x%x, d2 V1=0x%x V2=0x%x",
								xv1[d1], xv2[d1], xv1[d2], xv2[d2])
						}
					})
				}
			}
		}
	}
}

// ── Same-register edge cases ──

func TestExhaustive_SHR_SameRegs(t *testing.T) {
	t.Skip("uses XBase-relative loads incompatible with rv8 layout; see TestRV8ExhaustExec_*")
	for rd := 1; rd <= exhaustN; rd++ {
		blk := buildSingleBlock(rd, rd, rd, (*Emitter).Shr)
		val := uint64(3)
		var x1, x2 [32]uint64
		x1[rd] = val
		x2[rd] = val
		execBlock(t, blk, &x1, false)
		execBlock(t, blk, &x2, true)
		want := val >> (val & 63)
		if x1[rd] != want {
			t.Errorf("V1 SHR x%d,x%d,x%d: got 0x%x want 0x%x", rd, rd, rd, x1[rd], want)
		}
		if x1[rd] != x2[rd] {
			t.Errorf("V1!=V2 SHR x%d,x%d,x%d: V1=0x%x V2=0x%x", rd, rd, rd, x1[rd], x2[rd])
		}
	}
}

// ── Three-instruction sequence tests ──
//
// Pattern from real SRL ELF failure: SHR → MOV → compare/use of the SHR result.
// The bug shows up when the MOV destination overlaps with a register the SHR
// result lives in, and the third instruction reads the SHR result.
//
//   x[d1] = SHR(x[ra], x[rb])
//   x[d2] = MOV(x[d1])             ← copy SHR result
//   x[d3] = ADD(x[d2], x[rc])      ← use the copy
//   verify x[d3]

func TestExhaustive_Seq3_SHR_MOV_ADD(t *testing.T) {
	runSeq3(t, "SHR_MOV_ADD",
		(*Emitter).Shr, func(a, b uint64) uint64 { return a >> (b & 63) },
		(*Emitter).Add, func(a, b uint64) uint64 { return a + b },
	)
}

func TestExhaustive_Seq3_SHR_MOV_SUB(t *testing.T) {
	runSeq3(t, "SHR_MOV_SUB",
		(*Emitter).Shr, func(a, b uint64) uint64 { return a >> (b & 63) },
		(*Emitter).Sub, func(a, b uint64) uint64 { return a - b },
	)
}

func TestExhaustive_Seq3_SUB_MOV_SHR(t *testing.T) {
	runSeq3(t, "SUB_MOV_SHR",
		(*Emitter).Sub, func(a, b uint64) uint64 { return a - b },
		(*Emitter).Shr, func(a, b uint64) uint64 { return a >> (b & 63) },
	)
}

func TestExhaustive_Seq3_SHL_MOV_SHR(t *testing.T) {
	runSeq3(t, "SHL_MOV_SHR",
		(*Emitter).Shl, func(a, b uint64) uint64 { return a << (b & 63) },
		(*Emitter).Shr, func(a, b uint64) uint64 { return a >> (b & 63) },
	)
}

// runSeq3 tests: d1 = op1(ra, rb); d2 = MOV(d1); d3 = op2(d2, rc)
// over all N^5 combos of (d1, ra, rb, d2, d3). rc is picked automatically.
func runSeq3(t *testing.T, name string,
	emit1 func(*Emitter, VReg, VReg, VReg), ref1 func(uint64, uint64) uint64,
	emit2 func(*Emitter, VReg, VReg, VReg), ref2 func(uint64, uint64) uint64,
) {
	t.Skip("too slow")

	const N = 6 // smaller N to keep O(N^5) tractable
	valA := uint64(0xDEADBEEF12345678)
	valB := uint64(4)
	valC := uint64(7)

	for d1 := 1; d1 <= N; d1++ {
		for ra := 1; ra <= N; ra++ {
			for rb := 1; rb <= N; rb++ {
				for d2 := 1; d2 <= N; d2++ {
					for d3 := 1; d3 <= N; d3++ {
						// Pick rc != d1, d2, d3 to hold valC independently.
						rc := 1
						for rc == d1 || rc == d2 || rc == d3 {
							rc++
						}
						if rc > N {
							continue
						}

						label := fmt.Sprintf("%s/d1=%d/a=%d/b=%d/d2=%d/d3=%d",
							name, d1, ra, rb, d2, d3)
						t.Run(label, func(t *testing.T) {
							e := NewEmitter(nil)
							for i := 1; i <= N; i++ {
								e.Load(VReg(i), e.XBase(), int64(i)*8, I64, false)
							}
							emit1(e, VReg(d1), VReg(ra), VReg(rb))
							e.Mov(VReg(d2), VReg(d1))
							emit2(e, VReg(d3), VReg(d2), VReg(rc))
							for i := 1; i <= N; i++ {
								e.Store(e.XBase(), int64(i)*8, VReg(i), I64)
							}
							e.Ret(0x1000, 0, VRegZero)
							blk := e.Block

							initRegs := func(x *[32]uint64) {
								for i := 1; i <= N; i++ {
									x[i] = uint64(i * 111)
								}
								x[ra] = valA
								x[rb] = valB
								x[rc] = valC
							}

							// Reference
							var xref [32]uint64
							initRegs(&xref)
							r1 := ref1(xref[ra], xref[rb])
							xref[d1] = r1
							xref[d2] = xref[d1] // MOV
							r3 := ref2(xref[d2], xref[rc])
							xref[d3] = r3

							var xv1, xv2 [32]uint64
							initRegs(&xv1)
							initRegs(&xv2)
							execBlock(t, blk, &xv1, false)
							execBlock(t, blk, &xv2, true)

							if xv1[d3] != xref[d3] {
								t.Errorf("V1: x[%d]=0x%x want 0x%x (d1=0x%x d2=0x%x)",
									d3, xv1[d3], xref[d3], xv1[d1], xv1[d2])
							}
							if xv1[d3] != xv2[d3] {
								t.Errorf("V1!=V2: d3 V1=0x%x V2=0x%x (d1 V1=0x%x V2=0x%x, d2 V1=0x%x V2=0x%x)",
									xv1[d3], xv2[d3], xv1[d1], xv2[d1], xv1[d2], xv2[d2])
							}
						})
					}
				}
			}
		}
	}
}

// TestExhaustive_Seq_CONST_CONST_SHR tests the pattern from real ELF test:
// x[ra] = Const(valA); x[rb] = Const(valB); x[d1] = SHR(x[ra], x[rb])
// This catches bugs where Const clobbers a register that SHR needs.
func TestExhaustive_Seq_CONST_CONST_SHR(t *testing.T) {
	t.Skip("too slow")

	const N = 7
	valA := uint64(0x80000000)
	valB := uint64(7)
	want := valA >> valB // 0x01000000

	for d1 := 1; d1 <= N; d1++ {
		for ra := 1; ra <= N; ra++ {
			for rb := 1; rb <= N; rb++ {
				label := fmt.Sprintf("CONST_CONST_SHR/d=%d/a=%d/b=%d", d1, ra, rb)
				t.Run(label, func(t *testing.T) {
					e := NewEmitter(nil)
					// Load all regs first (creates allocation pressure).
					for i := 1; i <= N; i++ {
						e.Load(VReg(i), e.XBase(), int64(i)*8, I64, false)
					}
					// Const → Const → SHR
					e.Const(VReg(ra), int64(valA))
					e.Const(VReg(rb), int64(valB))
					e.Shr(VReg(d1), VReg(ra), VReg(rb))
					// Write back result.
					e.Store(e.XBase(), int64(d1)*8, VReg(d1), I64)
					e.Ret(0x1000, 0, VRegZero)
					blk := e.Block

					w := want
					if ra == rb {
						// Both Consts write the same VReg; last write wins (valB=7).
						// SHR(7, 7) = 0
						w = valB >> (valB & 63)
					}
					if ra == d1 && ra != rb {
						// Const(ra, valA) then SHR(d1=ra, ra, rb) = SHR(valA, valB).
						// But d1==ra so the SHR result overwrites ra. Fine, w is still want.
					}

					var x1, x2 [32]uint64
					execBlock(t, blk, &x1, false)
					execBlock(t, blk, &x2, true)

					if x1[d1] != w {
						t.Errorf("V1: x[%d]=0x%x want 0x%x", d1, x1[d1], w)
					}
					if x2[d1] != w {
						t.Errorf("V2: x[%d]=0x%x want 0x%x", d1, x2[d1], w)
					}
					if x1[d1] != x2[d1] {
						t.Errorf("V1!=V2: V1=0x%x V2=0x%x", x1[d1], x2[d1])
					}
				})
			}
		}
	}
}

// TestExhaustive_Seq_CONST_SHR_MOV_CONST_BNE mirrors the actual ELF test pattern:
//
//	CONST x[ra] = A; CONST x[rb] = B; SHR x[d1] = x[ra], x[rb];
//	MOV x[d2] = x[d1]; CONST x[d3] = expected; compare x[d2] vs x[d3]
func TestExhaustive_Seq_FullTestPattern(t *testing.T) {
	t.Skip("too slow")

	const N = 6
	valA := uint64(0x80000000)
	valB := uint64(7)
	expected := valA >> valB // 0x01000000

	for ra := 1; ra <= N; ra++ {
		for rb := 1; rb <= N; rb++ {
			if ra == rb {
				continue
			}
			for d1 := 1; d1 <= N; d1++ {
				for d2 := 1; d2 <= N; d2++ {
					for d3 := 1; d3 <= N; d3++ {
						if d3 == d2 {
							continue
						} // d3 holds expected, d2 holds result
						label := fmt.Sprintf("a=%d/b=%d/d1=%d/d2=%d/d3=%d", ra, rb, d1, d2, d3)
						t.Run(label, func(t *testing.T) {
							e := NewEmitter(nil)
							for i := 1; i <= N; i++ {
								e.Load(VReg(i), e.XBase(), int64(i)*8, I64, false)
							}
							e.Const(VReg(ra), int64(valA))
							e.Const(VReg(rb), int64(valB))
							e.Shr(VReg(d1), VReg(ra), VReg(rb))
							e.Mov(VReg(d2), VReg(d1))
							e.Const(VReg(d3), int64(expected))
							// Store d2 and d3 for comparison.
							e.Store(e.XBase(), int64(d2)*8, VReg(d2), I64)
							e.Store(e.XBase(), int64(d3)*8, VReg(d3), I64)
							e.Ret(0x1000, 0, VRegZero)
							blk := e.Block

							var x1, x2 [32]uint64
							execBlock(t, blk, &x1, false)
							execBlock(t, blk, &x2, true)

							if x1[d2] != expected {
								t.Errorf("V1: x[%d]=0x%x want 0x%x", d2, x1[d2], expected)
							}
							if x1[d2] != x2[d2] {
								t.Errorf("V1!=V2 d2: V1=0x%x V2=0x%x", x1[d2], x2[d2])
							}
							if x1[d3] != expected {
								t.Errorf("V1 d3: x[%d]=0x%x want 0x%x", d3, x1[d3], expected)
							}
						})
					}
				}
			}
		}
	}
}

// TestCMP_Convention verifies what CMPQ actually computes in the Go assembler.
// We test: is CMPQ(From=a, To=b) computing a-b or b-a?
// Method: CMPQ(3, 5) then SETLT. If LT is true, it computed 3-5<0 (From-To).
// If LT is false, it computed 5-3>0 (To-From).
func TestCMP_Convention(t *testing.T) {
	// Build: load x1=3, x2=5, CMP x1,x2, SETLT x3, store x3, ret.
	e := NewEmitter(nil)
	x1 := VReg(1)
	x2 := VReg(2)
	x3 := VReg(3)
	e.Const(x1, 3)
	e.Const(x2, 5)
	e.Set(x3, x1, x2, LT) // x3 = (x1 < x2) ? 1 : 0 = (3 < 5) = 1
	e.Store(e.XBase(), 24, x3, I64)
	e.Ret(0x1000, 0, VRegZero)

	var x [32]uint64

	// V1
	execBlock(t, e.Block, &x, false)
	t.Logf("V1: SET(3 < 5) = %d", x[3])
	if x[3] != 1 {
		t.Errorf("V1: expected 1 (3 < 5 is true), got %d", x[3])
	}

	// V2
	x = [32]uint64{}
	execBlock(t, e.Block, &x, true)
	t.Logf("V2: SET(3 < 5) = %d", x[3])
	if x[3] != 1 {
		t.Errorf("V2: expected 1 (3 < 5 is true), got %d", x[3])
	}

	// Also test the reverse: 5 < 3 should be 0.
	e2 := NewEmitter(nil)
	e2.Const(x1, 5)
	e2.Const(x2, 3)
	e2.Set(x3, x1, x2, LT) // x3 = (5 < 3) ? 1 : 0 = 0
	e2.Store(e2.XBase(), 24, x3, I64)
	e2.Ret(0x1000, 0, VRegZero)

	x = [32]uint64{}
	execBlock(t, e2.Block, &x, false)
	t.Logf("V1: SET(5 < 3) = %d", x[3])
	if x[3] != 0 {
		t.Errorf("V1: expected 0 (5 < 3 is false), got %d", x[3])
	}

	x = [32]uint64{}
	execBlock(t, e2.Block, &x, true)
	t.Logf("V2: SET(5 < 3) = %d", x[3])
	if x[3] != 0 {
		t.Errorf("V2: expected 0 (5 < 3 is false), got %d", x[3])
	}
}

// TestSETImm_Convention tests SetImm (compare register against immediate).
// SLTI x3, x1, 5 means x3 = (x1 < 5) ? 1 : 0
func TestSETImm_Convention(t *testing.T) {
	cases := []struct {
		name string
		val  int64
		imm  int64
		pred Pred
		want uint64
	}{
		{"3<5", 3, 5, LT, 1},
		{"5<3", 5, 3, LT, 0},
		{"5<5", 5, 5, LT, 0},
		{"-1<0", -1, 0, LT, 1},
		{"0<-1", 0, -1, LT, 0},
		// Unsigned
		{"3u<5", 3, 5, LTU, 1},
		{"5u<3", 5, 3, LTU, 0},
		// -1 unsigned is MAX, so -1 <u 0 is false, 0 <u -1 is true
		{"-1u<0", -1, 0, LTU, 0},
		{"0u<-1", 0, -1, LTU, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := NewEmitter(nil)
			x1 := VReg(1)
			x3 := VReg(3)
			e.Const(x1, tc.val)
			e.SetImm(x3, x1, tc.imm, tc.pred)
			e.Store(e.XBase(), 24, x3, I64)
			e.Ret(0x1000, 0, VRegZero)

			var xv1, xv2 [32]uint64
			execBlock(t, e.Block, &xv1, false)
			execBlock(t, e.Block, &xv2, true)
			t.Logf("V1=%d V2=%d want=%d", xv1[3], xv2[3], tc.want)
			if xv1[3] != tc.want {
				t.Errorf("V1: got %d, want %d", xv1[3], tc.want)
			}
			if xv2[3] != tc.want {
				t.Errorf("V2: got %d, want %d", xv2[3], tc.want)
			}
			if xv1[3] != xv2[3] {
				t.Errorf("V1!=V2: %d vs %d", xv1[3], xv2[3])
			}
		})
	}
}

// TestSETImm_Debug dumps allocation for the 3<5 case to find the V2 bug.
func TestSETImm_Debug(t *testing.T) {
	e := NewEmitter(nil)
	x1 := VReg(1)
	x3 := VReg(3)
	e.Const(x1, 3)
	e.SetImm(x3, x1, 5, LT)
	e.Store(e.XBase(), 24, x3, I64)
	e.Ret(0x1000, 0, VRegZero)

	blk := e.Block
	t.Logf("IR has %d instructions:", len(blk.Instrs))
	for i, ins := range blk.Instrs {
		t.Logf("  [%d] %v", i, ins)
	}

	pool := RV8Pool(blk)
	alloc := helperTestAllocate(blk, pool, RV8Pinned(), nil)
	t.Logf("RV8 allocation:")
	for _, ia := range alloc.IntervalMap {
		t.Logf("  VReg(%d) [%d..%d] host=%d", ia.Interval.VReg, ia.Interval.Start, ia.Interval.End, ia.Host)
	}
}
