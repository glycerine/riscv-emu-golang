package riscv

import (
	"testing"
	"time"
)

func installFakeEmunetForLinuxSmoke(t *testing.T) string {
	t.Helper()
	t.Setenv("RPC25519_SERVER_DATA_DIR", t.TempDir())
	setTestEmunetHome(t, t.TempDir())
	installFakeEmunetLeaderHook(t, 20*time.Millisecond)
	return reserveTestEmunetAddr(t)
}
