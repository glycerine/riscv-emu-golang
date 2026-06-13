// Package jitcall provides a zero-cgo-overhead trampoline for calling
// JIT-compiled native code blocks from Go.
package jitcall

import (
	"fmt"
)

// Result is the return value from a JIT-compiled block.
// The first three fields are written by assembly trampolines at fixed
// offsets — do not reorder them.
type Result struct {
	PC        uint64 // next PC to execute
	Status    uint64 // 0=ok, 1=ecall, 2=ebreak, 3=load_fault, 4=store_fault, 5=illegal
	FaultAddr uint64 // guest address that faulted (when Status >= 3)
	ICdelta   uint64 // Instruction Counter delta (R15): guest instructions begun during this dispatch
}

func statusToString(status uint64) string {
	switch status {
	case 0:
		return "ok"
	case 1:
		return "ecall"
	case 2:
		return "ebreak"
	case 3:
		return "load_fault"
	case 4:
		return "store_fault"
	case 5:
		return "illegal"

		// we see alot of case 6, what does it mean?
		// case 6:
		// return "what?"
	}
	return fmt.Sprintf("unknown status: 0x%x", status)
}

func (r *Result) String() string {
	return fmt.Sprintf(`
&jitcall.Result{
              PC: 0x%x
          Status: %v
       FaultAddr: 0x%x
         ICdelta: %v
}`, r.PC, statusToString(r.Status), r.FaultAddr, r.ICdelta)
}
