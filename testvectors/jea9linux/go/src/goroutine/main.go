package main

import "fmt"

func main() {
	ch := make(chan int)
	go func() {
		ch <- 41
	}()
	fmt.Println(<-ch + 1)
}
