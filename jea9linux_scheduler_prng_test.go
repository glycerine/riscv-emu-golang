package riscv

import (
	"bytes"
	"errors"
	"reflect"
	"testing"
)

func TestJea9Linux_SchedulerPRNGStateInitializesDeterministically(t *testing.T) {
	first := NewJea9Linux(Jea9LinuxOptions{EntropySeed: []byte("scheduler seed")})
	second := NewJea9Linux(Jea9LinuxOptions{EntropySeed: []byte("scheduler seed")})
	third := NewJea9Linux(Jea9LinuxOptions{EntropySeed: []byte("different scheduler seed")})

	firstState := first.SchedulerPRNGState()
	secondState := second.SchedulerPRNGState()
	thirdState := third.SchedulerPRNGState()

	if len(firstState) == 0 {
		t.Fatal("initial scheduler PRNG snapshot is empty")
	}
	if !bytes.Equal(firstState, secondState) {
		t.Fatalf("same seed scheduler PRNG snapshots differ:\nfirst=%x\nsecond=%x", firstState, secondState)
	}
	if bytes.Equal(firstState, thirdState) {
		t.Fatalf("different seed produced identical scheduler PRNG snapshot: %x", firstState)
	}
}

func TestJea9Linux_SchedulerPRNGStateReadDoesNotAdvance(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{EntropySeed: []byte("read-only scheduler state")})

	before := j.SchedulerPRNGState()
	drawsBefore := j.SchedulerPRNGDraws()
	eventsBefore := j.SchedulerEventID()
	for i := 0; i < 5; i++ {
		got := j.SchedulerPRNGState()
		if !bytes.Equal(got, before) {
			t.Fatalf("SchedulerPRNGState read %d changed state:\nbefore=%x\nafter=%x", i, before, got)
		}
	}
	if got := j.SchedulerPRNGDraws(); got != drawsBefore {
		t.Fatalf("SchedulerPRNGDraws changed after reads: got %d want %d", got, drawsBefore)
	}
	if got := j.SchedulerEventID(); got != eventsBefore {
		t.Fatalf("SchedulerEventID changed after reads: got %d want %d", got, eventsBefore)
	}
}

func TestJea9Linux_SchedulerPRNGDrawUpdatesSnapshot(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{EntropySeed: []byte("draw scheduler state")})

	before := j.SchedulerPRNGState()
	v := j.schedUint64(nil, "test")
	after := j.SchedulerPRNGState()
	if v == 0 {
		t.Log("first deterministic scheduler PRNG draw happened to be zero")
	}
	if bytes.Equal(before, after) {
		t.Fatalf("scheduler PRNG snapshot did not change after draw: %x", before)
	}
	if got := j.SchedulerPRNGDraws(); got != 1 {
		t.Fatalf("SchedulerPRNGDraws = %d, want 1", got)
	}
	if got := j.SchedulerEventID(); got != 0 {
		t.Fatalf("SchedulerEventID = %d, want 0 after raw draw", got)
	}
}

func TestJea9Linux_ClockPolicyOnlyDeadlockAdvances(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{
		ClockPolicy:      ClockPolicyOnlyDeadlockAdvances,
		MonotonicStartNS: 10,
	})
	cpu, mem := newClockPolicyCPU(t)
	defer mem.Free()

	current := j.ensureScheduler(cpu)
	waiting := &jea9LinuxContext{
		tid:             current.tid + 1,
		state:           jea9LinuxContextWaiting,
		waitKind:        jea9LinuxWaitNanosleep,
		waitDeadlineNS:  100,
		waitHasDeadline: true,
	}
	j.contexts[waiting.tid] = waiting
	j.contextOrder = append(j.contextOrder, waiting.tid)

	if delta, advanced := j.advanceVirtualClockForSchedulerEvent(cpu, "test"); advanced || delta != 0 {
		t.Fatalf("clock advanced with runnable context: advanced=%v delta=%d now=%d", advanced, delta, j.MonotonicNS())
	}
	if got := j.MonotonicNS(); got != 10 {
		t.Fatalf("MonotonicNS with runnable context = %d, want 10", got)
	}

	current.state = jea9LinuxContextWaiting
	current.waitKind = jea9LinuxWaitNanosleep
	current.waitDeadlineNS = 50
	current.waitHasDeadline = true
	delta, advanced := j.advanceVirtualClockForSchedulerEvent(cpu, "test")
	if !advanced {
		t.Fatal("clock did not advance when every context was waiting")
	}
	if delta != 40 {
		t.Fatalf("clock delta = %d, want 40", delta)
	}
	if got := j.MonotonicNS(); got != 50 {
		t.Fatalf("MonotonicNS after deadlock advance = %d, want 50", got)
	}
}

func TestJea9Linux_ClockPolicyPRNGAdvanceWithinBoundsAndReplays(t *testing.T) {
	first := NewJea9Linux(Jea9LinuxOptions{
		EntropySeed: []byte("clock prng seed"),
		ClockPolicy: ClockPolicyPRNG,
	})
	second := NewJea9Linux(Jea9LinuxOptions{
		EntropySeed: []byte("clock prng seed"),
		ClockPolicy: ClockPolicyPRNG,
	})

	for i := 0; i < 8; i++ {
		firstDelta, firstAdvanced := first.advanceVirtualClockForSchedulerEvent(nil, "test")
		secondDelta, secondAdvanced := second.advanceVirtualClockForSchedulerEvent(nil, "test")
		if !firstAdvanced || !secondAdvanced {
			t.Fatalf("PRNG clock advance %d did not advance: first=%v second=%v", i, firstAdvanced, secondAdvanced)
		}
		if firstDelta < defaultJea9LinuxClockPRNGMinNS || firstDelta > defaultJea9LinuxClockPRNGMaxNS {
			t.Fatalf("PRNG clock delta %d = %d, want [%d,%d]", i, firstDelta, defaultJea9LinuxClockPRNGMinNS, defaultJea9LinuxClockPRNGMaxNS)
		}
		if firstDelta != secondDelta {
			t.Fatalf("same seed PRNG clock delta %d mismatch: %d != %d", i, firstDelta, secondDelta)
		}
		if !bytes.Equal(first.SchedulerPRNGState(), second.SchedulerPRNGState()) {
			t.Fatalf("same seed PRNG state mismatch after clock draw %d", i)
		}
	}
	if got := first.SchedulerPRNGDraws(); got != 8 {
		t.Fatalf("SchedulerPRNGDraws after PRNG clock advances = %d, want 8", got)
	}
}

func TestJea9Linux_ClockPolicyFixedUsesStateValue(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{
		ClockPolicy:      ClockPolicyFixed,
		MonotonicStartNS: 7,
	})
	j.SetClockFixedAdvanceNS(12345)

	delta, advanced := j.advanceVirtualClockForSchedulerEvent(nil, "test")
	if !advanced {
		t.Fatal("fixed clock policy did not advance")
	}
	if delta != 12345 {
		t.Fatalf("fixed clock delta = %d, want 12345", delta)
	}
	if got := j.MonotonicNS(); got != 12352 {
		t.Fatalf("MonotonicNS after fixed advance = %d, want 12352", got)
	}
	if got := j.ClockFixedAdvanceNS(); got != 12345 {
		t.Fatalf("ClockFixedAdvanceNS = %d, want 12345", got)
	}
	if got := j.SchedulerPRNGDraws(); got != 0 {
		t.Fatalf("fixed clock policy drew scheduler PRNG %d times, want 0", got)
	}
}

func TestJea9Linux_ClockPolicyPRNGDoesNotDrawOnHostBudget(t *testing.T) {
	cpu, mem, j := testLoopCPU(t, 1)
	defer mem.Free()
	j.clockPolicy = ClockPolicyPRNG
	j.schedulerConfig = Jea9LinuxSchedulerConfig{
		MinQuantumRetired: 5,
		MaxQuantumRetired: 5,
	}
	j.normalizeSchedulerConfig()
	j.installNextSchedulerQuantum(cpu)

	beforeDraws := j.SchedulerPRNGDraws()
	beforeState := j.SchedulerPRNGState()
	if err := j.Run(cpu); !errors.Is(err, ErrJea9LinuxBudget) {
		t.Fatalf("Run error = %v, want ErrJea9LinuxBudget", err)
	}
	if got := j.SchedulerPRNGDraws(); got != beforeDraws {
		t.Fatalf("SchedulerPRNGDraws after host budget = %d, want %d", got, beforeDraws)
	}
	if got := j.SchedulerPRNGState(); !bytes.Equal(got, beforeState) {
		t.Fatalf("scheduler PRNG state changed after host budget:\ngot  %x\nwant %x", got, beforeState)
	}
}

func TestJea9Linux_RandomQuantumWithinBoundsAndReplays(t *testing.T) {
	opts := Jea9LinuxOptions{
		EntropySeed: []byte("quantum seed"),
		Scheduler: Jea9LinuxSchedulerConfig{
			Mode:                   Jea9SchedulerDST,
			MinQuantumRetired:      3,
			MaxQuantumRetired:      9,
			LowPriorityDenominator: 10,
		},
	}
	first := NewJea9Linux(opts)
	second := NewJea9Linux(opts)
	cpuA, memA := newClockPolicyCPU(t)
	defer memA.Free()
	cpuB, memB := newClockPolicyCPU(t)
	defer memB.Free()

	for i := 0; i < 8; i++ {
		qa := first.installNextSchedulerQuantum(cpuA)
		qb := second.installNextSchedulerQuantum(cpuB)
		if qa < 3 || qa > 9 {
			t.Fatalf("quantum %d = %d, want [3,9]", i, qa)
		}
		if qa != qb {
			t.Fatalf("same seed quantum %d mismatch: %d != %d", i, qa, qb)
		}
		if first.nextScheduleAtRetired != cpuA.RiscvInstrRetired()+qa {
			t.Fatalf("nextScheduleAtRetired = %d, want %d", first.nextScheduleAtRetired, cpuA.RiscvInstrRetired()+qa)
		}
	}
	if got := first.SchedulerPRNGDraws(); got != 8 {
		t.Fatalf("SchedulerPRNGDraws after quantum draws = %d, want 8", got)
	}
}

func TestJea9Linux_RandomQuantumDoesNotDrawOnHostBudget(t *testing.T) {
	cpu, mem, j := testLoopCPU(t, 1)
	defer mem.Free()
	j.schedulerConfig = Jea9LinuxSchedulerConfig{
		Mode:                   Jea9SchedulerDST,
		MinQuantumRetired:      5,
		MaxQuantumRetired:      9,
		LowPriorityDenominator: 10,
	}
	j.normalizeSchedulerConfig()
	j.installNextSchedulerQuantum(cpu)
	beforeDraws := j.SchedulerPRNGDraws()
	beforeState := j.SchedulerPRNGState()
	if err := j.Run(cpu); !errors.Is(err, ErrJea9LinuxBudget) {
		t.Fatalf("Run error = %v, want ErrJea9LinuxBudget", err)
	}
	if got := j.SchedulerPRNGDraws(); got != beforeDraws {
		t.Fatalf("SchedulerPRNGDraws after host budget = %d, want %d", got, beforeDraws)
	}
	if got := j.SchedulerPRNGState(); !bytes.Equal(got, beforeState) {
		t.Fatalf("scheduler PRNG state changed after host budget:\ngot  %x\nwant %x", got, beforeState)
	}
}

func TestJea9Linux_PriorityShuffleStableByContextOrder(t *testing.T) {
	opts := Jea9LinuxOptions{
		EntropySeed: []byte("priority seed"),
		Scheduler: Jea9LinuxSchedulerConfig{
			Mode:                   Jea9SchedulerDST,
			LowPriorityNumerator:   1,
			LowPriorityDenominator: 2,
		},
	}
	first := NewJea9Linux(opts)
	second := NewJea9Linux(opts)
	cpuA, memA := newClockPolicyCPU(t)
	defer memA.Free()
	cpuB, memB := newClockPolicyCPU(t)
	defer memB.Free()
	addPriorityTestContexts(first, cpuA, 4)
	addPriorityTestContexts(second, cpuB, 4)

	first.reshuffleSchedulerPriorities(cpuA)
	second.reshuffleSchedulerPriorities(cpuB)

	for _, tid := range first.contextOrder {
		a := first.contexts[tid]
		b := second.contexts[tid]
		if a == nil || b == nil {
			t.Fatalf("missing context tid=%d", tid)
		}
		if a.schedPriority != b.schedPriority {
			t.Fatalf("priority tid=%d mismatch: %s != %s", tid, a.schedPriority, b.schedPriority)
		}
	}
	if !bytes.Equal(first.SchedulerPRNGState(), second.SchedulerPRNGState()) {
		t.Fatal("same seed priority shuffle produced different PRNG states")
	}
}

func TestJea9Linux_DSTChoosesRunnableInStableOrder(t *testing.T) {
	j := NewJea9Linux(Jea9LinuxOptions{
		Scheduler: Jea9LinuxSchedulerConfig{Mode: Jea9SchedulerDST},
	})
	cpu, mem := newClockPolicyCPU(t)
	defer mem.Free()
	current := j.ensureScheduler(cpu)
	waitingTID := current.tid + 1
	exitedTID := current.tid + 2
	runnableTID := current.tid + 3
	j.contexts[waitingTID] = &jea9LinuxContext{tid: waitingTID, state: jea9LinuxContextWaiting}
	j.contexts[exitedTID] = &jea9LinuxContext{tid: exitedTID, state: jea9LinuxContextExited}
	j.contexts[runnableTID] = &jea9LinuxContext{tid: runnableTID, state: jea9LinuxContextRunnable}
	j.contextOrder = append(j.contextOrder, waitingTID, exitedTID, runnableTID)

	got, ok := j.nextRunnableByPolicyAfterCurrent()
	if !ok {
		t.Fatal("nextRunnableByPolicyAfterCurrent found no runnable context")
	}
	if got != runnableTID {
		t.Fatalf("next runnable tid = %d, want stable runnable tid %d", got, runnableTID)
	}
}

func TestJea9Linux_ChaosWindowSkipsLowPriority(t *testing.T) {
	j, cpu, mem := newChaosPolicyOS(t)
	defer mem.Free()
	current := j.ensureScheduler(cpu)
	lowTID := current.tid + 1
	highTID := current.tid + 2
	j.contexts[lowTID] = &jea9LinuxContext{
		tid:           lowTID,
		state:         jea9LinuxContextRunnable,
		schedPriority: jea9LinuxSchedLow,
	}
	j.contexts[highTID] = &jea9LinuxContext{
		tid:           highTID,
		state:         jea9LinuxContextRunnable,
		schedPriority: jea9LinuxSchedHigh,
	}
	j.contextOrder = append(j.contextOrder, lowTID, highTID)
	j.chaosActive = true
	j.chaosStartNS = 100
	j.chaosUntilNS = 200
	j.monotonicNS = 150

	got, ok := j.nextRunnableByPolicyAfterCurrent()
	if !ok {
		t.Fatal("nextRunnableByPolicyAfterCurrent found no runnable context")
	}
	if got != highTID {
		t.Fatalf("next runnable tid = %d, want high-priority tid %d", got, highTID)
	}
}

func TestJea9Linux_ChaosWindowBlocksWhenOnlyLowPriorityRunnable(t *testing.T) {
	j, cpu, mem := newChaosPolicyOS(t)
	defer mem.Free()
	current := j.ensureScheduler(cpu)
	lowTID := current.tid + 1
	j.contexts[lowTID] = &jea9LinuxContext{
		tid:           lowTID,
		state:         jea9LinuxContextRunnable,
		schedPriority: jea9LinuxSchedLow,
	}
	j.contextOrder = append(j.contextOrder, lowTID)
	j.chaosActive = true
	j.chaosStartNS = 100
	j.chaosUntilNS = 200
	j.monotonicNS = 150

	if got, ok := j.nextRunnableByPolicyAfterCurrent(); ok {
		t.Fatalf("next runnable tid = %d, want no runnable low-priority context during chaos", got)
	}
}

func TestJea9Linux_ChaosStarvationBudgetCappedAtTwentyPercent(t *testing.T) {
	j, cpu, mem := newChaosPolicyOS(t)
	defer mem.Free()
	j.monotonicNS = 1000

	if !j.startChaosWindow(cpu, 500) {
		t.Fatal("startChaosWindow returned false")
	}
	if got := j.chaosUntilNS - j.chaosStartNS; got != 200 {
		t.Fatalf("chaos window duration = %d, want capped 200", got)
	}
	if got := j.ClockPolicy(); got != ClockPolicyFixed {
		t.Fatalf("ClockPolicy = %s, want fixed", got)
	}
	if got := j.ClockFixedAdvanceNS(); got != 200 {
		t.Fatalf("ClockFixedAdvanceNS = %d, want 200", got)
	}
	j.SetMonotonicNS(j.chaosUntilNS)
	j.refreshChaosWindow()
	if j.chaosActive {
		t.Fatal("chaos window still active after reaching chaosUntilNS")
	}
	if got := j.chaosBlockedNS; got != 200 {
		t.Fatalf("chaosBlockedNS = %d, want 200", got)
	}
	if got := j.remainingChaosBudgetNS(); got != 40 {
		t.Fatalf("remainingChaosBudgetNS after elapsed-time refill = %d, want 40", got)
	}
	j.chaosBlockedNS += 40
	if got := j.remainingChaosBudgetNS(); got != 0 {
		t.Fatalf("remainingChaosBudgetNS at cap = %d, want 0", got)
	}
	if j.startChaosWindow(cpu, 1) {
		t.Fatal("startChaosWindow succeeded at the current elapsed-time cap")
	}
}

func TestJea9Linux_ChaosWindowStartSameSeedSameState(t *testing.T) {
	opts := Jea9LinuxOptions{
		EntropySeed:      []byte("chaos replay seed"),
		MonotonicStartNS: 10_000,
		Scheduler: Jea9LinuxSchedulerConfig{
			Mode:                       Jea9SchedulerChaos,
			ChaosWindowProbNumerator:   1,
			ChaosWindowProbDenominator: 1,
			ChaosWindowMaxNS:           100,
			ChaosBudgetNumerator:       1,
			ChaosBudgetDenominator:     5,
			LowPriorityNumerator:       0,
			LowPriorityDenominator:     10,
			PriorityShuffleMinRetired:  100,
			PriorityShuffleMaxRetired:  100,
			MinQuantumRetired:          10,
			MaxQuantumRetired:          10,
		},
	}
	first := NewJea9Linux(opts)
	second := NewJea9Linux(opts)
	cpuA, memA := newClockPolicyCPU(t)
	defer memA.Free()
	cpuB, memB := newClockPolicyCPU(t)
	defer memB.Free()

	if !first.maybeStartChaosWindow(cpuA) {
		t.Fatal("first maybeStartChaosWindow did not start with probability 1")
	}
	if !second.maybeStartChaosWindow(cpuB) {
		t.Fatal("second maybeStartChaosWindow did not start with probability 1")
	}
	if !first.chaosActive || !second.chaosActive {
		t.Fatalf("chaosActive mismatch: first=%v second=%v", first.chaosActive, second.chaosActive)
	}
	if first.chaosUntilNS != second.chaosUntilNS {
		t.Fatalf("chaosUntilNS mismatch: %d != %d", first.chaosUntilNS, second.chaosUntilNS)
	}
	if first.ClockFixedAdvanceNS() != second.ClockFixedAdvanceNS() {
		t.Fatalf("ClockFixedAdvanceNS mismatch: %d != %d", first.ClockFixedAdvanceNS(), second.ClockFixedAdvanceNS())
	}
	if first.SchedulerPRNGDraws() != second.SchedulerPRNGDraws() {
		t.Fatalf("SchedulerPRNGDraws mismatch: %d != %d", first.SchedulerPRNGDraws(), second.SchedulerPRNGDraws())
	}
	if !bytes.Equal(first.SchedulerPRNGState(), second.SchedulerPRNGState()) {
		t.Fatal("same seed chaos start produced different PRNG states")
	}
}

func TestJea9Linux_ChaosSameSeedSameTrace(t *testing.T) {
	first := runChaosReplayTrace(t, []byte("chaos trace replay seed"))
	second := runChaosReplayTrace(t, []byte("chaos trace replay seed"))
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("same seed chaos traces differ:\nfirst  %+v\nsecond %+v", first, second)
	}
	if len(first) == 0 {
		t.Fatal("chaos replay trace is empty")
	}
	if !first[0].ChaosActive {
		t.Fatalf("first trace did not record an active chaos window: %+v", first[0])
	}
}

func runChaosReplayTrace(t *testing.T, seed []byte) []Jea9LinuxScheduleTraceEntry {
	t.Helper()
	cpu, mem := newTestCPU(t, Size64MB, 0x1000, []uint32{
		ienc(opOPIMM, 0, 1, 1, 1),
		jenc(0, -4),
	})
	defer mem.Free()
	j := NewJea9Linux(Jea9LinuxOptions{
		EntropySeed:       seed,
		InstructionBudget: 100,
		Trace:             true,
		MonotonicStartNS:  1000,
		Scheduler: Jea9LinuxSchedulerConfig{
			Mode:                       Jea9SchedulerChaos,
			MinQuantumRetired:          2,
			MaxQuantumRetired:          2,
			LowPriorityNumerator:       0,
			LowPriorityDenominator:     10,
			PriorityShuffleMinRetired:  4,
			PriorityShuffleMaxRetired:  4,
			ChaosWindowProbNumerator:   1,
			ChaosWindowProbDenominator: 1,
			ChaosWindowMaxNS:           50,
			ChaosBudgetNumerator:       1,
			ChaosBudgetDenominator:     5,
		},
	})
	parent := j.ensureScheduler(cpu)
	for i := uint64(1); i <= 2; i++ {
		tid := parent.tid + i
		j.contexts[tid] = &jea9LinuxContext{
			tid:   tid,
			state: jea9LinuxContextRunnable,
			snapshot: jea9LinuxCPUSnapshot{
				pc: 0x1000,
			},
		}
		j.contextOrder = append(j.contextOrder, tid)
	}
	for len(j.TraceSnapshot().Schedule) < 6 {
		if err := j.Run(cpu); !errors.Is(err, ErrJea9LinuxBudget) {
			t.Fatalf("Run error = %v, want ErrJea9LinuxBudget", err)
		}
	}
	trace := j.TraceSnapshot().Schedule
	return append([]Jea9LinuxScheduleTraceEntry(nil), trace...)
}

func newClockPolicyCPU(t *testing.T) (*CPU, *GuestMemory) {
	t.Helper()
	mem, err := NewGuestMemory(Size1MB)
	if err != nil {
		t.Fatal(err)
	}
	return NewCPU(*mem), mem
}

func newChaosPolicyOS(t *testing.T) (*Jea9Linux, *CPU, *GuestMemory) {
	t.Helper()
	cpu, mem := newClockPolicyCPU(t)
	j := NewJea9Linux(Jea9LinuxOptions{
		Scheduler: Jea9LinuxSchedulerConfig{
			Mode:                   Jea9SchedulerChaos,
			LowPriorityDenominator: 10,
			ChaosBudgetNumerator:   1,
			ChaosBudgetDenominator: 5,
		},
	})
	return j, cpu, mem
}

func addPriorityTestContexts(j *Jea9Linux, cpu *CPU, n int) {
	j.ensureScheduler(cpu)
	for i := 1; i < n; i++ {
		tid := j.pid + uint64(i)
		j.contexts[tid] = &jea9LinuxContext{
			tid:   tid,
			state: jea9LinuxContextRunnable,
			snapshot: jea9LinuxCPUSnapshot{
				pc: 0x1000 + uint64(i)*4,
			},
		}
		j.contextOrder = append(j.contextOrder, tid)
	}
}
