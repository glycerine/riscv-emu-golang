//go:build !linux || !riscv64

package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "rstrace only runs on linux/riscv64")
	os.Exit(1)
}
