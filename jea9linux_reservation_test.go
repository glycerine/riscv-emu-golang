package riscv

import "testing"

func TestJea9Linux_LRSCReservationIsHartStateAcrossContextSwitch(t *testing.T) {
	const (
		dataVA = uint64(0x20000)
		pcA    = uint64(0x10000)
		pcB    = uint64(0x11000)
	)

	mem, err := NewGuestMemory(Size1MB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()
	if f := mem.Store32(dataVA, 0); f != nil {
		t.Fatal(f)
	}

	cpu := NewCPU(*mem)
	jos := NewJea9Linux(Jea9LinuxOptions{})
	ctxA := jos.ensureScheduler(cpu)

	cpu.SetPC(pcA)
	cpu.SetReg(10, dataVA)
	cpu.SetReg(11, 0x11111111)
	if err := cpu.stepFromInsn(amoenc(amoFunct5LR, amoFunct3W, 12, 10, 0)); err != nil {
		t.Fatalf("context A LR.W: %v", err)
	}
	if !cpu.resvValid || cpu.resvAddr != dataVA {
		t.Fatalf("context A reservation = {0x%x,%v}, want {0x%x,true}", cpu.resvAddr, cpu.resvValid, dataVA)
	}
	ctxA.snapshot = snapshotJea9LinuxCPU(cpu)
	ctxA.state = jea9LinuxContextRunnable

	var xB [32]uint64
	xB[10] = dataVA
	xB[11] = 0x22222222
	ctxB := &jea9LinuxContext{
		tid:   jos.pid + 1,
		state: jea9LinuxContextRunnable,
		snapshot: jea9LinuxCPUSnapshot{
			pc: pcB,
			x:  xB,
		},
	}
	jos.contexts[ctxB.tid] = ctxB
	jos.contextOrder = append(jos.contextOrder, ctxB.tid)

	if !jos.loadContext(cpu, ctxB.tid) {
		t.Fatalf("load context B failed")
	}
	if err := cpu.stepFromInsn(amoenc(amoFunct5LR, amoFunct3W, 14, 10, 0)); err != nil {
		t.Fatalf("context B LR.W: %v", err)
	}
	if err := cpu.stepFromInsn(amoenc(amoFunct5SC, amoFunct3W, 15, 10, 11)); err != nil {
		t.Fatalf("context B SC.W: %v", err)
	}
	if got := cpu.Reg(15); got != 0 {
		t.Fatalf("context B SC.W result = %d, want success 0", got)
	}
	if got := mustLoad32AMO(t, mem, dataVA); got != 0x22222222 {
		t.Fatalf("memory after context B SC.W = 0x%x, want 0x22222222", got)
	}
	if cpu.resvValid {
		t.Fatalf("context B successful SC.W left hart reservation valid")
	}
	ctxB.snapshot = snapshotJea9LinuxCPU(cpu)

	if !jos.loadContext(cpu, jos.pid) {
		t.Fatalf("load context A failed")
	}
	if cpu.resvValid {
		t.Errorf("context A reload restored stale LR/SC reservation {addr:0x%x}; reservation is hart state, not guest-thread state", cpu.resvAddr)
	}
	if err := cpu.stepFromInsn(amoenc(amoFunct5SC, amoFunct3W, 13, 10, 11)); err != nil {
		t.Fatalf("context A SC.W: %v", err)
	}
	if got := cpu.Reg(13); got != 1 {
		t.Errorf("context A SC.W result = %d, want failure 1 after context B cleared the hart reservation", got)
	}
	if got := mustLoad32AMO(t, mem, dataVA); got != 0x22222222 {
		t.Errorf("memory after stale context A SC.W = 0x%x, want context B value 0x22222222", got)
	}
}

func TestJea9Linux_LRSCReservationIsHartStateAcrossContextSwitch_LazyJIT(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*JIT)
	}{
		{name: "abjit"},
		{
			name: "rv8",
			configure: func(jit *JIT) {
				jit.SetRegPolicy(PolicyRV8)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runJITLRSCReservationContextSwitch(t, tt.configure)
		})
	}
}

func runJITLRSCReservationContextSwitch(t *testing.T, configure func(*JIT)) {
	t.Helper()
	const (
		dataVA = uint64(0x20000)
		pcA    = uint64(0x10000)
		pcB    = uint64(0x11000)
	)

	mem, err := NewGuestMemory(Size1MB)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Free()
	if f := mem.Store32(dataVA, 0); f != nil {
		t.Fatal(f)
	}
	storeInsns(mem, pcA, []uint32{
		amoenc(amoFunct5LR, amoFunct3W, 12, 10, 0),
		0x00000013, // ADDI x0, x0, 0: keep LR/SC unfused.
		amoenc(amoFunct5SC, amoFunct3W, 13, 10, 11),
	})
	storeInsns(mem, pcB, []uint32{
		amoenc(amoFunct5LR, amoFunct3W, 14, 10, 0),
		0x00000013, // ADDI x0, x0, 0: keep LR/SC unfused.
		amoenc(amoFunct5SC, amoFunct3W, 15, 10, 11),
	})

	cpu := NewCPU(*mem)
	jit := NewJIT()
	defer jit.Close()
	if configure != nil {
		configure(jit)
	}
	jos := NewJea9Linux(Jea9LinuxOptions{})
	ctxA := jos.ensureScheduler(cpu)

	cpu.SetPC(pcA)
	cpu.SetReg(10, dataVA)
	cpu.SetReg(11, 0x11111111)
	if res, err := jit.StepBlockBudget(cpu, 1); err != nil || res != RunBudgetExpired {
		t.Fatalf("context A LR.W budget step = (%v,%v), want (%v,nil)", res, err, RunBudgetExpired)
	}
	if !cpu.resvValid || cpu.resvAddr != dataVA {
		t.Fatalf("context A reservation = {0x%x,%v}, want {0x%x,true}", cpu.resvAddr, cpu.resvValid, dataVA)
	}
	ctxA.snapshot = snapshotJea9LinuxCPU(cpu)
	ctxA.state = jea9LinuxContextRunnable

	var xB [32]uint64
	xB[10] = dataVA
	xB[11] = 0x22222222
	ctxB := &jea9LinuxContext{
		tid:   jos.pid + 1,
		state: jea9LinuxContextRunnable,
		snapshot: jea9LinuxCPUSnapshot{
			pc: pcB,
			x:  xB,
		},
	}
	jos.contexts[ctxB.tid] = ctxB
	jos.contextOrder = append(jos.contextOrder, ctxB.tid)

	if !jos.loadContext(cpu, ctxB.tid) {
		t.Fatalf("load context B failed")
	}
	if res, err := jit.StepBlockBudget(cpu, 3); err != nil || res != RunBudgetExpired {
		t.Fatalf("context B LR.W/SC.W budget step = (%v,%v), want (%v,nil)", res, err, RunBudgetExpired)
	}
	if got := cpu.Reg(15); got != 0 {
		t.Fatalf("context B SC.W result = %d, want success 0", got)
	}
	if got := mustLoad32AMO(t, mem, dataVA); got != 0x22222222 {
		t.Fatalf("memory after context B SC.W = 0x%x, want 0x22222222", got)
	}
	if cpu.resvValid {
		t.Fatalf("context B successful SC.W left hart reservation valid")
	}
	ctxB.snapshot = snapshotJea9LinuxCPU(cpu)

	if !jos.loadContext(cpu, jos.pid) {
		t.Fatalf("load context A failed")
	}
	if cpu.resvValid {
		t.Errorf("context A reload restored stale LR/SC reservation {addr:0x%x}; reservation is hart state, not guest-thread state", cpu.resvAddr)
	}
	if res, err := jit.StepBlockBudget(cpu, 2); err != nil || res != RunBudgetExpired {
		t.Fatalf("context A NOP/SC.W budget step = (%v,%v), want (%v,nil)", res, err, RunBudgetExpired)
	}
	if got := cpu.Reg(13); got != 1 {
		t.Errorf("context A SC.W result = %d, want failure 1 after context B cleared the hart reservation", got)
	}
	if got := mustLoad32AMO(t, mem, dataVA); got != 0x22222222 {
		t.Errorf("memory after stale context A SC.W = 0x%x, want context B value 0x22222222", got)
	}
	if jit.DispatchInterp != 0 {
		t.Fatalf("DispatchInterp = %d, want 0 native LR/SC fallbacks", jit.DispatchInterp)
	}
}
