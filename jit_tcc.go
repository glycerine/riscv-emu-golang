//go:build tcc

package riscv

// jit_tcc.go — cgo bridge to TCC for compiling C source to native code.
// This is the COLD path: called once per basic block (~1ms), then cached.

/*
#cgo CFLAGS: -I${SRCDIR}/xendor/tcc
#cgo LDFLAGS: -L${SRCDIR}/xendor/tcc -ltcc

#include <libtcc.h>
#include <stdlib.h>
#include <stdint.h>
#include <unistd.h>
#include <math.h>

// jit_trace — callable from TCC-compiled blocks for debugging.
// Uses raw write() syscall to avoid deep fprintf stack usage.
static void jit_trace(const char *label, uint64_t addr, uint64_t val) {
    char buf[128];
    // Manual hex formatting to avoid fprintf stack depth.
    static const char hex[] = "0123456789abcdef";
    int i = 0;
    buf[i++] = '['; buf[i++] = 'J'; buf[i++] = 'I'; buf[i++] = 'T'; buf[i++] = ']'; buf[i++] = ' ';
    while (*label) buf[i++] = *label++;
    buf[i++] = ' '; buf[i++] = 'a'; buf[i++] = '='; buf[i++] = '0'; buf[i++] = 'x';
    for (int s = 60; s >= 0; s -= 4) buf[i++] = hex[(addr >> s) & 0xF];
    buf[i++] = ' '; buf[i++] = 'v'; buf[i++] = '='; buf[i++] = '0'; buf[i++] = 'x';
    for (int s = 60; s >= 0; s -= 4) buf[i++] = hex[(val >> s) & 0xF];
    buf[i++] = '\n';
    write(2, buf, i);
}

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
    // Inject debug tracing function.
    tcc_add_symbol(s, "jit_trace", jit_trace);

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
	fn     uintptr        // native function pointer
	state  unsafe.Pointer // *C.TCCState — prevents code memory from being freed
	shadow *compiledBlock // V2 shadow block for DebugV1V2 comparison
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

// jitCompile dispatches to tccCompile under the tcc build tag.
func jitCompile(res *emitResult) (*compiledBlock, error) {
	return tccCompile(res.csrc)
}

// jitCompileWith is the TCC stub — ignores useV2 (V2 is native-only).
func jitCompileWith(res *emitResult, _ bool) (*compiledBlock, error) {
	return tccCompile(res.csrc)
}
