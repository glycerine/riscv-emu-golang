package riscv

import (
	"os"
	"testing"
)

func TestHostIOErrnoUsesLinuxABIForCommonErrors(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want uint32
	}{
		{name: "not-exist", err: os.ErrNotExist, want: hostIOErrENOENT},
		{name: "permission", err: os.ErrPermission, want: hostIOErrEACCES},
		{name: "exist", err: os.ErrExist, want: hostIOErrEEXIST},
		{name: "invalid", err: os.ErrInvalid, want: hostIOErrEINVAL},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hostIOErrno(tt.err); got != tt.want {
				t.Fatalf("hostIOErrno(%v) = %d, want %d", tt.err, got, tt.want)
			}
		})
	}
}
