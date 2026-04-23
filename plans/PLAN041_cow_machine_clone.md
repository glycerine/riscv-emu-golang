# Phase 2c: Machine.Clone() — Instant Fork with CoW Guest Memory

## Context

Phase 2b (multi-segment `DecodedExecuteSegment` + ref-counting) shipped.
Segments are structurally immutable post-install (blocks map read-only,
decoder_cache mprotect RO, native code already patched) — the invariant
that makes sharing safe. Phase 2c turns the dormant ref-counting
infrastructure into an actual fork API: clone a running sandbox
instantly, with shared immutable JIT state and copy-on-write guest
memory.

### Architectural decision: OS-level CoW, not software page-table CoW

libriscv uses a two-tier memory model:
- **Fast path**: flat linear arena (`flat_readwrite_arena=true` by
  default). Direct pointer math, no lookup. (memory_inline.hpp:30–41)
- **Slow path**: `std::unordered_map<address_t, Page> m_pages` hash-map
  page table. `Page` has per-page `PageAttributes` including `is_cow`,
  `non_owning` (memory.hpp:318, page.hpp:10–181). On fork, writable
  pages are "loaned" with `is_cow=true`, `write=false` (memory.cpp:
  518–533). On first write, `create_writable_pageno` intercepts via
  the attribute check, invokes the page-write handler, allocates a
  new `PageData`, and detaches from the master (memory_rw.cpp:24–47).

Our architecture is **pure flat mmap** (`hostPtr = base + (addr &
mask)`) with no page table. Adding one would invade the JIT's emitted
load/store asm (which uses R14 = base + direct offset). An OS-level
CoW approach — `mach_vm_remap` on darwin, `memfd + MAP_PRIVATE` on
Linux — gives us:

- Write interception for free: kernel handles the page fault, copies
  the page, updates the child's mapping. No emulator code change.
- Parent's hot path untouched: continues to use the same mmap it
  already had.
- Child's hot path untouched: it too has a flat mmap, with the same
  `hostPtr = base + (addr & mask)` invariant. The only difference is
  that physical pages are shared with the parent until first write.

The user has sketched `ir/cow_darwin.go` (`MachVMRemap`) and
`ir/cow_linux.go` (`COWRemap`). Step one is verifying those actually
deliver CoW semantics; step two is building `Machine.Clone()` on top.

## Phase 1 — Test and fix the CoW utilities

### Suspected bugs in the sketches (tests will confirm or refute)

**`ir/cow_darwin.go`** (uses CGO `mach_vm_remap`):

Apple's `boolean_t copy` parameter: `copy=TRUE` ⇒ copy-on-write mapping
(writes to target are private); `copy=FALSE` ⇒ shared mapping (writes
visible in both). Current sketch passes `C.boolean_t(0)` = FALSE = Shared.
The inline comment "0 for CoW/Shared" is wrong — that value gives Shared,
not CoW. **Expected fix: flip to `C.boolean_t(1)`.** The test-first
approach will prove this empirically by writing to the "child" and
checking the parent byte.

No release helper. **Add:** `COWUnmap(addr uintptr, size uint64) error`
wrapping `mach_vm_deallocate(mach_task_self(), addr, size)`.
(POSIX `munmap` works on darwin for mach_vm regions, but using the
matched API is cleaner.)

**Also rename** `MachVMRemap` → `COWRemap` so the darwin and linux
files expose an identical public API. Platform dispatch then lives
entirely in build tags — callers write `ir.COWRemap(size, base)` and
`ir.COWUnmap(addr, size)` regardless of GOOS.

**`ir/cow_linux.go`** (uses `memfd_create` + `MAP_PRIVATE`):

Logic is correct (memfd → truncate → mmap SHARED → copy → unmap →
mmap PRIVATE ⇒ CoW). Two hygiene issues:

1. Returns only `uintptr`. The caller loses the `[]byte` slice header
   needed for `unix.Munmap`. **Fix:** provide a companion
   `COWUnmap(addr uintptr, size uint64) error` that reconstructs the
   slice via `unsafe.Slice` and calls `unix.Munmap`. Identical name
   and signature to the darwin helper.
2. `defer unix.Close(fd)` is correct as-is — the mmap retains a
   kernel refcount on the file; closing the fd does not unmap.

### Test plan (`ir/cow_darwin_test.go`, `ir/cow_linux_test.go`)

Each file has `//go:build <goos>` and only imports `testing`, the
platform helper from the same package, and `unsafe` for pointer
reads/writes. Same test set on both platforms; differ only in the
function call.

```go
// Pseudocode — same shape both platforms
func TestCoW_BasicRemap(t *testing.T) {
    src := mustAllocateSource(t, 2*pageSize, 0xAA) // fill with 0xAA
    childAddr, err := COWRemap(2*pageSize, src)    // or MachVMRemap
    if err != nil { t.Fatal(err) }
    t.Cleanup(func() { _ = COWUnmap(childAddr, 2*pageSize) })

    // initial: child bytes == source bytes
    if childByte(childAddr, 0) != 0xAA { t.Fatal("initial mismatch") }
}

func TestCoW_WriteToChildIsolatesFromParent(t *testing.T) {
    // Write a new pattern to child, read parent — parent must be unchanged.
    // This is THE CoW correctness invariant. On darwin, fails with copy=0.
}

func TestCoW_WriteToParentDoesNotAffectChildAfterChildWrote(t *testing.T) {
    // Child writes first (page becomes unique to child).
    // Then parent writes same offset — child must keep its value.
}

func TestCoW_ParentWriteBeforeChildWrite(t *testing.T) {
    // Subtle case: before child writes, parent writes. On some CoW
    // implementations the child *will* see the new value (shared-
    // until-first-write). On others the child sees the fork-time
    // snapshot. Test documents the observed behavior rather than
    // asserting one or the other — the guarantee we need is only
    // "post-child-write isolation," which the previous two tests
    // already cover.
}

func TestCoW_MultipleForks(t *testing.T) {
    // Fork A, fork B from same source. Write distinct patterns to
    // each. Verify source, A, B all independent post-write.
}

func TestCoW_ReleaseOneForkLeavesOthersValid(t *testing.T) {
    // Deallocate A; verify source and B still readable + distinct.
}
```

Helper `mustAllocateSource` allocates via `syscall.Mmap` (anonymous
private) to get a clean source region sized in multiples of the page
size — independent of `GuestMemory`, so these tests pass even if
GuestMemory is unavailable.

### Exit criterion for Phase 1

All six tests pass on both platforms. The darwin `copy` flag fix (if
needed) is applied. `MachVMDeallocate` / `COWUnmap` helpers exist.

## Phase 2 — `GuestMemory.CowClone()`

File: `guestmem.go` (add method) + possibly a new `guestmem_cow.go`
for the platform-dispatch shim.

```go
// CowClone returns a new GuestMemory backed by a copy-on-write view
// of m's pages. Writes to the clone are private; writes to m are also
// private post-first-write. Shared physical pages until each side
// writes. The clone's vaddr space, size, mask, and execRegions match m.
func (m *GuestMemory) CowClone() (*GuestMemory, error) {
    newBase, err := ir.COWRemap(m.size, m.base)  // same name on both OSes
    if err != nil { return nil, err }
    return &GuestMemory{
        base:        newBase,
        mask:        m.mask,
        size:        m.size,
        execRegions: append([]ExecRegion(nil), m.execRegions...),
    }, nil
}
```

Because `ir.COWRemap` / `ir.COWUnmap` have identical signatures on
both platforms (dispatch via build tags in `ir/cow_darwin.go` and
`ir/cow_linux.go`), `CowClone` is a single implementation with no
`//go:build` guard. No additional `guestmem_cow_*.go` files needed.
Simpler than the earlier "two build-tagged files" plan.

### Free path

`C.guest_free` calls `munmap(p, size)`. Works correctly for both:
- Linux MAP_PRIVATE memfd-backed regions (standard munmap).
- Darwin mach_vm_remap regions (Darwin's munmap calls
  `mach_vm_deallocate` internally for Mach VM regions).

So `GuestMemory.Free()` is unchanged — it munmaps whatever `m.base`
points to, regardless of origin. No branching needed. `ir.COWUnmap`
is retained for direct callers of the `ir` package that want the
symmetric `COWRemap` ↔ `COWUnmap` API.

## Phase 3 — `JIT.CloneShared()`

File: `jit.go` (new method).

```go
// CloneShared returns a new JIT that shares j's AOT segments (via
// Retain) but has its own lazy block cache. Safe to install more AOT
// or lazy-compile blocks in the clone without affecting j.
func (j *JIT) CloneShared() *JIT {
    child := &JIT{
        aotSegments: append([]*DecodedExecuteSegment(nil), j.aotSegments...),
        irAlloc:     j.irAlloc,  // stateless; shared is fine
        // fresh: cache, noJIT, counters, hot/soleSegment
    }
    for _, s := range child.aotSegments {
        s.Retain()
    }
    child.refreshSoleSegment()
    return child
}
```

Design decisions:
- `aotSegments`: shared slice entries, each `Retain()`'d. Phase 2b
  segments are structurally immutable; sharing is safe.
- `cache`: fresh zero-valued array. Lazy blocks are per-JIT and not
  shared. (Future optimization could share a read-only lazy cache, but
  out of scope.)
- `noJIT`: fresh nil map. Child may re-discover untranslatable PCs;
  tiny cost. Avoids shared-map concurrency concern.
- `hotSegment`/`soleSegment`: recomputed by `refreshSoleSegment()`.
- Counters: zero, so the clone gets its own measurement baseline.
- Debug flags (`InterpOnly`, `UseV2`, `DebugV1V2`, `trace`): not
  copied — child defaults to production settings. Caller can re-enable
  after clone if needed.

## Phase 4 — `Machine` type + `Machine.Clone()`

The codebase currently has no `Machine` struct; `NewCPU(mem GuestMemory)`
takes memory by value and inlines it. The "sandbox unit" is just `*CPU`
+ optional `*JIT`. For the fork API, a thin `Machine` wrapper matches
the user's mental model and libriscv's `Machine(const Machine&)`
constructor semantics, without forcing existing callers to migrate.

File: new `machine.go`.

```go
// Machine bundles a CPU and its (optional) JIT for convenient fork.
// Existing code can continue to use *CPU + *JIT directly; Machine is
// only needed when you want Clone semantics.
type Machine struct {
    CPU *CPU
    JIT *JIT  // may be nil (interpreter-only sandbox)
}

func (m *Machine) Clone() (*Machine, error) {
    childMem, err := m.CPU.mem.CowClone()
    if err != nil { return nil, err }
    child := &CPU{
        mem:   *childMem,                  // value copy of the struct
        pc:    m.CPU.pc,
        x:     m.CPU.x,                    // array value copy
        f:     m.CPU.f,
        fcsr:  m.CPU.fcsr,
        cycle: m.CPU.cycle,
        // Notes: fresh NoteChain. Handler installation is caller's
        // responsibility post-fork (OS personality needs re-wiring
        // against the child, not the parent).
        resvAddr:  m.CPU.resvAddr,
        resvValid: m.CPU.resvValid,
        watchAddr: m.CPU.watchAddr,
        mtvec:     m.CPU.mtvec,
        mepc:      m.CPU.mepc,
        mcause:    m.CPU.mcause,
        mstatus:   m.CPU.mstatus,
        mtval:     m.CPU.mtval,
    }
    var childJIT *JIT
    if m.JIT != nil {
        childJIT = m.JIT.CloneShared()
    }
    return &Machine{CPU: child, JIT: childJIT}, nil
}
```

Notes:
- `mem *childMem` value-copies the `GuestMemory` struct (base+mask+
  size). The finalizer on `childMem` from `CowClone` is preserved via
  the returned pointer; `*childMem` is a value copy of the struct
  fields, but the runtime finalizer is registered on the allocated
  struct which we don't discard — actually, the finalizer is on
  `childMem`, a `*GuestMemory`, but we embed the VALUE into
  `CPU.mem`. That's a risk: the `*GuestMemory` pointed to by the
  finalizer is no longer reachable through the CPU, so GC will run
  the finalizer prematurely and munmap the child's memory while the
  CPU is still using it.
- **Fix**: Change CPU to hold `*GuestMemory` instead of `GuestMemory`,
  OR have `CowClone` not set a finalizer (caller's `Machine.Clone`
  ensures explicit Free). The second option is lighter. The invariant
  becomes: `m.Free()` must be called; no GC safety net for child
  memories. Document this in CowClone.
- `Notes`: fresh for child. The parent's `NoteChain` stack holds
  handlers (closures) that likely capture the parent `*CPU`. Sharing
  them would cross the fork boundary incorrectly. Caller re-installs
  OS handlers on the child. Document clearly.

### Free ordering for a forked Machine

`(m *Machine) Close()`:
1. `m.JIT.Close()` — Release each segment; when parent also Closes,
   each segment's refcount hits 0 exactly once.
2. `m.CPU.mem.Free()` — munmap the CoW region (parent's mmap stays).

## Phase 5 — Integration tests (`machine_clone_test.go`)

Assuming `GuestMemory` CoW is available on darwin + linux, and the
tests skip cleanly elsewhere.

| test | assertion |
|------|-----------|
| `TestMachineClone_MemoryIsolation_ParentUntouched` | Write 0xAA @ 0x1000 in parent, Clone, write 0xBB @ 0x1000 in child, verify parent reads 0xAA |
| `TestMachineClone_MemoryIsolation_ChildUntouched` | After the above, verify child still reads 0xBB |
| `TestMachineClone_SegmentSharing` | Install AOT on parent, Clone, verify `parent.JIT.aotSegments[0] == child.JIT.aotSegments[0]` (same pointer) |
| `TestMachineClone_RefcountBalance` | Install AOT → refcount 1. Clone 3× → refcount 4. Close all 4 machines → Release'd 4 times → segment mmaps are munmapped |
| `TestMachineClone_CPUStateCopy` | Set pc/x/f on parent, Clone, verify child state matches at fork time |
| `TestMachineClone_CPUStateDivergence` | Clone, write to child x[5], verify parent x[5] unchanged |
| `TestMachineClone_IndependentExecution` | Both run a coremark-style loop from the same PC after clone; each produces its own instruction counter |
| `TestMachineClone_WithoutJIT` | Clone a Machine with `JIT == nil`; child JIT also nil; interpreter path works |
| `TestMachineClone_LazyBlocksNotShared` | Install lazy-only (no AOT); parent compiles a block; Clone; child cache is empty; child compiles its own copy; parent/child function pointers differ |

## Files to create / modify

| file | change |
|------|--------|
| `ir/cow_darwin.go` | rename `MachVMRemap` → `COWRemap`; flip `copy` flag FALSE→TRUE for true CoW; add `COWUnmap(addr, size)` |
| `ir/cow_linux.go` | add `COWUnmap(addr, size)` helper; keep existing `COWRemap` signature |
| `ir/cow_darwin_test.go` (new) | `//go:build darwin`; 6 CoW semantic tests |
| `ir/cow_linux_test.go` (new) | `//go:build linux`; same 6 tests |
| `guestmem.go` | add `CowClone() (*GuestMemory, error)` method calling `ir.COWRemap`; no build tags needed since `ir.COWRemap` name is uniform; **no `runtime.SetFinalizer` on the child** — Free is mandatory, documented in method comment |
| `jit.go` | add `(j *JIT) CloneShared() *JIT` |
| `machine.go` (new) | `Machine` struct, `Clone()`, `Close()` |
| `machine_clone_test.go` (new) | 9 integration tests; `//go:build darwin || linux` |

Unchanged: interpreter; the JIT hot path; `internal/jitcall`;
existing `NewCPU` / callers of CPU; ref-counting primitives on
segments (already in place from Phase 2b).

## Execution order

1. **Test the CoW sketches.** Write `ir/cow_*_test.go`; run → expect
   darwin to fail on the "write to child doesn't affect parent" test;
   flip `copy` flag; re-run; expect all green on both platforms. Add
   `MachVMDeallocate` / `COWUnmap` if needed by tests.
2. **Build `GuestMemory.CowClone`.** Add platform-dispatched
   `cowClone`, `CowClone()` method. Unit test: create GuestMemory,
   fill with pattern, Clone, verify pattern in child; write in child,
   verify parent unaffected.
3. **Build `JIT.CloneShared`.** Unit test: create JIT, install AOT,
   Clone, verify segment pointers shared and refcount incremented.
4. **Build `Machine` + `Machine.Clone`.** Integration tests as
   listed. Ensure `Close` balances refcounts and frees CoW memory
   exactly once per Machine.
5. **Full regression sweep.**
   ```bash
   go test . ./ir/ ./bench/
   make fuzz-oracle    # 60s
   make fuzz-rvc       # 60s
   make fuzz-amo       # 60s
   make fuzz-bitmanip  # 60s
   make bench-chain-ref
   ```
   Coremark/dhrystone/bench_guest MIPS unchanged (parent path is
   untouched by any Phase 2c change).

## Verification

### Correctness
- `ir/cow_*_test.go` all green on each respective platform.
- `guestmem_cow_*_test.go` (if added; otherwise coverage via machine_clone_test).
- `machine_clone_test.go` all green.
- Existing test suite, fuzz targets, and bench-chain-ref green.

### Performance
- **Non-cloned workloads**: Phase 2b MIPS targets unchanged
  (coremark ≥ 940, dhrystone ≥ 785, bench_guest ≥ 3200). Phase 2c
  adds no per-instruction cost.
- **Clone latency**: sub-millisecond on a multi-MB parent
  (mach_vm_remap / memfd_create are O(1) in page count mapping, O(N)
  in source size for the memcpy-into-memfd step on Linux; typical
  guest sizes 64 MB → single-digit milliseconds on Linux, sub-ms on
  darwin). Not benchmarked formally in this pass.

## Non-goals (this pass)

- **No FENCE.I auto-invalidation.** If the clone's guest writes new
  code and jumps to it at an address inside an existing segment,
  Phase 2b's staleness caveat applies. User can call
  `InvalidateSegment` explicitly.
- **No os.go mmap/mprotect syscall hooks.** The `ExecRegion` table is
  frozen at the fork point. Clones that genuinely need dynamic exec
  regions will need those hooks wired, which is a separate pass.
- **No cross-process fork.** Both `mach_vm_remap` (with non-self
  target_task) and `memfd + MAP_SHARED` across fork(2) could support
  it, but we're strictly within one process here.
- **No segment-pointer indirection in JALR asm.** Phase 2c parent
  and child share the same native code mmap — a JALR crossing
  segments still takes the standard 2-way IC + Go round-trip path.
- **No libriscv-style software page table.** That approach pays per-
  access hash-map lookup cost on the slow path and needs a separate
  arena; adds complexity without a clear win for our flat-mmap fast
  path. Documented in Context.
- **No shared read-only lazy block cache.** Each JIT's lazy cache
  stays private. Simplifies lifetime reasoning; minor recompilation
  cost on a child only for PCs the parent already lazy-compiled.
- **No migration of existing `NewCPU(GuestMemory)` callers to
  `Machine`.** The `Machine` type is purely additive; existing tests
  and bench code continue to work unchanged.

## Risks / edge cases

- **GuestMemory finalizer on CoW child**: the existing finalizer
  would run on GC and munmap the child's region while the CPU still
  references it (CPU embeds GuestMemory by value; the pointer the
  finalizer watches is the one `CowClone` returned and immediately
  dereferenced into the CPU). **Mitigation**: `CowClone` does NOT
  install a finalizer on the returned `*GuestMemory`. Free is
  mandatory for CoW children. Documented in method comment.
- **NoteChain and OS handler closures**: parent's handlers likely
  capture parent `*CPU`. Sharing them into child is a correctness
  bug. **Mitigation**: child's `Notes` is fresh. Caller re-installs
  handlers. Documented.
- **Segment lifetime during parent Close before child finishes**:
  if parent calls `Close` first, Retain count drops but stays ≥ 1
  while children hold refs. When last child closes, mmaps are freed.
  Standard refcount semantics; tested.
- **mach_vm_remap on a MAP_NORESERVE source**: `guest_alloc` uses
  MAP_NORESERVE so physical pages are demand-faulted. mach_vm_remap
  with CoW on such a source should work (the source's demand-paging
  semantics carry through), but testable — the first-touch-after-
  clone in the child triggers the fault as normal. Test this.
- **`memfd_create` on older Linux**: available since kernel 3.17
  (2014). We target modern distros; not a concern but documented.
- **NaN canary / signaling NaNs across fork**: no interaction —
  GuestMemory contents are bytes; NaN boxing is an interpretation
  applied inside CPU registers, which are copied eagerly.
- **Linux CoW + fork(2)**: if the host process is later fork(2)'d
  (not our clone, but the OS syscall), MAP_PRIVATE regions are
  themselves CoW'd. Both our child and the OS fork would see
  copy-on-write state. Unlikely in our usage; noted.

## What to expect

If the sketch bug is just the darwin `copy` flag (most likely), Phase
1 completes in one fix + green tests. Phase 2–4 are mechanical. The
main implementation surprise tends to be in the CPU-state-copy
minefield: something overlooked (a CSR, the `Notes` chain, a flag)
causes subtle guest-visible behavior divergence. The integration
tests are structured to catch the common ones (registers, FP state,
traps) — anything not on that list we add as a follow-up.
