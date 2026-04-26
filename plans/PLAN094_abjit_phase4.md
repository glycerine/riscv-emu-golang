# Phase 4: Wire abjit into RunJIT Dispatch Loop

## Context

Phases 1-3 built all the pieces:
- **Phase 1**: Pluggable `RegPolicy` in `ir/`; `jit.go` and `jit_native.go` call `j.regPolicy.Pool/Pinned/Lower` instead of hardcoded RV8 functions. `SetRegPolicy()` clears caches.
- **Phase 2**: Standalone `abjit/` package — trampoline (`callJIT(code, regFileBase)`), CodeBuilder, State struct (with PC/IC/Status/FaultAddr at offsets 536-560).
- **Phase 3**: `LowerAMD64_ABJIT` lowerer — writes results to `[RBP+536..560]`, exit thunk restores callee-saves and returns through the Go trampoline. `PolicyABJIT.Lower = LowerAMD64_ABJIT`.

**What's missing**: The compilation pipeline is policy-aware (blocks compile with whichever lowerer the policy selects), but the **execution** pipeline always calls `sandboxCall()` → C trampoline → rv8 calling convention. Abjit-compiled blocks have a different calling convention (`callJIT(code, regFileBase)`, results in `[RBP+536..560]`) and crash if called through the rv8 trampoline.

Phase 4 adds the abjit execution path to RunJIT so that `j.SetRegPolicy(ir.PolicyABJIT)` produces a fully functional JIT.

---

## Design Decisions

### 1. Copy-in/copy-out via shadow register file page

The GuestMemory shadow page (last 4096 bytes of the mmap, accessed via `cpu.mem.RegFileBase()`) has the same layout as `abjit.State`:

| Offset | Field | Size |
|--------|-------|------|
| 0 | x[0..31] | 256 |
| 256 | f[0..31] | 256 |
| 512 | fcsr | 4 |
| 516 | padding | 4 |
| 520 | memBase | 8 |
| 528 | memMask | 8 |
| 536 | PC (result) | 8 |
| 544 | IC (result) | 8 |
| 552 | Status (result) | 8 |
| 560 | FaultAddr (result) | 8 |

Total used: 568 bytes, well within the 4096-byte page.

**Pattern** (matches `jit_sandbox.c` exactly):
1. Save 568 bytes of guest memory from the shadow page
2. Copy `cpu.x/f/fcsr` → shadow page; write `memBase/memMask`
3. Call `abjit.CallJIT(blk.fn, regFile)`
4. Read result from shadow page offsets 536-560
5. Copy shadow page → `cpu.x/f/fcsr`
6. Restore the saved 568 bytes

This is the same save/execute/restore cycle the C sandbox does (it saves 536 bytes; we save 568 because our result fields are also in the page).

### 2. `useABJIT` boolean flag

String comparison on `regPolicy.Name` in the hot dispatch loop is wasteful. Add `useABJIT bool` to the JIT struct, set in `SetRegPolicy()`:

```go
j.useABJIT = (p.Name == "abjit")
```

RunJIT branches on this boolean once per dispatch cycle.

### 3. Export `CallJIT` from abjit package

Currently `callJIT` is unexported. The root package needs to call it. Add:

```go
func CallJIT(code, regFileBase uintptr) { callJIT(code, regFileBase) }
```

### 4. `abjitDispatch()` helper

Encapsulate the save/setup/call/read/readback/restore sequence in a single function in `jit_abjit.go`. Returns `jitcall.Result` so the status-handling switch in RunJIT is unchanged.

### 5. No decoder_cache for abjit

The abjit JALR handler always returns `jitOKJalrMiss` to Go. No inline cache lookup against decoder_cache. `tryPatchJalrIC` has an existing bounds check (`siteIdx >= len(blk.jalrICs)`); since abjit blocks have empty `jalrICs`, the function returns immediately. Correct but slower than rv8 for JALR-heavy code. Future optimization.

### 6. IRCall → compilation failure → interpreter fallback

The abjit lowerer returns an error for `IRCall` (no C function call support). Blocks containing IRCall fail compilation and are added to `noJIT`, falling through to the interpreter. This is correct for Phase 4.

### 7. AOT works unchanged

`jitCompileAOTSegment()` already uses `j.regPolicy`. When `PolicyABJIT` is active, AOT blocks are compiled with `LowerAMD64_ABJIT`. The AOT decoder_cache is populated but unused by abjit JALR handlers. Harmless.

### 8. Chain patching works unchanged

Chain patching writes 8-byte addresses into MOVABS imm64 slots. The addresses are block `chainEntry` pointers. This mechanism is identical for both rv8 and abjit — the only difference is what code lives at `chainEntry`. `tryPatchChain` doesn't care which lowerer produced the blocks.

---

## Step 1: Export CallJIT from abjit package

**File**: `~/ris/abjit/abjit.go`

After the `Run` function, add:

```go
// CallJIT calls JIT-compiled native code with the given register file base.
// The memory at regFileBase must have the abjit.State layout (568+ bytes).
func CallJIT(code, regFileBase uintptr) {
	callJIT(code, regFileBase)
}
```

### Checkpoint 1

```bash
cd ~/ris && go build ./abjit/
```

---

## Step 2: Create jit_abjit.go — execution helper

**File**: `~/ris/jit_abjit.go` (NEW)

```go
package riscv

import (
	"riscv/abjit"
	"riscv/internal/jitcall"
	"unsafe"
)

const abjitShadowSize = 568

// abjitDispatch executes a JIT-compiled block via the abjit trampoline.
// It copies CPU state to the shadow register file page, executes the
// block, reads the result, copies state back, and restores the guest
// memory that was under the shadow.
func abjitDispatch(fn uintptr, cpu *CPU) jitcall.Result {
	rf := cpu.mem.RegFileBase()

	// Save guest memory under the shadow page.
	var saved [abjitShadowSize]byte
	saved = *(*[abjitShadowSize]byte)(unsafe.Pointer(rf))

	// Copy CPU state → shadow page.
	*(*[32]uint64)(unsafe.Pointer(rf)) = cpu.x
	*(*[32]uint64)(unsafe.Pointer(rf + 256)) = cpu.f
	*(*uint32)(unsafe.Pointer(rf + 512)) = cpu.fcsr
	*(*uintptr)(unsafe.Pointer(rf + 520)) = cpu.mem.Base()
	*(*uint64)(unsafe.Pointer(rf + 528)) = cpu.mem.Mask()

	// Execute.
	abjit.CallJIT(fn, rf)

	// Read result from shadow page.
	res := jitcall.Result{
		PC:        *(*uint64)(unsafe.Pointer(rf + 536)),
		IC:        *(*uint64)(unsafe.Pointer(rf + 544)),
		Status:    *(*uint64)(unsafe.Pointer(rf + 552)),
		FaultAddr: *(*uint64)(unsafe.Pointer(rf + 560)),
	}

	// Copy shadow page → CPU state.
	cpu.x = *(*[32]uint64)(unsafe.Pointer(rf))
	cpu.f = *(*[32]uint64)(unsafe.Pointer(rf + 256))
	cpu.fcsr = *(*uint32)(unsafe.Pointer(rf + 512))

	// Restore guest memory under shadow.
	*(*[abjitShadowSize]byte)(unsafe.Pointer(rf)) = saved

	return res
}
```

### Checkpoint 2

```bash
cd ~/ris && go build .
```

---

## Step 3: Add useABJIT flag to JIT struct

**File**: `~/ris/jit.go`

### 3a. Add field (after `regPolicy`)

```go
	irAlloc   ir.RegAllocator
	regPolicy ir.RegPolicy
	useABJIT  bool
```

### 3b. Update SetRegPolicy

```go
func (j *JIT) SetRegPolicy(p ir.RegPolicy) {
	j.regPolicy = p
	j.useABJIT = p.Name == "abjit"
	j.cache = [blockCacheSize]blockCacheEntry{}
	j.noJIT = make(map[uint64]bool)
}
```

### 3c. Copy in CloneShared

Find the `child := &JIT{` block and add `useABJIT`:

```go
	child := &JIT{
		aotSegments: append([]*DecodedExecuteSegment(nil), j.aotSegments...),
		noJIT:       make(map[uint64]bool),
		irAlloc:     j.irAlloc,
		regPolicy:   j.regPolicy,
		useABJIT:    j.useABJIT,
	}
```

### Checkpoint 3

```bash
cd ~/ris && go build .
```

---

## Step 4: Modify RunJIT to branch on useABJIT

**File**: `~/ris/jit.go`, inside `RunJIT`, lines 650-672.

Replace the block execution section. The current code:

```go
		if blk != nil {
			var res jitcall.Result
			regFile := cpu.mem.RegFileBase()
			stackTop := cpu.mem.StackTop()
			if seg := j.soleSegment; seg != nil {
				res = sandboxCall(blk.fn, cpu, regFile, stackTop,
					seg.decoderCacheBase, seg.decoderCacheMask,
					seg.vaddrBegin, seg.vaddrSize)
			} else if len(j.aotSegments) > 0 {
				seg := blk.segment
				if seg == nil {
					seg = j.hotSegment
					if seg == nil {
						seg = j.aotSegments[0]
					}
				}
				res = sandboxCall(blk.fn, cpu, regFile, stackTop,
					seg.decoderCacheBase, seg.decoderCacheMask,
					seg.vaddrBegin, seg.vaddrSize)
			} else {
				res = sandboxCall(blk.fn, cpu, regFile, stackTop,
					0, 0, 0, 0)
			}
```

Becomes:

```go
		if blk != nil {
			var res jitcall.Result
			if j.useABJIT {
				res = abjitDispatch(blk.fn, cpu)
			} else {
				regFile := cpu.mem.RegFileBase()
				stackTop := cpu.mem.StackTop()
				if seg := j.soleSegment; seg != nil {
					res = sandboxCall(blk.fn, cpu, regFile, stackTop,
						seg.decoderCacheBase, seg.decoderCacheMask,
						seg.vaddrBegin, seg.vaddrSize)
				} else if len(j.aotSegments) > 0 {
					seg := blk.segment
					if seg == nil {
						seg = j.hotSegment
						if seg == nil {
							seg = j.aotSegments[0]
						}
					}
					res = sandboxCall(blk.fn, cpu, regFile, stackTop,
						seg.decoderCacheBase, seg.decoderCacheMask,
						seg.vaddrBegin, seg.vaddrSize)
				} else {
					res = sandboxCall(blk.fn, cpu, regFile, stackTop,
						0, 0, 0, 0)
				}
			}
```

Everything after (`cpu.pc = res.PC; cpu.cycle += res.IC; switch ...`) is unchanged. The status handling, chain patching, JALR IC patching, and error delivery all work identically.

### Checkpoint 4

```bash
cd ~/ris && go build .
```

---

## Step 5: Integration tests

**File**: `~/ris/jit_abjit_test.go` (NEW)

### 5a. runABJITWithOS helper

```go
package riscv

import (
	"os"
	"path/filepath"
	"riscv/ir"
	"strings"
	"testing"
)

func runABJITWithOS(cpu *CPU) (exitCode int, err error) {
	o := NewOS()
	o.HandleSyscall(93, LinuxExit)
	o.HandleSyscall(94, LinuxExit)
	o.HandleEcall(RiscvTestsEcall)
	cpu.Notes.Push(o.Handle)
	defer cpu.Notes.Pop()

	defer func() {
		if r := recover(); r != nil {
			if ex, ok := r.(*ExitError); ok {
				exitCode = ex.Code
				err = nil
				return
			}
			panic(r)
		}
	}()

	jit := NewJIT()
	jit.SetRegPolicy(ir.PolicyABJIT)
	err = jit.RunJIT(cpu)
	return
}
```

### 5b. runABJITRISCVTest helper

```go
func runABJITRISCVTest(t *testing.T, elfPath string) {
	t.Helper()
	data, err := os.ReadFile(elfPath)
	if err != nil {
		t.Skipf("ELF not found: %s", elfPath)
		return
	}
	mem, merr := NewGuestMemory(Size4GB)
	if merr != nil {
		t.Fatal(merr)
	}
	defer mem.Free()

	entry, lerr := LoadELFBytes(mem, data)
	if lerr != nil {
		t.Fatalf("LoadELF: %v", lerr)
	}

	cpu := NewCPU(*mem)
	cpu.SetPC(entry)
	if addr, ok := FindSymbolAddr(data, "tohost"); ok {
		cpu.SetWatchAddr(addr)
	}

	exitCode, err := runABJITWithOS(cpu)
	if err != nil {
		t.Fatalf("RunJIT(abjit): %v", err)
	}
	if exitCode != 0 {
		testNum := exitCode >> 1
		t.Errorf("FAILED: test number %d (exit code %d)", testNum, exitCode)
	}
}
```

### 5c. Test: RISC-V integer test suite via abjit

```go
func TestABJIT_RISCVTests_UI(t *testing.T) {
	entries, err := filepath.Glob(filepath.Join(rvTestsDir, "rv64ui-p-*"))
	if err != nil || len(entries) == 0 {
		t.Skip("rv64ui ELFs not found — run: make riscv-tests")
	}
	for _, path := range entries {
		name := strings.TrimPrefix(filepath.Base(path), "rv64ui-p-")
		t.Run(name, func(t *testing.T) {
			runABJITRISCVTest(t, path)
		})
	}
}
```

### 5d. Test: RISC-V multiply/divide suite via abjit

```go
func TestABJIT_RISCVTests_UM(t *testing.T) {
	entries, err := filepath.Glob(filepath.Join(rvTestsDir, "rv64um-p-*"))
	if err != nil || len(entries) == 0 {
		t.Skip("rv64um ELFs not found")
	}
	for _, path := range entries {
		name := strings.TrimPrefix(filepath.Base(path), "rv64um-p-")
		t.Run(name, func(t *testing.T) {
			runABJITRISCVTest(t, path)
		})
	}
}
```

### 5e. Test: RISC-V atomic suite via abjit

```go
func TestABJIT_RISCVTests_UA(t *testing.T) {
	entries, err := filepath.Glob(filepath.Join(rvTestsDir, "rv64ua-p-*"))
	if err != nil || len(entries) == 0 {
		t.Skip("rv64ua ELFs not found")
	}
	for _, path := range entries {
		name := strings.TrimPrefix(filepath.Base(path), "rv64ua-p-")
		t.Run(name, func(t *testing.T) {
			runABJITRISCVTest(t, path)
		})
	}
}
```

### 5f. Test: RISC-V float test suites via abjit

```go
func TestABJIT_RISCVTests_UF(t *testing.T) {
	entries, err := filepath.Glob(filepath.Join(rvTestsDir, "rv64uf-p-*"))
	if err != nil || len(entries) == 0 {
		t.Skip("rv64uf ELFs not found")
	}
	for _, path := range entries {
		name := strings.TrimPrefix(filepath.Base(path), "rv64uf-p-")
		t.Run(name, func(t *testing.T) {
			runABJITRISCVTest(t, path)
		})
	}
}

func TestABJIT_RISCVTests_UD(t *testing.T) {
	entries, err := filepath.Glob(filepath.Join(rvTestsDir, "rv64ud-p-*"))
	if err != nil || len(entries) == 0 {
		t.Skip("rv64ud ELFs not found")
	}
	for _, path := range entries {
		name := strings.TrimPrefix(filepath.Base(path), "rv64ud-p-")
		t.Run(name, func(t *testing.T) {
			runABJITRISCVTest(t, path)
		})
	}
}
```

### 5g. Test: compressed instructions via abjit

```go
func TestABJIT_RISCVTests_UC(t *testing.T) {
	entries, err := filepath.Glob(filepath.Join(rvTestsDir, "rv64uc-p-*"))
	if err != nil || len(entries) == 0 {
		t.Skip("rv64uc ELFs not found")
	}
	for _, path := range entries {
		name := strings.TrimPrefix(filepath.Base(path), "rv64uc-p-")
		t.Run(name, func(t *testing.T) {
			runABJITRISCVTest(t, path)
		})
	}
}
```

### 5h. Test: Single-block ADD via abjit (minimal smoke test)

```go
func TestABJIT_SingleBlock_ADD(t *testing.T) {
	// ADDI x1, x0, 7     → x1 = 7
	// ADDI x2, x0, 35    → x2 = 35
	// ADD  x3, x1, x2    → x3 = 42
	// ECALL               → exit
	insns := []uint32{
		0x00700093, // addi x1, x0, 7
		0x02300113, // addi x2, x0, 35
		0x002081b3, // add x3, x1, x2
		0x00000073, // ecall
	}
	mem, merr := NewGuestMemory(Size1MB)
	if merr != nil {
		t.Fatal(merr)
	}
	defer mem.Free()

	codeVA := uint64(0x1000)
	storeInsns(mem, codeVA, insns)

	cpu := NewCPU(*mem)
	cpu.SetPC(codeVA)
	cpu.Notes.Push(ecallStop)
	defer cpu.Notes.Pop()

	jit := NewJIT()
	jit.SetRegPolicy(ir.PolicyABJIT)
	_ = jit.RunJIT(cpu)

	if cpu.x[3] != 42 {
		t.Errorf("x[3] = %d, want 42", cpu.x[3])
	}
}
```

### Checkpoint 5

```bash
cd ~/ris && go test -v -run TestABJIT_SingleBlock .
cd ~/ris && go test -v -run TestABJIT_RISCVTests_UI .
```

---

## Step 6: Benchmarks

**File**: `~/ris/jit_abjit_test.go` (append)

```go
func BenchmarkABJIT_RISCVTest_add(b *testing.B) {
	data, err := os.ReadFile(filepath.Join(rvTestsDir, "rv64ui-p-add"))
	if err != nil {
		b.Skip("rv64ui-p-add not found")
	}
	for i := 0; i < b.N; i++ {
		mem, _ := NewGuestMemory(Size4GB)
		entry, _ := LoadELFBytes(mem, data)
		cpu := NewCPU(*mem)
		cpu.SetPC(entry)
		if addr, ok := FindSymbolAddr(data, "tohost"); ok {
			cpu.SetWatchAddr(addr)
		}
		_, _ = runABJITWithOS(cpu)
		mem.Free()
	}
}
```

---

## Files summary

| File | Action | Lines | Description |
|------|--------|-------|-------------|
| `~/ris/abjit/abjit.go` | Modify | +4 | Add exported `CallJIT` wrapper |
| `~/ris/jit.go` | Modify | +8 | `useABJIT` field, `SetRegPolicy` update, `CloneShared` copy |
| `~/ris/jit.go` | Modify | +5 | RunJIT dispatch branch for abjit |
| `~/ris/jit_abjit.go` | Create | ~45 | `abjitDispatch()` helper |
| `~/ris/jit_abjit_test.go` | Create | ~170 | Integration tests + benchmark |

## Files NOT changed

- `abjit/` package (except the 4-line CallJIT export)
- `ir/` package (no changes)
- `internal/jitcall/` (unchanged)
- `jit_native.go` (already uses regPolicy from Phase 1)
- `jit_aot.go` (already uses regPolicy from Phase 1)
- `jit_sandbox_cgo.go` / `jit_sandbox.c` (rv8 path untouched)
- All existing test files (no modifications)

## Verification

### 7a. Build everything

```bash
cd ~/ris && go build ./...
```

### 7b. abjit package tests (Phase 0-2 still pass)

```bash
cd ~/ris && go test -v ./abjit/
```

### 7c. ir/ package tests (Phase 1+3 still pass)

```bash
cd ~/ris && go test -v ./ir/
```

### 7d. Root package — default policy regression (must pass unchanged)

```bash
cd ~/ris && go test -count=1 -timeout 300s .
```

### 7e. Smoke test — single block through abjit

```bash
cd ~/ris && go test -v -run TestABJIT_SingleBlock .
```

### 7f. Full RISC-V test suite through abjit

```bash
cd ~/ris && go test -v -run TestABJIT_RISCVTests .
```

### 7g. Benchmark comparison

```bash
cd ~/ris && go test -run='^$' -bench='BenchmarkABJIT' -benchtime=3s .
```

## What this does NOT do

- Does not make abjit the default policy (PolicyRV8 remains default)
- Does not add decoder_cache to abjit JALR (always returns miss)
- Does not add IRCall/gocall support to abjit lowerer (blocks with IRCall fall back to interpreter)
- Does not eliminate copy-in/copy-out overhead (future: move CPU state into shadow page permanently)
- Does not add abjit-specific AOT optimizations
- Does not modify the `Machine` type or `RunWithOS` (callers use `JIT.SetRegPolicy()` directly)
