# Self-Describing Foreign Frame Protocol

## Specification v1.4

---

## 1. Overview

This document specifies a protocol that allows
JIT-compiled ("foreign") code to execute on a Go
goroutine stack with full garbage collection
correctness and panic/recover participation.

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

When Go's panic machinery unwinds through a
foreign frame, it invokes the frame's cleanup
handler if one is present. The cleanup handler
can release resources (defer-like) or stop the
unwind entirely (recover-like).

No external metadata registration is required.
No moduledata synthesis. No go:linkname hacks.
No runtime data structures to maintain. The
frame describes itself.

### 1.1 Prerequisites

Three companion changes to the Go runtime are
required.

**Prerequisite 1: runtime.LockOSThreadForeign(stackSize)**

Locks the current goroutine to its OS thread
and provisions a fixed-size stack (e.g. 1MB).
This call changes four runtime behaviors for
this goroutine:

1. **Thread locked.** The goroutine will not
   be rescheduled to another OS thread.

2. **Stack fixed.** The stack will never be
   copied for growth. Any stack growth attempt
   panics. This eliminates the stack-copying
   hazard: Go normally grows goroutine stacks
   by copying them, rewriting internal
   pointers during the copy. Foreign frames
   may contain pointers into the stack that
   the runtime cannot find or rewrite.

3. **No SIGURG.** The runtime will not send
   async preemption signals (SIGURG) to this
   goroutine's thread. sysmon skips it
   entirely. This eliminates signal-related
   latency and the hazard of interrupting
   foreign code at an arbitrary PC. See
   Section 6 (Preemption Protocol).

4. **g.foreignStack = true.** Enables the
   stack scanner and panic unwinder to
   recognize self-describing frames on this
   goroutine's stack.

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

**Prerequisite 3: Modified panic unwinder**

When gopanic encounters a foreign frame during
stack unwinding, it reads the cleanup pointer
at RSP+24. If non-nil, it calls the cleanup
function. The cleanup's return value determines
whether unwinding continues or stops. This
mirrors Go's existing defer/recover mechanism
through a return-value protocol rather than
internal runtime flags.

### 1.2 Design Principles

**Immutable bitmaps.** The pointer bitmap is
written once in the function prologue and never
modified. This eliminates all race conditions
between the GC and the executing foreign code.

**(JIT) Compile-time complete.** Every aspect of the
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

**Language-neutral cleanup.** The cleanup
handler is foreign code — JIT-emitted, in
whatever language the JIT targets. It is not
required to be Go code. It communicates with
the Go runtime through a simple return-value
protocol: return 0 to continue unwinding,
return nonzero to stop (recover).

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

RSP+24             Cleanup pointer
                   (Section 5)

RSP+32             If numTrackedSlots > 32:
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

For v1.1: `magic = 0xFFFFFFFFFFF10001`

Note: v1.1 uses the same protocol version
number (1) as v1. The cleanup slot is an
additive extension — the version number
reflects the wire format of magic+header,
which is unchanged. The cleanup slot at RSP+24
is always present in the frame layout; a nil
value means no cleanup.

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
compiler. The JIT writes the magic+version,
header, and cleanup pointer in the prologue,
before any callback into Go. The GC can only
observe the frame at a safepoint (during a
callback). Therefore the GC never sees the
frame before the prologue has written correct
data.

The edge case of async preemption (SIGURG)
interrupting the prologue is eliminated by
LockOSThreadForeign suppressing SIGURG
entirely for this goroutine's thread
(Section 6, Section 13.2 invariant 5).

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

The header describes the frame layout:

```
RSP+0:   return address
RSP+8:   magic+version (8 bytes)
RSP+16:  frameSize16(15)
         | extensionBit(1)
         | numTrackedSlots(16)
         | inlineBitmap(32)
RSP+24:  cleanup pointer (8 bytes)
RSP+32:  bitmap_word[0] (slots 0-63)
RSP+40:  bitmap_word[1] (slots 64-127)
...
         [tracked slots]
         [untracked space]
```

When numTrackedSlots <= 32, the bitmap fits
entirely in the inlineBitmap field. No bitmap
words appear on the stack. The tracked region
begins at RSP+32.

When numTrackedSlots > 32, inlineBitmap must be
zero and bitmap words follow at RSP+32.

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
    Bitmap words follow at RSP+32.
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
  Tracked region begins at RSP+32.

- numTrackedSlots > 32:
  inlineBitmap must be zero.
  Emit ceil(numTrackedSlots / 64) bitmap
  words starting at RSP+32.
  Tracked region begins at
  RSP + 32 + 8×ceil(numTrackedSlots/64).

### 4.5 Frame Size Computation

```
header_bytes  = 32  (ret+magic+hdr+cleanup)
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

frameSize16 is a (JIT) compile-time constant.

---

## 5. Cleanup Pointer (RSP+24)

### 5.1 Purpose

The cleanup pointer enables foreign frames to
participate in Go's panic/recover mechanism.
When gopanic unwinds through a foreign frame
and finds a non-nil cleanup pointer, it calls
the cleanup function. The cleanup can release
resources (defer behavior) and optionally stop
the panic (recover behavior).

### 5.2 Layout

```
RSP+24, 8 bytes:

A pointer to JIT-emitted cleanup code, or 0.
If 0, no cleanup runs during panic unwind
and the frame is simply skipped.
```

The cleanup pointer is always present in every
self-describing frame. It is written in the
prologue as a (JIT) compile-time constant (either
a code address or 0).

The cleanup pointer is NOT a Go heap pointer.
It is a code address pointing into JIT-emitted
executable memory. The GC does not trace it.
(QUESTION: should we relax this in a principled way so 
we don't have to mmap/malloc all
JIT-ed memory? mmap is useful to maintain W^X -- and darwin
requires it -- but being able to GC JIT-ed code could
also be very convenient in non-darwin places).

### 5.3 Calling Convention

When gopanic encounters a foreign frame with
a non-nil cleanup pointer, it CALLs the
cleanup function using the platform's standard
C calling convention:

```
Arguments:
  arg0 (RDI on amd64, X0 on arm64):
    The original foreign frame's SP value.
    The cleanup uses this to access the
    original frame's tracked slots,
    untracked region, and context.

  arg1 (RSI on amd64, X1 on arm64):
    Pointer to the panic value (Go any).
    The cleanup may inspect this to decide
    whether to recover. May be nil for
    runtime panics that carry no value.

Return value (RAX on amd64, X0 on arm64):
  0 = continue unwinding (defer behavior)
  nonzero = panic handled (recover behavior)
```

### 5.4 Cleanup Frame

The cleanup function is foreign (JIT-emitted)
code. When called, it creates its own stack
frame. This frame MUST follow the same
self-describing protocol: magic+version at
RSP+8, header at RSP+16, cleanup at RSP+24.

The cleanup frame will typically have:
- numTrackedSlots = 0 (no Go pointers)
- cleanup pointer = 0 (no nested cleanup)
- A small frame for scratch space only

This ensures the GC can scan the stack
correctly even if GC fires during cleanup
execution (which is possible if the cleanup
calls back into Go).

During cleanup, the stack looks like:

```
High addresses (stack bottom)
──────────────────────────────────
[Go caller frame]
[original foreign frame]  ← intact
[Go frames: gopanic etc]
[cleanup frame]           ← new
──────────────────────────────────
Low addresses (RSP)
```

The original foreign frame is untouched.
The cleanup reads it via the SP argument.
The GC can trace the entire stack: every
frame is either in moduledata or
self-describing.

### 5.5 Return Value Protocol

The cleanup return value maps directly to
Go's existing panic/recover mechanism:

**Return 0: continue unwinding (defer).**

The cleanup has done its work — released
resources, logged errors, etc. The panic
continues propagating up the stack. This is
equivalent to a Go deferred function that
does not call recover().

The runtime advances past the foreign frame
and continues the unwind loop.

**Return nonzero: panic handled (recover).**

The cleanup has decided to absorb the panic.
The runtime stops unwinding and arranges for
the original foreign function to appear as
though it returned normally to its Go caller.

This uses the same recovery mechanism that
Go's own recover() triggers: the runtime
reads the foreign frame's SP, finds the Go
caller's frame, and resumes execution there.
The foreign frame and all frames above it
are torn down.

This is equivalent to a Go deferred function
that calls recover() and returns normally.

### 5.6 Cleanup Ownership

The cleanup pointer at RSP+24 is exclusively
for the panic unwind path. It is invoked only
by the Go runtime during gopanic. Two distinct
cleanup paths exist:

**Panic path.** The Go runtime reads RSP+24
and calls the cleanup function. Foreign code
does not call it — the runtime does. This
cleanup handles abnormal termination: the
foreign code was interrupted mid-execution,
state may be partially constructed, and the
cleanup must handle arbitrary interruption
points.

**Normal return path.** The Go runtime does
nothing with RSP+24 on normal return. Foreign
code is solely responsible for its own
cleanup before RET. This may be inline in the
epilogue, a call to a separate teardown
function, or nothing at all. This cleanup
handles orderly shutdown: the function
completed, state is consistent.

JIT authors will typically want different
logic for these two paths. The protocol
enforces this separation: RSP+24 is the
runtime's hook for panic; normal teardown is
the JIT author's concern and is not part of
this protocol.

### 5.7 Cleanup Constraints

1. The cleanup MUST create its own
   self-describing frame.

2. The cleanup MUST NOT panic. A panic inside
   a cleanup during unwind is undefined
   behavior. (Future protocol versions may
   define nested panic semantics.)

3. The cleanup MUST return. It must not
   longjmp, exit, or otherwise transfer
   control outside the normal return path.

4. The cleanup SHOULD be fast. It runs during
   panic propagation. Long-running cleanup
   delays the unwind.

5. If the cleanup returns nonzero (recover),
   the original foreign frame is torn down by
   the runtime. The cleanup must not assume
   the original frame survives after returning
   nonzero.

### 5.8 Runtime Integration

The panic unwinder's logic for foreign frames:

```go
// In gopanic's unwind loop:
// (after identifying a foreign frame)

cleanup := *(*uintptr)(
    unsafe.Pointer(foreignSP + 24))
if cleanup != 0 {
    result := callForeignCleanup(
        cleanup,       // code address
        foreignSP,     // arg0: frame SP
        panicVal,      // arg1: panic value
    )
    if result != 0 {
        // Recover: stop unwinding.
        // Use foreignSP to find the Go
        // caller and resume there, as if
        // the foreign function returned.
        mcall(recovery)
        return
    }
}
// Continue unwinding past this frame.
```

The `callForeignCleanup` function is a small
runtime helper that:
1. Restores the g pointer (R14/R28) so the
   cleanup can call back into Go if needed
2. CALLs the cleanup address with the two
   arguments per the platform calling conv
3. Returns the result in RAX/X0

This helper is analogous to `reflectcall`
which gopanic already uses to invoke Go
deferred functions.

---

## 6. Preemption Protocol

### 6.1 Problem

Go's scheduler uses two preemption mechanisms:

- **Cooperative:** compiler-inserted stack
  checks in function prologues compare RSP
  against g.stackguard0. When the runtime
  wants to preempt, it sets stackguard0 to a
  sentinel value. The next prologue check
  triggers a yield.

- **Async (SIGURG):** sysmon detects a
  goroutine running >10ms and sends SIGURG
  to its thread. The signal handler captures
  the goroutine's state and preempts it.

Foreign code has no Go-style prologue checks,
so cooperative preemption doesn't reach it.
SIGURG delivery interrupts at arbitrary PCs
within JIT code — the signal handler cannot
safely capture the state because the PC is
not in moduledata and the register layout is
unknown. Additionally, signal delivery and
handling introduces microseconds of latency,
which is unacceptable for latency-sensitive
applications (games, audio, real-time).

### 6.2 Solution

`LockOSThreadForeign` disables SIGURG for the
goroutine entirely. sysmon skips it for async
preemption. No signals are delivered to the
thread while the goroutine is foreign-capable.

Instead, preemption is fully cooperative:
foreign code calls back into Go at natural
boundaries. Any Go function's prologue checks
stackguard0 and services pending preemption
or GC requests automatically. No explicit
yield API is needed — any callback into Go is
a safepoint.

### 6.3 Foreign Code Contract

**Foreign code must call back into Go within
a bounded time.** This is the sole obligation.

While foreign code runs without calling into
Go, the goroutine is unpreemptible. GC
stop-the-world pauses are delayed.

The recommended maximum interval between Go
callbacks is 10ms, matching sysmon's existing
preemption threshold. For latency-sensitive
applications, shorter intervals are better.

The callbacks do not need to be explicit
yield points. Any call into Go suffices:
- Allocating a Go object
- Accessing a Go data structure
- Calling runtime.Gosched()
- Any Go function whatsoever

The Go function's prologue handles everything
transparently.

### 6.4 GC Cooperation

The GC does not require SIGURG to scan a
foreign-capable goroutine's stack. The GC
can scan the stack at any time using the
self-describing frame protocol — it does not
need the goroutine to be stopped at a
Go-specific safepoint.

When a GC stop-the-world is requested, the
foreign-capable goroutine will participate
when it next calls into Go. The STW pause
is bounded by the foreign code's maximum
callback interval.

For applications that need deterministic GC
timing, the foreign code can call
`runtime.Gosched()` or `runtime.GC()` at
explicit points (e.g. between game frames,
between physics ticks).

### 6.5 What This Replaces

This cooperative model replaces the cgocall
approach where Go transitions the goroutine
to "syscall" state on every foreign call and
releases the P for other goroutines. In our
model:

- The transition from Go to foreign code
  does not change goroutine state. The
  goroutine remains in _Grunning and
  retains its P.
- The scheduler does not need to steal the P.
- Preemption happens at Go callbacks, not
  via signals.

If foreign code calls back into Go and that
Go code performs a system call, the normal
syscall state transitions apply — the
goroutine enters _Gsyscall and the P may be
released, as with any Go code. This is
transparent and correct.

The tradeoff: one P is dedicated to this
goroutine while it executes foreign code.
For a game or JIT engine that runs one
primary goroutine doing foreign work, this
is the natural model.

---

## 7. Relationship to cgocall

This section explains why this protocol
exists as an alternative to Go's existing
cgocall infrastructure.

### 7.1 What cgocall Provides

Go's runtime.cgocall is the existing
mechanism for calling foreign code. Despite
the "c" in the name, it is not C-specific —
it handles any foreign call. It provides:

- Precise stack tracing by switching to a
  separate C stack, so the Go stack contains
  only Go frames.
- Scheduler coordination by transitioning
  the goroutine to syscall state and
  releasing the P.
- Symbolization via runtime.SetCgoCallback.

### 7.2 Why cgocall Is Insufficient

cgocall's overhead is approximately 43ns per
call. A direct CALL/RET is approximately 2ns.
This is a 21× performance difference.

For coarse-grained foreign calls (calling a
C library function once per operation), 43ns
is negligible. For a JIT compiler dispatching
translated basic blocks at high frequency —
potentially millions of transitions per
second — it is catastrophic. The overhead
dominates execution time and makes the JIT
non-competitive with native execution.

The overhead comes from cgocall's model:
switching stacks, transitioning goroutine
state, releasing and reacquiring the P, and
signal mask manipulation. These are all
necessary for cgocall's generality (it
handles any foreign code on any goroutine),
but unnecessary when the goroutine is locked
to a thread with a fixed stack and the
foreign code follows a self-describing frame
protocol.

### 7.3 How This Protocol Differs

This protocol takes a different approach:
instead of switching to a separate stack, the
foreign code runs directly on the goroutine
stack. Instead of transitioning goroutine
state, the goroutine remains in running state.
Instead of releasing the P, the goroutine
keeps it. The GC traces foreign frames using
the self-describing bitmap rather than by
requiring all foreign frames to be on a
separate stack.

The result: 2ns call overhead (a bare
CALL/RET), full GC correctness, and
panic/recover participation. The cost is
that one P is dedicated to the goroutine
and the foreign code must follow the
self-describing frame protocol.

### 7.4 Symbolization

This protocol does not provide function
names or line numbers for foreign frames.
Stack traces show `<foreign frame at PC>`.
A future version could support a
symbolization callback analogous to
runtime.SetCgoCallback, where the JIT
registers a function that maps PCs to
human-readable names.

### 7.5 No cgo Toolchain Required

This protocol requires no cgo toolchain
involvement. The JIT compiler emits
self-describing frames directly. No
`import "C"`, no cgo build step, no
dynamic linking. This is particularly
valuable for pure-Go projects that
generate foreign code at runtime (JIT
compilers, Wasm engines, emulators) and
do not otherwise need C interop.

---

## 8. Tracked Region

### 8.1 Location

When numTrackedSlots <= 32 (inline bitmap):
```
tracked_slot[i] is at RSP + 32 + 8×i
```

When numTrackedSlots > 32 (external bitmap):
```
B = ceil(numTrackedSlots / 64)
tracked_slot[i] is at RSP + 32 + 8×B + 8×i
```

### 8.2 Bitmap Interpretation

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

### 8.3 Maximal Bitmap Rule

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

### 8.4 Zero-Initialization Requirement

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

### 8.5 Slot Lifecycle

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

### 8.6 Clearing Dead Pointer Slots

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

## 9. Untracked Region

### 9.1 Location

```
untracked_start = RSP + 32
                + bitmap_bytes
                + 8 × numTrackedSlots
untracked_end   = RSP + frameSize16 × 16
```

### 9.2 GC Behavior

The GC does not read, trace, or interpret any
data in the untracked region.

### 9.3 Permitted Uses

The JIT may use the untracked region for:
- Guest register spills (integers)
- Scratch space
- Temporary buffers
- Saved callee-saved registers
- Saved g pointer
  (R14 on amd64, R28 on arm64)
- Any data that is NOT a Go heap pointer

**Critical rule.** The untracked region MUST
NOT be the sole location of a live Go heap
pointer. Any Go pointer must have its
authoritative copy in a tracked slot with
bitmap bit = 1. A redundant copy of an
already-tracked pointer in the untracked
region is harmless — the GC already sees it
via the tracked slot.

---

## 10. GC Scanner Integration

### 10.1 Complete Scanner Logic

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
    // (RSP+24 is the cleanup pointer —
    //  a code address, not a Go pointer.
    //  The GC does NOT trace it.)

    var trackedBase uintptr

    if numTS <= 32 {
        // Inline bitmap
        trackedBase = sp + 32
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
        trackedBase = sp + 32 + uintptr(B*8)
        for i := uint16(0); i < numTS; i++ {
            wIdx := uint64(i) / 64
            bIdx := uint64(i) % 64
            word := *(*uint64)(
                unsafe.Pointer(
                    sp + 32 +
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

### 10.2 Integration Point

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

### 10.3 Stack Traces

runtime.Callers and runtime.Stack should emit:
```
<foreign frame at 0x7f1234560080>
```
The function name is unknown. The PC is useful
for debugging.

---

## 11. JIT Compiler Requirements

### 11.1 At Compilation Time

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
header_bytes  = 32  (ret+magic+hdr+cleanup)
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

**Step 5: Determine the cleanup pointer.**

If the function needs cleanup on panic:
  Set cleanup = address of JIT-emitted
  cleanup code for this function.

If no cleanup needed:
  Set cleanup = 0.

The cleanup pointer is a compile-time
constant.

### 11.2 Emitted Prologue

```asm
foreign_function:
    sub   rsp, FRAME_SIZE
    mov   qword [rsp+8],  MAGIC
    mov   qword [rsp+16], HEADER
    mov   qword [rsp+24], CLEANUP  ; or 0

    ; External bitmap (numTrackedSlots > 32):
    mov   qword [rsp+32], BITMAP_WORD_0
    ; mov qword [rsp+40], BITMAP_WORD_1

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

### 11.3 Emitted Epilogue

```asm
    mov   r14, [rsp+UNTRACKED+0]
    mov   rbx, [rsp+UNTRACKED+8]
    mov   rbp, [rsp+UNTRACKED+16]
    add   rsp, FRAME_SIZE
    ret
```

### 11.4 Emitted Callback Sequence

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

### 11.5 Emitted Cleanup Function

The cleanup is a separate JIT-emitted function
that follows the same self-describing protocol.

```asm
my_block_cleanup:
    sub   rsp, 32             ; minimal frame
    mov   qword [rsp+8],  MAGIC
    mov   qword [rsp+16], CLEANUP_HEADER
    mov   qword [rsp+24], 0  ; no nested cleanup

    ; RDI = original frame's SP
    ; RSI = pointer to panic value

    ; Access original frame's context:
    ;   context is at RDI+32 (tracked_slot[0])
    mov   rax, [rdi+32]

    ; ... do cleanup work ...

    ; Return: 0 = continue unwind
    ;         1 = recover (stop unwind)
    xor   eax, eax            ; 0 = continue

    add   rsp, 32
    ret
```

The cleanup header for a frame with no tracked
slots and no cleanup of its own:

```
frameSize16 = 2  (32 bytes)
numTrackedSlots = 0
inlineBitmap = 0

CLEANUP_HEADER = 0x0000000000000002
```

### 11.6 g Pointer Protocol

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

Note: the runtime's callForeignCleanup helper
restores g before calling the cleanup, so the
cleanup does not need to restore g itself.

---

## 12. Worked Example

### 12.1 Scenario

A RISC-V JIT function that:
- Receives a context pointer in RDI (Go heap)
- Calls one Go callback returning a Go pointer
- Uses 5 RISC-V register spill slots
- Saves R14, RBX, RBP
- Has a cleanup handler

### 12.2 Frame Design

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

### 12.3 Layout Computation

```
numTrackedSlots = 2 → inline (2 <= 32)

header_bytes    = 32  (ret+magic+hdr+cleanup)
bitmap_bytes    = 0
tracked_bytes   = 16  (2 × 8)
untracked_bytes = 64
total           = 112
aligned         = 112 (already 16-aligned)
frameSize16     = 7
```

### 12.4 Header Computation

```
bitmap: bit 0 = 1, bit 1 = 1 → 0x3

header = 0
header |= 7              // frameSize16
// bit 15 = 0
header |= 2 << 16        // numTrackedSlots
header |= 0x3 << 32      // inlineBitmap

header = 0x0000000300020007
```

### 12.5 Stack Layout

```
RSP+0:   return address
RSP+8:   0xFFFFFFFFFFF10001  magic+version
RSP+16:  0x0000000300020007  header
RSP+24:  cleanup pointer     (code addr)
RSP+32:  tracked[0]  context ptr
RSP+40:  tracked[1]  0 → callback ptr later
RSP+48:  saved R14   (untracked)
RSP+56:  saved RBX   (untracked)
RSP+64:  saved RBP   (untracked)
RSP+72:  RISC-V x10  (untracked)
RSP+80:  RISC-V x11  (untracked)
RSP+88:  RISC-V x12  (untracked)
RSP+96:  RISC-V x13  (untracked)
RSP+104: RISC-V x14  (untracked)
RSP+112: [next frame]
```

### 12.6 Emitted Code

```asm
jit_block:
    sub   rsp, 112
    mov   qword [rsp+8],  0xFFFFFFFFFFF10001
    mov   qword [rsp+16], 0x0000000300020007
    mov   qword [rsp+24], jit_block_cleanup
    mov   [rsp+32], rdi        ; slot 0: ctx
    xor   eax, eax
    mov   [rsp+40], rax        ; slot 1: zero
    mov   [rsp+48], r14        ; save g
    mov   [rsp+56], rbx
    mov   [rsp+64], rbp

    ; ... translated RISC-V code ...
    ; ... use rsp+72..111 for spills ...

    ; Callback
    mov   r14, [rsp+48]        ; restore g
    mov   rdi, [rsp+32]        ; arg: ctx
    call  go_allocate_something
    mov   [rsp+40], rax        ; slot 1
    mov   [rsp+48], r14        ; re-save g

    ; ... continue with Go ptr in slot 1 ...

    ; Epilogue
    mov   r14, [rsp+48]
    mov   rbx, [rsp+56]
    mov   rbp, [rsp+64]
    add   rsp, 112
    ret

jit_block_cleanup:
    sub   rsp, 32
    mov   qword [rsp+8],  0xFFFFFFFFFFF10001
    mov   qword [rsp+16], 0x0000000000000002
    mov   qword [rsp+24], 0   ; no nested cleanup

    ; RDI = original frame SP
    ; RSI = panic value pointer
    ; Access context: [rdi+32]
    mov   rax, [rdi+32]       ; context ptr
    ; ... release resources via context ...

    xor   eax, eax            ; 0 = continue
    add   rsp, 32
    ret
```

### 12.7 GC Trace Walkthrough

GC fires during `call go_allocate_something`.
Goroutine stopped inside Go code. Stack walker
reaches the foreign frame.

```
RSP+0:   return addr (Go caller)
RSP+8:   0xFFFFFFFFFFF10001
RSP+16:  0x0000000300020007
RSP+24:  0x00007f0012340080  (cleanup addr)
RSP+32:  0x00c000123456  (context ptr)
RSP+40:  0x0000000000000000  (zero)
RSP+48:  0x00c0000001a0  (R14, untracked)
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
   trackedBase = RSP+32.

5. Bit 0 set: trace RSP+32 = 0x00c000123456.
   Valid Go pointer. Mark reachable.

6. Bit 1 set: trace RSP+40 = 0. Nil. Skip.

7. RSP+24 (cleanup pointer) is NOT traced.
   It is a code address, not a Go pointer.

8. Advance: RSP + 7×16 = RSP+112.

9. Continue stack walk from RSP+112.

### 12.8 Panic Unwind Walkthrough

A Go callback panics. gopanic begins unwinding.
It reaches the foreign frame.

1. gopanic reads RSP+24: non-nil cleanup.

2. gopanic calls callForeignCleanup:
   - Restores g pointer
   - Sets RDI = foreign frame SP
   - Sets RSI = &panicValue
   - CALLs cleanup address

3. Stack during cleanup:

   ```
   [Go caller frame]
   [original foreign frame]  ← intact
   [gopanic frames]
   [callForeignCleanup]
   [jit_block_cleanup frame] ← self-describing
   ```

4. Cleanup reads context from [RDI+32],
   releases resources, returns 0 in RAX.

5. callForeignCleanup returns 0 to gopanic.

6. gopanic continues unwinding past the
   foreign frame using frameSize16 × 16.

If the cleanup had returned 1 instead:

5b. callForeignCleanup returns 1 to gopanic.

6b. gopanic stops unwinding. Uses the foreign
    frame's SP to find the Go caller. Resumes
    execution as though the foreign function
    returned normally. Same mechanism as
    Go's recover().

---

## 13. Constraints and Invariants

### 13.1 JIT Compiler Invariants

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
   slots as their authoritative location.
   Redundant copies in the untracked region
   are harmless.

8. The g pointer must be valid before any
   callback into Go.

9. Frame must be 16-byte aligned (guaranteed
   by frameSize16 × 16).

10. Cleanup pointer at RSP+24 must be either
    0 or a valid code address pointing to a
    JIT-emitted cleanup function that follows
    this protocol.

11. Cleanup functions must create their own
    self-describing frames, must not panic,
    and must return normally.

### 13.2 Go Runtime Invariants

1. Stack scanner checks g.foreignStack before
   foreign frame discovery. If false, fatal
   on unknown PCs as today. Normal goroutines
   unaffected.

2. If g.foreignStack is true and findfunc(pc)
   returns nil, read RSP+8 for magic. If
   valid, trace per bitmap and advance. If
   invalid, fatal.

3. The GC does NOT trace RSP+24 (cleanup
   pointer). It is a code address, not a Go
   heap pointer.

4. Stack must not be copied while foreign
   frames present. LockOSThreadForeign sets
   g.foreignStack; stack growth code panics
   instead of copying.

5. No SIGURG is sent to foreign-capable
   goroutines. sysmon skips them for async
   preemption. Preemption is cooperative:
   it occurs when foreign code calls into
   Go and the Go prologue checks
   stackguard0. See Section 6.

6. During panic unwind, if a foreign frame has
   a non-nil cleanup pointer, gopanic CALLs
   it with (frameSP, &panicValue). If the
   return value is 0, continue unwinding. If
   nonzero, recover.

### 13.3 Not Provided

- Function names for foreign frames.
- Line number information.
- Profiling support (unknown PCs skipped).
- Stack growth (fixed stack required).
- Nested panic in cleanup (undefined in v1.1).

---

## 14. Portability

### 14.1 ARM64

The protocol is architecture-neutral. On ARM64:

- Sentinel valid (user addresses < 1<<48).
- Stack alignment 16 bytes (same).
- g pointer is R28 not R14.
- Cleanup args in X0, X1 (not RDI, RSI).
- Cleanup return in X0 (not RAX).
- Frame layout protocol identical.

### 14.2 Extensibility

Future adaptation via:
- Version field in magic (bits [0:15])
- extensionBit in header (bit 15)
- Cleanup return values > 1 (reserved for
  future semantics)

---

## 15. Runtime Changes Summary

### 15.1 Stack scanner
  (~50-80 lines in traceback.go / stkframe.go)

When findfunc(pc) returns nil, check
g.foreignStack. If false, fatal. If true,
call scanForeignFrame. Zero cost for normal
goroutines.

### 15.2 Stack growth
  (~3 lines in stack.go)

In newstack/copystack, if g.foreignStack is
true, panic instead of copying.

### 15.3 sysmon preemption
  (~5 lines in proc.go)

In sysmon's preemption loop, skip goroutines
where g.foreignStack is true. Do not send
SIGURG to their threads.

### 15.4 Panic unwinder
  (~30-50 lines in panic.go)

In gopanic's unwind loop, when encountering a
foreign frame: read cleanup pointer at SP+24.
If non-nil, call it via callForeignCleanup.
Check return value. If 0, continue. If nonzero,
trigger recovery using existing mechanism.

### 15.5 callForeignCleanup
  (~20 lines, new runtime helper)

Small function that restores g, sets up args
per platform calling convention, CALLs the
cleanup address, returns the result. Analogous
to existing reflectcall.

### 15.6 runtime.LockOSThreadForeign
  (new public API)

```go
// LockOSThreadForeign locks the calling
// goroutine to its OS thread and provisions
// a fixed stack of at least the given size.
// The stack will never be copied. No SIGURG
// signals will be sent to this thread.
// Sets g.foreignStack = true, enabling the
// stack scanner and panic unwinder to
// recognize self-describing foreign frames.
// Must be called before executing foreign
// code.
func LockOSThreadForeign(stackSize uintptr)
```

Implementation:
- Lock goroutine to OS thread.
- Set g.foreignStack = true.
- Mark goroutine as exempt from async
  preemption (sysmon skips it for SIGURG).
- Provision a fixed stack of the requested
  size. The current goroutine stack is
  copied to the new stack (safe — no
  foreign frames exist yet). Alternatively,
  the implementation may use the OS thread's
  own stack via pthread_attr_setstacksize.
- The stack is marked non-copyable. Any
  future stack growth attempt panics.

---

## 16. Reference

### 16.1 Constants

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

CLEANUP_CONTINUE   = 0
CLEANUP_RECOVER    = 1
```

### 16.2 Header Bit Map

```
Bit(s)   Field             Width
──────   ─────             ─────
[0:14]   frameSize16       15
[15]     extensionBit      1
[16:31]  numTrackedSlots   16
[32:63]  inlineBitmap      32
```

### 16.3 Frame Layout Summary

```
RSP+0    return address      8 bytes
RSP+8    magic+version       8 bytes
RSP+16   header              8 bytes
RSP+24   cleanup pointer     8 bytes
RSP+32   bitmap words        8×B bytes
         tracked slots       8×numTS bytes
         untracked space     remainder
```

### 16.4 Frame Size Formula

```
B = 0                    if numTS <= 32
B = ceil(numTS / 64)     if numTS > 32

tracked_offset = 32 + 8 × B
tracked_size   = 8 × numTS
untracked_size = frameSize16×16
               - tracked_offset
               - tracked_size

tracked_slot[i] at
  RSP + tracked_offset + 8×i
```
