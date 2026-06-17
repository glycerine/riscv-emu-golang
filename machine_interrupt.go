package riscv

const (
	mipSSIP = uint64(1) << InterruptSSIP
	mipMSIP = uint64(1) << InterruptMSIP
	mipSTIP = uint64(1) << InterruptSTIP
	mipMTIP = uint64(1) << InterruptMTIP
	mipSEIP = uint64(1) << InterruptSEIP
	mipMEIP = uint64(1) << InterruptMEIP
)

type biosMachineTimerMMIO interface {
	AdvanceMachineTimer(delta uint64)
	MachineTimerValue() uint64
	MachineTimerPending() bool
}

func (c *CPU) serviceBiosMachineTimer() {
	timer, ok := c.mem.mmio.(biosMachineTimerMMIO)
	if !ok {
		return
	}
	timer.AdvanceMachineTimer(1000)
	if timer.MachineTimerPending() {
		c.mip |= mipMTIP
	} else {
		c.mip &^= mipMTIP
	}
	c.refreshSupervisorTimerPendingAt(timer.MachineTimerValue())
}

func (c *CPU) timerValue() uint64 {
	timer, ok := c.mem.mmio.(biosMachineTimerMMIO)
	if !ok {
		return c.riscvInstrBegun
	}
	return timer.MachineTimerValue()
}

func (c *CPU) refreshSupervisorTimerPending() {
	c.refreshSupervisorTimerPendingAt(c.timerValue())
}

func (c *CPU) refreshSupervisorTimerPendingAt(now uint64) {
	c.stipSTime = c.stimecmp != ^uint64(0) && now >= c.stimecmp
}

func (c *CPU) mipValue() uint64 {
	pending := c.mip &^ mipSTIP
	if c.stipMIP || c.stipSTime {
		pending |= mipSTIP
	}
	return pending
}

func (c *CPU) setMIPCSR(val uint64) {
	c.mip = val &^ mipSTIP
	c.stipMIP = val&mipSTIP != 0
}

func (c *CPU) takePendingBiosInterrupt() bool {
	c.serviceBiosMachineTimer()
	if irq, ok := c.pendingMachineInterrupt(); ok {
		return c.trapToMachineInterruptAt(c.pc, irq)
	}
	if irq, ok := c.pendingSupervisorInterrupt(); ok {
		c.trapToSupervisorInterruptAt(c.pc, irq)
		return true
	}
	return false
}

func (c *CPU) pendingMachineInterrupt() (uint64, bool) {
	pending := c.mipValue() & c.mie &^ c.mideleg
	if pending == 0 {
		return 0, false
	}
	if c.priv == PrivMachine && c.mstatus&statusMIE == 0 {
		return 0, false
	}
	return highestPendingInterrupt(pending)
}

func (c *CPU) pendingSupervisorInterrupt() (uint64, bool) {
	if c.priv == PrivMachine {
		return 0, false
	}
	pending := (c.mipValue() | c.sip) & (c.mie | c.sie) & c.mideleg
	if pending == 0 {
		return 0, false
	}
	if c.priv == PrivSupervisor && c.mstatus&statusSIE == 0 {
		return 0, false
	}
	return highestPendingInterrupt(pending)
}

func highestPendingInterrupt(pending uint64) (uint64, bool) {
	for _, irq := range [...]uint64{
		InterruptMEIP,
		InterruptMSIP,
		InterruptMTIP,
		InterruptSEIP,
		InterruptSSIP,
		InterruptSTIP,
	} {
		if pending&(uint64(1)<<irq) != 0 {
			return irq, true
		}
	}
	return 0, false
}

func interruptCause(irq uint64) uint64 {
	return InterruptCauseFlag | irq
}

func (c *CPU) trapToMachineInterruptAt(pc, irq uint64) bool {
	return c.trapToMachineAt(pc, interruptCause(irq), 0)
}

func (c *CPU) trapToSupervisorInterruptAt(pc, irq uint64) {
	c.trapToSupervisorAt(pc, interruptCause(irq), 0)
}
