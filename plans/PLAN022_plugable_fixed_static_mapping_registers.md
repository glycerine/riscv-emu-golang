# Plan: Pluggable Register Allocator with Fixed Static Mapping

## Context

The native JIT path (non-TCC) currently uses a single register allocation strategy: Extended Linear Scan (ELS) in `ir/regalloc.go`. ELS performs liveness analysis, spill costing, and interval-based register assignment — powerful but with JIT compile-time cost.

The user wants to make register allocation **pluggable** so that different strategies can be benchmarked against each other. The first alternative: **Fixed Static Mapping** — a hardcoded mapping of high-priority RISC-V registers to native x86-64 registers, with all others spilled to memory. This is what QEMU's TCG and other production binary translators use: simple, fast to emit, and it piggybacks on the source compiler's allocation quality.

## Architecture

The existing pipeline in `jit_native.go:44` (`jitCompileWith`):
```
pool = ir.AMD64Pool(block)         // available host registers
pinned = ir.AMD64Pinned()          // R12-R15, RBP, RBX reserved
alloc = j.irAlloc.Allocate(block, pool, pinned, nil)  // ELS
LowerAMD64(ctx, block, alloc)      // consumes *Allocation
```

The lowerer **only** consumes `*Allocation` — it doesn't care how it was produced. This is the natural seam for pluggability.

## Plan

### Step 1: Define `RegAllocator` interface (`ir/regalloc.go`)

```go
// RegAllocator is the interface for register allocation strategies.
type RegAllocator interface {
    Allocate(b *Block, pool RegPool, pinned map[VReg]int16, freq []float64) *Allocation
}
```

The existing `Allocator` (ELS) already satisfies this. No changes to it.

### Step 2: Create `FixedStaticAllocator` (`ir/regalloc_fixed.go`)

New file. The struct holds the priority table: which RISC-V registers get mapped to which native registers.

```go
type FixedStaticAllocator struct{}

func NewFixedStaticAllocator() *FixedStaticAllocator { return &FixedStaticAllocator{} }
```

Its `Allocate` method:
1. Scans the block to find which guest VRegs (0-31 int, 32-63 FP) are actually used
2. Assigns used VRegs from the priority list to pool registers (first-come from the pool)
3. Everything not mapped → `AllocStack` with sequential spill slots
4. For each mapped VReg: creates a single `IntervalAlloc` spanning `[0, len(instrs)-1]`
5. For pinned VRegs: same as ELS — fixed assignment, never spilled
6. No `Moves` needed (fixed mapping doesn't reassign across edges)
7. Returns a valid `*Allocation` that the lowerer consumes unchanged

**Integer priority order** (ABI-driven, highest traffic first):
```
x10(a0), x11(a1), x12(a2), x13(a3), x14(a4), x15(a5),  // args/return
x2(sp), x1(ra), x8(s0/fp), x9(s1),                       // ABI-critical
x5(t0), x6(t1), x7(t2), x28(t3)                           // temporaries
```

Available integer pool: 7 registers (RAX, CX, DX, SI, DI, R8, R9) — or 5 with DIV/MUL. So top 5-7 from the priority list get mapped; rest spill.

**FP priority order**: f0-f15 mapped to XMM0-XMM15 (16 available), f16-f31 spill.

Only VRegs that are actually *used* in the block consume pool registers.

### Step 3: Change `JIT.irAlloc` to use the interface (`jit.go`, `jit_native.go`)

In `jit.go`:
- Change `irAlloc *ir.Allocator` → `irAlloc ir.RegAllocator`
- Add `AllocStrategy string` field (values: `"els"`, `"fixed"`)
- `NewJIT()` defaults to `"els"` and `ir.NewAllocator()`
- Add `NewJITWithAllocator(name string)` or just set strategy before use

In `jit_native.go`:
- `jitCompileWith` already calls `j.irAlloc.Allocate(...)` — no change needed (it's already going through what will become the interface).

### Step 4: Add benchmark support (`bench/jit_bench_test.go`)

Add a benchmark that runs the same guest ELF with both allocators:

```go
func BenchmarkCPU_FullExecution_JIT_Fixed(b *testing.B) {
    // Same as BenchmarkCPU_FullExecution_JIT but with:
    jit := riscv.NewJIT()
    jit.SetAllocStrategy("fixed")
    // ... same benchmark body
}
```

This allows direct `go test -bench='BenchmarkCPU_FullExecution_JIT' ./bench/` comparison.

## Files to modify

| File | Change |
|------|--------|
| `ir/regalloc.go` | Add `RegAllocator` interface (3 lines) |
| `ir/regalloc_fixed.go` | **New file**: `FixedStaticAllocator` (~120 lines) |
| `jit.go` | Change `irAlloc` field type, add `AllocStrategy`, add `SetAllocStrategy()` |
| `jit_native.go` | No changes needed (already calls through the field) |
| `bench/jit_bench_test.go` | Add `BenchmarkCPU_FullExecution_JIT_Fixed` |

## Verification

```bash
# Unit tests — both allocators must pass the full suite
go test -v -run TestRISCVTests ./...

# Lockstep — correctness validation
go test -v -run TestRISCVTests_Lockstep_UI .

# Benchmark comparison
go test -run='^$' -bench='BenchmarkCPU_FullExecution_JIT' -benchtime=3x ./bench/

# Smoke test with fixed allocator
go test -v -run TestJIT_BenchGuest_Smoke ./bench/
```
