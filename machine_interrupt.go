package riscv

import "time"

const (
	mipSSIP = uint64(1) << InterruptSSIP
	mipMSIP = uint64(1) << InterruptMSIP
	mipSTIP = uint64(1) << InterruptSTIP
	mipMTIP = uint64(1) << InterruptMTIP
	mipSEIP = uint64(1) << InterruptSEIP
	mipMEIP = uint64(1) << InterruptMEIP
)

const (
	biosTimerTicksPerInstruction = uint64(1)
	biosTimerTimebaseHz          = uint64(10000000)
)

type biosWFISleepFunc func(time.Duration, <-chan struct{}) bool

var (
	biosWFIHostSleep    biosWFISleepFunc = biosWFISelectSleep
	biosWFIHostSleepCap                  = time.Millisecond
)

func biosWFISelectSleep(d time.Duration, wake <-chan struct{}) bool {
	if d <= 0 {
		return false
	}
	if wake == nil {
		time.Sleep(d)
		return false
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-wake:
		return true
	case <-timer.C:
		return false
	}
}

func SetBiosIdleSleepCap(d time.Duration) func() {
	old := biosWFIHostSleepCap
	biosWFIHostSleepCap = d
	return func() {
		biosWFIHostSleepCap = old
	}
}

func (c *CPU) serviceBiosMachineTimer() {
	if c.mem.mmio != nil {
		c.mem.mmio.AdvanceMachineTimer(biosTimerTicksPerInstruction)
		c.refreshSupervisorTimerPendingAt(c.mem.mmio.MachineTimerValue())
	}
	c.refreshSupervisorExternalPending()
}

func (c *CPU) serviceBiosWFI() {
	if !c.consumeWFI() {
		return
	}
	c.refreshSupervisorTimerPending()
	c.refreshSupervisorExternalPending()
	if c.hasPendingBiosInterrupt() {
		return
	}
	if c.mem.mmio == nil {
		return
	}
	sleepFor, ticks := c.biosWFISleepPlan(c.mem.mmio.MachineTimerValue())
	if sleepFor <= 0 || ticks == 0 {
		return
	}
	sleepStart := time.Now()
	woke := biosWFIHostSleep(sleepFor, c.mem.mmio.wfiWakeChannel())
	if woke {
		elapsedTicks := biosTimerDurationToTicksAllowZero(time.Since(sleepStart))
		if elapsedTicks < ticks {
			ticks = elapsedTicks
		}
	}
	c.mem.mmio.AdvanceMachineTimer(ticks)
	c.refreshSupervisorTimerPendingAt(c.mem.mmio.MachineTimerValue())
	c.refreshSupervisorExternalPending()
}

func (c *CPU) consumeWFI() bool {
	if !c.wfi {
		return false
	}
	c.wfi = false
	return true
}

func (c *CPU) hasPendingBiosInterrupt() bool {
	if _, ok := c.pendingMachineInterrupt(); ok {
		return true
	}
	if _, ok := c.pendingSupervisorInterrupt(); ok {
		return true
	}
	return false
}

func (c *CPU) refreshSupervisorExternalPending() {
	if c.mem.mmio != nil && c.mem.mmio.SupervisorExternalInterruptPending() {
		c.mip |= mipSEIP
	} else {
		c.mip &^= mipSEIP
	}
}

func (c *CPU) biosWFISleepPlan(now uint64) (time.Duration, uint64) {
	sleepFor := biosWFIHostSleepCap
	ticks := biosTimerDurationToTicks(sleepFor)
	if ticks == 0 {
		return 0, 0
	}
	if c.stimecmp != ^uint64(0) {
		if now >= c.stimecmp {
			return 0, 0
		}
		if until := c.stimecmp - now; until < ticks {
			ticks = until
			sleepFor = biosTimerTicksToDuration(ticks)
		}
	}
	return sleepFor, ticks
}

func biosTimerDurationToTicks(d time.Duration) uint64 {
	if d <= 0 {
		return 0
	}
	ticks := uint64(d) * biosTimerTimebaseHz / uint64(time.Second)
	if ticks == 0 {
		return 1
	}
	return ticks
}

func biosTimerDurationToTicksAllowZero(d time.Duration) uint64 {
	if d <= 0 {
		return 0
	}
	return uint64(d) * biosTimerTimebaseHz / uint64(time.Second)
}

func biosTimerTicksToDuration(ticks uint64) time.Duration {
	if ticks == 0 {
		return 0
	}
	ns := ticks * uint64(time.Second) / biosTimerTimebaseHz
	if ns == 0 {
		return time.Nanosecond
	}
	const maxDuration = (uint64(1) << 63) - 1
	if ns > maxDuration {
		return time.Duration(maxDuration)
	}
	return time.Duration(ns)
}

func (c *CPU) timerValue() uint64 {
	if c.mem.mmio == nil {
		return c.riscvInstrBegun
	}
	return c.mem.mmio.MachineTimerValue()
}

func (c *CPU) refreshSupervisorTimerPending() {
	c.refreshSupervisorTimerPendingAt(c.timerValue())
}

func (c *CPU) refreshSupervisorTimerPendingAt(now uint64) {
	c.stip = c.stimecmp != ^uint64(0) && now >= c.stimecmp
}

func (c *CPU) mipValue() uint64 {
	pending := c.mip &^ mipSTIP
	if c.stip {
		pending |= mipSTIP
	}
	return pending
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
