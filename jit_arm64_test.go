//go:build arm64

package riscv

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/glycerine/riscv-emu-golang/internal/syscalls"
)

func TestARM64_RV8IC_MatchesInterpreter(t *testing.T) {
	elfPath := filepath.Join(rvTestsDir, "rv64ui-p-add")
	data, err := os.ReadFile(elfPath)
	if err != nil {
		t.Skip("rv64ui-p-add not found")
	}

	interpCPU, interpMem := newARM64ICTestCPU(t, data)
	defer interpMem.Free()
	interpCode, interpErr := RunWithOS(interpCPU)
	if interpErr != nil {
		t.Fatalf("interpreter: %v", interpErr)
	}
	if interpCode != 0 {
		t.Fatalf("interpreter failed: exit %d", interpCode)
	}

	jitCPU, jitMem := newARM64ICTestCPU(t, data)
	defer jitMem.Free()
	o := NewOS()
	o.HandleSyscall(93, LinuxExit)
	o.HandleSyscall(94, LinuxExit)
	o.HandleEcall(RiscvTestsEcall)
	jitCPU.Notes.Push(o.Handle)

	jit := NewJIT()
	jit.SetRegPolicy(PolicyRV8)
	jit.DisableAutoAOT = true
	err = jit.RunJIT(jitCPU)
	if ex, ok := err.(*ExitError); ok {
		if ex.Code != 0 {
			t.Fatalf("RV8 JIT failed: exit %d", ex.Code)
		}
	} else if err != nil {
		t.Fatalf("RV8 JIT: %v", err)
	}

	interpIC := interpCPU.RiscvInstrBegun()
	jitIC := jitCPU.RiscvInstrBegun()
	t.Logf("interpreter IC=%d, ARM64 RV8 JIT IC=%d", interpIC, jitIC)
	if interpIC == 0 || jitIC == 0 {
		t.Fatalf("zero IC: interpreter=%d jit=%d", interpIC, jitIC)
	}
	if interpIC != jitIC {
		t.Fatalf("IC mismatch: interpreter=%d jit=%d", interpIC, jitIC)
	}
}

func TestARM64_RV8HostCall_PreservesLR(t *testing.T) {
	callAddr := syscalls.NullWriteCallbackAddr()
	if callAddr == 0 {
		t.Skip("native ARM64 callback stub unavailable")
	}

	e := NewEmitter(nil)
	e.Call("arm64_null_callback", callAddr)
	e.Ret(0x1004, jitOK, VRegZero)
	MaxVReg(e.Block)

	jit := NewJIT()
	jit.SetRegPolicy(PolicyRV8)
	blk, err := jit.jitCompile(&emitResult{
		block:    e.Block,
		startPC:  0x1000,
		endPC:    0x1004,
		numInsns: 1,
	})
	if err != nil {
		t.Fatalf("jitCompile: %v", err)
	}
	defer syscall.Munmap(blk.nativeMmap)

	var x, f [32]uint64
	var fcsr uint32
	res := jitcallCall(jit, blk.fn, &x, &f, &fcsr, 0, 0)
	if res.Status != uint64(jitOK) || res.PC != 0x1004 {
		t.Fatalf("result = %+v, want status=jitOK pc=0x1004", res)
	}
}

func TestARM64_DirectSyscall_ReloadsA0BeforeChain(t *testing.T) {
	if !DirectSyscallEnabled() {
		t.Skip("direct syscall fast path disabled")
	}
	cb := syscalls.NullWriteCallbackAddr()
	if cb == 0 {
		t.Skip("native ARM64 null write callback unavailable")
	}
	syscalls.RegisterWriteCallback(cb)
	defer syscalls.RegisterWriteCallback(0)

	const codeVA = uint64(0x1000)
	insns := []uint32{
		ienc(opOPIMM, 0, 10, 0, 1),  // a0 = stdout
		ienc(opOPIMM, 0, 11, 0, 64), // a1 = guest buffer VA
		ienc(opOPIMM, 0, 12, 0, 7),  // a2 = count
		ienc(opOPIMM, 0, 17, 0, 64), // a7 = SYS_write
		instrECALL,
		ienc(opOPIMM, 0, 5, 10, 0),  // x5 = a0 after write
		ienc(opOPIMM, 0, 10, 0, 0),  // a0 = exit code
		ienc(opOPIMM, 0, 17, 0, 93), // a7 = exit
		instrECALL,
	}
	cpu, mem := newTestCPU(t, Size1MB, codeVA, insns)
	defer mem.Free()
	cleanup := InstallLinuxOS(cpu, os.Stdout)
	defer cleanup()

	jit := NewJIT()
	jit.DisableAutoAOT = true
	err := jit.RunJIT(cpu)
	if ex, ok := err.(*ExitError); ok {
		if ex.Code != 0 {
			t.Fatalf("exit code = %d, want 0", ex.Code)
		}
	} else if err != nil {
		t.Fatalf("RunJIT: %v", err)
	}
	if got := cpu.Reg(5); got != 7 {
		t.Fatalf("x5 = %d, want 7 from direct write return value", got)
	}
}

func newARM64ICTestCPU(t *testing.T, elfData []byte) (*CPU, *GuestMemory) {
	t.Helper()
	mem, err := NewGuestMemory(Size1MB)
	if err != nil {
		t.Fatal(err)
	}
	elf, err := LoadELFBytes(mem, elfData)
	if err != nil {
		mem.Free()
		t.Fatal(err)
	}
	cpu := NewCPU(*mem)
	cpu.SetPC(elf.Entry)
	cpu.SetWatchAddr(elf.TohostAddr)
	return cpu, mem
}
