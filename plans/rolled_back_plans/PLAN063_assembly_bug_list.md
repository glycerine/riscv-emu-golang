# Bug Audit: Assembly Dumps for bench_guest AOT Compilation

## Files Audited
- `~/ris/debug_vizjit_dir/fa758bc3a0382817.gocpu.asm.pc_0x00001000.asm` (main function, 0x1000..0x10fc)
- `~/ris/debug_vizjit_dir/fa758bc3a0382817.gocpu.asm.pc_0x000010fc.asm` (_start function, 0x10fc..0x110c)
- `~/ris/bench/libriscv_guest/bench_guest.c` (source)

## Bug List

### BUG 1: Duplicate label placement — L0 placed twice (IR lines 116-117)
```
L0:
L0:
mov.i64 x14 = x12
```
Label L0 appears on two consecutive lines. L0 maps to dispatch PC 0x100c (the Fibonacci loop header). The pre-created label placement at the top of the emission loop AND the `emitLabel()` call inside `emitRVC`/`emit32` both place L0, creating a double-placement. The second `PlaceLabel` call overwrites the first with the same IR index, which is harmless in isolation, but indicates the label machinery is confused. **This is likely benign** but reveals a structural issue.

### BUG 2: Same duplicate pattern for L7, L1, L6, L2, L8, L5, L9 (all backward branch targets)
Every pre-created backward branch target label appears twice:
- L7 at lines 350-351 (0x103c, memstress outer loop)
- L1 at lines 355-356 (0x1040, memstress store loop)
- L6 at lines 436-437 (0x1050, memstress load loop)
- L2 at lines 607-608 (0x107c, sieve init loop)
- L8 at lines 648-649 (0x10b0, sieve outer body)
- L5 at lines 675-676 (0x10c0, sieve clearing loop)
- L9 at lines 705-706 (0x10de, prime counting loop)

**Same root cause as Bug 1.** Two code paths both call PlaceLabel for the same label at the same PC.

### BUG 3: `auipc` at 0x1026 emits a CONSTANT instead of PC-relative computation (IR line 338)
Guest: `0x00001026  auipc   a2, 0x1` → should compute `a2 = 0x1026 + (0x1 << 12) = 0x1026 + 0x1000 = 0x2026`
IR line 338: `const x12 = 8230`
8230 decimal = 0x2026. **Correct.** The emitter pre-computes AUIPC at emission time. Not a bug.

### BUG 4: `auipc` at 0x1064 (IR line 595)
Guest: `0x00001064  auipc   a0, 0x9` → `a0 = 0x1064 + 0x9000 = 0xA064`
IR line 595: `const x10 = 41060` = 0xA064. **Correct.**

### BUG 5: `auipc` at 0x1088 (IR line 627)
Guest: `0x00001088  auipc   a0, 0x9` → `a0 = 0x1088 + 0x9000 = 0xA088`
IR line 627: `const x10 = 41096` = 0xA088. **Correct.**

### BUG 6: Sieve outer loop at 0x10a0 — `c.j +18` at 0x109e skips over 0x10a0 (IR line 639)
Guest code:
```
0x109c  c.li    a3, 2
0x109e  c.j     +18          → jumps to 0x10b0
0x10a0  c.addi  a3, 1        ← skipped on first iteration
```
IR line 639: `jump -> L8` where L8 is at IR line 648 (0x10b0). **This is correct** — the C code initializes `i=2` and the `c.j` jumps into the middle of the loop, bypassing the `i++` on the first iteration. The IR correctly jumps to L8 (0x10b0).

### BUG 7 (CRITICAL): `ld zero, -24(s0)` at 0x102a — loading into x0 (IR line 767: L124)
Guest: `0x0000102a  fe843003  ld      zero, -24(s0)`
This loads a 64-bit value from memory into register x0 (zero register). In RISC-V, writes to x0 are discarded. This is used by the compiler as a memory fence / touch. Looking at the IR... I need to find where this is emitted. The guest disassembly shows it at 0x102a but this instruction should effectively be a NOP (load into zero register). If the emitter treats this as a real load and assigns a VReg to x0's destination, it could corrupt state. **Need to verify the emitter handles rd=0 correctly for loads.** Looking at the IR, I don't see a load for this PC — it appears to be correctly suppressed or mapped to a dummy. **Likely benign.**

### BUG 8 (CRITICAL): Sieve loop condition uses SIGNED comparison for UNSIGNED values
Guest at 0x10ca: `bltu a4, a2, 0x10c0` — Branch if Less Than Unsigned.
IR line 690: `branch.ltu x14, x12 -> L107` — uses `.ltu` (unsigned). **Correct.**

Guest at 0x10bc: `bltu a1, a4, 0x10a0` — Branch if Less Than Unsigned.  
IR line 670: `branch.ltu x11, x14 -> L101` — uses `.ltu`. **Correct.**

Guest at 0x10ac: `bgeu a5, a2, 0x10d0` — Branch if Greater or Equal Unsigned.
IR line 647: `branch.geu x15, x12 -> L3` — uses `.geu`. **Correct.**

### BUG 9 (CRITICAL): Bail labels L4 and L3 exit to Go instead of being internal jumps
IR line 799-811:
```
L4:
store.i64 [t64 + 16] = x2
... (writeback all)
ret pc=0x10a0 status=0 fault=v0
```
IR line 812-824:
```
L3:
store.i64 [t64 + 16] = x2
... (writeback all)
ret pc=0x10d0 status=0 fault=v0
```

L4 is used as the backward branch target from:
- Line 664: `jump -> L4` (from `c.beqz a4` at 0x10b6 — sieve skip when buf[i]==0)
- Line 673: `jump -> L4` (from `bltu a1, a4` at 0x10bc — skip when a1 < a4)
- Line 696: `jump -> L4` (from `c.j -46` at 0x10ce — sieve clearing done, back to outer)

L3 is used from:
- Line 647: `branch.geu x15, x12 -> L3` (sieve termination: i*i > limit)

**These are the bail labels for 0x10a0 and 0x10d0.** They should be INTERNAL jumps (to L90 and L110 respectively) but instead they're bail-label exits that writeback and return to Go. 

**Root cause:** The goto targets 0x10a0 and 0x10d0 are in `gotoTargets` but were NOT visited during emission at those exact PCs because the labels were created by forward branches (e.g., `c.j +18` at 0x109e targeting 0x10b0 — NOT 0x10a0) or by the branch instructions themselves.

Wait — 0x10a0 IS visited during sequential emission (it's between 0x1000 and 0x10fc). So `e.visited[0x10a0]` should be true. The finalize bail-label check is `if !e.visited[target]` — since visited IS true, these shouldn't be bail labels!

**But the IR dump clearly shows them as bail labels (writeback + ret, not internal code).** This means either:
1. The label L4 is NOT the label for 0x10a0 — it's a different label
2. Or `visited[0x10a0]` is somehow false

Looking more carefully: L4 at line 799 has `ret pc=0x10a0`. But the DISPATCH TABLE at line 107 shows `0x10a0→L4`. So L4 IS the dispatch label for 0x10a0. But in the main code, 0x10a0 was emitted at L90 (line 640-644):
```
L90:
shl_imm.i64 x14 = x13, 32     ; 0x10a2: slli a4, a3, 32
```
Wait, L90 is at 0x10a2, not 0x10a0! The c.addi at 0x10a0 was emitted elsewhere.

Actually, looking carefully: 0x10a0 is `c.addi a3, 1`. In the sequential walk, this gets visited. But WHICH label was placed at 0x10a0? The `collectInternalTargets` pre-created a label for 0x10a0 (it's a branch target from 0x10ce `c.j -46`). But the emitRVC handler for `c.addi` calls `e.emitLabel()` which places `e.getOrCreateLabel(e.pc)`. If the pre-created label and the emitLabel label are DIFFERENT labels... no, `getOrCreateLabel` returns the same label for the same PC.

The dispatch table has `0x10a0→L4`. But in the main code body the code at 0x10a0 doesn't have L4 placed — it's somewhere else. Let me trace: in the main IR body, I see no `L4:` before the code at 0x10a0. The `c.addi a3, 1` at 0x10a0 appears in the flow after the `c.j -46` at 0x10ce. Since `emitJAL` for the `c.j -46` emits `jump -> L4` and returns WITHOUT setting `e.terminated` (because it's internal), the sequential walk continues to 0x10d0 and beyond. 0x10a0 was ALREADY visited (in the sequential walk from 0x1000 through 0x10fc). But when was its label placed?

**The sequential walk visits 0x10a0 and emits its code. The pre-created label was placed by my new PlaceLabel code at the top of the loop. But `emitLabel()` inside the RVC handler ALSO places it.** Both should place the same label at the same position. But then the code for 0x10a0 in the main body should have L4 placed there.

Wait, looking at the IR output more carefully around where 0x10a0's code should be. The sequential walk goes: 0x109c (L88), 0x109e (L89: `jump -> L8`). After the jump at 0x109e, the emitter does NOT set `e.terminated` (internal jump). So it continues to 0x10a0. At 0x10a0, the label L4 should be placed. Then `c.addi a3, 1` should be emitted.

But in the IR output, after L89 (line 639: `jump -> L8`), the next label is L90 (line 640: `shl_imm x14 = x13, 32`), which is 0x10a2 (slli). **Where is 0x10a0's code?**

0x10a0 is `c.addi a3, 1`. It should appear between L89 and L90. But it's NOT THERE. The IR jumps from `jump -> L8` (0x109e) directly to L90 (0x10a2).

**THE BUG: The `c.j +18` at 0x109e is a 2-byte instruction. After emitting it, `advancePC(2)` sets `e.pc = 0x10a0`. But `emitJAL` for the internal forward jump returns WITHOUT following the jump (line 2481: `return`). The sequential walk continues at `e.pc = 0x10a0`. At 0x10a0, the emitter checks `e.visited[0x10a0]`. Since 0x10a0 was already visited... wait, WAS it? The sequential walk goes 0x1000, 0x1002, 0x1004, ..., 0x109c, 0x109e. After 0x109e (c.j), advancePC sets pc=0x10a0. Then the loop continues at pc=0x10a0. `visited[0x10a0]` should be FALSE (first visit). So it emits 0x10a0.**

But then looking at the IR dump, 0x10a0's code (c.addi a3, 1) should be between L89 and L90. Let me look more carefully...

Oh wait, I see it now! After L89's `jump -> L8`, the emitter's `emitJAL` function returns. The loop checks `e.terminated` — it's false (internal jump doesn't terminate). Then `e.pc = 0x10a0`. The emitter visits 0x10a0. But I DON'T see a label or instruction for 0x10a0 in the IR!

Looking at lines 639-640:
```
L89:
jump -> L8
L90:
shl_imm.i64 x14 = x13, 32
```

There should be instructions for 0x10a0 (`c.addi a3, 1`) between `jump -> L8` and `L90`. But `jump -> L8` is UNREACHABLE code from the perspective of the lowerer — the jump transfers control to L8. Code after the jump is dead code. But the emitter still emits it because it's doing a sequential walk.

Actually, the code IS there — it's just that the lowerer might place L90 right after the jump and the code for 0x10a0 is between them but unlabeled. Let me re-read... No, L90 IS labeled as `shl_imm.i64 x14 = x13, 32` which corresponds to 0x10a2 (slli a4, a3, 32). The code for 0x10a0 (`c.addi a3, 1` = `add_imm x13 = x13, 1`) should be right after `jump -> L8`.

AH! But `emitJAL` calls `return` at line 2481, which returns from `emitJAL`. Then in the main emission loop, `advancePC` was NOT called by `emitJAL` for the jump case — looking at `emitJAL`:
```go
if rd == 0 {
    ...
    if internal {
        if backward {
            e.irEm.BudgetCheck(targetLabel, target)
        } else {
            e.irEm.Jump(targetLabel)
        }
        e.gotoTargets.add(target)
        return  ← returns WITHOUT calling advancePC
    }
```

But `advancePC` WAS called at line 2464 (before the `if rd == 0` block):
```go
e.advancePC(insnSize)
```

So `e.pc` was already advanced past the c.j instruction. Good. Then the loop continues at 0x10a0.

But wait, `advancePC` increments `e.pc` by `insnSize`. For `c.j` (a 2-byte RVC), `insnSize = 2`. So `e.pc = 0x109e + 2 = 0x10a0`. Good.

So the sequential walk DOES visit 0x10a0. The pre-created label for 0x10a0 gets placed. Then `c.addi a3, 1` is emitted. But in the IR output, I see no `add_imm x13 = x13, 1` between `jump -> L8` and `L90: shl_imm x14 = x13, 32`.

Unless... the code for 0x10a0 IS there but unlabeled. `L90` is the label for 0x10a2 placed by `emitLabel()` inside the RVC handler. The code for 0x10a0 might be between `jump -> L8` and `L90` but without a visible label in the dump.

Looking again at lines 638-641:
```
L89:
jump -> L8
L90:
shl_imm.i64 x14 = x13, 32
```

There's NOTHING between `jump -> L8` and `L90`. The `add_imm x13 = x13, 1` for 0x10a0 is MISSING.

**THE CODE FOR 0x10a0 IS NOT EMITTED.** The `c.addi a3, 1` at 0x10a0 is silently dropped.

**WHY?** Because `emitJAL` for the `c.j +18` at 0x109e returns from the function. Then back in the main emission loop, the next iteration starts. It checks `e.visited[e.pc]`. `e.pc = 0x10a0`. Since the pre-created labels triggered `PlaceLabel` for 0x10a0, and `emitLabel()` was called (which also calls `PlaceLabel`), the visited check at the TOP of the loop runs first:

```go
if e.visited[e.pc] {
    e.irEm.Jump(e.getOrCreateLabel(e.pc))
    e.gotoTargets.add(e.pc)
    e.terminated = true
    break
}
e.visited[e.pc] = true
```

At the top of the loop, `e.visited[0x10a0]` is checked. This is set to `true` DURING this very iteration (line `e.visited[e.pc] = true`). But the PRE-CREATED label placement code runs AFTER the visited check:

```go
e.visited[e.pc] = true
// Place label for this PC if one was pre-created (branch target).
if lab, ok := e.pcLabels.get(e.pc); ok {
    e.irEm.PlaceLabel(lab)
}
```

So `visited[0x10a0]` is false on first visit, the label is placed, and the instruction is emitted. That should work.

BUT WAIT — maybe the problem is that `emitRVC` for the `c.addi a3, 1` calls `emitLabel()` which calls `e.getOrCreateLabel(e.pc)` + `PlaceLabel`. And `getOrCreateLabel(0x10a0)` already has a label (pre-created). So `emitLabel()` places the pre-created label AGAIN (double placement, same index). And then the code for c.addi is emitted.

But in the IR dump, the code for 0x10a0 really does seem missing. Let me reconsider.

Actually, I think I was wrong. Let me re-trace the emission more carefully. The `c.j +18` at 0x109e has opcode c.j which calls `emitJAL(0, offset, 2)`. Inside emitJAL:
1. `target = e.pc + 18 = 0x109e + 18 = 0x10b0`
2. `rd = 0`, so we enter the `if rd == 0` block
3. `e.advancePC(2)` → `e.pc = 0x10a0`
4. `internal = e.visited[0x10b0] || (0x10b0 >= 0x1000 && 0x10b0 < 0x10fc)` → true
5. `backward = 0x10b0 < 0x10a0 || e.visited[0x10b0]` → false (0x10b0 > 0x10a0 and not yet visited)
6. `e.irEm.Jump(targetLabel)` → emits `jump -> L8` (L8 is the label for 0x10b0)
7. `e.gotoTargets.add(0x10b0)`
8. `return` — returns from emitJAL

Back in the emission loop, the RVC handler finishes. The loop continues: `e.pc = 0x10a0`. The loop checks `e.visited[0x10a0]` = false. Sets `visited[0x10a0] = true`. Places pre-created label for 0x10a0. Then calls `emitRVC(insn_at_0x10a0)`.

`emitRVC` for `c.addi a3, 1`:
1. Calls `e.emitLabel()` → places label for e.pc (0x10a0) — this is the SAME label as the pre-created one
2. Recognizes c.addi, emits `add_imm x13 = x13, 1`
3. Calls `e.advancePC(2)` → `e.pc = 0x10a2`

So the code for 0x10a0 IS emitted: `add_imm x13 = x13, 1`. But in the IR dump, this code appears AFTER `jump -> L8` (the c.j from 0x109e). Since `jump -> L8` is an unconditional jump, the `add_imm x13 = x13, 1` is dead code (unreachable by fall-through). It's only reachable via the label L4 (the pre-created label for 0x10a0).

So the code IS there — it's just between `jump -> L8` and `L90:`. And the label for 0x10a0 (L4) is placed right before it. But in the IR dump, the label appears as just `L4:` which I might have missed. Wait, let me re-read lines 638-641... I only see `L90:` after the jump. Maybe the `add_imm x13` and L4 placement ARE there but I missed them in my reading.

Actually, re-reading the IR: after `jump -> L8` at line 639, the next line is `L90:` at line 640. But L90 is 0x10a2. The code for 0x10a0 (L4 + add_imm x13 = x13, 1) should be between lines 639 and 640. But there's NOTHING between them in the dump!

OH! The `emitLabel()` in `emitRVC` calls `PlaceLabel` which sets `Block.Labels[label] = currentInstrIndex`. But the `add_imm` emitted by c.addi DOESN'T have a label prefix in the String() output. The PlaceLabel just updates the index — it doesn't add a visible marker in the IR dump unless the vizjit dump code explicitly checks for placed labels.

Let me look at how the vizjit dump prints labels. It iterates through `block.Instrs` and prints each instruction. Labels are printed separately. If the vizjit code only prints labels that appear as `IRLabel` instructions... but PlaceLabel doesn't insert an instruction — it just records the index.

So the code for 0x10a0 IS emitted as an instruction, but the label placement is invisible in the dump. And the `add_imm x13 = x13, 1` instruction IS in the Instrs slice but might not have a label marker printed before it.

BUT — looking at the dump, L90 DOES appear as a visible label. Why? Because `emitLabel()` calls `e.irEm.PlaceLabel(label)` which uses `e.emit(IRInstr{Op: IRLabel, ...})`. Wait, does PlaceLabel emit an instruction? Let me check...

Actually, looking at the IR, labels like `L0:`, `L1:`, etc. DO appear as separate lines. This means PlaceLabel does emit something visible. So if 0x10a0's label and code were emitted, they should appear in the dump.

The fact that they DON'T appear means the code for 0x10a0 is truly missing from the IR.

Hmm, but why? I traced through the logic and it should be emitted...

Actually, wait. Let me re-read `emitJAL` more carefully. Line 2464:
```go
e.advancePC(insnSize)
```

`advancePC` does `e.numInsns++; e.pc += size`. But it also called `emitIC()` which is now a no-op. But the key question: is `emitJAL` called from `emitRVC` or from `emit32`? For `c.j`, it's RVC, so called from `emitRVC`.

In `emitRVC`, the `c.j` case calls `e.emitJAL(0, offset, 2)`. Inside emitJAL, `advancePC(2)` is called, setting `e.pc = 0x10a0`. Then the function returns. Back in `emitRVC`, does `emitRVC` also call `advancePC`? If so, `e.pc` would be advanced AGAIN!

Let me check: in `emitRVC`, after the switch cases, does it call `advancePC`? Looking at the emitter pattern: usually the individual emit functions call `advancePC` internally. `emitJAL` calls `advancePC(insnSize)` at line 2464. So `emitRVC` should NOT call `advancePC` again.

But looking at the emission loop:
```go
if half&0x3 != 0x3 {
    e.emitRVC(uint16(half))
} else {
    ...
    e.emit32(insn)
}
```

Neither emitRVC nor emit32 is followed by an explicit advancePC in the loop — the advance is done inside the handlers. So `e.pc` is correct at 0x10a0 after emitJAL returns.

I think the real issue might be that `emitRVC`'s `c.j` handler sets `e.terminated = true` after emitJAL returns. Let me check the `emitRVC` code for c.j.

WAIT — I should look at the actual `emitRVC` code, not just reason about it. But I'm in plan mode and should be auditing the dumps, not reading code.

Let me move on and document this as a suspected bug.

### BUG 10 (CRITICAL): The `c.j +18` at 0x109e — emitter may set `e.terminated = true`
Looking at the IR, after `jump -> L8` (the c.j at 0x109e), the next code is L90 at 0x10a2, NOT L4 at 0x10a0. This suggests the code for 0x10a0 was never emitted. The `c.addi a3, 1` (the outer loop increment) is MISSING from the IR.

If the emitter sets `e.terminated = true` after emitting the c.j, the loop exits, and 0x10a0 onwards is not emitted. But `emitJAL` for internal jumps does NOT set terminated (line 2481: just `return`).

**However**, looking at the diff from earlier: the `emitJAL` code was CHANGED. The new code says "Do NOT follow the jump... Continue the sequential walk so all PCs in the function get labels placed." This means the emitter should NOT terminate. But if there's a bug where `emitRVC` or the calling code sets terminated...

This needs code inspection to confirm.

### BUG 11: The `auipc` at 0x1026 computes `a2 = 0x2026` but this value is used as a BUFFER POINTER
Guest: `auipc a2, 0x1` at 0x1026, then `addi a7, a2, -38` at 0x102e → `a7 = 0x2026 - 38 = 0x2000`

The address 0x2000 points to the membuf[] static buffer. The AUIPC uses the current PC to compute a PC-relative address to static data. The emitter pre-computes this as a constant (`const x12 = 8230` = 0x2026). **This is correct** — AUIPC is always PC-relative and the emitter knows the PC at emission time.

### BUG 12: `ld zero, -24(s0)` at 0x102a and `ld zero, -32(s0)` at 0x1068 — loads into x0
These are compiler-generated memory fences (loading into x0 to force the memory access without storing the result). They should be handled as loads that discard the result. If the emitter emits a real load into a VReg mapped to x0, the VReg might be allocated to a real register and overwrite something. **Need to verify the emitter handles rd=0 loads correctly.**

### BUG 13: `lw zero, -36(s0)` at 0x10ee — 32-bit load into x0
Same issue as Bug 12 but for a 32-bit load. The compiler uses this as a "touch" to force the store at 0x10ea to be visible.

### SUMMARY OF CRITICAL BUGS

1. **BUG 9 / BUG 10**: The outer sieve loop increment (`c.addi a3, 1` at 0x10a0) appears to be MISSING from the emitted IR. L4 (the dispatch label for 0x10a0) is a bail-label exit stub, not the actual code. This means when the sieve clearing loop finishes and jumps to L4 (0x10a0), instead of incrementing a3 and continuing the sieve, it exits to Go. Since there are no budget checks, the code should never exit to Go during normal operation — meaning the sieve loop never properly increments a3, and if the bail label somehow loops back, a3 stays at 2 forever → infinite loop.

2. **Root cause**: The emitter's sequential walk after `c.j +18` at 0x109e should continue to 0x10a0 and emit the `c.addi a3, 1`. But the code for 0x10a0 does not appear in the IR. Either `emitRVC`'s c.j handler sets `e.terminated = true`, or something else prevents 0x10a0 from being emitted. The `emitLabel()` inside emitRVC and the pre-created label placement may be interacting badly.

3. **Fix direction**: Check `emitRVC`'s handling of `c.j` — specifically whether it sets `e.terminated = true` after calling `emitJAL`. For function-level blocks, internal jumps should NOT terminate the sequential walk. Also verify that the code at 0x10a0 is actually emitted into the IR Instrs slice (it may be emitted but unlabeled/invisible in the dump).
