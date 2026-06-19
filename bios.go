package riscv

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var errBiosBudgetExpired = errors.New("bios instruction budget expired")

const (
	defaultBiosFDTAddr    = uint64(0x88000000)
	defaultBiosKernelAddr = uint64(0x80200000)
	defaultBiosInitrdAddr = uint64(0x84000000)
	fwJumpGenericFDTAddr  = uint64(0x82200000)
	fwDynamicInfoMagic    = uint64(0x4942534f)
	fwDynamicInfoVersion  = uint64(2)
	fwDynamicNextModeS    = uint64(1)
	biosInitrdGap         = uint64(16 << 20)
	biosPayloadAlign      = uint64(2 << 20)
	biosPageAlign         = uint64(4096)
	virtRAMBase           = uint64(0x80000000)
	virtCPUIntcPH         = uint32(1)
	virtPLICPH            = uint32(2)
	virtSysconPH          = uint32(3)
	biosSysconBase        = uint64(0x00100000)
	biosSysconSize        = uint64(0x1000)
	biosSysconResetOffset = uint32(0)
	biosSysconResetValue  = uint32(1)
	biosUARTBase          = uint64(0x10000000)
	biosUART1Base         = uint64(0x10000100)
	biosUARTSize          = uint64(0x100)
	biosHostIOBase        = uint64(0x10001000)
	biosHostIOSize        = uint64(0x1000)
	biosVirtioNetBase     = uint64(0x10008000)
	biosVirtioNetSize     = uint64(0x1000)
	biosCLINTBase         = uint64(0x02000000)
	biosCLINTSize         = uint64(0x00010000)
	biosPLICBase          = uint64(0x0c000000)
	biosPLICSize          = uint64(0x04000000)
	biosVirtioNetIRQ      = uint32(1)
	biosUARTIRQ           = uint32(10)
	biosUART1IRQ          = uint32(11)
	plicSContext          = uint32(1)
)

const (
	uartIERRDI  = byte(1) << 0
	uartIERTHRI = byte(1) << 1
	uartIIRNone = byte(0x01)
	uartIIRTHRI = byte(0x02)
	uartIIRRDI  = byte(0x04)
	uartLCRDLAB = byte(0x80)
	uartMCRDTR  = byte(0x01)
	uartLSRDR   = byte(0x01)
	uartLSRTHRE = byte(0x20)
	uartLSRTEMT = byte(0x40)
	uartRXLimit = 4096
)

type biosGuest struct {
	mem         *GuestMemory
	cpu         *CPU
	elf         *ELF
	mmio        *biosMMIO
	fdt         []byte
	fdtAddr     uint64
	dynamicAddr uint64
	nextAddr    uint64
	kernel      biosBlob
	initrd      biosBlob
	externalDTB bool
}

func runEmuBios(cfg EmuConfig, budget uint64) (int, error) {
	if cfg.JITLazy || cfg.JITAOT {
		return 0, fmt.Errorf("-jitlazy and -jitaot are not supported with -bios yet")
	}
	restoreTerminal, raw, err := enableRawTerminal(cfg.Stdin)
	if err != nil {
		return 0, err
	}
	restore := func() {}
	if raw {
		var restoreOnce sync.Once
		restore = func() {
			restoreOnce.Do(func() {
				_ = restoreTerminal()
			})
		}
		defer restore()
	}
	guest, err := prepareBiosGuestWithReset(cfg, func() {
		restore()
		os.Exit(0)
	})
	if err != nil {
		return 0, err
	}
	defer guest.mem.Free()
	defer guest.mmio.closeUARTOutput()
	defer guest.mmio.closeHostIO()
	defer guest.mmio.closeVirtioNet()

	res, err := RunBiosMachineBudget(guest.cpu, &guest.cpu.Notes, budget)
	if err != nil {
		return 0, err
	}
	if res == RunBudgetExit {
		return guest.cpu.ExitCode, nil
	}
	if res == RunBudgetExpired {
		return 0, fmt.Errorf("%w after %d instructions", errBiosBudgetExpired, budget)
	}
	return 0, nil
}

func prepareBiosGuest(cfg EmuConfig) (*biosGuest, error) {
	return prepareBiosGuestWithReset(cfg, nil)
}

func prepareBiosGuestWithReset(cfg EmuConfig, onSystemReset func()) (*biosGuest, error) {
	cfg = cfg.withDefaults()
	if err := cfg.resolveMemory(); err != nil {
		return nil, err
	}
	if cfg.BiosPath == "" {
		return nil, fmt.Errorf("prepare BIOS guest requires -bios")
	}
	if cfg.MemorySize <= defaultBiosFDTAddr {
		return nil, fmt.Errorf("-mem %#x is too small for BIOS FDT address %#x", cfg.MemorySize, defaultBiosFDTAddr)
	}

	mem, err := NewGuestMemory(cfg.MemorySize)
	if err != nil {
		return nil, err
	}
	elf, err := LoadELF(mem, cfg.BiosPath)
	if err != nil {
		mem.Free()
		return nil, err
	}

	kernel, err := loadBiosKernel(mem, cfg)
	if err != nil {
		mem.Free()
		return nil, err
	}
	initrd, err := loadBiosInitrd(mem, cfg, kernel)
	if err != nil {
		mem.Free()
		return nil, err
	}

	fdt, externalDTB, err := loadBiosFDT(cfg, initrd)
	if err != nil {
		mem.Free()
		return nil, err
	}
	fdtAddr, err := chooseBiosFDTAddr(cfg.MemorySize, len(fdt), initrd)
	if err != nil {
		mem.Free()
		return nil, err
	}
	if fault := mem.WriteBytes(fdtAddr, fdt); fault != nil {
		mem.Free()
		return nil, fault
	}
	nextAddr := biosNextStageAddr(cfg, kernel)
	dynamicAddr := uint64(0)
	if isFWJumpBios(cfg.BiosPath) {
		if err := checkFWJumpFDTOverlap(kernel, len(fdt)); err != nil {
			mem.Free()
			return nil, err
		}
	}
	if isFWDynamicBios(cfg.BiosPath) {
		dynamicAddr, err = writeFWDynamicInfo(mem, cfg.MemorySize, nextAddr, fdtAddr, len(fdt), kernel, initrd)
		if err != nil {
			mem.Free()
			return nil, err
		}
	}
	if cfg.DumpDTBPath != "" {
		if err := os.WriteFile(cfg.DumpDTBPath, fdt, 0644); err != nil {
			mem.Free()
			return nil, err
		}
	}

	mmio := newBiosMMIOWithConsoleSockets(cfg.Stdin, cfg.Stdout, onSystemReset, cfg.Stdin == os.Stdin && cfg.Stdout == os.Stdout)
	if cfg.HostIO {
		mmio.enableHostIO(mem)
	}
	if cfg.Net {
		stack, err := newVirtioNetPacketStack(cfg)
		if err != nil {
			mem.Free()
			return nil, err
		}
		mmio.enableVirtioNet(mem, stack)
	}
	mem.SetMMIO(mmio)
	cpu := NewCPU(*mem)
	cpu.EnableStrictCSR()
	cpu.SetPrivilegeMode(PrivMachine)
	cpu.EnableMMU()
	cpu.SetPC(elf.Entry)
	cpu.SetReg(10, 0)       // a0: boot hart id
	cpu.SetReg(11, fdtAddr) // a1: flattened device tree pointer
	cpu.SetReg(12, dynamicAddr)

	return &biosGuest{
		mem:         mem,
		cpu:         cpu,
		elf:         elf,
		mmio:        mmio,
		fdt:         fdt,
		fdtAddr:     fdtAddr,
		dynamicAddr: dynamicAddr,
		nextAddr:    nextAddr,
		kernel:      kernel,
		initrd:      initrd,
		externalDTB: externalDTB,
	}, nil
}

type biosBlob struct {
	path   string
	addr   uint64
	end    uint64
	data   []byte
	loaded bool
	elf    bool
}

func loadBiosKernel(mem *GuestMemory, cfg EmuConfig) (biosBlob, error) {
	if cfg.KernelPath == "" {
		return biosBlob{}, nil
	}
	data, err := os.ReadFile(cfg.KernelPath)
	if err != nil {
		return biosBlob{}, err
	}
	if len(data) == 0 {
		return biosBlob{}, fmt.Errorf("-kernel path '%v' is empty", cfg.KernelPath)
	}
	if isELF(data) {
		elf, err := LoadELFBytes(mem, data)
		if err != nil {
			return biosBlob{}, err
		}
		return biosBlob{
			path:   cfg.KernelPath,
			addr:   elf.Entry,
			data:   data,
			loaded: true,
			elf:    true,
		}, nil
	}

	addr := cfg.effectiveKernelAddr()
	if err := checkBiosRange("-kernel", addr, len(data), mem.Size()); err != nil {
		return biosBlob{}, err
	}
	if fault := mem.WriteBytes(addr, data); fault != nil {
		return biosBlob{}, fault
	}
	return biosBlob{
		path:   cfg.KernelPath,
		addr:   addr,
		end:    addr + uint64(len(data)),
		data:   data,
		loaded: true,
	}, nil
}

func loadBiosInitrd(mem *GuestMemory, cfg EmuConfig, kernel biosBlob) (biosBlob, error) {
	if cfg.InitrdPath == "" {
		return biosBlob{}, nil
	}
	data, err := os.ReadFile(cfg.InitrdPath)
	if err != nil {
		return biosBlob{}, err
	}
	if len(data) == 0 {
		return biosBlob{}, fmt.Errorf("-initrd path '%v' is empty", cfg.InitrdPath)
	}
	addr := defaultBiosInitrdAddr
	if kernel.loaded && !kernel.elf && kernel.end != 0 {
		if afterKernel := alignUp64(kernel.end+biosInitrdGap, biosPayloadAlign); afterKernel > addr {
			addr = afterKernel
		}
	}
	if err := checkBiosRange("-initrd", addr, len(data), mem.Size()); err != nil {
		return biosBlob{}, err
	}
	if fault := mem.WriteBytes(addr, data); fault != nil {
		return biosBlob{}, fault
	}
	return biosBlob{
		path:   cfg.InitrdPath,
		addr:   addr,
		end:    addr + uint64(len(data)),
		data:   data,
		loaded: true,
	}, nil
}

func loadBiosFDT(cfg EmuConfig, initrd biosBlob) ([]byte, bool, error) {
	if cfg.DTBPath != "" {
		data, err := os.ReadFile(cfg.DTBPath)
		if err != nil {
			return nil, false, err
		}
		if !looksLikeFDT(data) {
			return nil, false, fmt.Errorf("-dtb path '%v' is not a flattened device tree blob", cfg.DTBPath)
		}
		return data, true, nil
	}
	fdt, err := buildVirtFDT(cfg.MemorySize, virtFDTOptions{
		Machine:     cfg.machine(),
		BootArgs:    cfg.biosBootArgs(),
		RAMSize:     cfg.BiosRAMSize,
		InitrdStart: initrd.addr,
		InitrdEnd:   initrd.end,
		HostIO:      cfg.HostIO,
		Net:         cfg.Net,
	})
	return fdt, false, err
}

func (c EmuConfig) biosBootArgs() string {
	var parts []string
	if appendArgs := strings.TrimSpace(c.Append); appendArgs != "" {
		parts = append(parts, appendArgs)
	}
	if c.BiosPath != "" && len(c.Args) > 1 {
		parts = append(parts, c.Args[1:]...)
	}
	return strings.Join(parts, " ")
}

// Hardware model boundary.
//
// biosMMIO and the device structs it owns model a real physical board. Keep
// that boundary honest even though this code now lives in the same Go package
// as CPU, MMU, and GuestMemory internals:
//
//   - Devices observe guest physical addresses, not guest virtual addresses.
//   - Devices move bytes through their MMIO registers, descriptor rings, and
//     guest physical RAM helpers. They must not inspect page tables, TLBs,
//     process state, Linux symbols, or other guest software internals.
//   - Devices must not set PC, privilege mode, trap CSRs, or directly deliver
//     traps. They assert interrupt-pending device state; the CPU interrupt path
//     decides when and how the guest takes the interrupt.
//   - MMIO load/store handlers must not call back into the CPU run loop or
//     otherwise execute guest instructions recursively.
//   - Fast paths are welcome, but only when they preserve the same guest-visible
//     physical-memory, fault, and interrupt behavior as the modeled hardware.
//
// These are package conventions, not Go-enforced restrictions. The lowercase
// device helpers below are intentionally private reminders that board code is
// allowed to be concrete and fast, but not allowed to cheat through CPU state.
type biosMMIO struct {
	stdout        io.Writer
	uarts         [2]biosUARTPort
	onSystemReset func()
	hostio        *hostIODevice
	virtioNet     *virtioNetDevice

	clint         [0x10000]byte
	mtime         uint64
	plicPriority  [64]uint32
	plicEnable    [2]uint64
	plicThreshold [2]uint32
	plicClaimed   [2]uint32
}

type biosUARTPort struct {
	regs        [0x100]byte
	txInterrupt bool
	rx          []byte
	rxCh        chan byte
	out         interface {
		WriteByte(byte)
		Close() error
	}
}

func newBiosMMIO(stdin io.Reader, stdout io.Writer, onSystemReset func()) *biosMMIO {
	return newBiosMMIOWithConsoleSockets(stdin, stdout, onSystemReset, false)
}

func newBiosMMIOWithConsoleSockets(stdin io.Reader, stdout io.Writer, onSystemReset func(), consoleSockets bool) *biosMMIO {
	if stdout == nil {
		stdout = io.Discard
	}
	m := &biosMMIO{stdout: stdout, onSystemReset: onSystemReset}
	if _, ok := stdout.(*bytes.Buffer); !ok && stdout != io.Discard {
		m.uarts[0].out = newAsyncUARTOutput(stdout)
	}
	for i := range m.uarts {
		m.uarts[i].regs[2] = 0x01 // IIR: no interrupt pending
		m.uarts[i].regs[5] = 0x60 // LSR: transmitter holding register empty, idle
	}
	storeLittleEndian(m.clint[:], 0x4000, 8, ^uint64(0))
	m.startUARTInput(0, stdin)
	if consoleSockets {
		m.enableConsoleSocket(1)
	}
	return m
}

func (m *biosMMIO) enableHostIO(mem *GuestMemory) {
	m.hostio = newHostIODevice(mem)
}

func (m *biosMMIO) enableVirtioNet(mem *GuestMemory, stack virtioNetPacketStack) {
	m.virtioNet = newVirtioNetDevice(mem, stack)
	if attach, ok := stack.(interface{ attachVirtioNet(*virtioNetDevice) }); ok {
		attach.attachVirtioNet(m.virtioNet)
	}
}

func (m *biosMMIO) enableConsoleSocket(index int) {
	if index < 0 || index >= len(m.uarts) {
		return
	}
	if m.uarts[index].rxCh == nil {
		m.uarts[index].rxCh = make(chan byte, uartRXLimit)
	}
	console, err := newEmuConsoleSocket(index, m.uarts[index].rxCh)
	if err != nil {
		writeTerminalStatusf("console%d: %v", index, err)
		return
	}
	m.uarts[index].out = console
}

func (m *biosMMIO) startUARTInput(index int, stdin io.Reader) {
	if stdin == nil {
		return
	}
	if index < 0 || index >= len(m.uarts) {
		return
	}
	m.uarts[index].rxCh = make(chan byte, uartRXLimit)
	rxCh := m.uarts[index].rxCh
	go func() {
		var buf [256]byte
		for {
			n, err := stdin.Read(buf[:])
			for i := 0; i < n; i++ {
				rxCh <- buf[i]
			}
			if err != nil {
				close(rxCh)
				return
			}
		}
	}()
}

func (m *biosMMIO) Load(addr, width uint64) (uint64, bool, *MemFault) {
	if off, ok := mmioRangeOffset(addr, width, biosSysconBase, biosSysconSize); ok {
		return m.loadSyscon(off, width), true, nil
	}
	if index, off, ok := biosUARTRangeOffset(addr, width); ok {
		return m.loadUARTPort(index, off, width), true, nil
	}
	if m.hostio != nil {
		if off, ok := mmioRangeOffset(addr, width, biosHostIOBase, biosHostIOSize); ok {
			return m.hostio.Load(off, width), true, nil
		}
	}
	if m.virtioNet != nil {
		if off, ok := mmioRangeOffset(addr, width, biosVirtioNetBase, biosVirtioNetSize); ok {
			return m.virtioNet.Load(off, width), true, nil
		}
	}
	if off, ok := mmioRangeOffset(addr, width, biosCLINTBase, biosCLINTSize); ok {
		return m.loadCLINT(off, width), true, nil
	}
	if off, ok := mmioRangeOffset(addr, width, biosPLICBase, biosPLICSize); ok {
		return m.loadPLIC(off, width), true, nil
	}
	if mmioRangeTouches(addr, width, biosSysconBase, biosSysconSize) ||
		biosUARTRangeTouches(addr, width) ||
		(m.hostio != nil && mmioRangeTouches(addr, width, biosHostIOBase, biosHostIOSize)) ||
		(m.virtioNet != nil && mmioRangeTouches(addr, width, biosVirtioNetBase, biosVirtioNetSize)) ||
		mmioRangeTouches(addr, width, biosCLINTBase, biosCLINTSize) ||
		mmioRangeTouches(addr, width, biosPLICBase, biosPLICSize) {
		return 0, true, &MemFault{Addr: addr, Width: width, Kind: FaultLoad}
	}
	return 0, false, nil
}

func (m *biosMMIO) Store(addr, width, value uint64) (bool, *MemFault) {
	if off, ok := mmioRangeOffset(addr, width, biosSysconBase, biosSysconSize); ok {
		m.storeSyscon(off, width, value)
		return true, nil
	}
	if index, off, ok := biosUARTRangeOffset(addr, width); ok {
		m.storeUARTPort(index, off, width, value)
		return true, nil
	}
	if m.hostio != nil {
		if off, ok := mmioRangeOffset(addr, width, biosHostIOBase, biosHostIOSize); ok {
			return true, m.hostio.Store(off, width, value)
		}
	}
	if m.virtioNet != nil {
		if off, ok := mmioRangeOffset(addr, width, biosVirtioNetBase, biosVirtioNetSize); ok {
			return true, m.virtioNet.Store(off, width, value)
		}
	}
	if off, ok := mmioRangeOffset(addr, width, biosCLINTBase, biosCLINTSize); ok {
		m.storeCLINT(off, width, value)
		return true, nil
	}
	if off, ok := mmioRangeOffset(addr, width, biosPLICBase, biosPLICSize); ok {
		m.storePLIC(off, width, value)
		return true, nil
	}
	if mmioRangeTouches(addr, width, biosSysconBase, biosSysconSize) ||
		biosUARTRangeTouches(addr, width) ||
		(m.hostio != nil && mmioRangeTouches(addr, width, biosHostIOBase, biosHostIOSize)) ||
		(m.virtioNet != nil && mmioRangeTouches(addr, width, biosVirtioNetBase, biosVirtioNetSize)) ||
		mmioRangeTouches(addr, width, biosCLINTBase, biosCLINTSize) ||
		mmioRangeTouches(addr, width, biosPLICBase, biosPLICSize) {
		return true, &MemFault{Addr: addr, Width: width, Kind: FaultStore}
	}
	return false, nil
}

func (m *biosMMIO) MMIOOverlaps(addr, size uint64) bool {
	return rangesOverlap(addr, addr+size, biosSysconBase, biosSysconBase+biosSysconSize) ||
		(rangesOverlap(addr, addr+size, biosUARTBase, biosUARTBase+biosUARTSize) ||
			rangesOverlap(addr, addr+size, biosUART1Base, biosUART1Base+biosUARTSize)) ||
		(m.hostio != nil && rangesOverlap(addr, addr+size, biosHostIOBase, biosHostIOBase+biosHostIOSize)) ||
		(m.virtioNet != nil && rangesOverlap(addr, addr+size, biosVirtioNetBase, biosVirtioNetBase+biosVirtioNetSize)) ||
		rangesOverlap(addr, addr+size, biosCLINTBase, biosCLINTBase+biosCLINTSize) ||
		rangesOverlap(addr, addr+size, biosPLICBase, biosPLICBase+biosPLICSize)
}

func (m *biosMMIO) loadSyscon(off, width uint64) uint64 {
	return 0
}

func (m *biosMMIO) storeSyscon(off, width, value uint64) {
	if !mmioRangeTouches(off, width, uint64(biosSysconResetOffset), 4) {
		return
	}
	m.closeUARTOutput()
	if m.onSystemReset != nil {
		m.onSystemReset()
	}
}

func biosUARTRangeOffset(addr, width uint64) (int, uint64, bool) {
	if off, ok := mmioRangeOffset(addr, width, biosUARTBase, biosUARTSize); ok {
		return 0, off, true
	}
	if off, ok := mmioRangeOffset(addr, width, biosUART1Base, biosUARTSize); ok {
		return 1, off, true
	}
	return 0, 0, false
}

func biosUARTRangeTouches(addr, width uint64) bool {
	return mmioRangeTouches(addr, width, biosUARTBase, biosUARTSize) ||
		mmioRangeTouches(addr, width, biosUART1Base, biosUARTSize)
}

func (m *biosMMIO) loadUART(off, width uint64) uint64 {
	return m.loadUARTPort(0, off, width)
}

func (m *biosMMIO) loadUARTPort(index int, off, width uint64) uint64 {
	var value uint64
	for i := uint64(0); i < width; i++ {
		value |= uint64(m.uartByte(index, off+i)) << (8 * i)
	}
	return value
}

func (m *biosMMIO) uartByte(index int, off uint64) byte {
	if index < 0 || index >= len(m.uarts) {
		return 0
	}
	uart := &m.uarts[index]
	m.drainUARTInput(index)
	dlab := uart.regs[3]&uartLCRDLAB != 0
	switch off {
	case 0:
		if !dlab {
			if len(uart.rx) == 0 {
				return 0
			}
			b := uart.rx[0]
			copy(uart.rx, uart.rx[1:])
			uart.rx = uart.rx[:len(uart.rx)-1]
			return b
		}
		return uart.regs[0]
	case 1:
		return uart.regs[1]
	case 2:
		if !dlab && m.uartRXInterruptPending(index) {
			return uartIIRRDI
		}
		if !dlab && m.uartTXInterruptPending(index) {
			uart.txInterrupt = false
			return uartIIRTHRI
		}
		return uartIIRNone
	case 5:
		lsr := uartLSRTHRE | uartLSRTEMT
		if len(uart.rx) != 0 {
			lsr |= uartLSRDR
		}
		return lsr
	default:
		return uart.regs[off]
	}
}

func (m *biosMMIO) storeUART(off, width, value uint64) {
	m.storeUARTPort(0, off, width, value)
}

func (m *biosMMIO) storeUARTPort(index int, off, width, value uint64) {
	if index < 0 || index >= len(m.uarts) {
		return
	}
	uart := &m.uarts[index]
	for i := uint64(0); i < width; i++ {
		b := byte(value >> (8 * i))
		idx := off + i
		dlab := uart.regs[3]&uartLCRDLAB != 0
		old := uart.regs[idx]
		uart.regs[idx] = b
		if idx == 0 && !dlab {
			m.writeUARTOutput(index, b)
			if uart.regs[1]&uartIERTHRI != 0 {
				uart.txInterrupt = true
			}
		}
		if idx == 1 && !dlab {
			if b&uartIERTHRI != 0 {
				uart.txInterrupt = true
			} else {
				uart.txInterrupt = false
			}
		}
		if idx == 4 {
			m.uartModemControlChanged(index, old, b)
		}
	}
	uart.regs[5] = uartLSRTHRE | uartLSRTEMT
}

func (m *biosMMIO) uartModemControlChanged(index int, old, new byte) {
	if old&uartMCRDTR == 0 || new&uartMCRDTR != 0 {
		return
	}
	out := m.uarts[index].out
	guestClose, ok := out.(interface{ GuestClose() error })
	if !ok {
		return
	}
	_ = guestClose.GuestClose()
}

func (m *biosMMIO) writeUARTOutput(index int, b byte) {
	if index < 0 || index >= len(m.uarts) {
		return
	}
	if m.uarts[index].out != nil {
		m.uarts[index].out.WriteByte(b)
		return
	}
	if index != 0 {
		return
	}
	var one [1]byte
	one[0] = b
	_, _ = m.stdout.Write(one[:])
}

func (m *biosMMIO) closeUARTOutput() {
	for i := range m.uarts {
		if m.uarts[i].out == nil {
			continue
		}
		_ = m.uarts[i].out.Close()
		m.uarts[i].out = nil
	}
}

func (m *biosMMIO) closeHostIO() {
	if m.hostio == nil {
		return
	}
	m.hostio.Close()
	m.hostio = nil
}

func (m *biosMMIO) closeVirtioNet() {
	if m.virtioNet == nil {
		return
	}
	m.virtioNet.Close()
	m.virtioNet = nil
}

type asyncUARTOutput struct {
	ch   chan byte
	done chan struct{}
}

func newAsyncUARTOutput(w io.Writer) *asyncUARTOutput {
	out := &asyncUARTOutput{
		ch:   make(chan byte, 64*1024),
		done: make(chan struct{}),
	}
	go out.run(w)
	return out
}

func (o *asyncUARTOutput) WriteByte(b byte) {
	o.ch <- b
}

func (o *asyncUARTOutput) Close() error {
	close(o.ch)
	<-o.done
	return nil
}

func (o *asyncUARTOutput) run(w io.Writer) {
	defer close(o.done)
	bw := bufio.NewWriterSize(w, 4096)
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	flush := func() {
		if bw.Buffered() != 0 {
			_ = bw.Flush()
		}
	}
	for {
		select {
		case b, ok := <-o.ch:
			if !ok {
				flush()
				return
			}
			_ = bw.WriteByte(b)
			if b == '\n' || bw.Buffered() >= 2048 {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

func (m *biosMMIO) uartInterruptPending() bool {
	for i := range m.uarts {
		if m.uartInterruptPendingAt(i) {
			return true
		}
	}
	return false
}

func (m *biosMMIO) uartInterruptPendingAt(index int) bool {
	m.drainUARTInput(index)
	return m.uartRXInterruptPending(index) || m.uartTXInterruptPending(index)
}

func (m *biosMMIO) uartRXInterruptPending(index int) bool {
	if index < 0 || index >= len(m.uarts) {
		return false
	}
	uart := &m.uarts[index]
	return uart.regs[1]&uartIERRDI != 0 && len(uart.rx) != 0
}

func (m *biosMMIO) uartTXInterruptPending(index int) bool {
	if index < 0 || index >= len(m.uarts) {
		return false
	}
	uart := &m.uarts[index]
	return uart.regs[1]&uartIERTHRI != 0 && uart.txInterrupt
}

func (m *biosMMIO) drainUARTInput(index int) {
	if index < 0 || index >= len(m.uarts) {
		return
	}
	uart := &m.uarts[index]
	for uart.rxCh != nil && len(uart.rx) < uartRXLimit {
		select {
		case b, ok := <-uart.rxCh:
			if !ok {
				uart.rxCh = nil
				return
			}
			uart.rx = append(uart.rx, b)
		default:
			return
		}
	}
}

func (m *biosMMIO) loadCLINT(off, width uint64) uint64 {
	m.syncCLINTTime()
	return loadLittleEndian(m.clint[:], off, width)
}

func (m *biosMMIO) storeCLINT(off, width, value uint64) {
	storeLittleEndian(m.clint[:], off, width, value)
	if mmioRangeTouches(off, width, 0xbff8, 8) {
		m.mtime = loadLittleEndian(m.clint[:], 0xbff8, 8)
	}
}

func (m *biosMMIO) AdvanceMachineTimer(delta uint64) {
	m.mtime += delta
	m.syncCLINTTime()
}

func (m *biosMMIO) MachineTimerValue() uint64 {
	return m.mtime
}

func (m *biosMMIO) SupervisorExternalInterruptPending() bool {
	return m.plicPendingForContext(plicSContext) != 0
}

func (m *biosMMIO) loadPLIC(off, width uint64) uint64 {
	if width != 4 {
		return 0
	}
	if off < 0x1000 {
		source := off / 4
		if source < uint64(len(m.plicPriority)) {
			return uint64(m.plicPriority[source])
		}
		return 0
	}
	if off >= 0x1000 && off < 0x2000 {
		return uint64(m.plicPendingBits())
	}
	if off >= 0x2000 && off < 0x2000+0x80*uint64(len(m.plicEnable)) {
		ctx := (off - 0x2000) / 0x80
		word := ((off - 0x2000) % 0x80) / 4
		if word == 0 {
			return uint64(uint32(m.plicEnable[ctx]))
		}
		if word == 1 {
			return uint64(uint32(m.plicEnable[ctx] >> 32))
		}
		return 0
	}
	if off >= 0x200000 && off < 0x200000+0x1000*uint64(len(m.plicThreshold)) {
		ctx := uint32((off - 0x200000) / 0x1000)
		reg := (off - 0x200000) % 0x1000
		switch reg {
		case 0:
			return uint64(m.plicThreshold[ctx])
		case 4:
			return uint64(m.plicClaim(ctx))
		default:
			return 0
		}
	}
	return 0
}

func (m *biosMMIO) storePLIC(off, width, value uint64) {
	if width != 4 {
		return
	}
	if off < 0x1000 {
		source := off / 4
		if source < uint64(len(m.plicPriority)) {
			m.plicPriority[source] = uint32(value)
		}
		return
	}
	if off >= 0x2000 && off < 0x2000+0x80*uint64(len(m.plicEnable)) {
		ctx := (off - 0x2000) / 0x80
		word := ((off - 0x2000) % 0x80) / 4
		switch word {
		case 0:
			m.plicEnable[ctx] = (m.plicEnable[ctx] &^ uint64(0xffffffff)) | uint64(uint32(value))
		case 1:
			m.plicEnable[ctx] = (m.plicEnable[ctx] & uint64(0xffffffff)) | uint64(uint32(value))<<32
		}
		return
	}
	if off >= 0x200000 && off < 0x200000+0x1000*uint64(len(m.plicThreshold)) {
		ctx := uint32((off - 0x200000) / 0x1000)
		reg := (off - 0x200000) % 0x1000
		switch reg {
		case 0:
			m.plicThreshold[ctx] = uint32(value)
		case 4:
			m.plicComplete(ctx, uint32(value))
		}
	}
}

func (m *biosMMIO) plicPendingBits() uint32 {
	pending := uint32(0)
	if m.uartInterruptPendingAt(0) {
		pending |= uint32(1) << biosUARTIRQ
	}
	if m.uartInterruptPendingAt(1) {
		pending |= uint32(1) << biosUART1IRQ
	}
	if m.virtioNet != nil && m.virtioNet.InterruptPending() {
		pending |= uint32(1) << biosVirtioNetIRQ
	}
	return pending
}

func (m *biosMMIO) plicPendingForContext(ctx uint32) uint32 {
	if ctx >= uint32(len(m.plicEnable)) || m.plicClaimed[ctx] != 0 {
		return 0
	}
	pending := m.plicPendingBits()
	best := uint32(0)
	bestPriority := uint32(0)
	for source := uint32(1); source < uint32(len(m.plicPriority)); source++ {
		if pending&(uint32(1)<<source) == 0 || m.plicEnable[ctx]&(uint64(1)<<source) == 0 {
			continue
		}
		priority := m.plicPriority[source]
		if priority <= m.plicThreshold[ctx] || priority <= bestPriority {
			continue
		}
		best = source
		bestPriority = priority
	}
	return best
}

func (m *biosMMIO) plicClaim(ctx uint32) uint32 {
	source := m.plicPendingForContext(ctx)
	m.plicClaimed[ctx] = source
	return source
}

func (m *biosMMIO) plicComplete(ctx, source uint32) {
	if ctx < uint32(len(m.plicClaimed)) && m.plicClaimed[ctx] == source {
		m.plicClaimed[ctx] = 0
	}
}

func (m *biosMMIO) syncCLINTTime() {
	storeLittleEndian(m.clint[:], 0xbff8, 8, m.mtime)
}

func mmioRangeOffset(addr, width, base, size uint64) (uint64, bool) {
	if width == 0 || addr < base {
		return 0, false
	}
	off := addr - base
	if off >= size || width > size-off {
		return 0, false
	}
	return off, true
}

func mmioRangeTouches(addr, width, base, size uint64) bool {
	if width == 0 || addr < base {
		return false
	}
	off := addr - base
	return off < size
}

func loadLittleEndian(buf []byte, off, width uint64) uint64 {
	var value uint64
	for i := uint64(0); i < width; i++ {
		value |= uint64(buf[off+i]) << (8 * i)
	}
	return value
}

func storeLittleEndian(buf []byte, off, width, value uint64) {
	for i := uint64(0); i < width; i++ {
		buf[off+i] = byte(value >> (8 * i))
	}
}

func chooseBiosFDTAddr(memSize uint64, fdtLen int, initrd biosBlob) (uint64, error) {
	addr := defaultBiosFDTAddr
	if initrd.loaded && rangesOverlap(addr, addr+uint64(fdtLen), initrd.addr, initrd.end) {
		addr = alignUp64(initrd.end+biosPayloadAlign, biosPayloadAlign)
	}
	if err := checkBiosRange("-dtb", addr, fdtLen, memSize); err != nil {
		return 0, err
	}
	return addr, nil
}

func checkBiosRange(name string, addr uint64, length int, memSize uint64) error {
	if length == 0 {
		return fmt.Errorf("%s payload is empty", name)
	}
	n := uint64(length)
	if addr > memSize || n > memSize-addr {
		return fmt.Errorf("%s payload at %#x..%#x exceeds guest memory size %#x", name, addr, addr+n, memSize)
	}
	return nil
}

func (c EmuConfig) effectiveKernelAddr() uint64 {
	if c.KernelAddr != 0 {
		return c.KernelAddr
	}
	return defaultBiosKernelAddr
}

func biosNextStageAddr(cfg EmuConfig, kernel biosBlob) uint64 {
	if kernel.loaded {
		return kernel.addr
	}
	return cfg.effectiveKernelAddr()
}

func isFWJumpBios(path string) bool {
	return strings.Contains(filepath.Base(path), "fw_jump")
}

func isFWDynamicBios(path string) bool {
	return strings.Contains(filepath.Base(path), "fw_dynamic")
}

func checkFWJumpFDTOverlap(kernel biosBlob, fdtLen int) error {
	if !kernel.loaded || kernel.elf || kernel.end == 0 {
		return nil
	}
	fdtEnd := fwJumpGenericFDTAddr + uint64(fdtLen)
	if !rangesOverlap(fwJumpGenericFDTAddr, fdtEnd, kernel.addr, kernel.end) {
		return nil
	}
	return fmt.Errorf("-bios fw_jump fixed FDT handoff %#x..%#x overlaps raw -kernel %#x..%#x; use fw_dynamic.elf or rebuild OpenSBI with a larger FW_JUMP_FDT_OFFSET",
		fwJumpGenericFDTAddr, fdtEnd, kernel.addr, kernel.end)
}

func writeFWDynamicInfo(mem *GuestMemory, memSize, nextAddr, fdtAddr uint64, fdtLen int, kernel, initrd biosBlob) (uint64, error) {
	addr, err := chooseFWDynamicInfoAddr(memSize, fdtAddr, fdtLen, kernel, initrd)
	if err != nil {
		return 0, err
	}
	var buf [48]byte
	fields := []uint64{
		fwDynamicInfoMagic,
		fwDynamicInfoVersion,
		nextAddr,
		fwDynamicNextModeS,
		0,
		0,
	}
	for i, field := range fields {
		binary.LittleEndian.PutUint64(buf[i*8:], field)
	}
	if fault := mem.WriteBytes(addr, buf[:]); fault != nil {
		return 0, fault
	}
	return addr, nil
}

func chooseFWDynamicInfoAddr(memSize, fdtAddr uint64, fdtLen int, kernel, initrd biosBlob) (uint64, error) {
	candidates := []uint64{}
	if fdtAddr >= biosPageAlign {
		candidates = append(candidates, fdtAddr-biosPageAlign)
	}
	candidates = append(candidates, alignUp64(fdtAddr+uint64(fdtLen), biosPageAlign))
	for _, addr := range candidates {
		end := addr + 48
		if addr > memSize || end > memSize {
			continue
		}
		if rangesOverlap(addr, end, fdtAddr, fdtAddr+uint64(fdtLen)) {
			continue
		}
		if kernel.loaded && kernel.end != 0 && rangesOverlap(addr, end, kernel.addr, kernel.end) {
			continue
		}
		if initrd.loaded && rangesOverlap(addr, end, initrd.addr, initrd.end) {
			continue
		}
		return addr, nil
	}
	return 0, fmt.Errorf("could not place OpenSBI fw_dynamic info block")
}

func isELF(data []byte) bool {
	return len(data) >= 4 && string(data[:4]) == "\x7fELF"
}

func looksLikeFDT(data []byte) bool {
	return len(data) >= 4 && binary.BigEndian.Uint32(data[:4]) == fdtMagic
}

func rangesOverlap(a0, a1, b0, b1 uint64) bool {
	return a0 < b1 && b0 < a1
}

func alignUp64(v, align uint64) uint64 {
	return (v + align - 1) &^ (align - 1)
}

type virtFDTOptions struct {
	Machine     string
	BootArgs    string
	RAMSize     uint64
	InitrdStart uint64
	InitrdEnd   uint64
	HostIO      bool
	Net         bool
}

func buildVirtFDT(memSize uint64, opts virtFDTOptions) ([]byte, error) {
	if memSize <= virtRAMBase {
		return nil, fmt.Errorf("memory size %#x does not cover virt RAM base %#x", memSize, virtRAMBase)
	}
	ramSize := memSize - virtRAMBase
	if opts.RAMSize != 0 {
		ramSize = opts.RAMSize
		if ramSize > memSize-virtRAMBase {
			return nil, fmt.Errorf("RAM size %#x exceeds memory slab %#x above virt RAM base %#x", ramSize, memSize, virtRAMBase)
		}
	}
	b := newFDTBuilder()
	machine := opts.Machine
	if machine == "" {
		machine = "virt"
	}

	b.beginNode("")
	b.propCells("#address-cells", 2)
	b.propCells("#size-cells", 2)
	b.propStringList("compatible", "riscv-emu-golang,"+machine, "riscv-virtio")
	b.propString("model", "riscv-emu-golang "+machine)

	b.beginNode("chosen")
	b.propString("bootargs", opts.BootArgs)
	b.propString("stdout-path", "/soc/uart@10000000")
	if opts.InitrdStart != 0 || opts.InitrdEnd != 0 {
		b.propCells64("linux,initrd-start", opts.InitrdStart)
		b.propCells64("linux,initrd-end", opts.InitrdEnd)
	}
	b.endNode()

	b.beginNode("aliases")
	b.propString("serial0", "/soc/uart@10000000")
	b.propString("serial1", "/soc/uart@10000100")
	b.endNode()

	b.beginNode("memory@80000000")
	b.propString("device_type", "memory")
	b.propCells64("reg", virtRAMBase, ramSize)
	b.endNode()

	b.beginNode("cpus")
	b.propCells("#address-cells", 1)
	b.propCells("#size-cells", 0)
	b.propCells("timebase-frequency", 10000000)

	b.beginNode("cpu@0")
	b.propString("device_type", "cpu")
	b.propCells("reg", 0)
	b.propString("status", "okay")
	b.propString("compatible", "riscv")
	b.propString("riscv,isa-base", "rv64i")
	b.propStringList("riscv,isa-extensions", "i", "m", "a", "f", "d", "c", "zba", "zbb", "zbc", "zicond", "zicsr", "zifencei", "sstc")
	b.propString("riscv,isa", "rv64imafdcsu_zba_zbb_zbc_zicond_zicsr_zifencei_sstc")
	b.propString("mmu-type", "riscv,sv48")

	b.beginNode("interrupt-controller")
	b.propEmpty("interrupt-controller")
	b.propCells("#interrupt-cells", 1)
	b.propString("compatible", "riscv,cpu-intc")
	b.propCells("phandle", virtCPUIntcPH)
	b.endNode()

	b.endNode()
	b.endNode()

	b.beginNode("soc")
	b.propCells("#address-cells", 2)
	b.propCells("#size-cells", 2)
	b.propEmpty("ranges")
	b.propString("compatible", "simple-bus")

	b.beginNode("syscon@100000")
	b.propString("compatible", "syscon")
	b.propCells64("reg", biosSysconBase, biosSysconSize)
	b.propCells("reg-io-width", 4)
	b.propEmpty("little-endian")
	b.propCells("phandle", virtSysconPH)
	b.endNode()

	b.beginNode("reboot")
	b.propString("compatible", "syscon-reboot")
	b.propCells("regmap", virtSysconPH)
	b.propCells("offset", biosSysconResetOffset)
	b.propCells("value", biosSysconResetValue)
	b.propCells("priority", 192)
	b.endNode()

	b.beginNode("clint@2000000")
	b.propStringList("compatible", "sifive,clint0", "riscv,clint0")
	b.propCells64("reg", 0x02000000, 0x00010000)
	b.propCells("interrupts-extended", virtCPUIntcPH, 3, virtCPUIntcPH, 7)
	b.endNode()

	b.beginNode("interrupt-controller@c000000")
	b.propEmpty("interrupt-controller")
	b.propCells("#interrupt-cells", 1)
	b.propStringList("compatible", "sifive,plic-1.0.0", "riscv,plic0")
	b.propCells64("reg", 0x0c000000, 0x04000000)
	b.propCells("interrupts-extended", virtCPUIntcPH, 11, virtCPUIntcPH, 9)
	b.propCells("riscv,ndev", 0x35)
	b.propCells("phandle", virtPLICPH)
	b.endNode()

	b.beginNode("uart@10000000")
	b.propString("compatible", "ns16550a")
	b.propCells64("reg", biosUARTBase, biosUARTSize)
	b.propCells("clock-frequency", 3686400)
	b.propCells("current-speed", 115200)
	b.propCells("interrupt-parent", virtPLICPH)
	b.propCells("interrupts", biosUARTIRQ)
	b.endNode()

	b.beginNode("uart@10000100")
	b.propString("compatible", "ns16550a")
	b.propCells64("reg", biosUART1Base, biosUARTSize)
	b.propCells("clock-frequency", 3686400)
	b.propCells("current-speed", 115200)
	b.propCells("interrupt-parent", virtPLICPH)
	b.propCells("interrupts", biosUART1IRQ)
	b.endNode()

	if opts.HostIO {
		b.beginNode("hostio@10001000")
		b.propString("compatible", "glycerine,riscv-hostio-v1")
		b.propCells64("reg", biosHostIOBase, biosHostIOSize)
		b.endNode()
	}

	if opts.Net {
		b.beginNode("virtio_net@10008000")
		b.propString("compatible", "virtio,mmio")
		b.propCells64("reg", biosVirtioNetBase, biosVirtioNetSize)
		b.propCells("interrupt-parent", virtPLICPH)
		b.propCells("interrupts", biosVirtioNetIRQ)
		b.endNode()
	}

	b.endNode()
	b.endNode()

	return b.finish(), nil
}

const (
	fdtMagic     = uint32(0xd00dfeed)
	fdtVersion   = uint32(17)
	fdtCompatVer = uint32(16)

	fdtBeginNode = uint32(1)
	fdtEndNode   = uint32(2)
	fdtProp      = uint32(3)
	fdtEnd       = uint32(9)
)

type fdtBuilder struct {
	structs      bytes.Buffer
	strings      bytes.Buffer
	stringOffset map[string]uint32
}

func newFDTBuilder() *fdtBuilder {
	return &fdtBuilder{stringOffset: make(map[string]uint32)}
}

func (b *fdtBuilder) beginNode(name string) {
	b.putStruct32(fdtBeginNode)
	b.structs.WriteString(name)
	b.structs.WriteByte(0)
	alignBuffer4(&b.structs)
}

func (b *fdtBuilder) endNode() {
	b.putStruct32(fdtEndNode)
}

func (b *fdtBuilder) propEmpty(name string) {
	b.prop(name, nil)
}

func (b *fdtBuilder) propString(name, value string) {
	b.prop(name, []byte(value+"\x00"))
}

func (b *fdtBuilder) propStringList(name string, values ...string) {
	var data bytes.Buffer
	for _, value := range values {
		data.WriteString(value)
		data.WriteByte(0)
	}
	b.prop(name, data.Bytes())
}

func (b *fdtBuilder) propCells(name string, cells ...uint32) {
	var data bytes.Buffer
	for _, cell := range cells {
		_ = binary.Write(&data, binary.BigEndian, cell)
	}
	b.prop(name, data.Bytes())
}

func (b *fdtBuilder) propCells64(name string, cells ...uint64) {
	var data bytes.Buffer
	for _, cell := range cells {
		_ = binary.Write(&data, binary.BigEndian, uint32(cell>>32))
		_ = binary.Write(&data, binary.BigEndian, uint32(cell))
	}
	b.prop(name, data.Bytes())
}

func (b *fdtBuilder) prop(name string, data []byte) {
	b.putStruct32(fdtProp)
	b.putStruct32(uint32(len(data)))
	b.putStruct32(b.stringRef(name))
	b.structs.Write(data)
	alignBuffer4(&b.structs)
}

func (b *fdtBuilder) stringRef(name string) uint32 {
	if off, ok := b.stringOffset[name]; ok {
		return off
	}
	off := uint32(b.strings.Len())
	b.strings.WriteString(name)
	b.strings.WriteByte(0)
	b.stringOffset[name] = off
	return off
}

func (b *fdtBuilder) putStruct32(v uint32) {
	_ = binary.Write(&b.structs, binary.BigEndian, v)
}

func (b *fdtBuilder) finish() []byte {
	b.putStruct32(fdtEnd)
	structBlock := b.structs.Bytes()
	stringBlock := b.strings.Bytes()

	const headerSize = 40
	memReserve := make([]byte, 16)
	offMemRsvMap := uint32(headerSize)
	offDtStruct := uint32(align4(headerSize + len(memReserve)))
	offDtStrings := uint32(align4(int(offDtStruct) + len(structBlock)))
	totalSize := uint32(int(offDtStrings) + len(stringBlock))

	out := make([]byte, totalSize)
	putFDTHeader(out[0:4], fdtMagic)
	putFDTHeader(out[4:8], totalSize)
	putFDTHeader(out[8:12], offDtStruct)
	putFDTHeader(out[12:16], offDtStrings)
	putFDTHeader(out[16:20], offMemRsvMap)
	putFDTHeader(out[20:24], fdtVersion)
	putFDTHeader(out[24:28], fdtCompatVer)
	putFDTHeader(out[28:32], 0)
	putFDTHeader(out[32:36], uint32(len(stringBlock)))
	putFDTHeader(out[36:40], uint32(len(structBlock)))
	copy(out[offMemRsvMap:], memReserve)
	copy(out[offDtStruct:], structBlock)
	copy(out[offDtStrings:], stringBlock)
	return out
}

func putFDTHeader(dst []byte, v uint32) {
	binary.BigEndian.PutUint32(dst, v)
}

func alignBuffer4(buf *bytes.Buffer) {
	for buf.Len()%4 != 0 {
		buf.WriteByte(0)
	}
}

func align4(v int) int {
	return (v + 3) &^ 3
}
