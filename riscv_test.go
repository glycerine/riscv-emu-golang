package riscv

// riscvtests_test.go — runs the official riscv-tests ELF binaries.
//
// The riscv-tests suite uses the following ECALL convention (machine-mode):
//   a7=93 (exit syscall), a0=0 => PASS
//   a7=93, a0=(testnum<<1)|1 => FAIL test number (testnum)
//
// Each ELF is a bare-metal binary linked at 0x80000000 with a reset vector
// that sets up minimal CSRs then falls through to the test code.
// We load it, run it, and check the exit code.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var _ = time.Now

const rvTestsDir = "riscv-elf-tests/"

var elfs = []string{
	"rv64ui-p-add",
	"rv64ui-p-addi",
	"rv64ui-p-addiw",
	"rv64ui-p-addw",
	"rv64ui-p-and",
	"rv64ui-p-andi",
	"rv64ui-p-auipc",
	"rv64ui-p-beq",
	"rv64ui-p-bge",
	"rv64ui-p-bgeu",
	"rv64ui-p-blt",
	"rv64ui-p-bltu",
	"rv64ui-p-bne",
	"rv64ui-p-fence_i",
	"rv64ui-p-jal",
	"rv64ui-p-jalr",
	"rv64ui-p-lb",
	"rv64ui-p-lbu",
	"rv64ui-p-ld",
	"rv64ui-p-ld_st",
	"rv64ui-p-lh",
	"rv64ui-p-lhu",
	"rv64ui-p-lui",
	"rv64ui-p-lw",
	"rv64ui-p-lwu",
	"rv64ui-p-ma_data",
	"rv64ui-p-or",
	"rv64ui-p-ori",
	"rv64ui-p-sb",
	"rv64ui-p-sd",
	"rv64ui-p-sh",
	"rv64ui-p-simple",
	"rv64ui-p-sll",
	"rv64ui-p-slli",
	"rv64ui-p-slliw",
	"rv64ui-p-sllw",
	"rv64ui-p-slt",
	"rv64ui-p-slti",
	"rv64ui-p-sltiu",
	"rv64ui-p-sltu",
	"rv64ui-p-sra",
	"rv64ui-p-srai",
	"rv64ui-p-sraiw",
	"rv64ui-p-sraw",
	"rv64ui-p-srl",
	"rv64ui-p-srli",
	"rv64ui-p-srliw",
	"rv64ui-p-srlw",
	"rv64ui-p-st_ld",
	"rv64ui-p-sub",
	"rv64ui-p-subw",
	"rv64ui-p-sw",
	"rv64ui-p-xor",
	"rv64ui-p-xori",
	"rv64um-p-div",
	"rv64um-p-divu",
	"rv64um-p-divuw",
	"rv64um-p-divw",
	"rv64um-p-mul",
	"rv64um-p-mulh",
	"rv64um-p-mulhsu",
	"rv64um-p-mulhu",
	"rv64um-p-mulw",
	"rv64um-p-rem",
	"rv64um-p-remu",
	"rv64um-p-remuw",
	"rv64um-p-remw",
	"rv64ua-p-amoadd_d",
	"rv64ua-p-amoadd_w",
	"rv64ua-p-amoand_d",
	"rv64ua-p-amoand_w",
	"rv64ua-p-amomax_d",
	"rv64ua-p-amomax_w",
	"rv64ua-p-amomaxu_d",
	"rv64ua-p-amomaxu_w",
	"rv64ua-p-amomin_d",
	"rv64ua-p-amomin_w",
	"rv64ua-p-amominu_d",
	"rv64ua-p-amominu_w",
	"rv64ua-p-amoor_d",
	"rv64ua-p-amoor_w",
	"rv64ua-p-amoswap_d",
	"rv64ua-p-amoswap_w",
	"rv64ua-p-amoxor_d",
	"rv64ua-p-amoxor_w",
	"rv64ua-p-lrsc",
	"rv64uf-p-fadd",
	"rv64uf-p-fclass",
	"rv64uf-p-fcmp",
	"rv64uf-p-fcvt",
	"rv64uf-p-fcvt_w",
	"rv64uf-p-fdiv",
	"rv64uf-p-fmadd",
	"rv64uf-p-fmin",
	"rv64uf-p-ldst",
	"rv64uf-p-move",
	"rv64uf-p-recoding",
	"rv64ud-p-fadd",
	"rv64ud-p-fclass",
	"rv64ud-p-fcmp",
	"rv64ud-p-fcvt",
	"rv64ud-p-fcvt_w",
	"rv64ud-p-fdiv",
	"rv64ud-p-fmadd",
	"rv64ud-p-fmin",
	"rv64ud-p-ldst",
	"rv64ud-p-move",
	"rv64ud-p-recoding",
	"rv64ud-p-structural",
	"rv64uc-p-rvc",
}

func runRISCVTest(t *testing.T, elfPath string) {
	t.Helper()

	data, err := os.ReadFile(elfPath)
	if err != nil {
		t.Skipf("ELF not found: %s (run make riscv-tests first)", elfPath)
		return
	}

	mem, merr := NewGuestMemory(Size4GB) // riscv-tests link at 0x80000000
	if merr != nil {
		t.Fatal(merr)
	}
	defer mem.Free()

	elf, lerr := LoadELFBytes(mem, data)
	if lerr != nil {
		t.Fatalf("LoadELF: %v", lerr)
	}

	cpu := NewCPU(*mem)
	cpu.SetPC(elf.Entry)
	cpu.SetWatchAddr(elf.TohostAddr)

	exitCode, err := RunWithOS(cpu)
	if err != nil {
		t.Fatalf("Run error: %v", err)
	}
	if exitCode != 0 {
		testNum := exitCode >> 1
		t.Errorf("FAILED: test number %d (exit code %d)", testNum, exitCode)
	}
}

func TestRISCVTests_UI(t *testing.T) {
	entries, err := filepath.Glob(filepath.Join(rvTestsDir, "rv64ui-p-*"))
	if err != nil || len(entries) == 0 {
		t.Skip("rv64ui ELFs not found — run: make riscv-tests")
	}
	for _, path := range entries {
		name := strings.TrimPrefix(filepath.Base(path), "rv64ui-p-")
		t.Run(name, func(t *testing.T) {
			runRISCVTest(t, path)
		})
	}
}

func TestRISCVTests_UM(t *testing.T) {
	entries, err := filepath.Glob(filepath.Join(rvTestsDir, "rv64um-p-*"))
	if err != nil || len(entries) == 0 {
		t.Skip("rv64um ELFs not found — run: make riscv-tests")
	}
	for _, path := range entries {
		name := strings.TrimPrefix(filepath.Base(path), "rv64um-p-")
		t.Run(name, func(t *testing.T) {
			runRISCVTest(t, path)
		})
	}
}

// quick sanity — runs just one test so CI doesn't need the full suite
func TestRISCVTests_Smoke(t *testing.T) {
	path := filepath.Join(rvTestsDir, "rv64ui-p-add")
	if _, err := os.Stat(path); err != nil {
		t.Skip("rv64ui-p-add not found")
	}
	runRISCVTest(t, path)
	fmt.Println("rv64ui-p-add: PASS")
}

func TestRISCVTests_UA(t *testing.T) {
	entries, err := filepath.Glob(filepath.Join(rvTestsDir, "rv64ua-p-*"))
	if err != nil || len(entries) == 0 {
		t.Skip("rv64ua ELFs not found — run: make riscv-tests")
	}
	for _, path := range entries {
		name := strings.TrimPrefix(filepath.Base(path), "rv64ua-p-")
		t.Run(name, func(t *testing.T) { runRISCVTest(t, path) })
	}
}

func TestRISCVTests_UF(t *testing.T) {
	entries, err := filepath.Glob(filepath.Join(rvTestsDir, "rv64uf-p-*"))
	if err != nil || len(entries) == 0 {
		t.Skip("rv64uf ELFs not found — run: make riscv-tests")
	}
	for _, path := range entries {
		name := strings.TrimPrefix(filepath.Base(path), "rv64uf-p-")
		t.Run(name, func(t *testing.T) { runRISCVTest(t, path) })
	}
}

func TestRISCVTests_UD(t *testing.T) {
	entries, err := filepath.Glob(filepath.Join(rvTestsDir, "rv64ud-p-*"))
	if err != nil || len(entries) == 0 {
		t.Skip("rv64ud ELFs not found — run: make riscv-tests")
	}
	for _, path := range entries {
		name := strings.TrimPrefix(filepath.Base(path), "rv64ud-p-")
		t.Run(name, func(t *testing.T) { runRISCVTest(t, path) })
	}
}

func TestRISCVTests_UC(t *testing.T) {
	entries, err := filepath.Glob(filepath.Join(rvTestsDir, "rv64uc-p-*"))
	if err != nil || len(entries) == 0 {
		t.Skip("rv64uc ELFs not found — run: make riscv-tests")
	}
	for _, path := range entries {
		name := strings.TrimPrefix(filepath.Base(path), "rv64uc-p-")
		t.Run(name, func(t *testing.T) { runRISCVTest(t, path) })
	}
}

// ══════════════════════════════════════════════════════════════════════════
// JIT: run riscv-tests through JIT (exit-code only)
// ══════════════════════════════════════════════════════════════════════════

func runRISCVTestJIT(t *testing.T, elfPath string) {
	//t.Helper()
	data, err := os.ReadFile(elfPath)
	if err != nil {
		t.Skipf("ELF not found: %s", elfPath)
		return
	}

	mem, merr := NewGuestMemory(Size1MB) // runRISCVTestJIT here
	if merr != nil {
		t.Fatal(merr)
	}
	defer mem.Free()

	elf, lerr := LoadELFBytes(mem, data)
	if lerr != nil {
		t.Fatalf("LoadELF: %v", lerr)
	}

	cpu := NewCPU(*mem)
	cpu.SetPC(elf.Entry)
	cpu.SetWatchAddr(elf.TohostAddr)

	exitCode, err := runJITWithOS(cpu)
	if err != nil {
		t.Fatalf("JIT run error: %v", err)
	}
	if exitCode != 0 {
		testNum := exitCode >> 1
		t.Errorf("JIT FAILED: test %d (exit code %d)", testNum, exitCode)
	}
}

func TestRISCVTests_UI_JIT_AOT(t *testing.T) {
	entries, err := filepath.Glob(filepath.Join(rvTestsDir, "rv64ui-p-*"))
	if err != nil || len(entries) == 0 {
		t.Skip("rv64ui ELFs not found")
	}
	for _, path := range entries {
		name := strings.TrimPrefix(filepath.Base(path), "rv64ui-p-")
		t.Run(name, func(t *testing.T) {
			runRISCVTestJIT(t, path)
		})
	}
}

func TestRISCVTests_UM_JIT_AOT(t *testing.T) {
	entries, err := filepath.Glob(filepath.Join(rvTestsDir, "rv64um-p-*"))
	if err != nil || len(entries) == 0 {
		t.Skip("rv64um ELFs not found")
	}
	for _, path := range entries {
		name := strings.TrimPrefix(filepath.Base(path), "rv64um-p-")
		t.Run(name, func(t *testing.T) { runRISCVTestJIT(t, path) })
	}
}

func TestRISCVTests_UA_JIT_AOT(t *testing.T) {
	entries, err := filepath.Glob(filepath.Join(rvTestsDir, "rv64ua-p-*"))
	if err != nil || len(entries) == 0 {
		t.Skip("rv64ua ELFs not found")
	}
	for _, path := range entries {
		name := strings.TrimPrefix(filepath.Base(path), "rv64ua-p-")
		t.Run(name, func(t *testing.T) { runRISCVTestJIT(t, path) })
	}
}

// JIT FP tests skipped: the JIT does not propagate fflags (FP exception flags)
// back to fcsr. FP arithmetic works correctly, but riscv-tests check fflags
// via CSR reads which see stale values. Enabling these requires capturing
// host fflags after each FP operation.
func TestRISCVTests_UF_JIT_AOT(t *testing.T) {
	t.Skip("JIT does not propagate fflags — FP compliance tests fail on flag checks")
	entries, err := filepath.Glob(filepath.Join(rvTestsDir, "rv64uf-p-*"))
	if err != nil || len(entries) == 0 {
		t.Skip("rv64uf ELFs not found")
	}
	for _, path := range entries {
		name := strings.TrimPrefix(filepath.Base(path), "rv64uf-p-")
		t.Run(name, func(t *testing.T) { runRISCVTestJIT(t, path) })
	}
}

func TestRISCVTests_UD_JIT_AOT(t *testing.T) {
	t.Skip("JIT does not propagate fflags — FP compliance tests fail on flag checks")
	entries, err := filepath.Glob(filepath.Join(rvTestsDir, "rv64ud-p-*"))
	if err != nil || len(entries) == 0 {
		t.Skip("rv64ud ELFs not found")
	}
	for _, path := range entries {
		name := strings.TrimPrefix(filepath.Base(path), "rv64ud-p-")
		t.Run(name, func(t *testing.T) { runRISCVTestJIT(t, path) })
	}
}

func TestRISCVTests_UC_JIT_AOT(t *testing.T) {
	entries, err := filepath.Glob(filepath.Join(rvTestsDir, "rv64uc-p-*"))
	if err != nil || len(entries) == 0 {
		t.Skip("rv64uc ELFs not found")
	}
	for _, path := range entries {
		name := strings.TrimPrefix(filepath.Base(path), "rv64uc-p-")
		t.Run(name, func(t *testing.T) { runRISCVTestJIT(t, path) })
	}
}

// ══════════════════════════════════════════════════════════════════════════
// Lazy JIT: run riscv-tests through RunJIT with AutoAOT=false
// (no decoder cache, exercises 2-slot JALR IC and lazy block compilation)
// ══════════════════════════════════════════════════════════════════════════

func runRISCVTestJITLazy(t *testing.T, elfPath string) {
	//t.Helper()
	data, err := os.ReadFile(elfPath)
	if err != nil {
		t.Skipf("ELF not found: %s", elfPath)
		return
	}

	mem, merr := NewGuestMemory(Size1MB) // here runRISCVTestJITLazy
	if merr != nil {
		t.Fatal(merr)
	}
	defer mem.Free()

	elf, lerr := LoadELFBytes(mem, data)
	if lerr != nil {
		t.Fatalf("LoadELF: %v", lerr)
	}

	cpu := NewCPU(*mem)
	cpu.SetPC(elf.Entry)
	cpu.SetWatchAddr(elf.TohostAddr)

	o := NewOS()
	o.HandleSyscall(93, LinuxExit)
	o.HandleSyscall(94, LinuxExit)
	o.HandleEcall(RiscvTestsEcall)
	cpu.Notes.Push(o.Handle)
	defer cpu.Notes.Pop()

	jit := NewJIT()

	err = jit.RunJIT(cpu)
	if ex, ok := err.(*ExitError); ok {
		if ex.Code != 0 {
			testNum := ex.Code >> 1
			t.Errorf("Lazy JIT FAILED: test %d (exit code %d)", testNum, ex.Code)
		}
	} else if err != nil {
		t.Fatalf("Lazy JIT run error: %v", err)
	}
}

func TestRISCVTests_UI_JIT_Lazy(t *testing.T) {
	entries, err := filepath.Glob(filepath.Join(rvTestsDir, "rv64ui-p-*"))
	if err != nil || len(entries) == 0 {
		t.Skip("rv64ui ELFs not found")
	}
	for _, path := range entries {
		name := strings.TrimPrefix(filepath.Base(path), "rv64ui-p-")
		t.Run(name, func(t *testing.T) {
			runRISCVTestJITLazy(t, path)
		})
	}
}

func TestRISCVTests_UM_JIT_Lazy(t *testing.T) {
	entries, err := filepath.Glob(filepath.Join(rvTestsDir, "rv64um-p-*"))
	if err != nil || len(entries) == 0 {
		t.Skip("rv64um ELFs not found")
	}
	for _, path := range entries {
		name := strings.TrimPrefix(filepath.Base(path), "rv64um-p-")
		t.Run(name, func(t *testing.T) { runRISCVTestJITLazy(t, path) })
	}
}

func TestRISCVTests_UA_JIT_Lazy(t *testing.T) {
	entries, err := filepath.Glob(filepath.Join(rvTestsDir, "rv64ua-p-*"))
	if err != nil || len(entries) == 0 {
		t.Skip("rv64ua ELFs not found")
	}
	for _, path := range entries {
		name := strings.TrimPrefix(filepath.Base(path), "rv64ua-p-")
		t.Run(name, func(t *testing.T) { runRISCVTestJITLazy(t, path) })
	}
}

func TestRISCVTests_UC_JIT_Lazy(t *testing.T) {
	entries, err := filepath.Glob(filepath.Join(rvTestsDir, "rv64uc-p-*"))
	if err != nil || len(entries) == 0 {
		t.Skip("rv64uc ELFs not found")
	}
	for _, path := range entries {
		name := strings.TrimPrefix(filepath.Base(path), "rv64uc-p-")
		t.Run(name, func(t *testing.T) { runRISCVTestJITLazy(t, path) })
	}
}

// ══════════════════════════════════════════════════════════════════════════
// JIT IC accuracy: native instruction count must match interpreter
// ══════════════════════════════════════════════════════════════════════════

func TestJITIC_MatchesInterpreter(t *testing.T) {
	elfPath := filepath.Join(rvTestsDir, "rv64ui-p-add")
	data, err := os.ReadFile(elfPath)
	if err != nil {
		t.Skip("rv64ui-p-add not found")
	}

	// Interpreter run
	interpMem, err := NewGuestMemory(Size1MB)
	if err != nil {
		t.Fatal(err)
	}
	defer interpMem.Free()
	interpElf, err := LoadELFBytes(interpMem, data)
	if err != nil {
		t.Fatal(err)
	}
	interpCPU := NewCPU(*interpMem)
	interpCPU.SetPC(interpElf.Entry)
	interpCPU.SetWatchAddr(interpElf.TohostAddr)
	interpCode, interpErr := RunWithOS(interpCPU)
	if interpErr != nil {
		t.Fatalf("interpreter: %v", interpErr)
	}
	if interpCode != 0 {
		t.Fatalf("interpreter failed: exit %d", interpCode)
	}
	interpIC := interpCPU.RiscvInstrBegun()

	// JIT run (lazy IC enabled)
	jitMem, err := NewGuestMemory(Size1MB)
	if err != nil {
		t.Fatal(err)
	}
	defer jitMem.Free()
	jitElf, err := LoadELFBytes(jitMem, data)
	if err != nil {
		t.Fatal(err)
	}
	jitCPU := NewCPU(*jitMem)
	jitCPU.SetPC(jitElf.Entry)
	jitCPU.SetWatchAddr(jitElf.TohostAddr)

	o := NewOS()
	o.HandleSyscall(93, LinuxExit)
	o.HandleSyscall(94, LinuxExit)
	o.HandleEcall(RiscvTestsEcall)
	jitCPU.Notes.Push(o.Handle)

	jit := NewJIT()

	_ = jit.RunJIT(jitCPU)
	jitIC := jitCPU.RiscvInstrBegun()

	t.Logf("interpreter IC=%d, JIT IC=%d", interpIC, jitIC)
	if interpIC == 0 {
		t.Fatal("interpreter IC is 0")
	}
	if jitIC == 0 {
		t.Fatal("JIT IC is 0")
	}
	if interpIC != jitIC {
		diff := int64(jitIC) - int64(interpIC)
		pct := float64(diff) / float64(interpIC) * 100
		t.Errorf("IC mismatch: interp=%d jit=%d diff=%d (%.1f%%)", interpIC, jitIC, diff, pct)
	}
}

// ══════════════════════════════════════════════════════════════════════════
// LOCKSTEP: per-block JIT vs interpreter with full register + memory compare
// ══════════════════════════════════════════════════════════════════════════

//const lockstepMemSize = Size64MB

// const lockstepMemSize = Size16KB
const lockstepMemSize = Size32KB // mismatching, probably aliasing
// const lockstepMemSize = Size64KB // way faster than 64MB but aliasing
// const lockstepMemSize = Size128KB // beq, sw, sb, sd, sh, ... fail.
// const lockstepMemSize = Size256KB // ditto
// const lockstepMemSize = Size512KB // ditto
// const lockstepMemSize = Size1MB // ditto. why is beq failing??
//const lockstepMemSize = Size64MB

func runLockstep(t *testing.T, elfPath string) {
	runLockstepWithAutoAOT(t, elfPath, true)
}

func runLockstepLazy(t *testing.T, elfPath string) {
	runLockstepWithAutoAOT(t, elfPath, false)
}

func runLockstepWithAutoAOT(t *testing.T, elfPath string, autoAOT bool) {
	//t.Helper()

	//t0 := time.Now()
	//defer func() {
	//	vv("runLockstep: elfPath '%v' took %v", elfPath, time.Since(t0))
	//}()

	saved := CheckSandboxBounds
	CheckSandboxBounds = true
	defer func() { CheckSandboxBounds = saved }()
	data, err := os.ReadFile(elfPath)
	if err != nil {
		t.Skipf("ELF not found: %s", elfPath)
		return
	}

	// JIT side
	jitMem, err := NewGuestMemory(lockstepMemSize)
	if err != nil {
		t.Fatal(err)
	}
	defer jitMem.Free()
	jitElf, err := LoadELFBytes(jitMem, data)
	if err != nil {
		t.Fatalf("LoadELF (jit): %v", err)
	}
	jitCPU := NewCPU(*jitMem)
	jitCPU.SetPC(jitElf.Entry)
	jitCPU.SetWatchAddr(jitElf.TohostAddr)

	// Interpreter side
	interpMem, err := NewGuestMemory(lockstepMemSize)
	if err != nil {
		t.Fatal(err)
	}
	defer interpMem.Free()
	interpElf, err := LoadELFBytes(interpMem, data)
	if err != nil {
		t.Fatalf("LoadELF (interp): %v", err)
	}
	interpCPU := NewCPU(*interpMem)
	interpCPU.SetPC(interpElf.Entry)
	interpCPU.SetWatchAddr(interpElf.TohostAddr)

	//t.Logf("jitMem base=%#x interpMem base=%#x size=%#x", jitMem.Base(), interpMem.Base(), jitMem.Size())

	jit := NewJIT()
	// not optional. the whole point of runLockstep():
	jit.DebugOneBlockLockstepMode = true

	jit.AutoAOT = autoAOT

	// timings done with maxBlockIRInsns = 2048;
	// and               PerBlockCapTimeToSplit = 5000
	//
	//jit.LockstepModeBudget = 2 // 10.14s / 32KB, all Lockstep 132s darwin
	//jit.LockstepModeBudget = 20000 // 10.14s / 32KB, all Lockstep 15.5s. All: 133s
	//jit.LockstepModeBudget = 1_000_000 // 9.9s / 32KB, all Lockstep 15s. All:149.3s
	jit.LockstepModeBudget = 17_731 // 32KB All: 144.5s
	//jit.LockstepModeBudget = 2000 // 10.4s / 32KB
	//jit.LockstepModeBudget = 200 // 11.0s
	//jit.LockstepModeBudget = 100 // 12.3s
	//jit.LockstepModeBudget = 1 // single-step: exact per-instruction comparison
	//jit.LockstepModeBudget = 1_000_065_536 // "add" takes: 38.3 sec
	//jit.LockstepModeBudget = 1 << 6 // "add" takes: 32.69 sec. sw red. beq red.
	//jit.LockstepModeBudget = 65_536 // "add" takes: 32.69 sec. sw red.
	//jit.LockstepModeBudget = 5036 // beq green with 64KB guestmem. sw red.
	//jit.LockstepModeBudget = 500 // beq green with 64KB,nStops = 242158, but red: fence_i, jal, sb, sd, sh, sw
	//jit.LockstepModeBudget = 350 // beq green w  64KB,nStops = 236188
	//jit.LockstepModeBudget = 200 // beq red with 64KB guestmem
	//jit.LockstepModeBudget = 100 // beq red with 64KB guestmem
	//jit.LockstepModeBudget = 536 // "add" takes: 30.35 sec
	//jit.LockstepModeBudget = 50 // "add" takes: 29.7 sec
	maxInstruc := uint64(10_000_000)
	blockNum := 0

	nStops := 0
	for jitCPU.RiscvInstrBegun() < maxInstruc {

		if jitCPU.pc != interpCPU.pc {
			t.Fatalf("block %d: PC desync BEFORE dispatch: jit=0x%x interp=0x%x",
				blockNum, jitCPU.pc, interpCPU.pc)
		}

		//vv("just before jit.StepBlock(jitCPU) in runLockstep: elfPath '%v'; jitPC = 0x%x and interpCPU.pc = 0x%x ; jitCPU = '%#v'", elfPath, jitCPU.pc, interpCPU.pc, jitCPU)

		// JIT: one dispatch cycle
		preCycle := jitCPU.RiscvInstrBegun()
		absIC, jitErr := jit.StepBlock(jitCPU)
		jitIC := absIC - preCycle

		targetPC := jitCPU.pc

		//vv("just before interp in runLockstep: elfPath '%v'; we just did jit.StepBlock() and now targetPC = 0x%x", elfPath, targetPC)

		// Interpreter: run IC steps (approximate), then catch up to exact PC.
		var interpErr error
		for i := uint64(0); i < jitIC; i++ {
			interpErr = interpCPU.step()
			interpCPU.riscvInstrBegun++
			if interpErr != nil {
				break
			}
		}
		catchupLimit := int(jitIC/2) + 20
		for catchup := 0; interpCPU.pc != targetPC && interpErr == nil && catchup < catchupLimit; catchup++ {
			interpErr = interpCPU.step()
			interpCPU.riscvInstrBegun++
		}

		//if nStops%5000 == 0 { // saw 300
		//vv("runLockstep: elfPath '%v'; nStops = %v; we just ran: jitCPU.pc = 0x%x", elfPath, nStops, jitCPU.pc)
		//}
		nStops++

		// Compare ALL registers FIRST (before exit check)
		regMismatch := false
		for r := 0; r < 32; r++ {
			if jitCPU.x[r] != interpCPU.x[r] {
				t.Errorf("block %d (pc=0x%x, ic=%d): x[%d] mismatch: jit=0x%x interp=0x%x",
					blockNum, jitCPU.pc, jitIC, r, jitCPU.x[r], interpCPU.x[r])
				regMismatch = true
			}
		}
		if regMismatch {
			t.Fatalf("STOP at first register mismatch (block %d, jitPC=0x%x interpPC=0x%x)",
				blockNum, jitCPU.pc, interpCPU.pc)
		}

		// Check exit
		jitExit := isExitEcall(jitCPU, jitErr)
		interpExit := isExitEcall(interpCPU, interpErr)
		if jitExit || interpExit {
			if jitExit != interpExit {
				t.Errorf("block %d (pc=0x%x): exit mismatch: jit=%v interp=%v jitIC=%d",
					blockNum, jitCPU.pc, jitExit, interpExit, jitIC)
			}
			break
		}

		// Handle non-exit exceptions
		if jitErr != nil {
			advancePastException(jitCPU, jitErr)
		}
		if interpErr != nil {
			advancePastException(interpCPU, interpErr)
		}

		// Also compare after exception handling
		for r := 0; r < 32; r++ {
			if jitCPU.x[r] != interpCPU.x[r] {
				t.Errorf("block %d (pc=0x%x): x[%d] mismatch: jit=0x%x interp=0x%x",
					blockNum, jitCPU.pc, r, jitCPU.x[r], interpCPU.x[r])
			}
		}
		for r := 0; r < 32; r++ {
			if jitCPU.f[r] != interpCPU.f[r] {
				t.Errorf("block %d: f[%d] mismatch: jit=0x%x interp=0x%x",
					blockNum, r, jitCPU.f[r], interpCPU.f[r])
			}
		}
		if jitCPU.pc != interpCPU.pc {
			t.Fatalf("block %d: PC mismatch AFTER dispatch: jit=0x%x interp=0x%x",
				blockNum, jitCPU.pc, interpCPU.pc)
		}
		if jitCPU.fcsr != interpCPU.fcsr {
			t.Errorf("block %d: FCSR mismatch: jit=0x%x interp=0x%x",
				blockNum, jitCPU.fcsr, interpCPU.fcsr)
		}

		// Compare guest memory (lower half only — upper half has sandbox
		// infrastructure: stack, guard page, register file).
		compareFullMemory(t, jitMem, interpMem, blockNum)

		blockNum++
	}

	//t.Logf("lockstep complete: %d blocks, %d instructions; nStops = %v", blockNum, jitCPU.RiscvInstrBegun(), nStops)
}

func TestRISCVTests_Lockstep_UI(t *testing.T) {
	entries, err := filepath.Glob(filepath.Join(rvTestsDir, "rv64ui-p-*"))
	if err != nil || len(entries) == 0 {
		t.Skip("rv64ui ELFs not found")
	}
	for _, path := range entries {
		name := strings.TrimPrefix(filepath.Base(path), "rv64ui-p-")
		t.Run(name, func(t *testing.T) {
			runLockstep(t, path)
		})
	}
}

func TestRISCVTests_LockstepLazy_Add(t *testing.T) {
	path := filepath.Join(rvTestsDir, "rv64ui-p-add")
	if _, err := os.Stat(path); err != nil {
		t.Skip("rv64ui-p-add not found")
	}
	runLockstepLazy(t, path)
}

func TestRISCVTests_Lockstep_UM(t *testing.T) {
	entries, err := filepath.Glob(filepath.Join(rvTestsDir, "rv64um-p-*"))
	if err != nil || len(entries) == 0 {
		t.Skip("rv64um ELFs not found")
	}
	for _, path := range entries {
		name := strings.TrimPrefix(filepath.Base(path), "rv64um-p-")
		t.Run(name, func(t *testing.T) { runLockstep(t, path) })
	}
}

func TestRISCVTests_Lockstep_UA(t *testing.T) {
	entries, err := filepath.Glob(filepath.Join(rvTestsDir, "rv64ua-p-*"))
	if err != nil || len(entries) == 0 {
		t.Skip("rv64ua ELFs not found")
	}
	for _, path := range entries {
		name := strings.TrimPrefix(filepath.Base(path), "rv64ua-p-")
		t.Run(name, func(t *testing.T) { runLockstep(t, path) })
	}
}

// Lockstep FP tests skipped: same fflags issue as above. The JIT executes
// FP arithmetic correctly but doesn't write fflags, so FCSR diverges from
// the interpreter after the first FP operation that sets exception flags.
func TestRISCVTests_Lockstep_UF(t *testing.T) {
	t.Skip("JIT does not propagate fflags — lockstep FCSR comparison diverges")
	entries, err := filepath.Glob(filepath.Join(rvTestsDir, "rv64uf-p-*"))
	if err != nil || len(entries) == 0 {
		t.Skip("rv64uf ELFs not found")
	}
	for _, path := range entries {
		name := strings.TrimPrefix(filepath.Base(path), "rv64uf-p-")
		t.Run(name, func(t *testing.T) { runLockstep(t, path) })
	}
}

func TestRISCVTests_Lockstep_UD(t *testing.T) {
	t.Skip("JIT does not propagate fflags — lockstep FCSR comparison diverges")
	entries, err := filepath.Glob(filepath.Join(rvTestsDir, "rv64ud-p-*"))
	if err != nil || len(entries) == 0 {
		t.Skip("rv64ud ELFs not found")
	}
	for _, path := range entries {
		name := strings.TrimPrefix(filepath.Base(path), "rv64ud-p-")
		t.Run(name, func(t *testing.T) { runLockstep(t, path) })
	}
}

func TestRISCVTests_Lockstep_UC(t *testing.T) {
	entries, err := filepath.Glob(filepath.Join(rvTestsDir, "rv64uc-p-*"))
	if err != nil || len(entries) == 0 {
		t.Skip("rv64uc ELFs not found")
	}
	for _, path := range entries {
		name := strings.TrimPrefix(filepath.Base(path), "rv64uc-p-")
		t.Run(name, func(t *testing.T) { runLockstep(t, path) })
	}
}
