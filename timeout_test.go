package riscv

import (
	"os"
	"testing"
	"time"
)

// TestMain enforces a hard timeout on the entire test suite.
// The limit is generous enough for fuzz baseline coverage gathering,
// but tight enough to catch infinite loops in the CPU step() function.
//
// Regular `go test` completes in ~100ms.
// `go test -fuzz` runs indefinitely but only passes through TestMain
// once for the baseline phase — fuzz iterations run in a child process.
func TestMain(m *testing.M) {
	timeout := 30 * time.Second
	// Shrink to 10s when not fuzzing (fast unit test path)
	if os.Getenv("FUZZ_TIMEOUT") == "" {
		timeout = 10 * time.Second
	}

	done := make(chan int, 1)
	go func() { done <- m.Run() }()

	select {
	case code := <-done:
		os.Exit(code)
	case <-time.After(timeout):
		panic("test suite exceeded timeout — likely an infinite loop in CPU")
	}
}
