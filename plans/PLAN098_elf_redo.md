# Refactor ELF API: return *ELF so callers can find tohost

## Context

`TestSubELF_Block39` (and similar tests) hang because they load RISC-V test ELFs but never call `cpu.SetWatchAddr`. The tohost write loop runs forever inside JIT blocks. The fix from the previous hang (`TestABJIT_RISCVTests_UI`) only works when `watchAddr` is set. We need tohost detection to be automatic.

Currently callers must do a manual two-step dance:
```go
entry, _ := LoadELFBytes(mem, data)
if addr, ok := FindSymbolAddr(data, "tohost"); ok {
    cpu.SetWatchAddr(addr)
}
```

We want callers to get an `*ELF` back so they can call `elf.FindSymbol("tohost")` naturally, and access `elf.Entry` directly.

## API Change

### New type: `ELF`

```go
type ELF struct {
    Entry       uint64     // copy of Header.Entry (always valid, 0 if no header)
    TohostAddr  uint64     // address of "tohost" symbol (0 if not found)
    Header      *Elf64Header
    Shdrs       []*Elf64Shdr
    Data        []byte     // raw ELF bytes for symbol/string table access
}
```

All types exported: `elf64Header` → `Elf64Header`, `elf64Shdr` → `Elf64Shdr`, `elf64Phdr` → `Elf64Phdr`, `elf64Sym` → `Elf64Sym`. Pointers throughout to avoid copying.

`Entry` is a top-level copy so callers can use `elf.Entry` directly without nil-checking `elf.Header`. `TohostAddr` is populated automatically by the loader via `elf.FindSymbolAddr("tohost")` — callers just check `elf.TohostAddr != 0`. `FindSymbolAddr` becomes a method that walks `elf.Shdrs` to find `SHT_SYMTAB` + its linked `SHT_STRTAB`, reads symbols from `elf.Data` — no re-parsing.

Callers simplify from:
```go
entry, _ := LoadELFBytes(mem, data)
if addr, ok := FindSymbolAddr(data, "tohost"); ok {
    cpu.SetWatchAddr(addr)
}
```
to:
```go
elf, err := LoadELFBytes(mem, data)
if err == nil {
    cpu.SetPC(elf.Entry)
    cpu.SetWatchAddr(elf.TohostAddr)  // 0 is harmless if no tohost symbol
} else { /* handle err */ }
```

```go
func (e *ELF) FindSymbolAddr(name string) (uint64, bool)
```

`loadELFReader` already parses the header; it just needs to also parse section headers into `Shdrs` before returning.

### Changed signatures

```go
// Old:
func LoadELF(mem *GuestMemory, path string) (entry uint64, err error)
func LoadELFBytes(mem *GuestMemory, data []byte) (entry uint64, err error)

// New:
func LoadELF(mem *GuestMemory, path string) (*ELF, error)
func LoadELFBytes(mem *GuestMemory, data []byte) (*ELF, error)
```

`FindSymbolAddr` standalone function is removed; replaced by the method.

### Caller migration

All callers change from:
```go
entry, err := LoadELFBytes(mem, data)  →  elf, err := LoadELFBytes(mem, data)
cpu.SetPC(entry)                       →  cpu.SetPC(elf.Entry)
```

And those that called `FindSymbolAddr(data, "tohost")` change to:
```go
if addr, ok := elf.FindSymbolAddr("tohost"); ok { ... }
```

## Files to modify

- `elf.go` — new `ELF` type, change `LoadELF`/`LoadELFBytes`/`loadELFReader` signatures, move `FindSymbolAddr` to method
- ~60 call sites across `*_test.go`, `bench/`, `fuzzoracle/`, `os_test.go`, `hello_test.go`, `machine_clone_test.go` — mechanical `entry` → `elf.Entry` rename
- `jit_emit_ir_test.go:397` (`TestSubELF_Block39`) — add `cpu.SetWatchAddr` using the new API

## Verification

```bash
go build ./...
go test -v -run TestSubELF_Block39 -timeout 10s .
go test -v -run TestABJIT_RISCVTests_UI -timeout 30s .
go test -v ./...
```
