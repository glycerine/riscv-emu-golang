package riscv

import (
	"fmt"
	"strconv"
)

// MemoryModel selects how guest virtual addresses become host memory
// references.
//
// MemoryModelLinear is the jea9linux default for deterministic performance
// testing. Interpreter accesses still use GuestMemory's ordinary bounds and
// personality-overlay checks before computing base+addr. JIT accesses do not:
// generated code emits naked base+addr loads/stores, without the sandbox mask,
// the sandbox bounds branch, or jea9linux mmap/mprotect/page-zero checks.
// Bad guest addresses may therefore produce host faults instead of guest
// MemFaults. Use MemoryModelSandbox when those checks/containment are needed.
type MemoryModel uint8

const (
	MemoryModelLinear MemoryModel = iota
	MemoryModelSandbox
)

func (m MemoryModel) String() string {
	switch m {
	case MemoryModelLinear:
		return "linear"
	case MemoryModelSandbox:
		return "sandbox"
	default:
		return fmt.Sprintf("MemoryModel(%d)", uint8(m))
	}
}

func (m MemoryModel) Validate() error {
	switch m {
	case MemoryModelLinear, MemoryModelSandbox:
		return nil
	default:
		return fmt.Errorf("invalid memory model %d", uint8(m))
	}
}

func (m MemoryModel) UsesSandbox() bool {
	return m == MemoryModelSandbox
}

// SandboxMemoryModelFlag adapts the boolean -sandbox CLI flag onto the
// MemoryModel enum while preserving flag package bool-flag behavior.
type SandboxMemoryModelFlag struct {
	Model *MemoryModel
}

func (f SandboxMemoryModelFlag) String() string {
	if f.Model == nil {
		return "false"
	}
	return strconv.FormatBool(*f.Model == MemoryModelSandbox)
}

func (f SandboxMemoryModelFlag) Set(value string) error {
	if f.Model == nil {
		return fmt.Errorf("nil memory model flag")
	}
	enabled, err := strconv.ParseBool(value)
	if err != nil {
		return err
	}
	if enabled {
		*f.Model = MemoryModelSandbox
	} else {
		*f.Model = MemoryModelLinear
	}
	return nil
}

func (f SandboxMemoryModelFlag) IsBoolFlag() bool { return true }
