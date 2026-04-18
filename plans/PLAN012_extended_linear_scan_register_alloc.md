# Phase 3: Extended Linear Scan Register Allocator — Implementation Plan

## Context

Phase 2 (IR definition layer) is complete. The `ir/` package has core types (`VReg`, `IRInstr`, `Block`), an emitter with 44 IR ops, peephole optimizer, and 158 unit tests + 4 fuzz targets. Phase 3 adds a register allocator that maps virtual registers to host registers or stack spill slots. This is the prerequisite for Phase 4 (amd64/arm64 lowering).

We implement the **Extended Linear Scan (ELS)** algorithm from Sarkar & Barik (IBM Research), which retains the O(|IR|+|I|) compile-time of basic linear scan while producing code quality matching or exceeding graph coloring (15-68x faster compilation, up to 5.8% better execution time vs GC on SPECint2000).

The allocator is **arch-agnostic** — the caller (future lowerer) constructs the `RegPool`. The IR will grow beyond single-block to multi-block CFG, so ELS is implemented with the full interval-set model, per-interval register assignment, and register-move insertion at control flow edges — no shortcuts that would need rewriting later.

### Reference

Sarkar, V. & Barik, R. "Extended Linear Scan: an Alternate Foundation for Global Register Allocation." IBM T.J. Watson Research Center / IBM India Research Laboratory. (`~/ris/extended_linear_scan_register_allocation.pdf`)

## Files to Create

1. `ir/regalloc.go` — types, ELS_0 + ELS_1 algorithm, exported `Allocate()` + utilities
2. `ir/regalloc_test.go` — ~55 unit tests + 3 fuzz targets

## TDD Workflow

**Stubs first -> red tests -> green implementations**, in 7 phases (A-G).

---

## Phase A: Types & Stubs (compile, all tests fail)

### Core Data Structures (`ir/regalloc.go`)

These follow the paper's formulation directly.

```go
// ── Interval representation (paper's I(s)) ──

// Interval is one contiguous live range [Start, End] for a symbolic register.
// A symbolic register may have multiple disjoint intervals (the "interval set").
type Interval struct {
    VReg  VReg
    Start int  // instruction index (paper's program point P)
    End   int  // instruction index (paper's program point Q)
}

// ── Allocation output ──

// AllocKind classifies a VReg's allocation.
type AllocKind uint8
const (
    AllocUnused AllocKind = iota // VReg never referenced
    AllocReg                     // assigned to a host register (possibly different per interval)
    AllocStack                   // totally spilled — all accesses via memory
)

// IntervalAlloc is the register assignment for one interval of a VReg.
// Paper's reg(s, [P,Q]) = r_j.
type IntervalAlloc struct {
    Interval Interval
    Host     int16  // physical register assigned for this interval
}

// Allocation is the output of the register allocator.
type Allocation struct {
    // Per-VReg summary: Kind indicates overall disposition.
    // For AllocReg, the per-interval assignments are in IntervalMap.
    // For AllocStack, the spill slot is in SpillSlot.
    Kind      []AllocKind          // indexed by VReg
    SpillSlot []int16              // indexed by VReg; valid when Kind == AllocStack

    // Per-interval register assignment (paper's reg(s,[P,Q])).
    // Multiple entries per VReg when it has multiple intervals or
    // different physical registers across intervals.
    IntervalMap []IntervalAlloc

    // Moves to insert at control flow edges (paper's step 6).
    // Key = instruction index of the branch/jump source.
    Moves []RegMove

    StackSlots int  // total 8-byte spill slots needed
}

// RegMove is a register-to-register move to insert at a control flow edge.
// Paper's step 6: when reg(s,P) != reg(s,Q) for a live VReg across an edge.
type RegMove struct {
    InsertAt int    // instruction index (before which to insert)
    From     int16  // source host register
    To       int16  // destination host register
}

// RegPool describes the available host registers, separated by class.
type RegPool struct {
    IntRegs []int16  // host register IDs for integer VRegs
    FPRegs  []int16  // host register IDs for FP VRegs
}
```

### Internal Data Structures

```go
// intervalSet is the collection of intervals for one symbolic register.
// Paper's I(s). In practice, average ~2 intervals per VReg (paper Section 4).
type intervalSet struct {
    VReg      VReg
    Intervals []Interval  // sorted by Start, non-overlapping
}

// iep is an Interval EndPoint — a start or end of an interval.
// Paper's IEP = set of all interval endpoints.
type iep struct {
    Point    int       // instruction index
    IsStart  bool      // true = interval starts here, false = interval ends here
    VReg     VReg      // which symbolic register
    Interval int       // index into the interval set for this VReg
}

// allocState holds mutable state during the allocation algorithm.
type allocState struct {
    // Paper's count[P] — number of simultaneously live VRegs at point P.
    count []int  // indexed by instruction index

    // Paper's avail — set of available physical registers at current point.
    availInt []int16
    availFP  []int16

    // Per-VReg: is it FP?
    isFP []bool

    // Per-VReg: spill(s) — true if totally spilled.
    spilled []bool

    // Paper's totalSpillCost(s).
    spillCost []float64

    // Per-VReg: the interval set.
    intervals []intervalSet

    // Per-VReg: last assigned host register (for assignment preference heuristic).
    lastReg []int16

    // Stack for spill resurrection (paper's ELS_1 step 2).
    spillStack []VReg

    // Collected interval allocations.
    intervalAllocs []IntervalAlloc

    // Collected register moves.
    moves []RegMove
}
```

### Function Signatures

```go
// ── Primary API ──

// Allocate performs Extended Linear Scan register allocation on the block.
//
// Parameters:
//   b      — Block with populated Instrs
//   pool   — available host registers (arch-specific, caller constructs)
//   pinned — VRegs with fixed host register assignments (e.g., parameter VRegs)
//   freq   — estimated execution frequency per instruction index (nil = all 1)
//            Used by ELS_1 for spill cost. For JIT blocks, backward branch
//            targets get higher weight (loop bodies).
//
// The algorithm implements ELS_1 from Sarkar & Barik with regMoves=true:
//   1. Compute interval sets (live ranges with holes)
//   2. Compute IEP and count[P] at each program point
//   3. Spill Identification: at points where count[P] > k, spill VRegs
//      with lowest totalSpillCost(s)/iDegree(s,P)
//   4. Spill Resurrection: un-spill VRegs when pressure has dropped
//   5. Register Assignment: ELS_0 algorithm for non-spilled VRegs
//   6. Insert register moves at edges where assignment differs
func Allocate(b *Block, pool RegPool, pinned map[VReg]int16, freq []float64) *Allocation

// ── Liveness & Interval Computation ──

// computeIntervalSets computes the interval set I(s) for each VReg.
// Unlike basic linear scan (one [Start,End] per VReg), this produces
// multiple disjoint intervals per VReg, capturing liveness holes.
//
// For a VReg that is defined at instruction 0, dead at 3-7, and reused at 8:
//   I(s) = {[0,2], [8,10]}  — two intervals, hole at [3,7]
//
// Guest regs (1-63) with any def are conservatively live to end
// (they may need writeback). This will be refined in future phases
// when dirty tracking is integrated.
func computeIntervalSets(b *Block) []intervalSet

// buildIEP collects all interval endpoints, sorted by program point.
// Paper's IEP. Each endpoint is tagged as start or end.
func buildIEP(intervals []intervalSet) []iep

// computeCount computes count[P] for each program point P.
// Paper's step 1: for each P in IEP, track numlive.
func computeCount(intervals []intervalSet, numInstrs int) []int

// ── VReg Classification ──

// classifyVRegs determines which VRegs are FP (true) vs integer (false).
// Guest FP regs (32-63) are trivially FP. Temps are classified by the
// type of their defining instruction.
func classifyVRegs(b *Block, intervals []intervalSet) []bool

// BlockHasDivMul scans b.Instrs for IRDivS, IRDivU, IRRem, IRMulHS,
// IRMulHU, IRMulHSU and returns true if any are present.
// Exported for the amd64 lowerer to decide whether to exclude RAX/RDX.
func BlockHasDivMul(b *Block) bool

// ── Instruction field accessors ──

// instrDefs returns the VReg defined by this instruction, or VRegZero if none.
func instrDefs(ins *IRInstr) VReg

// instrUses returns all VRegs used (read) by this instruction.
func instrUses(ins *IRInstr) []VReg

// maxVReg returns the highest VReg number referenced in any instruction.
func maxVReg(b *Block) VReg

// ── ELS_1: Spill Identification (paper Figure 6, step 1) ──

// computeSpillCosts computes totalSpillCost(s) for each VReg.
// totalSpillCost(s) = sum of freq[P] for all points P where s is read or written.
func computeSpillCosts(b *Block, intervals []intervalSet, freq []float64) []float64

// spillIdentify performs ELS_1 step 1: while any count[P] > k,
// pick the highest-pressure point P with largest freq[P], and spill
// the live VReg at P with smallest totalSpillCost(s)/iDegree(s,P).
func spillIdentify(st *allocState, k int, count []int, freq []float64)

// ── ELS_1: Spill Resurrection (paper Figure 6, step 2) ──

// spillResurrect pops from the spill stack and un-spills VRegs
// whose resurrection would not cause count[P] > k at any point.
func spillResurrect(st *allocState, k int, count []int)

// ── ELS_0: Register Assignment (paper Figure 4, steps 4-5) ──

// assignRegisters performs ELS_0 steps 4-5-6 for non-spilled VRegs.
// Scans IEP in order. At each endpoint:
//   - End of interval [O,P]: return reg to avail
//   - Start of interval [P,Q]: assign reg from avail with preference heuristics:
//     (a) prefer register previously assigned to same VReg (reduces moves)
//     (b) prefer register assigned to copy source if instruction is s := t
func assignRegisters(st *allocState, pool RegPool, pinned map[VReg]int16)

// ── ELS_0: Register Move Insertion (paper Figure 4, step 6) ──

// insertMoves detects control flow edges where reg(s,P) != reg(s,Q) for
// a live VReg, and inserts register-move instructions.
// Handles circular dependencies via SCC detection + XOR swap resolution.
func insertMoves(st *allocState, b *Block)

// findSCCs finds strongly connected components in a directed graph of
// register moves (m1 -> m2 if m1 reads the register written by m2).
// Used to resolve circular move dependencies with XOR swaps.
func findSCCs(moves []RegMove) [][]int
```

### Test Helpers (`ir/regalloc_test.go`)

```go
func makeBlock(instrs ...IRInstr) *Block
func testPool(nInt, nFP int) RegPool  // regs numbered 100+i (int), 200+i (FP)
func assertAllocReg(t *testing.T, alloc *Allocation, v VReg)
func assertAllocStack(t *testing.T, alloc *Allocation, v VReg)
func assertAllocUnused(t *testing.T, alloc *Allocation, v VReg)
func assertNoConflicts(t *testing.T, alloc *Allocation)
func assertRegAt(t *testing.T, alloc *Allocation, v VReg, instrIdx int, hostReg int16)
func regAt(alloc *Allocation, v VReg, instrIdx int) (int16, bool)
```

---

## Phase B: Interval Sets & Liveness (tests 1-25)

### `computeIntervalSets` — the key ELS data structure

Unlike basic linear scan's single `[Start, End]` per VReg, this produces an interval set `I(s)` with holes. Algorithm:

```
1. Walk b.Instrs backwards (reverse scan for precision):
   - Maintain a "currently live" bitset
   - At each instruction i:
     a. For each use (instrUses): if VReg not currently live, start a new interval ending at i
     b. For each def (instrDefs): if VReg is currently live, record interval start at i
        If VReg is not live, this is a dead def — create point interval [i,i]
   - At labels (branch targets): merge liveness from all branches targeting this label

2. For guest regs (1-63) with any interval: extend last interval's End to len(Instrs)-1
   (conservative: may need writeback at block exit)

3. For each VReg, sort intervals by Start, merge adjacent/overlapping ones
```

Per-op field semantics (same as before, critical for correctness):

| Category | Ops | Def | Uses |
|----------|-----|-----|------|
| ALU/data/FP | IRAdd, IRMov, IRConst, IRFAdd, ... | Dst | A, B |
| Store | IRStore | none | A (base), B (value) |
| StoreX | IRStoreX | none | A (base), B (index), Dst (value!) |
| Load/LoadX | IRLoad, IRLoadX | Dst | A (base), B (index for LoadX) |
| Branch | IRBranch, IRBranchImm | none | A, B |
| Jump/Label | IRJump, IRLabel | none | none |
| Call | IRCall | none | none |
| Ret | IRRet | none | A (faultAddr) |
| Pseudo | IRMarkLive, IRMarkDead | none | A |
| Writeback | IRWriteback | none | none |

### Tests (Group 1: instrDefs / instrUses)

1. `TestInstrDefs_ALU` — IRAdd: Dst is def
2. `TestInstrDefs_Store` — IRStore: no def (returns VRegZero)
3. `TestInstrDefs_StoreX` — IRStoreX: Dst is a use, not a def
4. `TestInstrDefs_Label` — IRLabel: no def
5. `TestInstrDefs_Ret` — IRRet: no def
6. `TestInstrUses_ALU` — IRAdd: A and B are uses
7. `TestInstrUses_Store` — IRStore: A (base) and B (value) are uses
8. `TestInstrUses_StoreX` — IRStoreX: A, B, and Dst are all uses
9. `TestInstrUses_Const` — IRConst: no uses (immediate only)
10. `TestInstrUses_Ret` — IRRet: A is a use

### Tests (Group 2: computeIntervalSets)

11. `TestIntervalSets_EmptyBlock` — no intervals
12. `TestIntervalSets_SingleDef` — `IRConst t64, 42` -> I(t64) = {[0,0]}
13. `TestIntervalSets_DefAndUse` — `IRConst t64; IRAdd x1,t64,x2` -> I(t64) = {[0,1]}
14. `TestIntervalSets_LiveRangeHole` — t64 defined at 0, used at 2, dead at 3-5, redefined at 6, used at 8 -> I(t64) = {[0,2], [6,8]} (two intervals with hole)
15. `TestIntervalSets_GuestRegsExtendToEnd` — x5 defined at 0, used at 2, block has 10 instrs -> End extended to 9
16. `TestIntervalSets_VRegZeroNoIntervals` — VRegZero never gets intervals
17. `TestIntervalSets_ParamVRegUsedBeforeDef` — t64 used at 0, never defined -> I(t64) = {[0,0]}
18. `TestIntervalSets_FPRegsExtendToEnd` — f5 (VReg 37) -> End = last instruction
19. `TestIntervalSets_MultipleVRegs` — three temps with different patterns; verify each set independent

### Tests (Group 3: buildIEP / computeCount)

20. `TestBuildIEP_Order` — endpoints sorted by program point
21. `TestBuildIEP_StartsBeforeEnds` — at same point, starts processed before ends
22. `TestComputeCount_NoPressure` — 3 non-overlapping intervals -> max count = 1
23. `TestComputeCount_FullOverlap` — 3 intervals all covering [0,5] -> count[0..5] = 3
24. `TestComputeCount_Gradient` — intervals starting at 0,1,2 ending at 5,5,5 -> count rises from 1 to 3

### Tests (Group 4: classifyVRegs / BlockHasDivMul / maxVReg)

25. `TestClassifyVRegs_GuestInt` — VRegs 1-31 -> not FP
26. `TestClassifyVRegs_GuestFP` — VRegs 32-63 -> FP
27. `TestClassifyVRegs_TempFromFAdd` — temp from IRFAdd(T=F64) -> FP
28. `TestClassifyVRegs_TempFromFCvtToI` — temp from IRFCvtToI(T=I64) -> not FP
29. `TestBlockHasDivMul_NoDivMul` -> false
30. `TestBlockHasDivMul_HasDivS` -> true
31. `TestBlockHasDivMul_HasRem` -> true
32. `TestMaxVReg_EmptyBlock` -> 0
33. `TestMaxVReg_HighTemp` -> handles VReg(200)

---

## Phase C: ELS_0 — Spill-Free Register Assignment (tests 34-42)

### Algorithm (paper Figure 4, steps 3-5)

```
Input: interval sets, IEP, k physical registers
Precondition: count[P] <= k at all program points (no spills needed)

1. avail := set of all physical registers {r1..rk}
2. For each program point P in IEP, in increasing order:
   a. For each interval [O,P] ending at P (O < P):
        avail := avail ∪ {r_j}   // return r_j assigned to [O,P]
   b. For each interval [P,Q] starting at P:
        s := symbolic register for [P,Q]
        Select r_j from avail using heuristics:
          - If s is live at P (continuing from prior interval), prefer
            the register previously assigned to s (reduces reg moves)
          - If instruction at P is a copy s := t, prefer the register
            assigned to t (copy coalescing)
        reg(s, [P,Q]) := r_j
        avail := avail - {r_j}
```

### Register Move Insertion (paper Figure 4, step 6)

```
For each control flow edge (P -> Q) in the block:
  M := empty set of moves
  For each symbolic register s live at both P and Q:
    if reg(s,P) != reg(s,Q):
      add move "reg(s,Q) := reg(s,P)" to M

  // Resolve circular dependencies:
  Build directed graph G on M: edge m1->m2 if m1 reads the reg m2 writes
  Compute SCCs of G
  For each SCC:
    if |SCC| == 1: emit simple MOV
    if |SCC| > 1: resolve cycle using XOR swap sequence:
      // For cycle r1->r2->r3->r1:
      //   XOR r1, r2; XOR r2, r1; XOR r1, r2  (swap r1,r2)
      //   then MOV r3 from the now-correct r2
```

### Tests (Group 5: ELS_0 basic assignment)

34. `TestAllocate_EmptyBlock` — returns non-nil Allocation, 0 stack slots
35. `TestAllocate_SingleInstrNoPressure` — 1 temp, 8 regs -> gets a register
36. `TestAllocate_AllFitNoOverlap` — 3 non-overlapping temps, 3 regs -> all in regs, 0 spills
37. `TestAllocate_AllFitOverlapping` — 3 overlapping temps, 4 regs -> all in regs
38. `TestAllocate_VRegZeroNeverAllocated` — always AllocUnused
39. `TestAllocate_ReuseAfterDeath` — t64:[0,2], t65:[4,6], 1 reg -> same reg reused
40. `TestAllocate_ManyShortRanges` — 10 sequential 2-instr temps, 1 reg -> all get same reg
41. `TestAllocate_IntervalHoleReuse` — t64 has hole [3,5]; another VReg allocated in the hole using same register

### Tests (Group 6: assignment preference heuristics)

42. `TestAllocate_PreferSameReg` — VReg with two intervals prefers same host reg across both (minimizes moves)
43. `TestAllocate_CopyCoalescing` — `IRMov dst, src` at interval start: dst prefers src's register

---

## Phase D: ELS_1 — Spill Identification & Resurrection (tests 44-54)

### Spill Identification (paper Figure 6, step 1)

```
While exists program point Q with count[Q] > k:
  a. P := point in IEP with count[P] > k and largest freq[P]
  b. s := symbolic register live at P, spill(s)==false, with smallest
     totalSpillCost(s) / iDegree(s,P)
     where iDegree(s,P) = count[P] - 1
  c. spill(s) := true; push s on stack T
  d. For each point X in IEP where s is live:
       count[X] := count[X] - 1
```

### Spill Resurrection (paper Figure 6, step 2)

```
While stack T is non-empty:
  a. s := pop(T)
  b. If count[Q] < k at every point Q where s is live:
       // Resurrecting s won't cause overflow — un-spill it
       spill(s) := false
       For each point X where s is live:
         count[X] := count[X] + 1
```

### Tests (Group 7: spill identification)

44. `TestAllocate_OneSpill` — 3 simultaneous live, 2 regs -> 1 spilled
45. `TestAllocate_MultipleSpills` — 5 simultaneous, 2 regs -> 3 spilled
46. `TestAllocate_SpillLowestCost` — with freq info, the VReg with lowest totalSpillCost/iDegree is spilled
47. `TestAllocate_SpillAtHighPressurePoint` — spill triggered at the program point with highest pressure
48. `TestAllocate_StackSlotCounting` — 3 spilled -> StackSlots==3, distinct slots 0,1,2

### Tests (Group 8: spill resurrection)

49. `TestAllocate_SpillResurrection` — VReg A spilled first, then B spilled which drops pressure enough to un-spill A. Verify A ends up in a register.
50. `TestAllocate_NoResurrection` — all spills necessary; none resurrected
51. `TestAllocate_ResurrectionOrder` — LIFO order (stack): last spilled = first candidate for resurrection

### Tests (Group 9: spill cost with freq)

52. `TestComputeSpillCosts_UniformFreq` — nil freq (all 1.0): cost = number of read+write points
53. `TestComputeSpillCosts_LoopWeight` — higher freq at backward branch targets -> VRegs used in loop body have higher spill cost -> less likely to be spilled
54. `TestComputeSpillCosts_DeadDef` — VReg defined but never used: low cost, first to spill

---

## Phase E: FP Pools, Guest Regs, Pinned, Control Flow (tests 55-70)

### Tests (Group 10: FP/int pool separation)

55. `TestAllocate_IntAndFP_SeparatePools` — 1 int + 1 FP, 1 of each -> both get regs
56. `TestAllocate_FPPressure_IntFree` — 2 FP simultaneous, 1 FP reg + 5 int regs -> 1 FP spills
57. `TestAllocate_GuestFPRegs` — f5 (VReg 37) -> assigned from FP pool

### Tests (Group 11: guest regs live to end)

58. `TestAllocate_GuestRegLiveToEnd` — x5 defined at 0, 10 instrs -> holds reg through end
59. `TestAllocate_GuestRegEvictsTemp` — x5 lives to end, temp overlaps, low reg count -> temp spills

### Tests (Group 12: pinned parameter VRegs)

60. `TestAllocate_PinnedRegs` — pinned map {t64->50, t65->51} -> exact host regs
61. `TestAllocate_PinnedRegsNotInPool` — pinned regs don't consume pool slots
62. `TestAllocate_PinnedRegsBlockHostReg` — pinned VReg's host reg unavailable to others

### Tests (Group 13: register moves at control flow edges)

63. `TestAllocate_NoMovesNeeded` — all intervals of VReg get same register -> 0 moves
64. `TestAllocate_SimpleMoveInserted` — VReg has two intervals getting different regs -> 1 move
65. `TestAllocate_CircularMovesSCC` — three VRegs forming a circular dependency r1->r2->r3->r1 -> resolved via XOR swaps
66. `TestAllocate_MoveAtBranchTarget` — move inserted at correct instruction index (label target)

### Tests (Group 14: edge cases)

67. `TestAllocate_OnlyVRegZeroRefs` — all AllocUnused
68. `TestAllocate_ManyTempsShortRanges` — 50 sequential 1-instr temps, 1 reg -> 0 spills
69. `TestAllocate_OneLongVsManyShort` — t64:[0,20] + 10 short temps, 2 regs -> 0 spills
70. `TestAllocate_PoolWithoutDivMulRegs` — caller trims pool, allocator works with trimmed set

---

## Phase F: Invariant Tests (tests 71-74)

71. `TestAllocate_NoConflicts_SmallBlock` — 5 overlapping temps, 3 regs -> at no program point do two simultaneously-live VRegs share a host register
72. `TestAllocate_AllReferencedVRegsAllocated` — every non-VRegZero VReg has Kind != AllocUnused
73. `TestAllocate_SpilledVRegsHaveSlots` — every AllocStack VReg has a valid unique slot index
74. `TestAllocate_MovesAreValid` — every RegMove references host regs that are actually in use

---

## Phase G: Fuzz Testing (3 fuzz targets)

### `FuzzRegAllocInvariants`

Strategy: random IR blocks from fuzzer bytes (4-byte tuples, following `FuzzPeepholeTermination` pattern). Run `Allocate` with small pool (3 int, 2 FP). Assert:

1. No panic
2. VRegZero is AllocUnused
3. Every referenced VReg has Kind != AllocUnused
4. StackSlots >= 0
5. **No two simultaneously-live VRegs share a host register at any program point**
6. All assigned host regs come from the pool
7. All RegMoves reference valid host regs

### `FuzzLiveRangeConsistency`

Strategy: random IR blocks, compute interval sets, verify:

1. For every interval: End >= Start
2. Intervals within a VReg's set are non-overlapping and sorted by Start
3. VRegZero has no intervals
4. No Start or End exceeds len(b.Instrs)-1
5. count[P] computed from intervals matches independent recount

### `FuzzSpillResurrection`

Strategy: random blocks with controlled high pressure (many overlapping VRegs, small pool). Verify:

1. After allocation, count[P] <= k at all points
2. Every resurrected VReg has AllocReg (not AllocStack)
3. Spill decisions are consistent: no VReg is both spilled and has interval allocations

---

## Key Design Decisions

1. **Full ELS with interval sets** — not simplified to single-range. The IR will grow to multi-block CFG; this avoids a rewrite.

2. **Interval sets capture liveness holes** — a temp defined at 0, dead at 3-7, reused at 8 gets two intervals {[0,2],[8,10]}. The register is available during the hole.

3. **Per-interval register assignment** — same VReg can be in different host regs at different intervals. Register moves inserted at edges.

4. **Spill resurrection** (ELS_1 step 2) — un-spills VRegs when later spills reduce pressure. Free optimization that basic linear scan misses.

5. **Copy coalescing in assignment** — when instruction at interval start is `s := t`, prefer assigning `s` the same register as `t`. Eliminates the move.

6. **Circular move resolution via SCC + XOR** — paper's step 6. For cycles in register-move graphs, use XOR swap (no temp register needed).

7. **Arch-agnostic** — `RegPool` constructed by caller. `BlockHasDivMul()` exported for amd64 lowerer.

8. **Frequency-weighted spill cost** — `freq[]` parameter lets JIT blocks weight loop bodies higher. Backward branch targets get elevated freq. Defaults to uniform (all 1.0).

9. **Guest regs conservatively live to end** — safe; will be refined when dirty tracking is integrated into the allocator.

10. **Separate int/FP pools and active sets** — prevents cross-class assignment.

## Critical Files

- `ir/ir.go` — VReg, IRInstr, Block, IROp definitions (allocator's input types)
- `ir/emit.go` — Emitter with param VRegs (t64-t68), dirty tracking
- `ir/emit_impl_test.go:5` — `newTestEmitter()` pattern
- `ir/fuzz_test.go` — fuzz target patterns (byte-tuple encoding, invariant checking)
- `~/ris/extended_linear_scan_register_allocation.pdf` — ELS algorithm reference

## Verification

```bash
# Unit tests
go test -v -run 'TestInstr|TestInterval|TestBuild|TestCompute|TestClassify|TestBlockHas|TestMaxVReg|TestAllocate' ./ir/

# Fuzz tests (short run)
go test -fuzz FuzzRegAllocInvariants -fuzztime 60s ./ir/
go test -fuzz FuzzLiveRangeConsistency -fuzztime 30s ./ir/
go test -fuzz FuzzSpillResurrection -fuzztime 30s ./ir/

# All ir tests (regression)
go test -v ./ir/
```
