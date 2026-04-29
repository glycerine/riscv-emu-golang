# Self-Describing Foreign Frame Protocol

## Specification v1

---

## 1. Overview

This document specifies a protocol that allows
JIT-compiled ("foreign") code to execute on a Go
goroutine stack with full garbage collection
correctness.

Go pointers may flow freely between Go and
foreign frames in all directions: Go to foreign,
foreign to Go via callbacks, and Go callback
returns back to foreign frames.

The protocol works by embedding an immutable
pointer bitmap directly in each foreign stack
frame. When the Go garbage collector encounters
a frame whose PC is not in any registered
moduledata, it reads the frame's self-description
from the stack, traces Go pointers according to
the bitmap, and advances to the next frame.

No external metadata registration is required.
No moduledata synthesis. No go:linkname hacks.
No runtime data structures to maintain. The
frame describes itself.

### 1.1 Prerequisites

Two companion changes to the Go runtime are
required.

**Prerequisite 1: runtime.LockOSThreadForeign(stackSize)**

Locks the current goroutine to its OS thread
and provisions a fixed-size stack (e.g. 1MB).
The stack will never be copied for growth. Any
stack growth attempt panics. This eliminates
the stack-copying hazard: Go normally grows
goroutine stacks by copying them, rewriting
internal pointers during the copy. Foreign
frames may contain pointers into the stack that
the runtime cannot find or rewrite. A fixed,
non-copyable stack makes this impossible.

This call also sets `g.foreignStack = true` on
the goroutine's g struct, enabling the stack
scanner to recognize self-describing frames on
this goroutine's stack (see Prerequisite 2).

**Prerequisite 2: Modified stack scanner**

When the stack scanner encounters a PC not in
any moduledata, it first checks whether
`g.foreignStack` is true.

If false: fatal immediately with
`throw("unknown caller pc")`. Identical to
today's behavior. Normal goroutines pay zero
cost for this feature's existence.

If true: check for the self-describing frame
protocol at RSP+8. If the magic+version
signature is present, trace the frame per the
embedded bitmap and advance to the next frame.
If absent, fatal as before — this is a genuine
bug, not a valid foreign frame.

### 1.2 Design Principles

**Immutable bitmaps.** The pointer bitmap is
written once in the function prologue and never
modified. This eliminates all race conditions
between the GC and the executing foreign code.

**Compile-time complete.** Every aspect of the
frame description is known at JIT compilation
time. The bitmap is the union of all pointer
slots that will ever hold a Go pointer during
the function's lifetime. Slots that will hold
pointers in the future are zero-initialized in
the prologue.

**Self-contained.** The frame description lives
on the stack within the frame itself. No
external data, no registration, no
deregistration. Born when the frame is entered,
dies when it returns.

**No register maps needed.** The GC only
observes foreign frames when the goroutine is
stopped at a safepoint — when foreign code has
called into Go and is waiting for the callback
to return. At that moment the foreign frame is
suspended: it is not executing, and the CPU
registers belong to the Go callee, not the
foreign frame. The foreign frame's only
surviving state is what it wrote to the stack
before the call. Any Go pointer the foreign
code needs to survive the call must already be
on the stack. This is not ABI-specific — it is
a logical consequence of what "calling a
function" means on any architecture. If a value
exists only in a register and the code executes
a call, the callee may overwrite that register.
The value is gone. Every calling convention on
every architecture shares this property.
Register contents are therefore never part of
this protocol. Only stack slots matter.

---

## 2. Stack Frame Layout

All offsets are relative to RSP after the
function prologue completes (after SUB RSP).

```
Offset             Contents
────────────────────────────────────────
RSP+0              Return address
                   (pushed by CALL)

RSP+8              Magic+version word
                   (Section 3)

RSP+16             Header word
                   (Section 4)

RSP+24             If numTrackedSlots > 32:
                     bitmap_word[0..B-1]
                     B = ceil(numTrk/64)
                   If numTrackedSlots <= 32:
                     (not present)

                   Tracked region:
                     numTrackedSlots × 8 bytes
                     Each slot has one bitmap
                     bit. 1 = live Go pointer.
                     0 = not a Go pointer.

                   Untracked region:
                     Remaining space to frame
                     end. GC ignores entirely.

RSP+frameSize16×16 Next frame's return address
────────────────────────────────────────
```

The frame size (frameSize16 × 16 bytes) includes
everything from RSP+0 through the last byte
before the next frame. Minimum frameSize16 = 2
(32 bytes).

---

## 3. Magic+Version Word (RSP+8)

### 3.1 Layout

```
RSP+8, 8 bytes, little-endian uint64:

Bits [63:16]  Sentinel (48 bits)
              Fixed: 0xFFFFFFFFFFF1

Bits [15:0]   Protocol version (16 bits)
              0x0001 for this specification

Complete value: 0xFFFFFFFFFFF10001
```

### 3.2 Construction

```
magic = (0xFFFFFFFFFFF1 << 16) | version
```

For v1: `magic = 0xFFFFFFFFFFF10001`

### 3.3 Validation by the GC

```go
const magicMask   = 0xFFFFFFFFFFFF0000
const magicExpect = 0xFFFFFFFFFFF10000
const versionMask = 0x000000000000FFFF

raw := *(*uint64)(unsafe.Pointer(sp + 8))
if raw & magicMask != magicExpect {
    throw("unknown caller pc")
}
version := raw & versionMask
if version != 1 {
    throw("unsupported foreign frame version")
}
```

### 3.4 Safety Analysis

The magic sentinel must not be misidentified in
stack data that is not a self-describing frame.
This section analyzes the false positive risk by
examining each layer of protection.

**Layer 1: g.foreignStack flag.**

The sentinel check is never performed unless
`g.foreignStack` is true. This flag is only set
by `runtime.LockOSThreadForeign`. On all normal
goroutines the flag is false and the foreign
frame path is unreachable. An unknown PC on a
normal goroutine fatals immediately without
reading RSP+8.

This eliminates all false positives on normal
goroutines.

**Layer 2: findfunc(pc) must return nil.**

Even on a foreign-capable goroutine, the
sentinel check only runs when `findfunc(pc)`
returns nil — the PC is not in any registered
moduledata.

Frames on a foreign-capable goroutine are:

- Go frames: in moduledata, findfunc succeeds.
  Sentinel check never runs.

- Go-to-foreign trampoline: a Go assembly
  function, in moduledata, findfunc succeeds.
  Sentinel check never runs.

- Foreign frames: JIT-emitted, not in any
  moduledata. findfunc returns nil. Sentinel
  check runs.

The sentinel check runs exclusively on frames
emitted by the JIT compiler.

**Layer 3: JIT prologue completes before any
safepoint.**

Every foreign frame was emitted by the JIT
compiler. The JIT writes the magic+version and
header in the prologue, before any callback
into Go. The GC can only observe the frame at
a safepoint (during a callback). Therefore the
GC never sees the frame before the prologue has
written correct data.

The edge case of async preemption (SIGURG)
interrupting the prologue is eliminated by the
requirement that async preemption is deferred
on foreign-capable goroutines while executing
foreign code (Section 10.2, invariant 4).

**Conclusion.**

The sentinel does not need to be unforgeable
against all possible stack contents. It
operates in a context that is triply gated:

1. Only on goroutines that called
   LockOSThreadForeign (explicit opt-in)
2. Only on frames whose PC is not in any
   moduledata (JIT-emitted code only)
3. Only at safepoints (after the prologue
   has written correct data)

Within this context, a false positive requires
a JIT compiler bug. The sentinel is a final
sanity check, not a security boundary. The true
protection comes from the structural invariants
of layers 1-3.

**Version field.** The lower 16 bits allow up
to 65534 future protocol versions (version 0
is reserved/invalid) while maintaining the same
sentinel for discovery.

---

## 4. Header Word (RSP+16)

The header describes the frame layout. Together
with the magic+version at RSP+8, it forms the
complete self-description:

```
RSP+0:   return address
RSP+8:   magic+version (8 bytes)
RSP+16:  frameSize16(15)
         | extensionBit(1)
         | numTrackedSlots(16)
         | inlineBitmap(32)
RSP+24:  bitmap_word[0] (slots 0-63)
RSP+32:  bitmap_word[1] (slots 64-127)
...
         [tracked slots]
         [untracked space]
```

When numTrackedSlots <= 32, the bitmap fits
entirely in the inlineBitmap field. No bitmap
words appear on the stack. The tracked region
begins at RSP+24.

When numTrackedSlots > 32, inlineBitmap must be
zero and bitmap words follow at RSP+24.

### 4.1 Layout

```
RSP+16, 8 bytes, little-endian uint64:

Bits [0:14]  frameSize16       (15 bits)
  Total frame size in 16-byte units.
  Actual bytes = frameSize16 × 16.
  Max: 32767 × 16 = 524,272 bytes.
  Min: 2 (= 32 bytes). 0 and 1 invalid.

Bit  [15]    extensionBit      (1 bit)
  Always 0 in this protocol. If 1, a
  different protocol applies. A v1
  reader must fatal.

Bits [16:31] numTrackedSlots   (16 bits)
  Count of 8-byte slots in the tracked
  region. Each gets one bit in the
  bitmap. Max: 65535.

Bits [32:63] inlineBitmap      (32 bits)
  If numTrackedSlots <= 32:
    The pointer bitmap itself.
    Bit 32 = tracked_slot[0].
    Bit 33 = tracked_slot[1].
    ...
    Bit 63 = tracked_slot[31].
    Bits beyond numTrackedSlots:
    ignored.
    No bitmap words on the stack.
  If numTrackedSlots > 32:
    Must be zero.
    Bitmap words follow at RSP+24.
    Count = ceil(numTrackedSlots / 64).
```

### 4.2 Bit Layout Diagram

```
 63             32 31      16 15 14         0
┌─────────────────┬─────────┬──┬────────────┐
│ inlineBitmap    │numTrk   │ex│frameSize16 │
│ [31:0]          │Slots    │Bt│[14:0]      │
└─────────────────┴─────────┴──┴────────────┘
```

### 4.3 Extension Bit

One extension bit: bit 15.

Always 0 in this protocol. If set, a v1
reader must fatal without reading further.
A future protocol version may use this bit
to signal a different encoding.

The GC checks before reading other fields:

```go
if header & (1<<15) != 0 {
    fatal("unsupported foreign frame")
}
```

### 4.4 Inline vs. External Bitmap

The decision is derived from numTrackedSlots.
No flag bit is needed.

- numTrackedSlots <= 32:
  Bitmap is in bits [32:63] of the header.
  No bitmap words on the stack.
  Tracked region begins at RSP+24.

- numTrackedSlots > 32:
  inlineBitmap must be zero.
  Emit ceil(numTrackedSlots / 64) bitmap
  words starting at RSP+24.
  Tracked region begins at
  RSP + 24 + 8×ceil(numTrackedSlots/64).

### 4.5 Frame Size Computation

```
header_bytes  = 24  (ret addr+magic+hdr)
if numTrackedSlots <= 32:
    bitmap_bytes = 0
else:
    B = ceil(numTrackedSlots / 64)
    bitmap_bytes = 8 × B
tracked_bytes   = 8 × numTrackedSlots
untracked_bytes = (JIT decides)

total = header_bytes
      + bitmap_bytes
      + tracked_bytes
      + untracked_bytes
total = align_up(total, 16)
frameSize16 = total / 16
```

frameSize16 is a compile-time constant.

---

## 5. Tracked Region

### 5.1 Location

When numTrackedSlots <= 32 (inline bitmap):
```
tracked_slot[i] is at RSP + 24 + 8×i
```

When numTrackedSlots > 32 (external bitmap):
```
B = ceil(numTrackedSlots / 64)
tracked_slot[i] is at RSP + 24 + 8×B + 8×i
```

### 5.2 Bitmap Interpretation

When numTrackedSlots <= 32 (inline):
```
Bit (32 + i) of the header corresponds
to tracked_slot[i]
for i in 0..numTrackedSlots-1
```

When numTrackedSlots > 32 (external):
```
Bit (i % 64) of bitmap_word[i / 64]
corresponds to tracked_slot[i]
for i in 0..numTrackedSlots-1
```

In both cases: bit=1 means the slot is
treated as a live Go heap pointer. The GC
traces it. Bit=0 means the GC ignores it.

### 5.3 Maximal Bitmap Rule

The bitmap is the union of all pointer slots
that will ever hold a Go pointer at any point
during the function's execution. Computed at
JIT compilation time:

```
For each tracked slot i:
  bitmap[i] = 1  if slot i will EVER hold
                  a Go pointer
  bitmap[i] = 0  if slot i will NEVER hold
                  a Go pointer
```

The JIT compiler knows every callback site,
which callbacks return Go pointers, and which
slot each return value will be spilled to.

### 5.4 Zero-Initialization Requirement

**Critical correctness requirement.** Every
tracked slot whose bitmap bit is 1 MUST be
zero-initialized in the prologue, before any
callback into Go.

Rationale: the bitmap claims these slots
contain Go pointers from frame creation. If a
slot contains uninitialized garbage and the GC
traces it, the garbage might look like a heap
address. Zeroing makes it nil, which the GC
handles correctly.

Slots with known valid pointers from entry
(e.g. the context pointer) may be initialized
with their actual value instead of zero.

```asm
; Prologue: 3 tracked slots, bitmap=0b101
; Slot 0: context pointer (from RDI)
; Slot 1: not a pointer (bit = 0)
; Slot 2: future callback return pointer
    sub   rsp, FRAME_SIZE
    mov   qword [rsp+8],  MAGIC_VERSION
    mov   qword [rsp+16], HEADER
    mov   [rsp+24], rdi      ; slot 0: live
    xor   eax, eax
    mov   [rsp+40], rax      ; slot 2: zero
```

### 5.5 Slot Lifecycle

```
1. PROLOGUE: slot = 0 (or valid ptr)
   bitmap bit = 1
   GC sees: nil or valid ptr. Correct.

2. PRE-CALL: slot = 0 (not yet populated)
   bitmap bit = 1
   GC sees: nil. Correct.

3. CALL: foreign calls into Go. Callback
   allocates, returns ptr in register.
   Foreign frame is suspended. GC cannot
   observe the register.

4. POST-CALL: foreign resumes, spills
   register to tracked slot.
   slot = valid Go pointer
   bitmap bit = 1
   GC sees: valid pointer. Correct.

5. DONE: foreign finished with pointer.
   Optionally zero the slot.
   bitmap bit = 1 (never changes)
   GC sees: nil or valid ptr. Correct.

6. EPILOGUE: frame destroyed (ADD RSP).
   Bitmap ceases to exist.
```

At no point does the GC observe a
pointer-marked slot containing uninit garbage.

### 5.6 Clearing Dead Pointer Slots

When foreign code overwrites a pointer slot
with a non-pointer, the bitmap still marks it.
The GC traces the value.

Two outcomes:
- Value outside Go heap range. GC's inheap()
  check rejects it. No harm.
- Value looks like a heap address. GC pins
  that object alive. Minor transient
  retention, not a correctness violation.

To avoid this, zero the slot when done:

```asm
    xor   eax, eax
    mov   [rsp+SLOT], rax  ; clear dead ptr
```

Recommended but not required for correctness.

---

## 6. Untracked Region

### 6.1 Location

```
untracked_start = RSP + 24
                + bitmap_bytes
                + 8 × numTrackedSlots
untracked_end   = RSP + frameSize16 × 16
```

### 6.2 GC Behavior

The GC does not read, trace, or interpret any
data in the untracked region.

### 6.3 Permitted Uses

The JIT may use the untracked region for:
- Guest register spills (integers)
- Scratch space
- Temporary buffers
- Saved callee-saved registers
- Saved g pointer
  (R14 on amd64, R28 on arm64)
- Any data that is NOT a Go heap pointer

**Critical rule.** The untracked region MUST
NOT contain live Go heap pointers. Any Go
pointer must reside in a tracked slot with
its bitmap bit set to 1.

---

## 7. GC Scanner Integration

### 7.1 Complete Scanner Logic

```go
func scanForeignFrame(
    sp uintptr,
) (nextSP uintptr, ok bool) {

    // ── Step 1: Validate magic+version ──

    magic := *(*uint64)(
        unsafe.Pointer(sp + 8))
    if magic & 0xFFFFFFFFFFFF0000 !=
       0xFFFFFFFFFFF10000 {
        return 0, false
    }
    if uint16(magic) != 1 {
        return 0, false
    }

    // ── Step 2: Read and validate header ──

    header := *(*uint64)(
        unsafe.Pointer(sp + 16))

    if header & (1<<15) != 0 {
        return 0, false  // extension bit
    }

    frameSize16 := header & 0x7FFF
    numTS := uint16(header >> 16)
    inBmp := uint32(header >> 32)

    if frameSize16 < 2 {
        return 0, false
    }

    frameSzBytes := uintptr(frameSize16) * 16

    // ── Step 3: Trace pointers ──

    var trackedBase uintptr

    if numTS <= 32 {
        // Inline bitmap
        trackedBase = sp + 24
        for i := uint16(0); i < numTS; i++ {
            if inBmp & (1 << i) != 0 {
                ptr := *(*uintptr)(
                    unsafe.Pointer(
                        trackedBase +
                        uintptr(i)*8))
                if ptr != 0 {
                    scanPointer(ptr)
                }
            }
        }
    } else {
        // External bitmap
        B := (uint64(numTS) + 63) / 64
        trackedBase = sp + 24 + uintptr(B*8)
        for i := uint16(0); i < numTS; i++ {
            wIdx := uint64(i) / 64
            bIdx := uint64(i) % 64
            word := *(*uint64)(
                unsafe.Pointer(
                    sp + 24 +
                    uintptr(wIdx*8)))
            if word & (1<<bIdx) != 0 {
                ptr := *(*uintptr)(
                    unsafe.Pointer(
                        trackedBase +
                        uintptr(i)*8))
                if ptr != 0 {
                    scanPointer(ptr)
                }
            }
        }
    }

    // ── Step 4: Advance ──

    return sp + frameSzBytes, true
}
```

### 7.2 Integration Point

```go
// In the stack walker:
if f := findfunc(pc); f == nil {
    if gp.foreignStack {
        next, ok := scanForeignFrame(sp)
        if ok {
            sp = next
            pc = *(*uintptr)(
                unsafe.Pointer(sp))
            continue
        }
    }
    throw("unknown caller pc")
}
```

### 7.3 Panic/Recover

Foreign frames do not participate in
defer/recover. A panic propagating through a
foreign frame skips it using frameSize16 × 16.

### 7.4 Stack Traces

runtime.Callers and runtime.Stack should emit:
```
<foreign frame at 0x7f1234560080>
```
The function name is unknown. The PC is useful
for debugging.

---

## 8. JIT Compiler Requirements

### 8.1 At Compilation Time

For each foreign function, the JIT must:

**Step 1: Design the frame layout.**

Decide which slots are tracked (may hold Go
pointers) and which are untracked (never hold
Go pointers).

Typical RISC-V translator layout:

```
Tracked:
  slot 0: context pointer (Go heap)
  slot 1: callback return A (Go heap)
  slot 2: callback return B (Go heap)

Untracked:
  saved g pointer
  saved callee-saved registers
  RISC-V x0..x31 spill area
  scratch space
```

**Step 2: Compute the maximal bitmap.**

For each tracked slot, set bit=1 if it will
ever hold a Go pointer.

**Step 3: Compute frameSize16.**

```
header_bytes  = 24
if numTrackedSlots <= 32:
    bitmap_bytes = 0
else:
    B = ceil(numTrackedSlots / 64)
    bitmap_bytes = 8 × B
tracked_bytes   = 8 × numTrackedSlots
untracked_bytes = (as needed)

total = header_bytes
      + bitmap_bytes
      + tracked_bytes
      + untracked_bytes
total = align_up(total, 16)
frameSize16 = total / 16
```

**Step 4: Assemble the header constant.**

```
header = 0
header |= uint64(frameSize16)
// bit 15 = 0 (extensionBit)
header |= uint64(numTrackedSlots) << 16
if numTrackedSlots <= 32:
    header |= uint64(bitmap) << 32
// else bits [32:63] = 0
```

The header is a single compile-time uint64.

### 8.2 Emitted Prologue

```asm
foreign_function:
    sub   rsp, FRAME_SIZE
    mov   qword [rsp+8],  MAGIC
    mov   qword [rsp+16], HEADER

    ; External bitmap (numTrackedSlots > 32):
    mov   qword [rsp+24], BITMAP_WORD_0
    ; mov qword [rsp+32], BITMAP_WORD_1

    ; Zero-init future pointer slots:
    xor   eax, eax
    mov   [rsp+TRACKED+8*i], rax

    ; Init known pointer slots:
    mov   [rsp+TRACKED+0], rdi    ; ctx ptr

    ; Save callee-saved (untracked):
    mov   [rsp+UNTRACKED+0],  r14 ; g ptr
    mov   [rsp+UNTRACKED+8],  rbx
    mov   [rsp+UNTRACKED+16], rbp
```

### 8.3 Emitted Epilogue

```asm
    mov   r14, [rsp+UNTRACKED+0]
    mov   rbx, [rsp+UNTRACKED+8]
    mov   rbp, [rsp+UNTRACKED+16]
    add   rsp, FRAME_SIZE
    ret
```

### 8.4 Emitted Callback Sequence

```asm
    ; Spill live Go ptrs to tracked slots
    mov   [rsp+TRACKED+8*K], r_ptr

    ; Restore g for Go callee
    mov   r14, [rsp+UNTRACKED+0]

    ; Call into Go
    mov   rdi, ARG1
    call  go_callback

    ; Spill returned Go pointer
    mov   [rsp+TRACKED+8*J], rax

    ; Re-save g
    mov   [rsp+UNTRACKED+0], r14
```

The tracked slot was already bitmap-marked and
zero-initialized in the prologue. The bitmap
was correct before the call. No race.

### 8.5 g Pointer Protocol

The g pointer (R14 on amd64, R28 on arm64)
is the Go runtime's goroutine pointer. Foreign
code may clobber it freely.

1. On entry: save g to untracked region.
2. Before callback: restore g from save area.
3. After callback: re-save g.
4. On exit: restore g before RET.

g is saved in the untracked region. It is not
a Go heap pointer — it points to the runtime's
g struct, which the GC handles separately.

---

## 9. Worked Example

### 9.1 Scenario

A RISC-V JIT function that:
- Receives a context pointer in RDI (Go heap)
- Calls one Go callback returning a Go pointer
- Uses 5 RISC-V register spill slots
- Saves R14, RBX, RBP

### 9.2 Frame Design

```
Tracked (numTrackedSlots = 2):
  slot 0: context ptr       bit 0 = 1
  slot 1: callback return   bit 1 = 1

Untracked:
  saved R14          8 bytes
  saved RBX          8 bytes
  saved RBP          8 bytes
  RISC-V x10..x14   40 bytes
  Total:             64 bytes
```

### 9.3 Layout Computation

```
numTrackedSlots = 2 → inline (2 <= 32)

header_bytes    = 24
bitmap_bytes    = 0
tracked_bytes   = 16  (2 × 8)
untracked_bytes = 64
total           = 104
aligned         = 112 (next multiple of 16)
frameSize16     = 7
```

### 9.4 Header Computation

```
bitmap: bit 0 = 1, bit 1 = 1 → 0x3

header = 0
header |= 7              // frameSize16
// bit 15 = 0
header |= 2 << 16        // numTrackedSlots
header |= 0x3 << 32      // inlineBitmap

header = 0x0000000300020007
```

### 9.5 Stack Layout

```
RSP+0:   return address
RSP+8:   0xFFFFFFFFFFF10001  magic+version
RSP+16:  0x0000000300020007  header
RSP+24:  tracked[0]  context ptr
RSP+32:  tracked[1]  0 → callback ptr later
RSP+40:  saved R14   (untracked)
RSP+48:  saved RBX   (untracked)
RSP+56:  saved RBP   (untracked)
RSP+64:  RISC-V x10  (untracked)
RSP+72:  RISC-V x11  (untracked)
RSP+80:  RISC-V x12  (untracked)
RSP+88:  RISC-V x13  (untracked)
RSP+96:  RISC-V x14  (untracked)
RSP+104: padding     (8 bytes)
RSP+112: [next frame]
```

### 9.6 Emitted Code

```asm
jit_block:
    sub   rsp, 112
    mov   qword [rsp+8],  0xFFFFFFFFFFF10001
    mov   qword [rsp+16], 0x0000000300020007
    mov   [rsp+24], rdi        ; slot 0: ctx
    xor   eax, eax
    mov   [rsp+32], rax        ; slot 1: zero
    mov   [rsp+40], r14        ; save g
    mov   [rsp+48], rbx
    mov   [rsp+56], rbp

    ; ... translated RISC-V code ...
    ; ... use rsp+64..103 for spills ...

    ; Callback
    mov   r14, [rsp+40]        ; restore g
    mov   rdi, [rsp+24]        ; arg: ctx
    call  go_allocate_something
    mov   [rsp+32], rax        ; slot 1
    mov   [rsp+40], r14        ; re-save g

    ; ... continue with Go ptr in slot 1 ...

    ; Epilogue
    mov   r14, [rsp+40]
    mov   rbx, [rsp+48]
    mov   rbp, [rsp+56]
    add   rsp, 112
    ret
```

### 9.7 GC Trace Walkthrough

GC fires during `call go_allocate_something`.
Goroutine stopped inside Go code. Stack walker
reaches the foreign frame.

```
RSP+0:   return addr (Go caller)
RSP+8:   0xFFFFFFFFFFF10001
RSP+16:  0x0000000300020007
RSP+24:  0x00c000123456  (context ptr)
RSP+32:  0x0000000000000000  (zero)
RSP+40:  0x00c0000001a0  (R14, untracked)
...
RSP+112: next frame's return addr
```

Scanner steps:

1. findfunc(pc) returns nil.
   g.foreignStack is true. Proceed.

2. Read RSP+8: 0xFFFFFFFFFFF10001.
   Upper 48 bits match. Version = 1. OK.

3. Read RSP+16: 0x0000000300020007.
   extensionBit (bit 15) = 0. OK.
   frameSize16 = 7.
   numTrackedSlots = 2.
   inlineBitmap = 0x3 (bits 0,1 set).

4. numTrackedSlots <= 32 → inline mode.
   trackedBase = RSP+24.

5. Bit 0 set: trace RSP+24 = 0x00c000123456.
   Valid Go pointer. Mark reachable.

6. Bit 1 set: trace RSP+32 = 0. Nil. Skip.

7. Advance: RSP + 7×16 = RSP+112.

8. Continue stack walk from RSP+112.

Context pointer is traced. Object survives GC.
Slot 1 is nil — no harm. When callback returns
and stores pointer, bitmap already marks it.

---

## 10. Constraints and Invariants

### 10.1 JIT Compiler Invariants

1. Magic+version at RSP+8 must be exactly
   0xFFFFFFFFFFF10001.

2. extensionBit (bit 15) must be 0.

3. frameSize16 must be >= 2 and accurate.
   Wrong value corrupts the stack walk.

4. numTrackedSlots must accurately count the
   tracked slots.

5. Bitmap must be the maximal union. Every
   slot that will ever hold a Go pointer must
   have its bit set.

6. All tracked pointer slots (bit=1) must be
   zero-initialized or set to a valid Go
   pointer before any callback into Go.

7. Go pointers must only reside in tracked
   slots. The untracked region must never
   contain Go heap pointers.

8. The g pointer must be valid before any
   callback into Go.

9. Frame must be 16-byte aligned (guaranteed
   by frameSize16 × 16).

### 10.2 Go Runtime Invariants

1. Stack scanner checks g.foreignStack before
   foreign frame discovery. If false, fatal
   on unknown PCs as today. Normal goroutines
   unaffected.

2. If g.foreignStack is true and findfunc(pc)
   returns nil, read RSP+8 for magic. If
   valid, trace per bitmap and advance. If
   invalid, fatal.

3. Stack must not be copied while foreign
   frames present. LockOSThreadForeign sets
   g.foreignStack; stack growth code panics
   instead of copying.

4. Async preemption (SIGURG) must be deferred
   while foreign code executes. Foreign code
   handles cooperative preemption via its own
   mechanism (e.g. TSC-budget checks).

### 10.3 Not Provided

- Function names for foreign frames.
- Line number information.
- Defer/recover in foreign frames.
- Profiling support (unknown PCs skipped).
- Stack growth (fixed stack required).

---

## 11. Portability

### 11.1 ARM64

The protocol is architecture-neutral. On ARM64:

- Sentinel valid (user addresses < 1<<48).
- Stack alignment 16 bytes (same).
- g pointer is R28 not R14.
- Arguments in X0-X7 not RDI/RSI/etc.
- Frame layout protocol identical.

### 11.2 Extensibility

Future adaptation via:
- Version field in magic (bits [0:15])
- extensionBit in header (bit 15)

---

## 12. Runtime Changes Summary

### 12.1 Stack scanner
  (~50-80 lines in traceback.go / stkframe.go)

When findfunc(pc) returns nil, check
g.foreignStack. If false, fatal. If true,
call scanForeignFrame. Zero cost for normal
goroutines.

### 12.2 Stack growth
  (~3 lines in stack.go)

In newstack/copystack, if g.foreignStack is
true, panic instead of copying.

### 12.3 runtime.LockOSThreadForeign
  (new public API)

```go
// LockOSThreadForeign locks the calling
// goroutine to its OS thread and provisions
// a fixed stack of at least the given size.
// The stack will never be copied. Sets
// g.foreignStack = true, enabling the stack
// scanner to recognize self-describing
// foreign frames. Must be called before
// executing foreign code.
func LockOSThreadForeign(stackSize uintptr)
```

Implementation:
- Lock goroutine to OS thread.
- Set g.foreignStack = true.
- If stack < requested, grow now (safe to
  copy — no foreign frames yet).
- Stack growth thereafter panics.

---

## 13. Reference

### 13.1 Constants

```
MAGIC_SENTINEL     = 0xFFFFFFFFFFF1
PROTOCOL_VERSION   = 0x0001
MAGIC_VERSION_V1   = 0xFFFFFFFFFFF10001
MAGIC_MASK         = 0xFFFFFFFFFFFF0000
VERSION_MASK       = 0x000000000000FFFF

EXTENSION_BIT      = 1 << 15

MAX_FRAME_BYTES    = 32767 × 16 = 524,272
MAX_INLINE_TRACKED = 32
MAX_TRACKED_SLOTS  = 65535
MIN_FRAME_SIZE_16  = 2
```

### 13.2 Header Bit Map

```
Bit(s)   Field             Width
──────   ─────             ─────
[0:14]   frameSize16       15
[15]     extensionBit      1
[16:31]  numTrackedSlots   16
[32:63]  inlineBitmap      32
```

### 13.3 Frame Size Formula

```
B = 0                    if numTS <= 32
B = ceil(numTS / 64)     if numTS > 32

tracked_offset = 24 + 8 × B
tracked_size   = 8 × numTS
untracked_size = frameSize16×16
               - tracked_offset
               - tracked_size

tracked_slot[i] at
  RSP + tracked_offset + 8×i
```
