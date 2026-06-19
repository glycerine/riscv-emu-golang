package main

import (
	"os"
	"path/filepath"
)

const defaultEmunetSubdir = ".emunet"

func emunetDir() string {
	if home := os.Getenv("HOME"); home != "" {
		return filepath.Join(home, defaultEmunetSubdir)
	}
	return filepath.Join(os.TempDir(), defaultEmunetSubdir)
}
