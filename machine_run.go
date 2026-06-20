package riscv

// RunMachineBudget executes m using the generic machine-budget semantics. If an
// accelerator is installed, it owns the run; otherwise this is exactly the
// package-level interpreter-backed RunMachineBudget.
func (m *Machine) RunMachineBudget(nc *NoteChain, budget uint64) (RunBudgetResult, error) {
	if m.Accel != nil {
		return m.Accel.RunMachineBudget(m.CPU, nc, budget, AccelRunPlain)
	}
	return RunMachineBudget(m.CPU, nc, budget)
}

// RunBiosMachineBudget executes m using BIOS-machine budget semantics. If an
// accelerator is installed, it must preserve the same WFI, timer, and interrupt
// points as RunBiosMachineBudget.
func (m *Machine) RunBiosMachineBudget(nc *NoteChain, budget uint64) (RunBudgetResult, error) {
	if m.Accel != nil {
		return m.Accel.RunMachineBudget(m.CPU, nc, budget, AccelRunBIOS)
	}
	return RunBiosMachineBudget(m.CPU, nc, budget)
}
