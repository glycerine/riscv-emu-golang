//go:build !amd64

package fenv

func SetRoundingMode(rm uint8) (uint32, bool) {
	return 0, rm == 0
}

func RestoreRoundingMode(uint32) {}
