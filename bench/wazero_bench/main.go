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

	fmt.Printf("runtime init:   %v\n", t1.Sub(t0))
	fmt.Printf("wasm compile:   %v    <- JIT happens here\n", t2.Sub(t1))
	fmt.Printf("run:            %v\n", t3.Sub(t2))
	fmt.Printf("total (cold):   %v\n", t3.Sub(t0))
}
