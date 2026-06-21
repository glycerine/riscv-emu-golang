package riscv

import (
	"bytes"
	"reflect"
	"testing"
)

func runJea9LinuxTraceFixture(t *testing.T) (*GuestMemory, *Jea9Linux, int) {
	t.Helper()
	const (
		codeVA = uint64(0x1000)
		tsVA   = uint64(0x5000)
		bufVA  = uint64(0x6000)
	)
	insns := []uint32{
		ienc(opOPIMM, 0, 10, 0, 1),    // a0 = CLOCK_MONOTONIC
		uenc(opLUI, 11, uint32(tsVA)), // a1 = timespec
		ienc(opOPIMM, 0, 17, 0, 113),  // a7 = clock_gettime
		instrECALL,
		uenc(opLUI, 10, uint32(bufVA)), // a0 = random buffer
		ienc(opOPIMM, 0, 11, 0, 4),     // a1 = len
		ienc(opOPIMM, 0, 12, 0, 0),     // a2 = flags
		ienc(opOPIMM, 0, 17, 0, 278),   // a7 = getrandom
		instrECALL,
		ienc(opOPIMM, 0, 10, 0, 7),  // a0 = exit code
		ienc(opOPIMM, 0, 17, 0, 93), // a7 = exit
		instrECALL,
	}
	cpu, mem := newTestCPU(t, Size64MB, codeVA, insns)
	j := NewJea9Linux(Jea9LinuxOptions{
		EntropySeed:       []byte("trace seed"),
		MonotonicStartNS:  44,
		InstructionBudget: 2,
		Trace:             true,
		Scheduler: Jea9LinuxSchedulerConfig{
			MinQuantumRetired: 2,
			MaxQuantumRetired: 2,
		},
	})
	code, err := RunWithJea9LinuxInterp(cpu, j)
	if err != nil {
		mem.Free()
		t.Fatalf("RunWithJea9LinuxInterp: %v", err)
	}
	return mem, j, code
}

func TestJea9Linux_TraceRecordsSyscallsScheduleRandomAndClock(t *testing.T) {
	mem, j, code := runJea9LinuxTraceFixture(t)
	defer mem.Free()
	if code != 7 {
		t.Fatalf("exit code = %d, want 7", code)
	}

	trace := j.TraceSnapshot()
	requireTraceSyscall(t, trace, jea9TestSysClockGettime)
	requireTraceSyscall(t, trace, jea9TestSysGetrandom)
	requireTraceSyscall(t, trace, jea9LinuxSysExit)
	if len(trace.Schedule) == 0 {
		t.Fatal("schedule trace is empty, want scheduler-quantum records")
	}
	if got := trace.Schedule[0].Event; got != "quantum" {
		t.Fatalf("schedule event = %q, want quantum", got)
	}
	if got := trace.Schedule[0].Reason; got != "quantum" {
		t.Fatalf("schedule reason = %q, want quantum", got)
	}
	if got := trace.Schedule[0].QuantumRetired; got != 2 {
		t.Fatalf("schedule quantum retired = %d, want 2", got)
	}
	if got := trace.Schedule[0].FromPriority; got != "high" {
		t.Fatalf("schedule from priority = %q, want high", got)
	}
	if got := trace.Schedule[0].ToPriority; got != "high" {
		t.Fatalf("schedule to priority = %q, want high", got)
	}
	if got := trace.Schedule[0].ClockPolicy; got != ClockPolicyOnlyDeadlockAdvances.String() {
		t.Fatalf("schedule clock policy = %q, want %q", got, ClockPolicyOnlyDeadlockAdvances.String())
	}
	if len(trace.Random) != 1 {
		t.Fatalf("random observations = %d, want 1", len(trace.Random))
	}
	randomBytes := readGuestBytes(t, mem, 0x6000, 4)
	if !bytes.Equal(trace.Random[0].Bytes, randomBytes) {
		t.Fatalf("random trace bytes = %x, guest bytes = %x", trace.Random[0].Bytes, randomBytes)
	}
	if len(trace.Clock) != 1 {
		t.Fatalf("clock observations = %d, want 1", len(trace.Clock))
	}
	if got := trace.Clock[0].NS; got != 44 {
		t.Fatalf("clock observation ns = %d, want 44", got)
	}
}

func TestJea9Linux_TraceReplayIdentical(t *testing.T) {
	firstMem, firstOS, firstCode := runJea9LinuxTraceFixture(t)
	defer firstMem.Free()
	secondMem, secondOS, secondCode := runJea9LinuxTraceFixture(t)
	defer secondMem.Free()
	if firstCode != secondCode {
		t.Fatalf("exit code replay mismatch: %d != %d", firstCode, secondCode)
	}
	first := firstOS.TraceSnapshot()
	second := secondOS.TraceSnapshot()
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("trace replay mismatch:\nfirst=%+v\nsecond=%+v", first, second)
	}
}

func TestJea9Linux_TraceSnapshotDeepCopy(t *testing.T) {
	mem, j, _ := runJea9LinuxTraceFixture(t)
	defer mem.Free()

	snap := j.TraceSnapshot()
	if len(snap.Syscalls) == 0 || len(snap.Schedule) == 0 || len(snap.Random) == 0 || len(snap.Clock) == 0 {
		t.Fatalf("trace snapshot missing coverage data: %+v", snap)
	}
	snap.Syscalls[0].Num = 0
	snap.Schedule[0].Event = "mutated"
	snap.Random[0].Bytes[0] ^= 0xff
	snap.Clock[0].NS = -1

	again := j.TraceSnapshot()
	if again.Syscalls[0].Num == 0 {
		t.Fatal("TraceSnapshot returned aliased syscall records")
	}
	if again.Schedule[0].Event == "mutated" {
		t.Fatal("TraceSnapshot returned aliased schedule records")
	}
	if bytes.Equal(again.Random[0].Bytes, snap.Random[0].Bytes) {
		t.Fatal("TraceSnapshot returned aliased random bytes")
	}
	if again.Clock[0].NS == -1 {
		t.Fatal("TraceSnapshot returned aliased clock records")
	}
}

func TestJea9Linux_TraceDisabledByDefault(t *testing.T) {
	const codeVA = uint64(0x1000)
	insns := []uint32{
		ienc(opOPIMM, 0, 10, 0, 0),  // a0 = exit code
		ienc(opOPIMM, 0, 17, 0, 93), // a7 = exit
		instrECALL,
	}
	cpu, mem := newTestCPU(t, Size64MB, codeVA, insns)
	defer mem.Free()
	j := NewJea9Linux(Jea9LinuxOptions{})
	code, err := RunWithJea9LinuxInterp(cpu, j)
	if err != nil {
		t.Fatalf("RunWithJea9LinuxInterp: %v", err)
	}
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	trace := j.TraceSnapshot()
	if len(trace.Syscalls) != 0 || len(trace.Schedule) != 0 ||
		len(trace.Random) != 0 || len(trace.Clock) != 0 {
		t.Fatalf("trace enabled by default: %+v", trace)
	}
}

func requireTraceSyscall(t *testing.T, trace Jea9LinuxTraceSnapshot, num uint64) {
	t.Helper()
	for _, rec := range trace.Syscalls {
		if rec.Num == num {
			return
		}
	}
	t.Fatalf("syscall trace missing syscall %d: %+v", num, trace.Syscalls)
}
