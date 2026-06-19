package riscv

import (
	"os"
	"path/filepath"
)

const defaultEmunetSubdir = ".emunet"

var emunetDirOverride string
var emunetStateDirOverride string

func emunetDir() string {
	if emunetDirOverride != "" {
		return emunetDirOverride
	}
	if home := os.Getenv("HOME"); home != "" {
		return filepath.Join(home, defaultEmunetSubdir)
	}
	return filepath.Join(os.TempDir(), defaultEmunetSubdir)
}

func emunetStateDir() string {
	if emunetStateDirOverride != "" {
		return emunetStateDirOverride
	}
	if home := os.Getenv("HOME"); home != "" {
		return filepath.Join(home, ".local", "state", "emunet")
	}
	return filepath.Join(os.TempDir(), ".local", "state", "emunet")
}
