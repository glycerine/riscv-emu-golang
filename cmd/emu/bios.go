package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	riscv "github.com/glycerine/riscv-emu-golang"
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
)

type biosGuest struct {
	mem         *riscv.GuestMemory
	cpu         *riscv.CPU
	elf         *riscv.ELF
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
	guest, err := prepareBiosGuest(cfg)
	if err != nil {
		return 0, err
	}
	defer guest.mem.Free()

	res, err := riscv.RunDefaultBudget(guest.cpu, &guest.cpu.Notes, budget)
	if err != nil {
		return 0, err
	}
	if res == riscv.RunBudgetExit {
		return guest.cpu.ExitCode, nil
	}
	if res == riscv.RunBudgetExpired {
		return 0, fmt.Errorf("%w after %d instructions", errBiosBudgetExpired, budget)
	}
	return 0, nil
}

func prepareBiosGuest(cfg EmuConfig) (*biosGuest, error) {
	cfg = cfg.withDefaults()
	if cfg.BiosPath == "" {
		return nil, fmt.Errorf("prepare BIOS guest requires -bios")
	}
	if cfg.MemorySize <= defaultBiosFDTAddr {
		return nil, fmt.Errorf("-mem %#x is too small for BIOS FDT address %#x", cfg.MemorySize, defaultBiosFDTAddr)
	}

	mem, err := riscv.NewGuestMemory(cfg.MemorySize)
	if err != nil {
		return nil, err
	}
	elf, err := riscv.LoadELF(mem, cfg.BiosPath)
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

	cpu := riscv.NewCPU(*mem)
	cpu.SetPC(elf.Entry)
	cpu.SetReg(10, 0)       // a0: boot hart id
	cpu.SetReg(11, fdtAddr) // a1: flattened device tree pointer
	cpu.SetReg(12, dynamicAddr)

	return &biosGuest{
		mem:         mem,
		cpu:         cpu,
		elf:         elf,
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

func loadBiosKernel(mem *riscv.GuestMemory, cfg EmuConfig) (biosBlob, error) {
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
		elf, err := riscv.LoadELFBytes(mem, data)
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

func loadBiosInitrd(mem *riscv.GuestMemory, cfg EmuConfig, kernel biosBlob) (biosBlob, error) {
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
		BootArgs:    cfg.Append,
		InitrdStart: initrd.addr,
		InitrdEnd:   initrd.end,
	})
	return fdt, false, err
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

func writeFWDynamicInfo(mem *riscv.GuestMemory, memSize, nextAddr, fdtAddr uint64, fdtLen int, kernel, initrd biosBlob) (uint64, error) {
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
	InitrdStart uint64
	InitrdEnd   uint64
}

func buildVirtFDT(memSize uint64, opts virtFDTOptions) ([]byte, error) {
	if memSize <= virtRAMBase {
		return nil, fmt.Errorf("memory size %#x does not cover virt RAM base %#x", memSize, virtRAMBase)
	}
	ramSize := memSize - virtRAMBase
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
	b.propString("riscv,isa", "rv64imafdcsu")
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

	b.beginNode("clint@2000000")
	b.propStringList("compatible", "sifive,clint0", "riscv,clint0")
	b.propCells64("reg", 0x02000000, 0x00010000)
	b.propCells("interrupts-extended", virtCPUIntcPH, 3, virtCPUIntcPH, 7)
	b.endNode()

	b.beginNode("interrupt-controller@c000000")
	b.propEmpty("interrupt-controller")
	b.propCells("#interrupt-cells", 1)
	b.propString("compatible", "riscv,plic0")
	b.propCells64("reg", 0x0c000000, 0x04000000)
	b.propCells("riscv,ndev", 0x35)
	b.propCells("phandle", virtPLICPH)
	b.endNode()

	b.beginNode("uart@10000000")
	b.propString("compatible", "ns16550a")
	b.propCells64("reg", 0x10000000, 0x100)
	b.propCells("clock-frequency", 3686400)
	b.propCells("current-speed", 115200)
	b.propCells("interrupt-parent", virtPLICPH)
	b.propCells("interrupts", 10)
	b.endNode()

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
