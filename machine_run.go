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

// RunMachineDualBudget executes m with both attempted-instruction and
// retired-instruction budget limits. Accelerators that implement
// DualBudgetAccelerator preserve both limits; other machines use the default
// cached interpreter path.
func (m *Machine) RunMachineDualBudget(nc *NoteChain, attemptBudget, retiredBudget uint64) (RunBudgetResult, RunBudgetLimit, error) {
	if m.Accel != nil {
		if accel, ok := m.Accel.(DualBudgetAccelerator); ok {
			return accel.RunMachineDualBudget(m.CPU, nc, attemptBudget, retiredBudget, AccelRunPlain)
		}
	}
	return RunDefaultDualBudget(m.CPU, nc, attemptBudget, retiredBudget)
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

// RunBiosMachineDualBudget is the BIOS-mode counterpart to
// RunMachineDualBudget. It is currently primarily useful for tests and future
// firmware runners; the package-level BIOS interpreter remains the fallback.
func (m *Machine) RunBiosMachineDualBudget(nc *NoteChain, attemptBudget, retiredBudget uint64) (RunBudgetResult, RunBudgetLimit, error) {
	if m.Accel != nil {
		if accel, ok := m.Accel.(DualBudgetAccelerator); ok {
			return accel.RunMachineDualBudget(m.CPU, nc, attemptBudget, retiredBudget, AccelRunBIOS)
		}
	}
	if retiredBudget == 0 {
		return RunBudgetExpired, RunBudgetLimitRetired, nil
	}
	if attemptBudget == 0 {
		return RunBudgetExpired, RunBudgetLimitAttempt, nil
	}
	retiredBase := m.CPU.RiscvInstrRetired()
	for used := uint64(0); ; used++ {
		if m.CPU.RiscvInstrRetired()-retiredBase >= retiredBudget {
			return RunBudgetExpired, RunBudgetLimitRetired, nil
		}
		if used >= attemptBudget {
			return RunBudgetExpired, RunBudgetLimitAttempt, nil
		}
		res, err := RunBiosMachineBudget(m.CPU, nc, 1)
		if err != nil {
			return res, RunBudgetLimitNone, err
		}
		if res == RunBudgetExit {
			return res, RunBudgetLimitNone, nil
		}
		if res == RunBudgetContinue {
			return res, RunBudgetLimitNone, nil
		}
	}
}
