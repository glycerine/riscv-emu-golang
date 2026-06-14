package main

import (
	"fmt"
	"runtime"
	"time"
)

var sink uint64

func main() {
	runtime.GOMAXPROCS(1)
	go func() {
		var x uint64
		for {
			x++
			sink = x
		}
	}()
	time.Sleep(5 * time.Millisecond)
	fmt.Print("sigurg_preempt_ok\n")
}
