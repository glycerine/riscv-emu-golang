# Implement tohost Watch: Standard RISC-V Test Exit Detection

## Context

The RISC-V test suite (riscv-tests) uses the `tohost` convention: when a test completes, it writes a non-zero value to a memory-mapped address (`tohost`), then enters a halt loop. Simulators like spike and libriscv detect this write and stop execution. Our emulator currently uses a different mechanism (intercepting ECALL at the Go level via NoteChain), which works but:

1. Doesn't handle the halt loop case (BudgetCheck exits the native block, but `RunJIT` re-enters the same halt block forever)
2. Isn't the standard approach used by other RISC-V simulators
3. Can't detect completion for binaries that don't use ECALL

**Goal**: Add the standard `tohost` watch mechanism. Parse the `tohost` symbol from the ELF, poll the address after each dispatch cycle, and exit when a non-zero value is detected.

## Investigation Findings

### ELF Binaries DO Have tohost Symbols

Verified via `nm` — all riscv-elf-tests binaries contain:
- `tohost` (data symbol, e.g. at 0x1000 for `rv64ui-p-add`, 0x2000 for `rv64ui-p-ld_st`)
- `write_tohost` (code at 0x36 — the halt loop)
- `fromhost` (companion address, used by HTIF)

### How riscv-tests Work

The test binary's trap handler (at address 0x0000) catches ECALL exceptions:
```asm
write_tohost:        # address 0x36
    sw gp, tohost    # store test result to tohost
    sw zero, fromhost
    j write_tohost   # halt loop
```

The pass/fail sequences set `gp` then call ECALL:
```asm
pass:  li gp, 1;  li a7, 93; li a0, 0; ecall
fail:  slli gp, gp, 1; ori gp, gp, 1; li a7, 93; mv a0, gp; ecall
```

### Current Dual Exit Path

**Our emulator intercepts ECALL at the Go level** (before the trap handler runs), so:
- Interpreter: ECALL → `ErrEcall` → NoteChain → `LinuxExit` panic → exit. Works.
- JIT: block returns `jitEcall` → NoteChain → same path. Works for most tests.

The tohost write only happens if the trap handler actually executes. Currently it doesn't because ECALL is intercepted first. But the tohost watch provides:
1. A safety net when ECALL handling fails or isn't reached
2. Standard behavior matching spike/libriscv
3. Foundation for future M-mode trap vectoring

### tohost Value Convention

- `tohost == 1` → PASS (exit code 0)
- `tohost == (testnum<<1)|1` → FAIL test number `testnum`
- `tohost == 0` → not done yet

### ELF Symbol Table Format

ELF section headers are 64 bytes each, located at `ShOff` in the ELF header. Need:
1. Find `SHT_SYMTAB` section (type 2) among section headers
2. Its `sh_link` field points to the associated `SHT_STRTAB` section
3. Each `Elf64_Sym` entry is 24 bytes: `{st_name(4), st_info(1), st_other(1), st_shndx(2), st_value(8), st_size(8)}`
4. Match `st_name` against string table to find "tohost"

Our `elf.go` already has the header struct with `ShOff`, `ShEntSize`, `ShNum`, `ShStrNdx` — currently marked "unused".

## Implementation Plan

### Step 1: Add ELF Symbol Lookup (`elf.go`)

Add `FindSymbolAddr` function to parse the ELF symbol table:

```go
// ELF section header constants
const (
    shtSymtab = 2  // SHT_SYMTAB
    shtStrtab = 3  // SHT_STRTAB
)

// elf64Shdr is a 64-byte ELF section header.
type elf64Shdr struct {
    Name      uint32
    Type      uint32
    Flags     uint64
    Addr      uint64
    Offset    uint64
    Size      uint64
    Link      uint32
    Info      uint32
    AddrAlign uint64
    EntSize   uint64
}

// elf64Sym is a 24-byte ELF symbol table entry.
type elf64Sym struct {
    Name  uint32
    Info  uint8
    Other uint8
    Shndx uint16
    Value uint64
    Size  uint64
}

// FindSymbolAddr parses the ELF symbol table in data and returns the
// address (st_value) of the named symbol. Returns (0, false) if the
// symbol is not found or the binary has no symbol table.
func FindSymbolAddr(data []byte, name string) (uint64, bool)
```

Implementation:
1. Re-read the ELF header to get `ShOff`, `ShEntSize`, `ShNum`
2. Iterate section headers to find `SHT_SYMTAB`
3. Read the associated string table (via `sh_link`)
4. Iterate symbol entries, compare names
5. Return `st_value` on match

This is a standalone function — doesn't modify `LoadELF` or its return type. Callers parse symbols separately if needed.

### Step 2: Add Watch Address to CPU (`cpu.go`)

Add an unexported field with exported accessor methods:

```go
type CPU struct {
    // ... existing fields ...
    watchAddr uint64 // if non-zero, tohost address to poll
}

func (c *CPU) SetWatchAddr(addr uint64) { c.watchAddr = addr }
func (c *CPU) WatchAddr() uint64        { return c.watchAddr }
```

### Step 3: Add Polling to Interpreter Dispatch (`note.go`)

Modify `RunWithChain` to check watchAddr after each instruction:

```go
func RunWithChain(cpu *CPU, nc *NoteChain) error {
    for {
        err := cpu.step()
        cpu.cycle++
        // Tohost polling: check after every instruction.
        // When watchAddr == 0 (the common case), this is a single
        // predicted-not-taken branch — negligible overhead.
        if cpu.watchAddr != 0 {
            if v, _ := (&cpu.mem).Load64(cpu.watchAddr); v != 0 {
                panic(&ExitError{Code: tohostExitCode(v)})
            }
        }
        if err == nil {
            continue
        }
        n := noteFromStepErr(err, cpu.PC())
        switch nc.Deliver(cpu, n) {
        case NoteHandled:
            continue
        default:
            return err
        }
    }
}
```

The `panic(&ExitError{...})` reuses the existing exit machinery — `RunWithOS` and `runJITWithOS` already have `defer/recover` blocks that catch `*ExitError` and return the exit code cleanly.

### Step 4: Add Polling to JIT Dispatch (`jit.go`)

Add the check at the top of the `RunJIT` loop, which runs once per block:

```go
func (j *JIT) RunJIT(cpu *CPU) error {
    for {
        // Tohost polling — once per dispatch cycle (block granularity).
        if cpu.watchAddr != 0 {
            if v, _ := cpu.mem.Load64(cpu.watchAddr); v != 0 {
                panic(&ExitError{Code: tohostExitCode(v)})
            }
        }

        pc := cpu.pc
        // ... rest of existing loop ...
    }
}
```

### Step 5: Add `tohostExitCode` Helper (`os.go`)

```go
// tohostExitCode converts a tohost value to an exit code matching the
// riscv-tests convention: tohost==1 means PASS (exit code 0),
// any other non-zero value is the raw fail code.
func tohostExitCode(v uint64) int {
    if v == 1 {
        return 0
    }
    return int(v)
}
```

### Step 6: Wire Up in Test Harness (`riscv_test.go`)

In `runRISCVTest` and `runRISCVTestJIT`, after loading the ELF:

```go
// Parse tohost symbol for standard exit detection.
if addr, ok := FindSymbolAddr(data, "tohost"); ok {
    cpu.SetWatchAddr(addr)
}
```

No other changes to the test harness — the existing exit code logic (`exitCode >> 1` for test number) already matches the tohost convention.

## Files to Modify

| File | Change |
|------|--------|
| `elf.go` | Add `elf64Shdr`, `elf64Sym` structs; add `FindSymbolAddr` function |
| `cpu.go` | Add `watchAddr` field, `SetWatchAddr`/`WatchAddr` methods |
| `note.go` | Add tohost polling in `RunWithChain` |
| `jit.go` | Add tohost polling in `RunJIT` |
| `os.go` | Add `tohostExitCode` helper |
| `riscv_test.go` | Call `FindSymbolAddr` + `SetWatchAddr` in test runners |

## Verification

```bash
# Unit test for ELF symbol parsing
go test -count=1 -run 'TestFindSymbolAddr' -timeout 10s -v .

# Interpreter tests (should still pass with tohost watch active)
go test -count=1 -run 'TestRISCVTests_UI$' -timeout 60s -v .

# JIT tests (primary beneficiary — halt loops now detected)
go test -count=1 -run 'TestRISCVTests_UI_JIT' -timeout 60s -v .

# Lockstep tests
go test -count=1 -run 'TestRISCVTests_Lockstep_UI' -timeout 120s -v .

# Full test suite
go test -count=1 -timeout 120s -v .
```

## Design Notes

- **Performance**: When `watchAddr == 0` (non-test binaries), the check is a single not-taken branch per instruction (interpreter) or per block (JIT). Negligible overhead.
- **Compatibility**: ECALL-based exit remains as the primary mechanism. Tohost is additive — both can coexist. Whichever fires first wins.
- **Correctness**: The `panic(&ExitError{})` approach reuses the existing exit machinery without adding new error types or return values. `RunWithOS` and `runJITWithOS` already recover from `*ExitError`.
- **Scope**: `FindSymbolAddr` is general-purpose — usable beyond tohost (e.g., finding `fromhost`, `begin_signature`, etc.).
- **Future**: When M-mode trap vectoring is implemented, the trap handler will actually run and write to tohost. The polling mechanism is already in place to catch it.
