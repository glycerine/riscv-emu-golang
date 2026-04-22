package syscalls

import (
	"bytes"
	"os"
	"testing"
	"unsafe"
)

// TestDispatchWrite calls the dispatcher directly and verifies a
// host write(2) actually landed on a pipe we own.
func TestDispatchWrite(t *testing.T) {
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Close()
	defer pw.Close()

	var regs [32]uint64
	msg := []byte("hi!\n")
	guestMem := make([]byte, 4096)
	copy(guestMem[0x100:], msg)

	regs[17] = 64                          // a7 = SYS_write (RV ABI)
	regs[10] = uint64(pw.Fd())             // a0 = fd
	regs[11] = 0x100                       // a1 = guest buf VA
	regs[12] = uint64(len(msg))            // a2 = count

	memBase := uintptr(unsafe.Pointer(&guestMem[0]))
	memMask := uint64(len(guestMem) - 1)

	ret := CallDispatch(unsafe.Pointer(&regs[0]), memBase, memMask)
	if ret != 0 {
		t.Fatalf("CallDispatch returned %d, want 0 (handled)", ret)
	}
	if int(int64(regs[10])) != len(msg) {
		t.Fatalf("x[10] after syscall = %d, want %d", int64(regs[10]), len(msg))
	}

	got := make([]byte, len(msg))
	n, err := pr.Read(got)
	if err != nil {
		t.Fatal(err)
	}
	if n != len(msg) || !bytes.Equal(got[:n], msg) {
		t.Fatalf("pipe got %q, want %q", got[:n], msg)
	}
}

// TestDispatchFallback verifies an unknown syscall returns 1 and
// leaves x[10] unchanged.
func TestDispatchFallback(t *testing.T) {
	var regs [32]uint64
	regs[17] = 0xDEAD
	regs[10] = 0xBEEF

	ret := CallDispatch(unsafe.Pointer(&regs[0]), 0, 0)
	if ret != 1 {
		t.Fatalf("CallDispatch returned %d, want 1 (fallback)", ret)
	}
	if regs[10] != 0xBEEF {
		t.Fatalf("x[10] modified in fallback path: %#x", regs[10])
	}
}

// TestDispatchAddr sanity-checks that DispatchAddr returns a plausible
// nonzero code-section pointer.
func TestDispatchAddr(t *testing.T) {
	if DispatchAddr() == 0 {
		t.Fatal("DispatchAddr() returned 0")
	}
}
