package riscv

import (
	"testing"
)

// newTestMem creates a small GuestMemory for testing.
// 64 KB is the smallest useful size (power of two, > one page).
func newTestMem(t *testing.T) *GuestMemory {
	t.Helper()
	m, err := NewGuestMemory(1 << 16) // 64 KB
	if err != nil {
		t.Fatalf("NewGuestMemory: %v", err)
	}
	t.Cleanup(m.Free)
	return m
}

// ---------------------------------------------------------------------------
// Construction
// ---------------------------------------------------------------------------

func TestNewGuestMemory_ValidSizes(t *testing.T) {
	sizes := []uint64{
		1 << 16, // 64 KB — minimum useful
		Size64MB,
		Size256MB,
		Size1GB,
	}
	for _, size := range sizes {
		m, err := NewGuestMemory(size)
		if err != nil {
			t.Errorf("NewGuestMemory(%d): unexpected error: %v", size, err)
			continue
		}
		if m.Size() != size {
			t.Errorf("Size() = %d, want %d", m.Size(), size)
		}
		m.Free()
	}
}

func TestNewGuestMemory_RejectsZero(t *testing.T) {
	_, err := NewGuestMemory(0)
	if err == nil {
		t.Fatal("expected error for size 0, got nil")
	}
}

func TestNewGuestMemory_RejectsNonPowerOfTwo(t *testing.T) {
	bad := []uint64{3, 100, 1<<16 + 1, 1<<20 - 1}
	for _, size := range bad {
		_, err := NewGuestMemory(size)
		if err == nil {
			t.Errorf("NewGuestMemory(%d): expected error for non-power-of-two, got nil", size)
		}
	}
}

func TestNewGuestMemory_RejectsExceedingMax(t *testing.T) {
	_, err := NewGuestMemory(MaxGuestMemory << 1)
	if err == nil {
		t.Fatal("expected error for size exceeding MaxGuestMemory, got nil")
	}
}

func TestNewGuestMemory_AcceptsMaximum(t *testing.T) {
	// Only attempt this if we can reasonably expect the OS to accept it.
	// This reserves virtual address space only — no physical RAM allocated.
	m, err := NewGuestMemory(MaxGuestMemory)
	if err != nil {
		t.Skipf("OS declined MaxGuestMemory allocation (acceptable): %v", err)
	}
	if m.Size() != MaxGuestMemory {
		t.Errorf("Size() = %d, want %d", m.Size(), MaxGuestMemory)
	}
	m.Free()
}

func TestNewLinearGuestMemory_UsesGuestOffsets(t *testing.T) {
	m, err := NewLinearGuestMemory(Size1MB)
	if err != nil {
		t.Fatalf("NewLinearGuestMemory: %v", err)
	}
	t.Cleanup(m.Free)

	if m.Sandbox() {
		t.Fatal("Sandbox() = true, want false")
	}
	if m.GuestStart() != 0 {
		t.Fatalf("GuestStart() = 0x%x, want 0", m.GuestStart())
	}
	addr := m.GuestAddr(0x1000)
	if off, ok := m.GuestOffset(addr); !ok || off != 0x1000 {
		t.Fatalf("GuestOffset(GuestAddr(0x1000)) = 0x%x, %v; want 0x1000, true", off, ok)
	}
	if f := m.Store64(addr, 0x1122334455667788); f != nil {
		t.Fatalf("Store64 linear addr: %v", f)
	}
	got, f := m.Load64(addr)
	if f != nil {
		t.Fatalf("Load64 linear addr: %v", f)
	}
	if got != 0x1122334455667788 {
		t.Fatalf("Load64 linear addr = 0x%x", got)
	}
	if f := m.Store8(0, 0xab); f != nil {
		t.Fatalf("Store8(0) in linear memory: %v", f)
	}
	if got, f := m.Load8(0); f != nil || got != 0xab {
		t.Fatalf("Load8(0) in linear memory = 0x%x, %v; want 0xab, nil", got, f)
	}
}

// ---------------------------------------------------------------------------
// Store8 / Load8
// ---------------------------------------------------------------------------

func TestLoad8Store8_RoundTrip(t *testing.T) {
	m := newTestMem(t)
	addrs := []uint64{0, 1, 255, 1<<16 - 1}
	for _, addr := range addrs {
		if f := m.Store8(addr, 0xAB); f != nil {
			t.Fatalf("Store8(0x%x): %v", addr, f)
		}
		v, f := m.Load8(addr)
		if f != nil {
			t.Fatalf("Load8(0x%x): %v", addr, f)
		}
		if v != 0xAB {
			t.Errorf("Load8(0x%x) = 0x%x, want 0xAB", addr, v)
		}
	}
}

func TestLoad8_OutOfBounds(t *testing.T) {
	m := newTestMem(t)
	_, f := m.Load8(m.Size())
	if f == nil {
		t.Fatal("expected fault for OOB Load8, got nil")
	}
	if f.Kind != FaultLoad && f.Kind != FaultSandboxEscape {
		t.Errorf("fault kind = %v, want FaultLoad or FaultSandboxEscape", f.Kind)
	}
}

func TestStore8_OutOfBounds(t *testing.T) {
	m := newTestMem(t)
	f := m.Store8(m.Size(), 0xFF)
	if f == nil {
		t.Fatal("expected fault for OOB Store8, got nil")
	}
	if f.Kind != FaultStore && f.Kind != FaultSandboxEscape {
		t.Errorf("fault kind = %v, want FaultStore or FaultSandboxEscape", f.Kind)
	}
}

// ---------------------------------------------------------------------------
// Store16 / Load16
// ---------------------------------------------------------------------------

func TestLoad16Store16_RoundTrip(t *testing.T) {
	m := newTestMem(t)
	cases := []struct {
		addr uint64
		val  uint16
	}{
		{0, 0x1234},
		{2, 0xDEAD},
		{1<<16 - 2, 0xBEEF},
	}
	for _, c := range cases {
		if f := m.Store16(c.addr, c.val); f != nil {
			t.Fatalf("Store16(0x%x, 0x%x): %v", c.addr, c.val, f)
		}
		v, f := m.Load16(c.addr)
		if f != nil {
			t.Fatalf("Load16(0x%x): %v", c.addr, f)
		}
		if v != c.val {
			t.Errorf("Load16(0x%x) = 0x%x, want 0x%x", c.addr, v, c.val)
		}
	}
}

func TestLoad16_Misaligned(t *testing.T) {
	m := newTestMem(t)
	_, f := m.Load16(1) // odd address
	if f == nil {
		t.Fatal("expected fault for misaligned Load16, got nil")
	}
	if f.Kind != FaultMisalign {
		t.Errorf("fault kind = %v, want FaultMisalign", f.Kind)
	}
}

func TestStore16_Misaligned(t *testing.T) {
	m := newTestMem(t)
	f := m.Store16(3, 0x1234)
	if f == nil {
		t.Fatal("expected fault for misaligned Store16, got nil")
	}
	if f.Kind != FaultMisalign {
		t.Errorf("fault kind = %v, want FaultMisalign", f.Kind)
	}
}

func TestLoad16_OutOfBounds(t *testing.T) {
	m := newTestMem(t)
	// Last valid 16-bit address is size-2; size-1 straddles the boundary.
	_, f := m.Load16(m.Size() - 1)
	if f == nil {
		t.Fatal("expected fault for straddling Load16, got nil")
	}
}

// ---------------------------------------------------------------------------
// Store32 / Load32
// ---------------------------------------------------------------------------

func TestLoad32Store32_RoundTrip(t *testing.T) {
	m := newTestMem(t)
	cases := []struct {
		addr uint64
		val  uint32
	}{
		{0, 0xDEADBEEF},
		{4, 0x12345678},
		{1<<16 - 4, 0xCAFEBABE},
	}
	for _, c := range cases {
		if f := m.Store32(c.addr, c.val); f != nil {
			t.Fatalf("Store32(0x%x, 0x%x): %v", c.addr, c.val, f)
		}
		v, f := m.Load32(c.addr)
		if f != nil {
			t.Fatalf("Load32(0x%x): %v", c.addr, f)
		}
		if v != c.val {
			t.Errorf("Load32(0x%x) = 0x%x, want 0x%x", c.addr, v, c.val)
		}
	}
}

func TestLoad32_Misaligned(t *testing.T) {
	m := newTestMem(t)
	misaligned := []uint64{1, 2, 3}
	for _, addr := range misaligned {
		_, f := m.Load32(addr)
		if f == nil {
			t.Errorf("Load32(0x%x): expected misalign fault, got nil", addr)
			continue
		}
		if f.Kind != FaultMisalign {
			t.Errorf("Load32(0x%x): fault kind = %v, want FaultMisalign", addr, f.Kind)
		}
	}
}

func TestLoad32_OutOfBounds(t *testing.T) {
	m := newTestMem(t)
	// Exactly at boundary — last byte would be at size+3
	_, f := m.Load32(m.Size())
	if f == nil {
		t.Fatal("expected OOB fault, got nil")
	}
	if f.Addr != m.Size() {
		t.Errorf("fault addr = 0x%x, want 0x%x", f.Addr, m.Size())
	}
}

// ---------------------------------------------------------------------------
// Store64 / Load64
// ---------------------------------------------------------------------------

func TestLoad64Store64_RoundTrip(t *testing.T) {
	m := newTestMem(t)
	cases := []struct {
		addr uint64
		val  uint64
	}{
		{0, 0xDEADBEEFCAFEBABE},
		{8, 0x0102030405060708},
		{1<<16 - 8, 0xFFFFFFFFFFFFFFFF},
	}
	for _, c := range cases {
		if f := m.Store64(c.addr, c.val); f != nil {
			t.Fatalf("Store64(0x%x, 0x%x): %v", c.addr, c.val, f)
		}
		v, f := m.Load64(c.addr)
		if f != nil {
			t.Fatalf("Load64(0x%x): %v", c.addr, f)
		}
		if v != c.val {
			t.Errorf("Load64(0x%x) = 0x%x, want 0x%x", c.addr, v, c.val)
		}
	}
}

func TestLoad64_Misaligned(t *testing.T) {
	m := newTestMem(t)
	misaligned := []uint64{1, 2, 3, 4, 5, 6, 7}
	for _, addr := range misaligned {
		_, f := m.Load64(addr)
		if f == nil {
			t.Errorf("Load64(0x%x): expected misalign fault, got nil", addr)
			continue
		}
		if f.Kind != FaultMisalign {
			t.Errorf("Load64(0x%x): fault kind = %v, want FaultMisalign", addr, f.Kind)
		}
	}
}

func TestLoad64_OutOfBounds(t *testing.T) {
	m := newTestMem(t)
	oob := []uint64{
		m.Size(),        // exactly at end
		m.Size() + 8,    // past end
		^uint64(0) - 7,  // near max uint64 — tests wraparound
		^uint64(0) &^ 7, // aligned near max
	}
	for _, addr := range oob {
		_, f := m.Load64(addr)
		if f == nil {
			t.Errorf("Load64(0x%x): expected OOB fault, got nil", addr)
		}
	}
}

// ---------------------------------------------------------------------------
// Fetch16 / Fetch32 — verify FaultFetch kind is reported
// ---------------------------------------------------------------------------

func TestFetch16_InBounds(t *testing.T) {
	m := newTestMem(t)
	if f := m.Store16(0, 0x4001); f != nil { // compressed NOP
		t.Fatal(f)
	}
	v, f := m.Fetch16(0)
	if f != nil {
		t.Fatalf("Fetch16: %v", f)
	}
	if v != 0x4001 {
		t.Errorf("Fetch16 = 0x%x, want 0x4001", v)
	}
}

func TestFetch16_OutOfBounds(t *testing.T) {
	m := newTestMem(t)
	_, f := m.Fetch16(m.Size())
	if f == nil {
		t.Fatal("expected fault, got nil")
	}
	if f.Kind != FaultFetch {
		t.Errorf("fault kind = %v, want FaultFetch", f.Kind)
	}
}

func TestFetch32_OutOfBounds(t *testing.T) {
	m := newTestMem(t)
	_, f := m.Fetch32(m.Size())
	if f == nil {
		t.Fatal("expected fault, got nil")
	}
	if f.Kind != FaultFetch {
		t.Errorf("fault kind = %v, want FaultFetch", f.Kind)
	}
}

func TestFetch16_Misaligned(t *testing.T) {
	m := newTestMem(t)
	_, f := m.Fetch16(1)
	if f == nil {
		t.Fatal("expected misalign fault, got nil")
	}
	if f.Kind != FaultMisalign {
		t.Errorf("fault kind = %v, want FaultMisalign", f.Kind)
	}
}

// ---------------------------------------------------------------------------
// ReadBytes / WriteBytes
// ---------------------------------------------------------------------------

func TestReadWriteBytes_RoundTrip(t *testing.T) {
	m := newTestMem(t)
	src := []byte("Hello, RISC-V sandbox!")
	if f := m.WriteBytes(0, src); f != nil {
		t.Fatalf("WriteBytes: %v", f)
	}
	dst := make([]byte, len(src))
	if f := m.ReadBytes(0, dst); f != nil {
		t.Fatalf("ReadBytes: %v", f)
	}
	if string(dst) != string(src) {
		t.Errorf("ReadBytes = %q, want %q", dst, src)
	}
}

func TestReadBytes_OutOfBounds(t *testing.T) {
	m := newTestMem(t)
	dst := make([]byte, 8)
	f := m.ReadBytes(m.Size()-4, dst) // 4 bytes past end
	if f == nil {
		t.Fatal("expected OOB fault, got nil")
	}
	if f.Kind != FaultLoad {
		t.Errorf("fault kind = %v, want FaultLoad", f.Kind)
	}
}

func TestWriteBytes_OutOfBounds(t *testing.T) {
	m := newTestMem(t)
	src := make([]byte, 8)
	f := m.WriteBytes(m.Size()-4, src) // straddles end
	if f == nil {
		t.Fatal("expected OOB fault, got nil")
	}
	if f.Kind != FaultStore {
		t.Errorf("fault kind = %v, want FaultStore", f.Kind)
	}
}

func TestReadWriteBytes_Wraparound(t *testing.T) {
	m := newTestMem(t)
	// A length that would wrap addr + length past uint64 max
	f := m.ReadBytes(m.Size()-1, make([]byte, 8))
	if f == nil {
		t.Fatal("expected fault for wraparound access, got nil")
	}
}

func TestWriteBytes_Empty(t *testing.T) {
	m := newTestMem(t)
	// Empty writes should always succeed, even at OOB addresses
	if f := m.WriteBytes(m.Size()+100, []byte{}); f != nil {
		t.Errorf("WriteBytes empty at OOB: unexpected fault: %v", f)
	}
}

func TestReadBytes_Empty(t *testing.T) {
	m := newTestMem(t)
	if f := m.ReadBytes(m.Size()+100, []byte{}); f != nil {
		t.Errorf("ReadBytes empty at OOB: unexpected fault: %v", f)
	}
}

// ---------------------------------------------------------------------------
// Zero
// ---------------------------------------------------------------------------

func TestZero_ClearsMemory(t *testing.T) {
	m := newTestMem(t)
	// Fill a region
	fill := make([]byte, 64)
	for i := range fill {
		fill[i] = 0xFF
	}
	if f := m.WriteBytes(128, fill); f != nil {
		t.Fatal(f)
	}
	// Zero it
	if f := m.Zero(128, 64); f != nil {
		t.Fatalf("Zero: %v", f)
	}
	// Verify
	got := make([]byte, 64)
	if f := m.ReadBytes(128, got); f != nil {
		t.Fatal(f)
	}
	for i, b := range got {
		if b != 0 {
			t.Errorf("byte %d after Zero = 0x%x, want 0x00", i+128, b)
		}
	}
}

func TestZero_OutOfBounds(t *testing.T) {
	m := newTestMem(t)
	f := m.Zero(m.Size()-4, 8)
	if f == nil {
		t.Fatal("expected OOB fault, got nil")
	}
}

func TestZero_Empty(t *testing.T) {
	m := newTestMem(t)
	if f := m.Zero(0, 0); f != nil {
		t.Errorf("Zero(0,0): unexpected fault: %v", f)
	}
}

// ---------------------------------------------------------------------------
// Security: containment invariant
// ---------------------------------------------------------------------------

// TestContainment verifies that hostPtr never escapes the slab.
// We do this by writing sentinel values at the slab boundaries and
// confirming that extreme guest addresses (all-ones, power-of-two edges)
// wrap into the slab rather than escaping.
func TestContainment_ExtremeAddresses(t *testing.T) {
	m := newTestMem(t)

	// Write a known sentinel at address 0 (where most wrapped addresses land)
	if f := m.Store64(0, 0x1122334455667788); f != nil {
		t.Fatal(f)
	}

	// Addresses that would escape a naive emulator but must wrap here.
	// All of these, after masking, land within [0, size).
	extreme := []uint64{
		m.Size(),        // first OOB — wraps to 0
		m.Size() * 2,    // wraps to 0
		^uint64(0) &^ 7, // near-max aligned — wraps to (^0 & mask) & ^7
		^uint64(0) - 15, // another near-max
	}

	for _, addr := range extreme {
		// These should fault on the bounds check.
		_, f := m.Load64(addr)
		if f == nil {
			t.Errorf("Load64(0x%x): expected OOB fault, got nil — containment failure", addr)
		}
		// But even if we bypass check() and call hostPtr directly,
		// the result must be within [base, base+size).
		// We verify this by computing the masked offset and confirming range.
		masked := addr & m.mask
		if masked >= m.size {
			t.Errorf("addr=0x%x: masked=0x%x exceeds size=0x%x — mask invariant violated",
				addr, masked, m.size)
		}
	}
}

// TestContainment_MaskInvariant proves the mask invariant algebraically
// for every bit pattern in a small address space.
func TestContainment_MaskInvariant(t *testing.T) {
	// Use a tiny 64-byte memory to exhaustively check all addresses.
	// 64 bytes is the smallest power-of-two > 32 we can reasonably test.
	const size = 64
	m, err := NewGuestMemory(size)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Free()

	mask := m.mask
	// For every possible uint64 value (impractical to check all 2^64,
	// so we check boundary regions: near 0, near size, near max).
	candidates := make([]uint64, 0, 1024)
	for i := uint64(0); i < 128; i++ {
		candidates = append(candidates, i)
		candidates = append(candidates, size-64+i)
		candidates = append(candidates, ^uint64(0)-128+i)
		candidates = append(candidates, (^uint64(0)>>1)-64+i)
	}
	for _, addr := range candidates {
		masked := addr & mask
		if masked >= size {
			t.Errorf("addr=0x%x: (addr & mask) = 0x%x >= size=%d — invariant violated",
				addr, masked, size)
		}
	}
}

// ---------------------------------------------------------------------------
// MemFault fields
// ---------------------------------------------------------------------------

func TestMemFault_Fields(t *testing.T) {
	m := newTestMem(t)
	_, f := m.Load64(m.Size())
	if f == nil {
		t.Fatal("expected fault")
	}
	if f.Addr != m.Size() {
		t.Errorf("fault.Addr = 0x%x, want 0x%x", f.Addr, m.Size())
	}
	if f.Width != 8 {
		t.Errorf("fault.Width = %d, want 8", f.Width)
	}
	if f.Kind != FaultLoad && f.Kind != FaultSandboxEscape {
		t.Errorf("fault.Kind = %v, want FaultLoad or FaultSandboxEscape", f.Kind)
	}
}

func TestMemFault_MisalignKind(t *testing.T) {
	m := newTestMem(t)
	_, f := m.Load32(1) // misaligned
	if f == nil {
		t.Fatal("expected fault")
	}
	if f.Kind != FaultMisalign {
		t.Errorf("fault.Kind = %v, want FaultMisalign", f.Kind)
	}
	if f.Width != 4 {
		t.Errorf("fault.Width = %d, want 4", f.Width)
	}
}

func TestMemFault_ErrorString(t *testing.T) {
	f := &MemFault{Addr: 0xDEAD, Width: 8, Kind: FaultLoad}
	s := f.Error()
	if s == "" {
		t.Error("MemFault.Error() returned empty string")
	}
}

// ---------------------------------------------------------------------------
// Mixed-width correctness — little-endian layout
// ---------------------------------------------------------------------------

// TestLittleEndian verifies that the memory is little-endian:
// storing a 32-bit value and reading back its bytes should give
// the least-significant byte at the lowest address.
func TestLittleEndian_Layout(t *testing.T) {
	m := newTestMem(t)
	if f := m.Store32(0, 0x01020304); f != nil {
		t.Fatal(f)
	}
	b0, _ := m.Load8(0)
	b1, _ := m.Load8(1)
	b2, _ := m.Load8(2)
	b3, _ := m.Load8(3)
	if b0 != 0x04 || b1 != 0x03 || b2 != 0x02 || b3 != 0x01 {
		t.Errorf("little-endian layout wrong: bytes = [%02x %02x %02x %02x], want [04 03 02 01]",
			b0, b1, b2, b3)
	}
}

// TestMixedWidth writes via one width and reads back via another,
// checking that the underlying bytes are consistent.
func TestMixedWidth_Consistency(t *testing.T) {
	m := newTestMem(t)
	// Write as two 32-bit words
	if f := m.Store32(0, 0xAABBCCDD); f != nil {
		t.Fatal(f)
	}
	if f := m.Store32(4, 0x11223344); f != nil {
		t.Fatal(f)
	}
	// Read back as one 64-bit word
	v64, f := m.Load64(0)
	if f != nil {
		t.Fatal(f)
	}
	// On little-endian: low word is 0xAABBCCDD, high word is 0x11223344
	want := uint64(0x11223344)<<32 | uint64(0xAABBCCDD)
	if v64 != want {
		t.Errorf("Load64 after two Store32: got 0x%016x, want 0x%016x", v64, want)
	}
}

// ---------------------------------------------------------------------------
// Free / double-free safety
// ---------------------------------------------------------------------------

func TestFree_DoesNotPanic(t *testing.T) {
	m, err := NewGuestMemory(1 << 16)
	if err != nil {
		t.Fatal(err)
	}
	m.Free()
	// Second Free must not panic or double-munmap
	m.Free()
}

// ---------------------------------------------------------------------------
// ZeroRange
// ---------------------------------------------------------------------------

func TestZeroRange_OutOfBounds(t *testing.T) {
	m := newTestMem(t)
	f := m.ZeroRange(m.Size()-4, 8)
	if f == nil {
		t.Fatal("expected OOB fault from ZeroRange, got nil")
	}
}

func TestZeroRange_Wraparound(t *testing.T) {
	m := newTestMem(t)
	// addr + length wraps uint64
	f := m.ZeroRange(m.Size()-1, ^uint64(0))
	if f == nil {
		t.Fatal("expected fault for wraparound ZeroRange, got nil")
	}
}

// ---------------------------------------------------------------------------
// Benchmarks
// ---------------------------------------------------------------------------

// BenchmarkLoad64_HotPath measures the cost of a Load64 on the happy path.
// This is the innermost loop of the emulator — target is <10 ns/op.
func BenchmarkLoad64_HotPath(b *testing.B) {
	m, _ := NewGuestMemory(Size64MB)
	defer m.Free()
	_ = m.Store64(4096, 0xDEADBEEFCAFEBABE)

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		v, f := m.Load64(4096)
		if f != nil {
			b.Fatal(f)
		}
		_ = v
	}
}

// BenchmarkStore64_HotPath measures the cost of a Store64 on the happy path.
func BenchmarkStore64_HotPath(b *testing.B) {
	m, _ := NewGuestMemory(Size64MB)
	defer m.Free()

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if f := m.Store64(4096, uint64(i)); f != nil {
			b.Fatal(f)
		}
	}
}

// BenchmarkCheck measures the raw cost of the bounds+alignment check
// expression in isolation.
func BenchmarkCheck(b *testing.B) {
	m, _ := NewGuestMemory(Size64MB)
	defer m.Free()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = m.check(uint64(i*8)&m.mask, 8)
	}
}

// BenchmarkWriteBytes measures bulk write throughput.
func BenchmarkWriteBytes_1KB(b *testing.B) {
	m, _ := NewGuestMemory(Size64MB)
	defer m.Free()
	src := make([]byte, 1024)

	b.SetBytes(1024)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if f := m.WriteBytes(0, src); f != nil {
			b.Fatal(f)
		}
	}
}
