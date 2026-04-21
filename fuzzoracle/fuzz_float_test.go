package fuzzoracle

import (
	"encoding/binary"
	"math"
	"testing"

	riscv "riscv"
)

// FuzzFDVsLibriscv fuzzes RV64F+D instructions against libriscv.
//
// Excluded (known libriscv bugs):
//   - FCLASS on infinity: libriscv sets two bits; spec requires one
//   - FCLASS on NaN:      libriscv sets two bits; spec requires one
//   - FCVT.S.L / FCVT.D.L / FCVT.S.LU / FCVT.D.LU: libriscv truncates to 32-bit
//
// Corpus: [family:1][rm:1][f2:8][f3:8][xi:8]
//   family — selects instruction class
//   rm     — rounding mode (0=RNE..3=RUP; 7=DYN → forced to RNE)
//   f2     — raw uint64 bits for f2 (rs1 source)
//   f3     — raw uint64 bits for f3 (rs2 source / store value)
//   xi     — raw uint64 for x3 (integer source for int→float conversions)

func FuzzFDVsLibriscv(f *testing.F) {
	// Seeds: one per instruction family, with well-defined values
	type seed struct {
		family uint8
		f2, f3 uint64
		xi     uint64
	}
	seeds := []seed{
		{0, nb32(bits32(1.5)), nb32(bits32(2.5)), 0},           // FADD.S
		{1, bits64(1.5), bits64(2.5), 0},                       // FADD.D
		{2, nb32(bits32(5.0)), nb32(bits32(2.0)), 0},           // FSUB.S
		{3, bits64(5.0), bits64(2.0), 0},                       // FSUB.D
		{4, nb32(bits32(2.0)), nb32(bits32(3.0)), 0},           // FMUL.S
		{5, bits64(2.0), bits64(3.0), 0},                       // FMUL.D
		{6, nb32(bits32(7.0)), nb32(bits32(2.0)), 0},           // FDIV.S
		{7, bits64(7.0), bits64(2.0), 0},                       // FDIV.D
		{8, nb32(bits32(4.0)), 0, 0},                           // FSQRT.S
		{9, bits64(4.0), 0, 0},                                  // FSQRT.D
		{10, nb32(bits32(-3.0)), nb32(bits32(1.0)), 0},         // FSGNJ.S
		{11, bits64(-3.0), bits64(1.0), 0},                     // FSGNJ.D
		{12, nb32(bits32(1.0)), nb32(bits32(2.0)), 0},          // FMIN.S
		{13, bits64(1.0), bits64(2.0), 0},                      // FMIN.D
		{14, nb32(bits32(-3.0)), nb32(bits32(1.0)), 0},         // FSGNJN.S
		{15, bits64(-3.0), bits64(1.0), 0},                     // FSGNJN.D
		{16, nb32(bits32(3.0)), nb32(bits32(-1.0)), 0},         // FSGNJX.S
		{17, bits64(3.0), bits64(-1.0), 0},                     // FSGNJX.D
		{18, nb32(bits32(3.0)), nb32(bits32(5.0)), 0},          // FMAX.S
		{19, bits64(3.0), bits64(5.0), 0},                      // FMAX.D
		{20, nb32(bits32(1.0)), 0, 0},                          // FMV.X.W
		{21, bits64(1.0), 0, 0},                                // FMV.X.D
		{22, 0, 0, uint64(bits32(1.5))},                        // FMV.W.X
		{23, 0, 0, bits64(1.5)},                                // FMV.D.X
		{24, nb32(bits32(1.0)), nb32(bits32(2.0)), 0},          // FEQ.S
		{25, bits64(1.0), bits64(2.0), 0},                      // FEQ.D
		{26, nb32(bits32(2.0)), nb32(bits32(3.0)), nb32(bits32(1.0))}, // FMSUB.S
		{27, bits64(2.0), bits64(3.0), bits64(1.0)},            // FMSUB.D
		{28, nb32(bits32(2.0)), nb32(bits32(3.0)), nb32(bits32(1.0))}, // FMADD.S
		{29, bits64(2.0), bits64(3.0), bits64(1.0)},            // FMADD.D
	}
	for _, s := range seeds {
		var buf [26]byte
		buf[0] = s.family
		buf[1] = 0 // rm=RNE
		binary.LittleEndian.PutUint64(buf[2:], s.f2)
		binary.LittleEndian.PutUint64(buf[10:], s.f3)
		binary.LittleEndian.PutUint64(buf[18:], s.xi)
		f.Add(buf[:])
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) < 26 {
			return
		}

		family := data[0] % 30
		rm     := uint32(data[1] & 0x3) // 0=RNE,1=RTZ,2=RDN,3=RUP (skip DYN)
		f2val  := binary.LittleEndian.Uint64(data[2:])
		f3val  := binary.LittleEndian.Uint64(data[10:])
		xival  := binary.LittleEndian.Uint64(data[18:])

		// For single-precision inputs: ensure NaN-boxed
		f2S := nb32(uint32(f2val))
		f3S := nb32(uint32(f3val))

		// Avoid infinities and NaNs for arithmetic (produce implementation-
		// defined results that libriscv may handle differently)
		f2clean := cleanF32(uint32(f2val))
		f3clean := cleanF32(uint32(f3val))
		f2cleanD := cleanF64(f2val)
		f3cleanD := cleanF64(f3val)

		var insn uint32
		var initF [32]uint64
		var initX [32]uint64
		initX[2] = oracleDataVA
		for i := range initF { initF[i] = nb32(0) }

		switch family {
		// ── Single-precision arithmetic ───────────────────────────────────
		case 0: // FADD.S f1,f2,f3
			insn = fpf(0x00, rm, 1, 2, 3)
			initF[2], initF[3] = nb32(f2clean), nb32(f3clean)
		case 2: // FSUB.S
			insn = fpf(0x01, rm, 1, 2, 3)
			initF[2], initF[3] = nb32(f2clean), nb32(f3clean)
		case 4: // FMUL.S
			insn = fpf(0x02, rm, 1, 2, 3)
			initF[2], initF[3] = nb32(f2clean), nb32(f3clean)
		case 6: // FDIV.S — avoid div by zero
			insn = fpf(0x03, rm, 1, 2, 3)
			d := f3clean; if d == 0 { d = bits32(1.0) }
			initF[2], initF[3] = nb32(f2clean), nb32(d)
		case 8: // FSQRT.S — positive only
			insn = fpf(0x0B, rm, 1, 2, 0)
			v := f2clean; if math.Signbit(float64(math.Float32frombits(v))) { v ^= 0x80000000 }
			initF[2] = nb32(v)
		// ── Double-precision arithmetic ───────────────────────────────────
		case 1: // FADD.D
			insn = fpfD(0x00, rm, 1, 2, 3)
			initF[2], initF[3] = f2cleanD, f3cleanD
		case 3: // FSUB.D
			insn = fpfD(0x01, rm, 1, 2, 3)
			initF[2], initF[3] = f2cleanD, f3cleanD
		case 5: // FMUL.D
			insn = fpfD(0x02, rm, 1, 2, 3)
			initF[2], initF[3] = f2cleanD, f3cleanD
		case 7: // FDIV.D — avoid div by zero
			insn = fpfD(0x03, rm, 1, 2, 3)
			d := f3cleanD; if d == 0 { d = bits64(1.0) }
			initF[2], initF[3] = f2cleanD, d
		case 9: // FSQRT.D — positive only
			insn = fpfD(0x0B, rm, 1, 2, 0)
			v := f2cleanD; if v>>63 != 0 { v ^= f64SignBit }
			initF[2] = v
		// ── Sign injection ────────────────────────────────────────────────
		case 10: // FSGNJ.S (all three: rm 0/1/2)
			insn = fpf(0x04, rm%3, 1, 2, 3)
			initF[2], initF[3] = f2S, f3S
		case 11: // FSGNJ.D
			insn = fpfD(0x04, rm%3, 1, 2, 3)
			initF[2], initF[3] = f2val, f3val
		// ── FMIN/FMAX ─────────────────────────────────────────────────────
		case 12: // FMIN.S / FMAX.S
			insn = fpf(0x05, rm&1, 1, 2, 3)
			initF[2], initF[3] = nb32(f2clean), nb32(f3clean)
		case 13: // FMIN.D / FMAX.D
			insn = fpfD(0x05, rm&1, 1, 2, 3)
			initF[2], initF[3] = f2cleanD, f3cleanD
		// ── FSGNJ variants (repurposing 14-19 slots) ────────────────────────
		case 14: // FSGNJN.S
			insn = fpf(0x04, 1, 1, 2, 3)
			initF[2], initF[3] = nb32(f2clean), nb32(f3clean)
		case 15: // FSGNJN.D
			insn = fpfD(0x04, 1, 1, 2, 3)
			initF[2], initF[3] = f2cleanD, f3cleanD
		case 16: // FSGNJX.S
			insn = fpf(0x04, 2, 1, 2, 3)
			initF[2], initF[3] = nb32(f2clean), nb32(f3clean)
		case 17: // FSGNJX.D
			insn = fpfD(0x04, 2, 1, 2, 3)
			initF[2], initF[3] = f2cleanD, f3cleanD
		// ── FMAX (repurposing 18-19) ──────────────────────────────────────
		case 18: // FMAX.S
			insn = fpf(0x05, 1, 1, 2, 3)
			initF[2], initF[3] = nb32(f2clean), nb32(f3clean)
		case 19: // FMAX.D
			insn = fpfD(0x05, 1, 1, 2, 3)
			initF[2], initF[3] = f2cleanD, f3cleanD
		// ── Bit moves ─────────────────────────────────────────────────────
		case 20: // FMV.X.W
			insn = fpf(0x1C, 0, 1, 2, 0)
			initF[2] = f2S
		case 21: // FMV.X.D
			insn = fpfD(0x1C, 0, 1, 2, 0)
			initF[2] = f2val
		case 22: // FMV.W.X
			insn = fpf(0x1E, 0, 1, 2, 0)
			initX[2] = xival
		case 23: // FMV.D.X
			insn = fpfD(0x1E, 0, 1, 2, 0)
			initX[2] = xival
		// ── Compare ───────────────────────────────────────────────────────
		case 24: // FEQ/FLT/FLE.S
			insn = fpf(0x14, rm%3, 1, 2, 3)
			initF[2], initF[3] = nb32(f2clean), nb32(f3clean)
		case 25: // FEQ/FLT/FLE.D
			insn = fpfD(0x14, rm%3, 1, 2, 3)
			initF[2], initF[3] = f2cleanD, f3cleanD
		// ── FMSUB / FNMADD / FNMSUB (replacing FCLASS — libriscv bug) ──────
		case 26: // FMSUB.S f1,f2,f3,f4
			insn = r4(0x47, 0, rm, 1, 2, 3, 4)
			initF[2] = nb32(f2clean)
			initF[3] = nb32(f3clean)
			initF[4] = nb32(cleanF32(uint32(xival)))
		case 27: // FMSUB.D
			insn = r4(0x47, 1, rm, 1, 2, 3, 4)
			initF[2] = f2cleanD
			initF[3] = f3cleanD
			initF[4] = cleanF64(xival)
		// ── Fused multiply-add ────────────────────────────────────────────
		case 28: // FMADD.S f1,f2,f3,f4 (f4=f3val)
			insn = r4(0x43, 0, rm, 1, 2, 3, 4)
			initF[2] = nb32(f2clean)
			initF[3] = nb32(f3clean)
			initF[4] = nb32(uint32(xival))
		case 29: // FMADD.D
			insn = r4(0x43, 1, rm, 1, 2, 3, 4)
			initF[2] = f2cleanD
			initF[3] = f3cleanD
			initF[4] = cleanF64(xival)
		default:
			return
		}

		runOneF(t, insn, initX, initF, nil)
	})
}

// cleanF32 replaces NaN/Inf with finite values to avoid implementation-
// defined behaviour that libriscv may handle differently.
func cleanF32(bits uint32) uint32 {
	if isSpecialF32(bits) { return math.Float32bits(1.0) }
	return bits
}

func cleanF64(bits uint64) uint64 {
	if isSpecialF64(bits) { return math.Float64bits(1.0) }
	return bits
}

func isSpecialF32(bits uint32) bool {
	exp := (bits >> 23) & 0xFF
	return exp == 0xFF // NaN or Inf
}

func isSpecialF64(bits uint64) bool {
	exp := (bits >> 52) & 0x7FF
	return exp == 0x7FF
}

// nb32 and bits32/bits64 are in float_test.go (same package)

// f64SignBit is also in float.go (package riscv), but we need it here as a
// local constant for the fuzz test (it's unexported from the riscv package).
const f64SignBit = uint64(0x8000000000000000)

// FuzzCFloatVsLibriscv fuzzes C.FLD, C.FSD, C.FLDSP, C.FSDSP against libriscv.
// Corpus: [op:1][fval:8][memval:8]
//   op     — 0=C.FLD, 1=C.FSD, 2=C.FLDSP, 3=C.FSDSP
//   fval   — raw bits placed in f1 (for stores)
//   memval — raw 8 bytes at data address (for loads)

func FuzzCFloatVsLibriscv(f *testing.F) {
	f.Add([]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0xDE, 0xAD, 0xBE, 0xEF, 0xCA, 0xFE, 0xBA, 0xBE})
	f.Add([]byte{1, 0x40, 0x09, 0x21, 0xFB, 0x54, 0x44, 0x2D, 0x18, 0, 0, 0, 0, 0, 0, 0, 0})
	f.Add([]byte{2, 0, 0, 0, 0, 0, 0, 0, 0, 0xDE, 0xAD, 0xBE, 0xEF, 0xCA, 0xFE, 0xBA, 0xBE})
	f.Add([]byte{3, 0x40, 0x09, 0x21, 0xFB, 0x54, 0x44, 0x2D, 0x18, 0, 0, 0, 0, 0, 0, 0, 0})

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) < 17 { return }
		op     := data[0] & 3
		fval   := binary.LittleEndian.Uint64(data[1:])
		memval := data[9:17]

		// Build 16-bit instruction + C.EBREAK + ECALL
		var insn16 uint16
		var initX [32]uint64
		var initF [32]uint64
		for i := range initF { initF[i] = nb32(0) }
		initX[2] = oracleDataVA + 64  // sp
		initX[9] = oracleDataVA       // rs1 for Q0 instructions

		initMem := make([]byte, 128)
		copy(initMem[0:], memval)      // Q0 load target at oracleDataVA
		copy(initMem[64:], memval)     // Q2 load target at sp offset

		switch op {
		case 0: // C.FLD f8(rd'=0), 0(x9(rs1'=1))
			insn16 = uint16(0b001<<13 | 0<<10 | 1<<7 | 0<<5 | 0<<2 | 0b00)
		case 1: // C.FSD f9(rs2'=1), 0(x9(rs1'=1))
			insn16 = uint16(0b101<<13 | 0<<10 | 1<<7 | 0<<5 | 1<<2 | 0b00)
			initF[9] = fval
		case 2: // C.FLDSP f1, 0
			insn16 = uint16(0b001<<13 | 0<<12 | 1<<7 | 0<<5 | 0<<2 | 0b10)
		case 3: // C.FSDSP f1, 0
			insn16 = uint16(0b101<<13 | 0<<10 | 0<<7 | 1<<2 | 0b10)
			initF[1] = fval
		}

		word0 := uint32(insn16) | (uint32(0x9002) << 16)
		elf := riscv.BuildELF(oracleCodeVA, []uint32{word0, 0x00000073})

		lm := NewMachine(elf)
		if lm == nil { return }
		defer lm.Close()
		lm.WriteGuest(oracleDataVA, initMem)
		lm.SetRegsAndPC(initX, oracleCodeVA)
		lm.SetFRegs(initF)
		lm.RunToEcall()
		lFRegs := lm.SnapshotFRegs()
		lMem   := lm.SnapshotMem(0, oracleMemSize)

		mem, err := riscv.NewGuestMemory(oracleMemSize)
		if err != nil { t.Fatal(err) }
		defer mem.Free()
		riscv.LoadELFBytes(mem, elf)
		mem.WriteBytes(oracleDataVA, initMem)

		cpu := riscv.NewCPU(*mem)
		cpu.SetPC(oracleCodeVA)
		cpu.SetReg(2, initX[2])
		cpu.SetReg(9, initX[9])
		for r := uint8(0); r < 32; r++ { cpu.SetFReg(r, initF[r]) }
		cpu.Step()

		for r := 0; r < 32; r++ {
			if cpu.FReg(uint8(r)) != lFRegs[r] {
				t.Fatalf("op=%d f%d: ours=0x%016X libriscv=0x%016X", op, r, cpu.FReg(uint8(r)), lFRegs[r])
			}
		}
		ourMem := make([]byte, oracleMemSize)
		if lMem != nil {
			if f := mem.ReadBytes(0, ourMem); f == nil {
				for i := range ourMem {
					if ourMem[i] != lMem[i] {
						t.Fatalf("op=%d mem[0x%05X]: ours=0x%02X libriscv=0x%02X", op, i, ourMem[i], lMem[i])
					}
				}
			}
		}
	})
}
