//go:build !amd64

package fenv

func FFlags() uint32 { return 0 }
func ClearFFlags()   {}

func RawMXCSR() uint32 { return 0 }
