package riscv

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/arch/x86/x86asm"
	"riscv/goasm"
)

// TestVizJit_BranchTargets_MatchMachineCode verifies that post-assembly
// DumpProgs branch targets agree with actual machine code decoded via
// x86asm. If this test fails, the VizJit diagnostic dumps are lying.
func TestVizJit_BranchTargets_MatchMachineCode(t *testing.T) {
	e := NewEmitter(nil)

	// Build a block with both a backward branch (loop) and a forward branch.
	loopTop := e.NewLabel()
	exitLabel := e.NewLabel()

	e.PlaceLabel(loopTop)
	e.AddImm(e.XReg(1), e.XReg(1), 1)
	e.Const(e.XReg(2), 10)
	e.Branch(e.XReg(1), e.XReg(2), NE, exitLabel)
	e.StopperLoad(0x1000)
	e.Jump(loopTop)

	e.PlaceLabel(exitLabel)
	e.WriteBackAll()
	e.Ret(0x80001000, 0, VRegZero)

	b := e.Block
	b.maxVreg = MaxVReg(b)

	pool := RV8Pool(b)
	pinned := RV8Pinned()
	alloc := helperTestAllocate(b, pool, pinned, nil)

	ctx := goasm.New(goasm.AMD64)
	ctx.Append(ctx.NewATEXT())
	if _, err := LowerAMD64_RV8(ctx, b, alloc); err != nil {
		t.Fatalf("LowerAMD64_RV8: %v", err)
	}

	code, err := ctx.Assemble()
	if err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if len(code) == 0 {
		t.Fatal("assembled code is empty")
	}

	// Post-assembly DumpProgs — this is what VizJit now writes to disk.
	progDump := ctx.DumpProgs()

	// Extract branch targets from DumpProgs output.
	progBranches := parseDumpProgsBranches(progDump)
	if len(progBranches) == 0 {
		t.Fatalf("no branches found in DumpProgs output:\n%s", progDump)
	}

	// Decode actual machine code with x86asm.
	x86Branches := decodeX86BranchTargets(code)
	if len(x86Branches) == 0 {
		t.Fatalf("no branches found in %d bytes of machine code", len(code))
	}

	t.Logf("DumpProgs branches: %v", progBranches)
	t.Logf("x86asm branches:    %v", x86Branches)

	if len(progBranches) != len(x86Branches) {
		t.Logf("DumpProgs output:\n%s", progDump)
		t.Logf("x86asm disassembly:\n%s", disasmX86(code))
		t.Fatalf("branch count mismatch: DumpProgs=%d x86asm=%d",
			len(progBranches), len(x86Branches))
	}
	for i := range progBranches {
		if progBranches[i] != x86Branches[i] {
			t.Logf("DumpProgs output:\n%s", progDump)
			t.Logf("x86asm disassembly:\n%s", disasmX86(code))
			t.Errorf("branch %d target mismatch: DumpProgs=%d x86asm=%d",
				i, progBranches[i], x86Branches[i])
		}
	}
}

// branchOps lists x86 branch mnemonics we care about (excludes CALL/RET).
var branchRE = regexp.MustCompile(`(?i)(?:^|\])\s*(JMP|JEQ|JNE|JGE|JLE|JGT|JLT|JBE|JAE|JA|JB|JCC|JCS|JHI|JLS|JMI|JPL|JVS|JVC|JOC|JOS|JPC|JPS)\s+(\d+)\s*$`)

// parseDumpProgsBranches extracts branch target byte offsets from
// DumpProgs output. Only matches lines like "JMP\t47" or "JNE\t103"
// (numeric targets — skips register-indirect jumps like "JMP CX").
func parseDumpProgsBranches(dump string) []int64 {
	var targets []int64
	for _, line := range strings.Split(dump, "\n") {
		line = strings.TrimSpace(line)
		m := branchRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		v, err := strconv.ParseInt(m[2], 10, 64)
		if err != nil {
			continue
		}
		targets = append(targets, v)
	}
	return targets
}

// decodeX86BranchTargets walks machine code bytes and returns the
// absolute byte offset target for every branch instruction.
func decodeX86BranchTargets(code []byte) []int64 {
	var targets []int64
	pc := 0
	for pc < len(code) {
		inst, err := x86asm.Decode(code[pc:], 64)
		if err != nil {
			pc++
			continue
		}
		if isX86Branch(inst) {
			if target, ok := x86BranchTarget(inst, uint64(pc)); ok {
				targets = append(targets, int64(target))
			}
		}
		pc += inst.Len
	}
	return targets
}

func isX86Branch(inst x86asm.Inst) bool {
	switch inst.Op {
	case x86asm.JMP, x86asm.JA, x86asm.JAE, x86asm.JB, x86asm.JBE,
		x86asm.JE, x86asm.JG, x86asm.JGE, x86asm.JL, x86asm.JLE,
		x86asm.JNE, x86asm.JNO, x86asm.JNP, x86asm.JNS,
		x86asm.JO, x86asm.JP, x86asm.JS,
		x86asm.JCXZ, x86asm.JECXZ, x86asm.JRCXZ:
		return true
	}
	return false
}

// x86BranchTarget returns the absolute byte offset target for a
// PC-relative branch. Returns false for register-indirect jumps.
func x86BranchTarget(inst x86asm.Inst, pc uint64) (uint64, bool) {
	if len(inst.Args) == 0 {
		return 0, false
	}
	rel, ok := inst.Args[0].(x86asm.Rel)
	if !ok {
		return 0, false
	}
	target := pc + uint64(inst.Len) + uint64(int64(rel))
	return target, true
}

// disasmX86 returns a full disassembly listing for debugging failures.
func disasmX86(code []byte) string {
	var sb strings.Builder
	pc := 0
	for pc < len(code) {
		inst, err := x86asm.Decode(code[pc:], 64)
		if err != nil {
			fmt.Fprintf(&sb, "%4d: ???\n", pc)
			pc++
			continue
		}
		text := x86asm.GoSyntax(inst, uint64(pc), nil)
		fmt.Fprintf(&sb, "%4d: %s\n", pc, text)
		pc += inst.Len
	}
	return sb.String()
}
