package riscv

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

// TestClassifyFlow_SystemOps confirms ECALL is ordinary fallthrough for
// lazy block formation, while EBREAK and CSR instructions still stop blocks.
func TestClassifyFlow_SystemOps(t *testing.T) {
	mem, err := NewGuestMemory(Size1MB)
	if err != nil {
		t.Fatalf("NewGuestMemory: %v", err)
	}
	defer mem.Free()

	const pc = uint64(0x100)

	writeInsn := func(insn uint32) {
		if f := mem.Store16(pc, uint16(insn)); f != nil {
			t.Fatalf("Store16: %v", f)
		}
		if f := mem.Store16(pc+2, uint16(insn>>16)); f != nil {
			t.Fatalf("Store16: %v", f)
		}
	}

	type row struct {
		name string
		insn uint32
		want flowClass
	}
	rows := []row{
		{"ECALL", 0x00000073, flowSeq},
		{"EBREAK", 0x00100073, flowTerm},
		{"CSRRW", 0x30001073, flowTerm},
	}

	for _, r := range rows {
		writeInsn(r.insn)
		gotFC, _, sz := classifyFlow(mem, pc)
		if gotFC != r.want || sz != 4 {
			t.Errorf("%s: got (fc=%v, sz=%d), want (fc=%v, sz=4)",
				r.name, gotFC, sz, r.want)
		}
	}
}

func TestScanLazyBlockUsesLargeLinearRegion(t *testing.T) {
	mem, err := NewGuestMemory(Size1MB)
	if err != nil {
		t.Fatalf("NewGuestMemory: %v", err)
	}
	defer mem.Free()

	oldSplit := PerBlockCapTimeToSplit
	PerBlockCapTimeToSplit = 3
	defer func() { PerBlockCapTimeToSplit = oldSplit }()

	const entry = uint64(0x1000)
	mem.AddExecRegion(entry, 0x1200, false)
	if f := mem.Store32(entry, jenc(1, 0x100)); f != nil { // JAL ra, +0x100
		t.Fatalf("Store32 call: %v", f)
	}
	if f := mem.Store32(entry+4, ienc(opOPIMM, 0, 5, 0, 1)); f != nil {
		t.Fatalf("Store32 fallthrough: %v", f)
	}
	if f := mem.Store32(entry+8, ienc(opOPIMM, 0, 6, 0, 2)); f != nil {
		t.Fatalf("Store32 second fallthrough: %v", f)
	}
	if f := mem.Store32(entry+12, ienc(opJALR, 0, 0, 1, 0)); f != nil {
		t.Fatalf("Store32 return: %v", f)
	}
	if f := mem.Store32(entry+0x100, ienc(opOPIMM, 0, 6, 0, 2)); f != nil {
		t.Fatalf("Store32 callee: %v", f)
	}

	lazy := scanLazyBlock(mem, entry)
	if lazy.endPC != entry+16 || lazy.pcCount != 4 {
		t.Fatalf("scanLazyBlock = {endPC:0x%x pcCount:%d}, want {0x%x, 4}",
			lazy.endPC, lazy.pcCount, entry+16)
	}
	bfs := scanRegion(mem, entry)
	if bfs.endPC <= lazy.endPC {
		t.Fatalf("scanRegion endPC = 0x%x, want it to chase callee beyond lazy end 0x%x",
			bfs.endPC, lazy.endPC)
	}
}

// TestInlineEcall_HelloEndToEnd runs the full hello-world ELF through
// the JIT with an installed Linux OS personality. ECALL must return to
// Go for OS handling; JIT/native code must not issue host syscalls.
func TestInlineEcall_HelloEndToEnd(t *testing.T) {

	data, err := os.ReadFile("bench/hello_guest/hello_gocpu.elf")
	if err != nil {
		t.Skipf("bench/hello_guest/hello_gocpu.elf: %v", err)
	}

	mem, err := NewGuestMemory(Size64MB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()
	elf, err := LoadELFBytes(mem, data)
	if err != nil {
		t.Fatalf("LoadELFBytes: %v", err)
	}
	cpu := NewCPU(*mem)
	cpu.SetPC(elf.Entry)
	cpu.SetReg(2, 0x03F00000)

	var out bytes.Buffer
	cleanup := InstallLinuxOS(cpu, &out)
	defer cleanup()

	j := NewJIT()
	runErr := j.RunJIT(cpu)
	if runErr != nil {
		if _, ok := runErr.(*ExitError); !ok {
			t.Fatalf("RunJIT: %v", runErr)
		}
	}

	want := strings.Repeat("Hello, Go CPU!\n", 10000)
	if out.Len() != len(want) {
		t.Fatalf("output length = %d, want %d", out.Len(), len(want))
	}
	if out.String() != want {
		t.Fatal("output mismatch (same length, different content)")
	}
}

func TestInlineEcall_TinyLinuxWrite(t *testing.T) {

	const (
		codeVA = uint64(0x1000)
		msgVA  = uint64(0x2000)
	)
	msg := []byte("jit linux write\n")
	insns := []uint32{
		ienc(opOPIMM, 0, 10, 0, 1),               // a0 = stdout
		uenc(opLUI, 11, uint32(msgVA)),           // a1 = msgVA
		ienc(opOPIMM, 0, 12, 0, int32(len(msg))), // a2 = len
		ienc(opOPIMM, 0, 17, 0, 64),              // a7 = SYS_write
		instrECALL,
		ienc(opOPIMM, 0, 10, 0, 0),  // a0 = exit code
		ienc(opOPIMM, 0, 17, 0, 93), // a7 = exit
		instrECALL,
	}

	cpu, mem := newTestCPU(t, Size64MB, codeVA, insns)
	defer mem.Free()
	for i, b := range msg {
		if fault := mem.Store8(msgVA+uint64(i), b); fault != nil {
			t.Fatalf("Store8 msg[%d]: %v", i, fault)
		}
	}
	var out bytes.Buffer
	cleanup := InstallLinuxOS(cpu, &out)
	defer cleanup()

	j := NewJIT()
	err := j.RunJIT(cpu)
	if err != nil {
		if exit, ok := err.(*ExitError); !ok || exit.Code != 0 {
			t.Fatalf("RunJIT: %v", err)
		}
	}
	if out.String() != string(msg) {
		t.Fatalf("stdout = %q, want %q", out.String(), msg)
	}
}
