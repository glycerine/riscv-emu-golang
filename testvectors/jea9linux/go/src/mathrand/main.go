package main

import (
	"fmt"
	"math/rand"
)

func main() {
	for i := 0; i < 4; i++ {
		fmt.Println(rand.Uint64())
	}
}
