package main

import (
	"crypto/rand"
	"fmt"
	"os"
)

func main() {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	fmt.Printf("%x\n", buf)
}
