package abjit

import (
	"runtime"
	"testing"
	"unsafe"
)

var callbackFlag bool

//go:nosplit
func nosplitCallback() {
	callbackFlag = true
}

func gcCallback() {
	callbackFlag = true
	runtime.GC()
}

// ---------------------------------------------------------------------------
// Phase 0 tests (updated for 2-arg callJIT + RBP-based access)
// ---------------------------------------------------------------------------

func TestBasicEntryExit(t *testing.T) {
	cb, err := NewCodeBuilder()
	if err != nil {
		t.Fatal(err)
	}
	defer cb.Free()

	cb.Exit()

	state := NewState()
	callJIT(cb.Addr(), state.RegFileBase())
	t.Log("basic entry/exit OK")
}

func TestCallback(t *testing.T) {
	cb, err := NewCodeBuilder()
	if err != nil {
		t.Fatal(err)
	}
	defer cb.Free()

	callbackFlag = false
	cb.Callback(funcAddr(nosplitCallback))
	cb.Exit()

	state := NewState()
	callJIT(cb.Addr(), state.RegFileBase())
	if !callbackFlag {
		t.Fatal("callback was not called")
	}
}

func TestStoreNoCallback(t *testing.T) {
	cb, err := NewCodeBuilder()
	if err != nil {
		t.Fatal(err)
	}
	defer cb.Free()

	cb.Movabs(RBX, 0xAAAAAAAAAAAAAAAA)
	cb.StoreToRBP(RBX, 0)
	cb.Movabs(R8, 0x1234567812345678)
	cb.StoreToRBP(R8, 8)
	cb.Exit()

	state := NewState()
	callJIT(cb.Addr(), state.RegFileBase())

	if state.X[0] != 0xAAAAAAAAAAAAAAAA {
		t.Errorf("X[0] = 0x%X, want 0xAAAAAAAAAAAAAAAA", state.X[0])
	}
	if state.X[1] != 0x1234567812345678 {
		t.Errorf("X[1] = 0x%X, want 0x1234567812345678", state.X[1])
	}
}

func TestAbsoluteStore(t *testing.T) {
	cb, err := NewCodeBuilder()
	if err != nil {
		t.Fatal(err)
	}
	defer cb.Free()

	state := NewState()
	stateAddr := uint64(state.RegFileBase())

	cb.Movabs(RAX, stateAddr)
	cb.Movabs(RCX, 0xDEADBEEFDEADBEEF)
	cb.emit(0x48, 0x89, 0x08) // MOV [RAX], RCX
	cb.Exit()

	callJIT(cb.Addr(), state.RegFileBase())
	if state.X[0] != 0xDEADBEEFDEADBEEF {
		t.Errorf("X[0] = 0x%X, want 0xDEADBEEFDEADBEEF", state.X[0])
	}
}

func TestDumpAndVerify(t *testing.T) {
	cb, err := NewCodeBuilder()
	if err != nil {
		t.Fatal(err)
	}
	defer cb.Free()

	state := NewState()
	stateAddr := uint64(state.RegFileBase())

	cb.Movabs(RAX, stateAddr)
	cb.Movabs(RCX, 0xDEADBEEFDEADBEEF)
	cb.emit(0x48, 0x89, 0x08) // MOV [RAX], RCX
	cb.Exit()

	t.Logf("code (%d bytes): % x", cb.Len(), cb.buf[:cb.Len()])

	callJIT(cb.Addr(), state.RegFileBase())
	if state.X[0] != 0xDEADBEEFDEADBEEF {
		t.Errorf("X[0] = 0x%X, want 0xDEADBEEFDEADBEEF", state.X[0])
	}
}

func TestCalleeSaveVerification(t *testing.T) {
	cb, err := NewCodeBuilder()
	if err != nil {
		t.Fatal(err)
	}
	defer cb.Free()

	const (
		sentRBX = 0xAAAAAAAAAAAAAAAA
		sentR13 = 0xBBBBBBBBBBBBBBBB
		sentR15 = 0xCCCCCCCCCCCCCCCC
		sentR12 = 0xDDDDDDDDDDDDDDDD
	)

	callbackFlag = false
	cb.Movabs(RBX, sentRBX)
	cb.Movabs(R13, sentR13)
	cb.Movabs(R15, sentR15)
	cb.Movabs(R12, sentR12)

	cb.Callback(funcAddr(nosplitCallback))

	cb.StoreToRBP(RBX, 0)
	cb.StoreToRBP(R13, 8)
	cb.StoreToRBP(R15, 16)
	cb.StoreToRBP(R12, 24)
	cb.Exit()

	state := NewState()
	callJIT(cb.Addr(), state.RegFileBase())

	if !callbackFlag {
		t.Fatal("callback was not called")
	}
	check := func(name string, got, want uint64) {
		if got != want {
			t.Errorf("%s not preserved: got 0x%X, want 0x%X", name, got, want)
		}
	}
	check("RBX", state.X[0], sentRBX)
	check("R13", state.X[1], sentR13)
	check("R15", state.X[2], sentR15)
	check("R12", state.X[3], sentR12)
}

func TestGCSafety(t *testing.T) {
	cb, err := NewCodeBuilder()
	if err != nil {
		t.Fatal(err)
	}
	defer cb.Free()

	callbackFlag = false
	cb.Callback(funcAddr(gcCallback))
	cb.Exit()

	state := NewState()
	callJIT(cb.Addr(), state.RegFileBase())
	if !callbackFlag {
		t.Fatal("callback was not called")
	}
}

func TestGCSafetyStress(t *testing.T) {
	cb, err := NewCodeBuilder()
	if err != nil {
		t.Fatal(err)
	}
	defer cb.Free()

	cb.Callback(funcAddr(gcCallback))
	cb.Exit()

	state := NewState()
	for i := 0; i < 100; i++ {
		callbackFlag = false
		callJIT(cb.Addr(), state.RegFileBase())
		if !callbackFlag {
			t.Fatalf("iteration %d: callback not called", i)
		}
	}
}

// ---------------------------------------------------------------------------
// Phase 2 new tests
// ---------------------------------------------------------------------------

func TestRBPValid(t *testing.T) {
	cb, err := NewCodeBuilder()
	if err != nil {
		t.Fatal(err)
	}
	defer cb.Free()

	cb.MovRegReg(RAX, RBP)
	cb.StoreToRBP(RAX, 0)
	cb.Exit()

	state := NewState()
	rfBase := state.RegFileBase()
	callJIT(cb.Addr(), rfBase)

	if state.X[0] != uint64(rfBase) {
		t.Errorf("RBP = 0x%X, want 0x%X", state.X[0], rfBase)
	}
}

func TestRBPPreservedAcrossCallback(t *testing.T) {
	cb, err := NewCodeBuilder()
	if err != nil {
		t.Fatal(err)
	}
	defer cb.Free()

	cb.MovRegReg(RAX, RBP)
	cb.StoreToRBP(RAX, 0)

	callbackFlag = false
	cb.Callback(funcAddr(nosplitCallback))

	cb.MovRegReg(RAX, RBP)
	cb.StoreToRBP(RAX, 8)

	cb.Exit()

	state := NewState()
	rfBase := state.RegFileBase()
	callJIT(cb.Addr(), rfBase)

	if !callbackFlag {
		t.Fatal("callback not called")
	}
	if state.X[0] != uint64(rfBase) {
		t.Errorf("RBP before callback: 0x%X, want 0x%X", state.X[0], rfBase)
	}
	if state.X[1] != uint64(rfBase) {
		t.Errorf("RBP after callback: 0x%X, want 0x%X", state.X[1], rfBase)
	}
}

func TestStateLayout(t *testing.T) {
	var s State
	checks := []struct {
		name string
		got  uintptr
		want uintptr
	}{
		{"X", unsafe.Offsetof(s.X), 0},
		{"F", unsafe.Offsetof(s.F), 256},
		{"FCSR", unsafe.Offsetof(s.FCSR), 512},
		{"MemBase", unsafe.Offsetof(s.MemBase), 520},
		{"MemMask", unsafe.Offsetof(s.MemMask), 528},
		{"PC", unsafe.Offsetof(s.PC), 536},
		{"IC", unsafe.Offsetof(s.IC), 544},
		{"Status", unsafe.Offsetof(s.Status), 552},
		{"FaultAddr", unsafe.Offsetof(s.FaultAddr), 560},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s offset = %d, want %d", c.name, c.got, c.want)
		}
	}
}

func TestLoadFromRBP(t *testing.T) {
	cb, err := NewCodeBuilder()
	if err != nil {
		t.Fatal(err)
	}
	defer cb.Free()

	cb.LoadFromRBP(RAX, 5*8)
	cb.StoreToRBP(RAX, 0)
	cb.Exit()

	state := NewState()
	state.X[5] = 0x42
	callJIT(cb.Addr(), state.RegFileBase())

	if state.X[0] != 0x42 {
		t.Errorf("X[0] = 0x%X, want 0x42", state.X[0])
	}
}

func TestAddReg(t *testing.T) {
	cb, err := NewCodeBuilder()
	if err != nil {
		t.Fatal(err)
	}
	defer cb.Free()

	cb.LoadFromRBP(RAX, 10*8)
	cb.LoadFromRBP(RCX, 11*8)
	cb.AddReg(RAX, RCX)
	cb.StoreToRBP(RAX, 0)
	cb.Exit()

	state := NewState()
	state.X[10] = 100
	state.X[11] = 200
	callJIT(cb.Addr(), state.RegFileBase())

	if state.X[0] != 300 {
		t.Errorf("X[0] = %d, want 300", state.X[0])
	}
}

func TestRunAPI(t *testing.T) {
	cb, err := NewCodeBuilder()
	if err != nil {
		t.Fatal(err)
	}
	defer cb.Free()

	cb.LoadFromRBP(RAX, 1*8)
	cb.LoadFromRBP(RCX, 2*8)
	cb.AddReg(RAX, RCX)
	cb.StoreToRBP(RAX, 0)
	cb.Exit()

	state := NewState()
	state.X[1] = 7
	state.X[2] = 35
	Run(cb, state)

	if state.X[0] != 42 {
		t.Errorf("X[0] = %d, want 42", state.X[0])
	}
}

// ---------------------------------------------------------------------------
// Benchmarks
// ---------------------------------------------------------------------------

func BenchmarkTrampolineOverhead(b *testing.B) {
	cb, err := NewCodeBuilder()
	if err != nil {
		b.Fatal(err)
	}
	defer cb.Free()

	cb.Exit()

	state := NewState()
	codeAddr := cb.Addr()
	rfBase := state.RegFileBase()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		callJIT(codeAddr, rfBase)
	}
}

func BenchmarkCallbackRoundTrip(b *testing.B) {
	cb, err := NewCodeBuilder()
	if err != nil {
		b.Fatal(err)
	}
	defer cb.Free()

	cb.Callback(funcAddr(nosplitCallback))
	cb.Exit()

	state := NewState()
	codeAddr := cb.Addr()
	rfBase := state.RegFileBase()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		callJIT(codeAddr, rfBase)
	}
}
