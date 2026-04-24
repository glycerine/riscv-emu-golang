# Plan: Vestibule Optimization — Trampoline-Based Reg Load/Store

## Context

We're at 2963 MIPS after the direct-destination optimization (Steps 0–2a done).
Target is ≥3644 MIPS. The user proposes a "vestibule" pattern: do frame
setup and register loads **once** when entering JIT world from Go, and
teardown **once** when exiting — not on every block-to-block chain.

## Lesson from Failed Attempt

The first attempt tried to solve this by:
1. **Uniform allocation** — forcing all 12 priority regs into host registers
   regardless of usage, to make the mapping identical across blocks.
2. **Per-block fast/slow entry** — duplicating the prologue into two paths.

This failed because:
- Uniform allocation consumed all pool slots, starving temps — caused
  an infinite loop hang in CoreMark
- Per-block duplication bloated code (+1000 bytes per block)
- Chain exits are rarely taken (budget-check returns dominate:
  `insns/DispatchOK ≈ maxIC ≈ 4096`), so chain patching never fires

## Correct Architecture: Trampoline as Vestibule

The reg load/store code is identical for every block (same `FixedStaticAllocator`
mapping). It belongs in the trampoline (`internal/jitcall/call_amd64.s`),
not generated per-block.

### Current Trampoline Flow

```
CallAOT trampoline:
  1. Save callee-saved (RBX, RBP, R12-R15)
  2. Set up: RDI=sret, RSI=regfile, R8=memBase, R9=memMask
  3. CALL AX (block fn)
  4. Copy Result struct
  5. Restore callee-saved
  6. RET
```

### Proposed Trampoline Flow

```
CallAOT trampoline:
  1. Save callee-saved (RBX, RBP, R12-R15)
  2. Set up: RDI=sret, RSI=regfile, R8=memBase, R9=memMask
  3. MOV RSI → RBP           // pin regfile base BEFORE loading regs
  4. Publish memBase/memMask to sret buffer
  5. Zero sret.IC
  6. Load 12 RISC-V int regs from [RBP+vr*8]  // THE VESTIBULE ENTRY
     MOV  8(RBP), DX    // ra → RDX
     MOV 16(RBP), BX    // sp → RBX
     MOV 40(RBP), SI    // t0 → RSI  (clobbers regfile ptr — RBP has it)
     MOV 48(RBP), DI    // t1 → RDI  (clobbers sret ptr — on stack)
     MOV 80(RBP), R8    // a0 → R8   (clobbers memBase — in sret buffer)
     MOV 88(RBP), R9    // a1 → R9   (clobbers memMask — in sret buffer)
     MOV 96(RBP), R10   // a2 → R10
     MOV 104(RBP), R11  // a3 → R11
     MOV 112(RBP), R12  // a4 → R12
     MOV 120(RBP), R13  // a5 → R13
     MOV 128(RBP), R14  // a6 → R14
     MOV 136(RBP), R15  // a7 → R15
  7. MOV RDI → RAX           // sret into RAX (block expects RAX=sret)
     Wait — RDI was clobbered in step 6! Need to save sret BEFORE step 6.
     Fix: MOV RDI → RAX before loading regs. Or stash RDI to stack first.
  8. CALL AX (block fn)
  9. Store 12 RISC-V int regs back to [RBP+vr*8]  // THE VESTIBULE EXIT
     MOV DX,  8(RBP)
     MOV BX, 16(RBP)
     MOV SI, 40(RBP)
     MOV DI, 48(RBP)
     MOV R8, 80(RBP)
     ... etc.
 10. Copy Result struct
 11. Restore callee-saved
 12. RET
```

### What Changes in Blocks

**Block prologue** (first entry AND chain entry) becomes minimal:
```
// First entry only:
  (nothing — trampoline already did RBP, reg loads, sret→RAX)

// Chain entry (NOP label):
  SUBQ frameSize, RSP
  MOV RAX → [RSP+sretOffset]
  Load IC from sret.IC → spill slot
  Load memBase from sret → spill slot
  Load memMask from sret → spill slot
```

No reg loads! The trampoline already loaded them.

**Block chain exit** becomes minimal:
```
  stage IC → sret.IC
  MOV [RSP+sretOffset] → RAX    // load sret
  ADDQ frameSize, RSP           // dealloc
  MOVABS RCX, target            // patchable
  JMP RCX
```

No storeRegsBack! Regs stay in host registers.

**Block return to Go** becomes minimal:
```
  // Write Result fields (PC, IC, Status, FaultAddr) to [RAX+...]
  ADDQ frameSize, RSP
  RET
```

No storeRegsBack! The trampoline does it after RET.

**Slow exit stub** becomes trivial:
```
  // RAX = sret (from chain exit)
  MOV targetPC → [RAX+0]    // Result.PC
  MOV 0 → [RAX+16]          // Result.Status = jitOK
  MOV 0 → [RAX+24]          // Result.FaultAddr
  RET                        // returns to trampoline, which stores regs
```

No storeRegsBack needed in the stub either!

### No Allocator Changes Needed

The FixedStaticAllocator is unchanged. Blocks still allocate only the
RISC-V regs they actually use. The trampoline always loads/stores all 12
regardless — a few wasted MOVs at the Go boundary (once per dispatch)
is negligible since `insns/dispatch ≈ 4096`.

Different blocks may allocate different subsets of the 12 priority regs.
That's fine: the trampoline loads ALL 12, the block only uses the ones
it cares about, and on return the trampoline stores ALL 12 back.
Unused regs round-trip through the host register harmlessly.

### Impact on FP Regs

Same pattern for FP regs: trampoline loads fa0-fa7, ft0-ft7 etc. from
`[RBP+256+r*8]` using MOVSD, and stores them back after RET. Or we can
defer FP to a later step if it's less critical.

### Complications

1. **RDI/RSI clobbering**: Loading t0→RSI and t1→RDI clobbers the sret
   and regfile pointers. Fix: RBP is set first (holds regfile), and sret
   is moved to RAX or stashed on the stack before loading regs.

2. **R8/R9 clobbering**: Loading a0→R8 and a1→R9 clobbers memBase and
   memMask. Fix: these are already published to the sret buffer before
   reg loading.

3. **Trampoline is assembly**: call_amd64.s is hand-written Go asm.
   Adding 24 MOVs (12 load + 12 store) is straightforward.

4. **CallAOT vs Call**: The `Call` trampoline (non-AOT) should get the
   same vestibule treatment. Or we can unify them.

## Critical Files

| File | Change |
|------|--------|
| `internal/jitcall/call_amd64.s` | Add vestibule reg loads before CALL, stores after |
| `ir/lower_amd64_rv8.go` | Remove reg loads from prologue, remove storeRegsBack from exits |

## Verification

1. `go test ./...` — all tests green
2. `make bench` — target ≥3644 MIPS
3. `go test -run TestJIT_CoreMark -v ./bench/` — no hang

## Execution Order

1. Modify trampoline to load/store 12 int regs
2. Remove reg loads from block prologue
3. Remove storeRegsBack from all return paths and slow stubs
4. Test and benchmark
5. (Optional) Extend to FP regs
