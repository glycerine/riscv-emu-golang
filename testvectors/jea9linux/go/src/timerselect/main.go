package main

import (
	"fmt"
	"os"
	"time"
)

func main() {
	start := time.Now()
	timer := time.NewTimer(2 * time.Millisecond)
	<-timer.C
	elapsed := time.Since(start)
	if elapsed < 2*time.Millisecond {
		fmt.Printf("early_ns=%d\n", elapsed.Nanoseconds())
		os.Exit(1)
	}
	fmt.Printf("elapsed_ms=%d\n", elapsed.Milliseconds())
}
