package riscv

// jit_tcc.go — cgo bridge to TCC for compiling C source to native code.
// This is the COLD path: called once per basic block (~1ms), then cached.

/*
#cgo CFLAGS: -I${SRCDIR}/vendor/tcc
#cgo LDFLAGS: -L${SRCDIR}/vendor/tcc -ltcc

#include <libtcc.h>
#include <stdlib.h>
#include <stdint.h>

// compile_block compiles C source to native code in memory.
// Returns the function pointer for "block_entry", or NULL on error.
static void* compile_block(const char *csrc, char *errbuf, int errbuf_len) {
    TCCState *s = tcc_new();
    if (!s) return NULL;

    // Capture errors into buffer
    errbuf[0] = '\0';
    tcc_set_error_func(s, errbuf, (TCCErrorFunc)0); // TODO: wire error callback

    tcc_set_output_type(s, TCC_OUTPUT_MEMORY);
    tcc_set_options(s, "-nostdlib");

    if (tcc_compile_string(s, csrc) < 0) {
        tcc_delete(s);
        return NULL;
    }
    if (tcc_relocate(s, TCC_RELOCATE_AUTO) < 0) {
        tcc_delete(s);
        return NULL;
    }
    void *fn = tcc_get_symbol(s, "block_entry");
    if (!fn) {
        tcc_delete(s);
        return NULL;
    }
    // NOTE: we intentionally do NOT call tcc_delete(s) here.
    // The TCCState owns the allocated code memory. Deleting it
    // would free the native code we're about to execute.
    // The Go side holds a reference to prevent GC.
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

	var errbuf [1024]C.char
	fn := C.compile_block(cs, &errbuf[0], 1024)
	if fn == nil {
		return nil, fmt.Errorf("jit: TCC compilation failed: %s", C.GoString(&errbuf[0]))
	}
	return &compiledBlock{
		fn:    uintptr(fn),
		state: fn, // prevent GC from collecting the code pages
	}, nil
}
