package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
	"github.com/tetratelabs/wazero/sys"
)

// The number of riscv instructions retired during a
// run on compiled bench/libriscv_guest/bench_guest.c ;
// we use this number for the numerator in native
// benchmarking for an apples-to-apples comparison.
const NATIVE_RETIRED = 2524935201
const NATIVE_RETIRED_MILLIONS = 2_524_935_201 / 1_000_000

func main() {
	wasm, err := os.ReadFile(os.Args[1])
	if err != nil {
		panic(err)
	}
	ctx := context.Background()
	t0 := time.Now()
	r := wazero.NewRuntime(ctx) // default = compiler (JIT) on amd64/arm64
	defer r.Close(ctx)
	wasi_snapshot_preview1.MustInstantiate(ctx, r)

	t1 := time.Now()
	compiled, err := r.CompileModule(ctx, wasm)
	if err != nil {
		panic(err)
	}
	t2 := time.Now()

	cfg := wazero.NewModuleConfig().WithArgs("bench_guest").WithStdout(os.Stdout).WithStderr(os.Stderr)
	_, err = r.InstantiateModule(ctx, compiled, cfg)
	t3 := time.Now()
	if err != nil {
		if ee, ok := err.(*sys.ExitError); !ok || ee.ExitCode() != 0 {
			panic(err)
		}
	}

	elap10 := t1.Sub(t0)
	elap21 := t2.Sub(t1)
	runElap := t3.Sub(t2)
	aotAndRunElap := t3.Sub(t1)
	aotAndRunElapNanos := aotAndRunElap

	fmt.Printf("runtime init:   %v\n", elap10)
	fmt.Printf("wasm compile:   %v    <- AOT compile happens here\n", elap21)
	fmt.Printf("run:            %v\n", runElap)
	fmt.Printf("total (cold):   %v\n", t3.Sub(t0))
	fmt.Printf("wazero wasm aot-and-run MIPS               %0.1f\n", 1e9*float64(NATIVE_RETIRED_MILLIONS)/float64(aotAndRunElapNanos))
}
