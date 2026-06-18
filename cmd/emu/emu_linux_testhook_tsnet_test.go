//go:build tsnet

package main

import (
	"testing"
	"time"
)

func installFakeEmunetForLinuxSmoke(t *testing.T) {
	t.Helper()
	t.Setenv("RPC25519_SERVER_DATA_DIR", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("RISCV_EMU_EMUNET_ADDR", reserveTestEmunetAddr(t))
	installFakeEmunetLeaderHook(t, 20*time.Millisecond)
}
