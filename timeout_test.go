package riscv

import (
	"os"
	"testing"
	"time"
)

// TestMain enforces a hard 3-second timeout on the entire test suite.
// Any infinite loop in the CPU will trip this rather than hanging forever.
func TestMain(m *testing.M) {
	done := make(chan int, 1)
	go func() { done <- m.Run() }()

	select {
	case code := <-done:
		os.Exit(code)
	case <-time.After(3 * time.Second):
		panic("test suite exceeded 3s — likely an infinite loop in CPU")
	}
}
