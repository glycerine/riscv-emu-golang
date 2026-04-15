package riscv

// jit_tcc.go — cgo bridge to TCC for compiling C source to native code.
// This is the COLD path: called once per basic block (~1ms), then cached.

/*
#cgo CFLAGS: -I${SRCDIR}/vendor/tcc
#cgo LDFLAGS: -L${SRCDIR}/vendor/tcc -ltcc

#include <libtcc.h>
#include <stdlib.h>
#include <stdint.h>
#include <math.h>

// compile_block compiles C source to native code in memory.
// Returns the function pointer for "block_entry", or NULL on error.
// *out_state receives the TCCState (caller must keep alive to prevent code free).
static void* compile_block(const char *csrc, TCCState **out_state) {
    TCCState *s = tcc_new();
    if (!s) return NULL;
    *out_state = NULL;

    tcc_set_output_type(s, TCC_OUTPUT_MEMORY);
    tcc_set_options(s, "-nostdlib");

    if (tcc_compile_string(s, csrc) < 0) {
        tcc_delete(s);
        return NULL;
    }
    // Inject math symbols for FP JIT blocks (sqrtf/sqrt from libm).
    tcc_add_symbol(s, "jit_sqrtf", sqrtf);
    tcc_add_symbol(s, "jit_sqrt", sqrt);

    if (tcc_relocate(s) < 0) {
        tcc_delete(s);
        return NULL;
    }
    void *fn = tcc_get_symbol(s, "block_entry");
    if (!fn) {
        tcc_delete(s);
        return NULL;
    }
    // Keep the TCCState alive — it owns the compiled code memory.
    *out_state = s;
    return fn;
}
*/
import "C"
import (
	"fmt"
	"unsafe"
)

// compiledBlock holds a TCC-compiled native function pointer.
type compiledBlock struct {
	fn    uintptr        // native function pointer
	state unsafe.Pointer // *C.TCCState — prevents code memory from being freed
}

// tccCompile compiles C source into a native function pointer.
// Cold path: ~1ms per call, amortized over billions of block executions.
func tccCompile(csrc string) (*compiledBlock, error) {
	cs := C.CString(csrc)
	defer C.free(unsafe.Pointer(cs))

	var state *C.TCCState
	fn := C.compile_block(cs, &state)
	if fn == nil {
		return nil, fmt.Errorf("jit: TCC compilation failed")
	}
	return &compiledBlock{
		fn:    uintptr(fn),
		state: unsafe.Pointer(state),
	}, nil
}
