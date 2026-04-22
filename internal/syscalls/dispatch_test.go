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
	if NullWriteCallbackAddr() == 0 {
		t.Fatal("NullWriteCallbackAddr() returned 0")
	}
}

// TestDispatchNullCallback verifies that when the null callback is
// registered, SYS_write takes the callback path — no kernel syscall
// happens (we prove this by using a fd that would fail if written:
// fd = -1), and x[10] ends up equal to count.
func TestDispatchNullCallback(t *testing.T) {
	RegisterWriteCallback(NullWriteCallbackAddr())
	defer RegisterWriteCallback(0)

	var regs [32]uint64
	guestMem := make([]byte, 4096)
	regs[17] = 64
	regs[10] = ^uint64(0) // fd = -1, would EBADF in kernel; callback ignores it
	regs[11] = 0x100
	regs[12] = 42 // arbitrary count

	memBase := uintptr(unsafe.Pointer(&guestMem[0]))
	memMask := uint64(len(guestMem) - 1)

	ret := CallDispatch(unsafe.Pointer(&regs[0]), memBase, memMask)
	if ret != 0 {
		t.Fatalf("CallDispatch returned %d, want 0 (handled)", ret)
	}
	if regs[10] != 42 {
		t.Fatalf("x[10] after null callback = %d, want 42 (count)", int64(regs[10]))
	}
}

// TestDispatchClearCallback verifies that setting the callback back
// to 0 restores the SYSCALL fast path.
func TestDispatchClearCallback(t *testing.T) {
	// First install null cb so we know state can be toggled.
	RegisterWriteCallback(NullWriteCallbackAddr())
	RegisterWriteCallback(0) // clear

	// With callback cleared, a write to an invalid fd should flow
	// through the kernel and get back -EBADF (a large negative as u64).
	var regs [32]uint64
	guestMem := make([]byte, 4096)
	regs[17] = 64
	regs[10] = ^uint64(0) // -1
	regs[11] = 0x100
	regs[12] = 4

	memBase := uintptr(unsafe.Pointer(&guestMem[0]))
	memMask := uint64(len(guestMem) - 1)

	ret := CallDispatch(unsafe.Pointer(&regs[0]), memBase, memMask)
	if ret != 0 {
		t.Fatalf("CallDispatch returned %d, want 0", ret)
	}
	// Expect kernel to have returned an error (negative ssize_t cast
	// to uint64 → high bit set). Just check it's not equal to count.
	if regs[10] == 4 {
		t.Fatalf("x[10] = count after clearing callback; callback still active")
	}
}
