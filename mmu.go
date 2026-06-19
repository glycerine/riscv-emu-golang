package riscv

import "unsafe"

const (
	tlbSize = 512
	tlbMask = tlbSize - 1

	satpModeBare = 0
	satpModeSv39 = 8
	satpModeSv48 = 9

	pteV = uint64(1) << 0
	pteR = uint64(1) << 1
	pteW = uint64(1) << 2
	pteX = uint64(1) << 3
	pteU = uint64(1) << 4
	pteA = uint64(1) << 6
	pteD = uint64(1) << 7

	tlbR = uint8(1) << 0
	tlbW = uint8(1) << 1
	tlbX = uint8(1) << 2
	tlbU = uint8(1) << 3
)

// MMU is a BIOS-machine-only Sv39/Sv48 software TLB. It is intentionally not
// part of GuestMemory, so process-style personalities keep the flat fast path.
type MMU struct {
	loadTLB  [tlbSize]tlbEntry
	storeTLB [tlbSize]tlbEntry
	fetchTLB [tlbSize]tlbEntry
}

type tlbEntry struct {
	tag       uint64
	mask      uint64
	paddrBase uint64
	hostBase  uintptr
	perms     uint8
	valid     bool
	direct    bool
	mmio      bool
}

type mmioRangeChecker interface {
	MMIOOverlaps(addr, size uint64) bool
}

func (c *CPU) EnableMMU() {
	if c.mmu == nil {
		c.mmu = new(MMU)
	}
}

func (c *CPU) DisableMMU() {
	c.mmu = nil
}

func (c *CPU) flushTLB() {
	if c.mmu != nil {
		c.mmu.flush()
	}
}

func (m *MMU) flush() {
	for i := range m.loadTLB {
		m.loadTLB[i].valid = false
		m.storeTLB[i].valid = false
		m.fetchTLB[i].valid = false
	}
}

func RunMachineBudget(cpu *CPU, nc *NoteChain, budget uint64) (RunBudgetResult, error) {
	return runMachineBudget(cpu, nc, budget, false)
}

func RunBiosMachineBudget(cpu *CPU, nc *NoteChain, budget uint64) (RunBudgetResult, error) {
	return runMachineBudget(cpu, nc, budget, true)
}

func runMachineBudget(cpu *CPU, nc *NoteChain, budget uint64, biosMode bool) (RunBudgetResult, error) {
	if budget == 0 {
		return RunBudgetExpired, nil
	}
	for used := uint64(0); used < budget; used++ {
		if biosMode && cpu.takePendingBiosInterrupt() {
			continue
		}
		err := cpu.Step()
		cpu.riscvInstrBegun++
		if cpu.watchAddr != 0 {
			if v, _ := (&cpu.mem).Load64(cpu.watchAddr); v != 0 {
				return RunBudgetExit, &ExitError{Code: tohostExitCode(v)}
			}
		}
		if err == nil {
			if biosMode {
				cpu.serviceBiosWFI()
			}
			continue
		}
		if cpu.trapMachineError(err) {
			continue
		}
		n := noteFromCPUError(cpu, err)
		switch nc.Deliver(cpu, n) {
		case NoteHandled:
			continue
		case NoteExit:
			return RunBudgetExit, &ExitError{Code: cpu.ExitCode}
		default:
			return RunBudgetContinue, err
		}
	}
	return RunBudgetExpired, nil
}

func (c *CPU) trapMachineError(err error) bool {
	switch e := err.(type) {
	case *MemFault:
		cause, _ := faultCauseAndText(e)
		return c.trapToPrivilegedAt(c.pc, cause, e.Addr, 0)
	}
	if err == ErrIllegalInstruction {
		return c.trapToPrivilegedAt(c.pc, CauseIllegalInsn, 0, 0)
	}
	return false
}

func (c *CPU) fetch16(addr uint64) (uint16, *MemFault) {
	if c.mmu == nil {
		return (&c.mem).Fetch16(addr)
	}
	return c.mmu.fetch16(c, addr)
}

func (c *CPU) fetch32(addr uint64) (uint32, *MemFault) {
	if c.mmu == nil {
		return (&c.mem).Fetch32(addr)
	}
	return c.mmu.fetch32(c, addr)
}

func (c *CPU) fetch32U(addr uint64) (uint32, *MemFault) {
	lo, f := c.fetch16(addr)
	if f != nil {
		return 0, f
	}
	hi, f := c.fetch16(addr + 2)
	if f != nil {
		return 0, f
	}
	return uint32(lo) | uint32(hi)<<16, nil
}

func (c *CPU) load8(addr uint64) (uint8, *MemFault) {
	if c.mmu == nil {
		return (&c.mem).Load8(addr)
	}
	v, f := c.mmu.load(c, addr, 1)
	return uint8(v), f
}

func (c *CPU) load16(addr uint64) (uint16, *MemFault) {
	if c.mmu == nil {
		return (&c.mem).Load16(addr)
	}
	v, f := c.mmu.load(c, addr, 2)
	return uint16(v), f
}

func (c *CPU) load32(addr uint64) (uint32, *MemFault) {
	if c.mmu == nil {
		return (&c.mem).Load32(addr)
	}
	v, f := c.mmu.load(c, addr, 4)
	return uint32(v), f
}

func (c *CPU) load64(addr uint64) (uint64, *MemFault) {
	if c.mmu == nil {
		return (&c.mem).Load64(addr)
	}
	return c.mmu.load(c, addr, 8)
}

func (c *CPU) load16U(addr uint64) (uint16, *MemFault) {
	b0, f := c.load8(addr)
	if f != nil {
		return 0, f
	}
	b1, f := c.load8(addr + 1)
	if f != nil {
		return 0, f
	}
	return uint16(b0) | uint16(b1)<<8, nil
}

func (c *CPU) load32U(addr uint64) (uint32, *MemFault) {
	v := uint32(0)
	for i := uint64(0); i < 4; i++ {
		b, f := c.load8(addr + i)
		if f != nil {
			return 0, f
		}
		v |= uint32(b) << (i * 8)
	}
	return v, nil
}

func (c *CPU) load64U(addr uint64) (uint64, *MemFault) {
	v := uint64(0)
	for i := uint64(0); i < 8; i++ {
		b, f := c.load8(addr + i)
		if f != nil {
			return 0, f
		}
		v |= uint64(b) << (i * 8)
	}
	return v, nil
}

func (c *CPU) store8(addr uint64, v uint8) *MemFault {
	if c.mmu == nil {
		return (&c.mem).Store8(addr, v)
	}
	return c.mmu.store(c, addr, 1, uint64(v))
}

func (c *CPU) store16(addr uint64, v uint16) *MemFault {
	if c.mmu == nil {
		return (&c.mem).Store16(addr, v)
	}
	return c.mmu.store(c, addr, 2, uint64(v))
}

func (c *CPU) store32(addr uint64, v uint32) *MemFault {
	if c.mmu == nil {
		return (&c.mem).Store32(addr, v)
	}
	return c.mmu.store(c, addr, 4, uint64(v))
}

func (c *CPU) store64(addr uint64, v uint64) *MemFault {
	if c.mmu == nil {
		return (&c.mem).Store64(addr, v)
	}
	return c.mmu.store(c, addr, 8, v)
}

func (c *CPU) store16U(addr uint64, v uint16) *MemFault {
	if f := c.store8(addr, uint8(v)); f != nil {
		return f
	}
	return c.store8(addr+1, uint8(v>>8))
}

func (c *CPU) store32U(addr uint64, v uint32) *MemFault {
	for i := uint64(0); i < 4; i++ {
		if f := c.store8(addr+i, uint8(v>>(i*8))); f != nil {
			return f
		}
	}
	return nil
}

func (c *CPU) store64U(addr uint64, v uint64) *MemFault {
	for i := uint64(0); i < 8; i++ {
		if f := c.store8(addr+i, uint8(v>>(i*8))); f != nil {
			return f
		}
	}
	return nil
}

func (m *MMU) fetch16(c *CPU, addr uint64) (uint16, *MemFault) {
	if addr&1 != 0 {
		return 0, &MemFault{Addr: addr, Width: 2, Kind: FaultMisalign}
	}
	e, paddr, f := m.lookup(c, addr, 2, FaultFetch)
	if f != nil {
		return 0, f
	}
	if e != nil && e.direct {
		return *(*uint16)(unsafe.Pointer(e.hostBase + uintptr(addr&e.mask))), nil
	}
	if e != nil && e.mmio {
		return 0, &MemFault{Addr: addr, Width: 2, Kind: FaultFetch}
	}
	return (&c.mem).Fetch16(paddr)
}

func (m *MMU) fetch32(c *CPU, addr uint64) (uint32, *MemFault) {
	if addr&3 != 0 {
		return 0, &MemFault{Addr: addr, Width: 4, Kind: FaultMisalign}
	}
	e, paddr, f := m.lookup(c, addr, 4, FaultFetch)
	if f != nil {
		return 0, f
	}
	if e != nil && e.direct {
		return *(*uint32)(unsafe.Pointer(e.hostBase + uintptr(addr&e.mask))), nil
	}
	if e != nil && e.mmio {
		return 0, &MemFault{Addr: addr, Width: 4, Kind: FaultFetch}
	}
	return (&c.mem).Fetch32(paddr)
}

func (m *MMU) load(c *CPU, addr, width uint64) (uint64, *MemFault) {
	if addr&(width-1) != 0 {
		return 0, &MemFault{Addr: addr, Width: width, Kind: FaultMisalign}
	}
	e, paddr, f := m.lookup(c, addr, width, FaultLoad)
	if f != nil {
		return 0, f
	}
	if e != nil && e.direct {
		ptr := unsafe.Pointer(e.hostBase + uintptr(addr&e.mask))
		switch width {
		case 1:
			return uint64(*(*uint8)(ptr)), nil
		case 2:
			return uint64(*(*uint16)(ptr)), nil
		case 4:
			return uint64(*(*uint32)(ptr)), nil
		case 8:
			return *(*uint64)(ptr), nil
		}
	}
	switch width {
	case 1:
		v, f := (&c.mem).Load8(paddr)
		return uint64(v), f
	case 2:
		v, f := (&c.mem).Load16(paddr)
		return uint64(v), f
	case 4:
		v, f := (&c.mem).Load32(paddr)
		return uint64(v), f
	case 8:
		return (&c.mem).Load64(paddr)
	default:
		return 0, &MemFault{Addr: addr, Width: width, Kind: FaultLoad}
	}
}

func (m *MMU) store(c *CPU, addr, width, value uint64) *MemFault {
	if addr&(width-1) != 0 {
		return &MemFault{Addr: addr, Width: width, Kind: FaultMisalign}
	}
	e, paddr, f := m.lookup(c, addr, width, FaultStore)
	if f != nil {
		return f
	}
	if e != nil && e.direct {
		ptr := unsafe.Pointer(e.hostBase + uintptr(addr&e.mask))
		switch width {
		case 1:
			*(*uint8)(ptr) = uint8(value)
		case 2:
			*(*uint16)(ptr) = uint16(value)
		case 4:
			*(*uint32)(ptr) = uint32(value)
		case 8:
			*(*uint64)(ptr) = value
		default:
			return &MemFault{Addr: addr, Width: width, Kind: FaultStore}
		}
		return nil
	}
	switch width {
	case 1:
		return (&c.mem).Store8(paddr, uint8(value))
	case 2:
		return (&c.mem).Store16(paddr, uint16(value))
	case 4:
		return (&c.mem).Store32(paddr, uint32(value))
	case 8:
		return (&c.mem).Store64(paddr, value)
	default:
		return &MemFault{Addr: addr, Width: width, Kind: FaultStore}
	}
}

func (m *MMU) lookup(c *CPU, addr, width uint64, kind FaultKind) (*tlbEntry, uint64, *MemFault) {
	if c.mmuBypass(kind) || satpMode(c.satp) == satpModeBare {
		return nil, addr, nil
	}
	table := m.table(kind)
	idx := (addr >> 12) & tlbMask
	e := &table[idx]
	if e.valid && (addr&^e.mask) == e.tag && e.allow(c, kind) {
		return e, e.paddrBase + (addr & e.mask), nil
	}
	paddr, fill, f := m.walk(c, addr, kind)
	if f != nil {
		return nil, 0, f
	}
	*e = fill
	return e, paddr, nil
}

func (m *MMU) table(kind FaultKind) *[tlbSize]tlbEntry {
	switch kind {
	case FaultStore:
		return &m.storeTLB
	case FaultFetch:
		return &m.fetchTLB
	default:
		return &m.loadTLB
	}
}

func (e *tlbEntry) allow(c *CPU, kind FaultKind) bool {
	return accessAllowed(c, e.perms, kind)
}

func (c *CPU) mmuBypass(kind FaultKind) bool {
	priv := c.memoryPrivilege(kind)
	return priv == PrivMachine
}

func (c *CPU) memoryPrivilege(kind FaultKind) PrivilegeMode {
	if kind != FaultFetch && c.priv == PrivMachine && c.mstatus&statusMPRV != 0 {
		mode := PrivilegeMode((c.mstatus >> 11) & 3)
		if mode == 2 {
			return PrivUser
		}
		return mode
	}
	return c.priv
}

func (m *MMU) walk(c *CPU, addr uint64, kind FaultKind) (uint64, tlbEntry, *MemFault) {
	mode := satpMode(c.satp)
	levels := 0
	switch mode {
	case satpModeSv39:
		levels = 3
	case satpModeSv48:
		levels = 4
	default:
		return 0, tlbEntry{}, m.pageFault(addr, kind)
	}
	if !canonicalVA(addr, levels) {
		return 0, tlbEntry{}, m.pageFault(addr, kind)
	}
	vpn := [4]uint64{}
	for i := 0; i < levels; i++ {
		vpn[i] = (addr >> (12 + 9*uint(i))) & 0x1ff
	}
	pt := (c.satp & ((uint64(1) << 44) - 1)) << 12
	for level := levels - 1; level >= 0; level-- {
		pteAddr := pt + vpn[level]*8
		pte, f := (&c.mem).Load64(pteAddr)
		if f != nil {
			return 0, tlbEntry{}, f
		}
		if pte&pteV == 0 || (pte&pteW != 0 && pte&pteR == 0) {
			return 0, tlbEntry{}, m.pageFault(addr, kind)
		}
		if pte&(pteR|pteX) == 0 {
			pt = ((pte >> 10) & ((uint64(1) << 44) - 1)) << 12
			continue
		}
		perms := tlbPerms(pte)
		if !accessAllowed(c, perms, kind) {
			return 0, tlbEntry{}, m.pageFault(addr, kind)
		}
		ppn := (pte >> 10) & ((uint64(1) << 44) - 1)
		if level > 0 && ppn&((uint64(1)<<(9*uint(level)))-1) != 0 {
			return 0, tlbEntry{}, m.pageFault(addr, kind)
		}
		updated := pte | pteA
		if kind == FaultStore {
			updated |= pteD
		}
		if updated != pte {
			if f := (&c.mem).Store64(pteAddr, updated); f != nil {
				return 0, tlbEntry{}, f
			}
		}
		pageShift := uint(12 + 9*level)
		pageSize := uint64(1) << pageShift
		mask := pageSize - 1
		paddrBase := (ppn << 12) &^ mask
		paddr := paddrBase | (addr & mask)
		fill := tlbEntry{
			tag:       addr &^ mask,
			mask:      mask,
			paddrBase: paddrBase,
			perms:     perms,
			valid:     true,
		}
		fill.direct = m.canDirectMap(c, paddrBase, pageSize)
		if fill.direct {
			fill.hostBase = uintptr(c.mem.hostPtr(paddrBase))
		} else if r, ok := c.mem.mmio.(mmioRangeChecker); ok && r.MMIOOverlaps(paddrBase, pageSize) {
			fill.mmio = true
		}
		return paddr, fill, nil
	}
	return 0, tlbEntry{}, m.pageFault(addr, kind)
}

func (m *MMU) pageFault(addr uint64, kind FaultKind) *MemFault {
	return &MemFault{Addr: addr, Width: 1, Kind: pageFaultKind(kind)}
}

func pageFaultKind(kind FaultKind) FaultKind {
	switch kind {
	case FaultStore:
		return FaultPageStore
	case FaultFetch:
		return FaultPageFetch
	default:
		return FaultPageLoad
	}
}

func (m *MMU) canDirectMap(c *CPU, paddr, size uint64) bool {
	if paddr > c.mem.size || size > c.mem.size-paddr {
		return false
	}
	if c.mem.mmio != nil {
		r, ok := c.mem.mmio.(mmioRangeChecker)
		if !ok || r.MMIOOverlaps(paddr, size) {
			return false
		}
	}
	return true
}

func satpMode(satp uint64) uint64 {
	return satp >> 60
}

// SATP MODE is WARL: unsupported paging modes leave the whole CSR unchanged.
func satpWriteSupported(satp uint64) bool {
	switch satpMode(satp) {
	case satpModeBare, satpModeSv39, satpModeSv48:
		return true
	default:
		return false
	}
}

func canonicalVA(addr uint64, levels int) bool {
	switch levels {
	case 3:
		sign := (addr >> 38) & 1
		upper := addr >> 39
		if sign == 0 {
			return upper == 0
		}
		return upper == (uint64(1)<<25)-1
	case 4:
		sign := (addr >> 47) & 1
		upper := addr >> 48
		if sign == 0 {
			return upper == 0
		}
		return upper == (uint64(1)<<16)-1
	default:
		return false
	}
}

func tlbPerms(pte uint64) uint8 {
	perms := uint8(0)
	if pte&pteR != 0 {
		perms |= tlbR
	}
	if pte&pteW != 0 {
		perms |= tlbW
	}
	if pte&pteX != 0 {
		perms |= tlbX
	}
	if pte&pteU != 0 {
		perms |= tlbU
	}
	return perms
}

func accessAllowed(c *CPU, perms uint8, kind FaultKind) bool {
	priv := c.memoryPrivilege(kind)
	userPage := perms&tlbU != 0
	if priv == PrivUser && !userPage {
		return false
	}
	if priv == PrivSupervisor {
		if kind == FaultFetch && userPage {
			return false
		}
		if kind != FaultFetch && userPage && c.mstatus&statusSUM == 0 {
			return false
		}
	}
	switch kind {
	case FaultFetch:
		return perms&tlbX != 0
	case FaultStore:
		return perms&tlbW != 0
	default:
		return perms&tlbR != 0 || (c.mstatus&statusMXR != 0 && perms&tlbX != 0)
	}
}
