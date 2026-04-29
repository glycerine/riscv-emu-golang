# Plan: Change GuestMemory.base from uintptr to unsafe.Pointer

## Context

Go's `unsafe.Pointer` rule #4 says converting `uintptr` back to `unsafe.Pointer` is only valid when the `uintptr` was obtained in the same expression. Our `GuestMemory.base` is stored as `uintptr`, then later used as `unsafe.Pointer(m.base + uintptr(offset))` — violating this rule. Go 1.26 enforces this more strictly, causing the PLAN082 "found pointer to free object" crash.

The fix: store `base` as `unsafe.Pointer` and use `unsafe.Add()` (Go 1.17+) for arithmetic. `unsafe.Add(ptr, offset)` is the blessed way to do pointer math from an `unsafe.Pointer` (rule #3).

## The Change

**Illegal (current):**
```go
base uintptr
*(*uint64)(unsafe.Pointer(m.base + uintptr(addr & m.mask)))
```

**Legal (new):**
```go
base unsafe.Pointer
*(*uint64)(unsafe.Add(m.base, addr & m.mask))
```

## Files and Locations

### 1. `guestmem.go` — Field + core methods

**Field (line 170):**
```go
base unsafe.Pointer  // was: base uintptr
```

**NewGuestMemory (line 217):**
```go
base: unsafe.Pointer(ptr),  // was: uintptr(ptr)
```

**Free (lines 239-241):**
```go
if m.base != nil {                    // was: m.base != 0
    C.guest_free(m.base, ...)         // was: C.guest_free(unsafe.Pointer(m.base), ...)
    m.base = nil                      // was: m.base = 0
}
```

**CowClone guard ops (lines 261, 266):**
```go
C.guest_unguard(unsafe.Add(m.base, guardOff), ...)  // was: unsafe.Pointer(m.base+guardOff)
C.guest_guard(unsafe.Add(m.base, guardOff), ...)
```

**COWRemap (line 263):**
```go
newBase, err := COWRemap(m.size, m.base)  // COWRemap signature may need update
```

**Base() accessor (line 294):**
```go
func (m *GuestMemory) Base() uintptr { return uintptr(m.base) }
// Still returns uintptr — callers that need raw address (JIT, abjit) use this
```

**RegFileBase, StackTop (lines 296-297):**
```go
func (m *GuestMemory) RegFileBase() uintptr {
    return uintptr(m.base) + uintptr(m.size) - GuestPageSize
}
func (m *GuestMemory) StackTop() uintptr {
    return uintptr(m.base) + uintptr(m.size) - 2*GuestPageSize
}
```
These return `uintptr` for JIT use — the conversion `uintptr(m.base)` is legal (rule #1).

**RawSlice (line 301):**
```go
return unsafe.Slice((*byte)(m.base), m.size)  // was: unsafe.Slice((*byte)(unsafe.Pointer(m.base)), m.size)
```

**ZeroRange (line 312):**
```go
C.guest_zero_range(unsafe.Add(m.base, uintptr(addr)), ...)  // was: unsafe.Pointer(m.base+uintptr(addr))
```

**hostPtr (line 365):**
```go
return unsafe.Add(m.base, addr & m.mask)  // was: unsafe.Pointer(m.base + uintptr(addr&m.mask))
```

### 2. `run_cached.go` — 6 hot-path load/store sites

Lines 244, 411, 685, 706, 822, 863. All follow the same pattern:

**Loads (lines 244, 685, 822):**
```go
cpu.x[slot.rd] = *(*uint64)(unsafe.Add(cpu.mem.base, addr & cpu.mem.mask))
```

**Stores (lines 411, 706, 863):**
```go
*(*uint64)(unsafe.Add(cpu.mem.base, addr & cpu.mem.mask)) = cpu.x[slot.rs2]
```

### 3. `exec_slot.go` — 4 sites

Lines 69, 86, 109, 126. Same pattern as run_cached.go:
```go
// Loads:
c.x[slot.rd] = *(*uint64)(unsafe.Add(c.mem.base, addr & c.mem.mask))
// Stores:
*(*uint64)(unsafe.Add(c.mem.base, addr & c.mem.mask)) = c.x[slot.rs2]
```

### 4. `exec_slot32.go` — 2 sites

Lines 55, 118. Same pattern.

### 5. `jit_abjit.go` — JIT boundary (no change needed)

Line 52: `s.MemBase = cpu.mem.Base()` — already calls `Base()` which returns `uintptr`. No change.

### 6. `abjit/abjit.go` — State struct (no change needed)

`MemBase uintptr` stays as-is — JIT native code needs a raw address at offset 520.

### 7. `cow_clone.go` or similar — COWRemap signature

Check if `COWRemap` takes `uintptr` and needs to take `unsafe.Pointer` instead. Update accordingly.

## What Stays uintptr

- `abjit.State.MemBase` — native code reads this at a fixed offset
- `GuestMemory.Base()` return type — callers that need raw addresses
- `RegFileBase()`, `StackTop()` return types — same reason
- `mask` field — it's `uint64`, not a pointer

## Verification

```bash
# Build
cd ~/ris && go build ./...

# Full test suite
cd ~/ris && go test -v -count=1 .
cd ~/ris && go test -v -count=1 ./bench/

# With checkptr (catches remaining violations)
cd ~/ris && go test -gcflags=all=-d=checkptr -count=1 .

# Race detector
cd ~/ris && go test -race -count=1 .
```
