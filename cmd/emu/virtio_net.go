package main

import (
	"encoding/binary"
	"io"
	"sync"

	riscv "github.com/glycerine/riscv-emu-golang"
)

const (
	virtioMMIOMagicValue        = uint64(0x000)
	virtioMMIOVersion           = uint64(0x004)
	virtioMMIODeviceID          = uint64(0x008)
	virtioMMIOVendorID          = uint64(0x00c)
	virtioMMIODeviceFeatures    = uint64(0x010)
	virtioMMIODeviceFeaturesSel = uint64(0x014)
	virtioMMIODriverFeatures    = uint64(0x020)
	virtioMMIODriverFeaturesSel = uint64(0x024)
	virtioMMIOQueueSel          = uint64(0x030)
	virtioMMIOQueueNumMax       = uint64(0x034)
	virtioMMIOQueueNum          = uint64(0x038)
	virtioMMIOQueueReady        = uint64(0x044)
	virtioMMIOQueueNotify       = uint64(0x050)
	virtioMMIOInterruptStatus   = uint64(0x060)
	virtioMMIOInterruptACK      = uint64(0x064)
	virtioMMIOStatus            = uint64(0x070)
	virtioMMIOQueueDescLow      = uint64(0x080)
	virtioMMIOQueueDescHigh     = uint64(0x084)
	virtioMMIOQueueAvailLow     = uint64(0x090)
	virtioMMIOQueueAvailHigh    = uint64(0x094)
	virtioMMIOQueueUsedLow      = uint64(0x0a0)
	virtioMMIOQueueUsedHigh     = uint64(0x0a4)
	virtioMMIOConfigGeneration  = uint64(0x0fc)
	virtioMMIOConfig            = uint64(0x100)

	virtioMMIOMagic = uint32(0x74726976) // "virt"

	virtioDeviceIDNet = uint32(1)
	virtioVendorID    = uint32(0x52564547) // "GEVR", little-endian.

	virtioMMIOIntVring  = uint32(1 << 0)
	virtioMMIOIntConfig = uint32(1 << 1)

	virtioNetQueueRX = uint16(0)
	virtioNetQueueTX = uint16(1)

	virtioQueueSize      = uint16(256)
	virtioNetHeaderSize  = 12
	virtioNetMTU         = uint16(1280)
	virtioNetConfigSize  = 12
	virtioNetMaxFrameLen = 65536

	virtqDescFNext  = uint16(1)
	virtqDescFWrite = uint16(2)

	virtioNetFMac    = uint64(1 << 5)
	virtioNetFStatus = uint64(1 << 16)
	virtioNetFMTU    = uint64(1 << 3)
	virtioFVersion1  = uint64(1 << 32)

	virtioNetStatusLinkUp = uint16(1)
)

type virtioNetPacketStack interface {
	InjectInboundPacket(frame []byte)
}

type virtioNetMemoryStack struct {
	mu     sync.Mutex
	frames [][]byte
}

func newVirtioNetMemoryStack() *virtioNetMemoryStack {
	return &virtioNetMemoryStack{}
}

func (s *virtioNetMemoryStack) InjectInboundPacket(frame []byte) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.frames = append(s.frames, append([]byte(nil), frame...))
}

func (s *virtioNetMemoryStack) Frames() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([][]byte, len(s.frames))
	for i := range s.frames {
		out[i] = append([]byte(nil), s.frames[i]...)
	}
	return out
}

type virtioNetDevice struct {
	mu sync.Mutex

	mem   *riscv.GuestMemory
	stack virtioNetPacketStack

	deviceFeaturesSel uint32
	driverFeaturesSel uint32
	driverFeatures    uint64
	queueSel          uint16
	status            uint32
	interruptStatus   uint32
	configGeneration  uint32
	queues            [2]virtioNetQueue
	pendingRX         [][]byte
	mac               [6]byte
}

type virtioNetQueue struct {
	num       uint16
	numMax    uint16
	ready     bool
	desc      uint64
	avail     uint64
	used      uint64
	lastAvail uint16
}

type virtqDesc struct {
	addr  uint64
	len   uint32
	flags uint16
	next  uint16
}

func newVirtioNetDevice(mem *riscv.GuestMemory, stack virtioNetPacketStack) *virtioNetDevice {
	if stack == nil {
		stack = newVirtioNetMemoryStack()
	}
	d := &virtioNetDevice{
		mem:   mem,
		stack: stack,
		mac:   [6]byte{0x02, 0x72, 0x69, 0x73, 0x00, 0x01},
	}
	for i := range d.queues {
		d.queues[i].numMax = virtioQueueSize
	}
	return d
}

func (d *virtioNetDevice) Close() {
	if c, ok := d.stack.(io.Closer); ok {
		_ = c.Close()
	}
}

func (d *virtioNetDevice) Load(off, width uint64) uint64 {
	d.mu.Lock()
	defer d.mu.Unlock()

	if off >= virtioMMIOConfig {
		cfg := d.configBytesLocked()
		cfgOff := off - virtioMMIOConfig
		if cfgOff >= uint64(len(cfg)) {
			return 0
		}
		if width > uint64(len(cfg))-cfgOff {
			width = uint64(len(cfg)) - cfgOff
		}
		return loadLittleEndian(cfg, cfgOff, width)
	}

	reg := uint32(0)
	switch off {
	case virtioMMIOMagicValue:
		reg = virtioMMIOMagic
	case virtioMMIOVersion:
		reg = 2
	case virtioMMIODeviceID:
		reg = virtioDeviceIDNet
	case virtioMMIOVendorID:
		reg = virtioVendorID
	case virtioMMIODeviceFeatures:
		reg = uint32(d.deviceFeaturesLocked() >> (32 * d.deviceFeaturesSel))
	case virtioMMIOQueueNumMax:
		reg = uint32(d.selectedQueueLocked().numMax)
	case virtioMMIOQueueReady:
		if d.selectedQueueLocked().ready {
			reg = 1
		}
	case virtioMMIOInterruptStatus:
		reg = d.interruptStatus
	case virtioMMIOStatus:
		reg = d.status
	case virtioMMIOConfigGeneration:
		reg = d.configGeneration
	default:
		reg = 0
	}
	return loadLittleEndianFromU32(reg, width)
}

func (d *virtioNetDevice) Store(off, width, value uint64) *riscv.MemFault {
	var frames [][]byte
	var fault *riscv.MemFault

	d.mu.Lock()
	switch off {
	case virtioMMIODeviceFeaturesSel:
		d.deviceFeaturesSel = uint32(value)
	case virtioMMIODriverFeaturesSel:
		d.driverFeaturesSel = uint32(value)
	case virtioMMIODriverFeatures:
		shift := 32 * d.driverFeaturesSel
		mask := uint64(0xffffffff) << shift
		d.driverFeatures = (d.driverFeatures &^ mask) | (uint64(uint32(value)) << shift)
	case virtioMMIOQueueSel:
		d.queueSel = uint16(value)
	case virtioMMIOQueueNum:
		d.selectedQueueLocked().num = uint16(value)
	case virtioMMIOQueueReady:
		q := d.selectedQueueLocked()
		q.ready = value != 0
		if !q.ready {
			q.lastAvail = 0
		}
	case virtioMMIOQueueDescLow:
		q := d.selectedQueueLocked()
		q.desc = (q.desc & 0xffffffff00000000) | uint64(uint32(value))
	case virtioMMIOQueueDescHigh:
		q := d.selectedQueueLocked()
		q.desc = (q.desc & 0x00000000ffffffff) | (uint64(uint32(value)) << 32)
	case virtioMMIOQueueAvailLow:
		q := d.selectedQueueLocked()
		q.avail = (q.avail & 0xffffffff00000000) | uint64(uint32(value))
	case virtioMMIOQueueAvailHigh:
		q := d.selectedQueueLocked()
		q.avail = (q.avail & 0x00000000ffffffff) | (uint64(uint32(value)) << 32)
	case virtioMMIOQueueUsedLow:
		q := d.selectedQueueLocked()
		q.used = (q.used & 0xffffffff00000000) | uint64(uint32(value))
	case virtioMMIOQueueUsedHigh:
		q := d.selectedQueueLocked()
		q.used = (q.used & 0x00000000ffffffff) | (uint64(uint32(value)) << 32)
	case virtioMMIOQueueNotify:
		frames, fault = d.notifyQueueLocked(uint16(value))
	case virtioMMIOInterruptACK:
		d.interruptStatus &^= uint32(value)
	case virtioMMIOStatus:
		d.status = uint32(value)
		if value == 0 {
			d.resetLocked()
		}
	}
	d.mu.Unlock()

	for _, frame := range frames {
		d.stack.InjectInboundPacket(frame)
	}
	return fault
}

func (d *virtioNetDevice) InterruptPending() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.interruptStatus != 0
}

func (d *virtioNetDevice) InjectGuestFrame(frame []byte) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(frame) == 0 || len(frame) > virtioNetMaxFrameLen {
		return false
	}
	d.pendingRX = append(d.pendingRX, append([]byte(nil), frame...))
	return d.pumpRXLocked()
}

func (d *virtioNetDevice) deviceFeaturesLocked() uint64 {
	return virtioFVersion1 | virtioNetFMac | virtioNetFStatus | virtioNetFMTU
}

func (d *virtioNetDevice) selectedQueueLocked() *virtioNetQueue {
	if int(d.queueSel) >= len(d.queues) {
		return &virtioNetQueue{}
	}
	return &d.queues[d.queueSel]
}

func (d *virtioNetDevice) configBytesLocked() []byte {
	var cfg [virtioNetConfigSize]byte
	copy(cfg[0:6], d.mac[:])
	binary.LittleEndian.PutUint16(cfg[6:], virtioNetStatusLinkUp)
	binary.LittleEndian.PutUint16(cfg[8:], 1)
	binary.LittleEndian.PutUint16(cfg[10:], virtioNetMTU)
	return cfg[:]
}

func (d *virtioNetDevice) resetLocked() {
	d.driverFeatures = 0
	d.driverFeaturesSel = 0
	d.deviceFeaturesSel = 0
	d.queueSel = 0
	d.interruptStatus = 0
	d.pendingRX = nil
	for i := range d.queues {
		d.queues[i] = virtioNetQueue{numMax: virtioQueueSize}
	}
}

func (d *virtioNetDevice) notifyQueueLocked(index uint16) ([][]byte, *riscv.MemFault) {
	switch index {
	case virtioNetQueueTX:
		return d.drainTXLocked()
	case virtioNetQueueRX:
		d.pumpRXLocked()
		return nil, nil
	default:
		return nil, nil
	}
}

func (d *virtioNetDevice) drainTXLocked() ([][]byte, *riscv.MemFault) {
	q := &d.queues[virtioNetQueueTX]
	if !q.ready || q.num == 0 {
		return nil, nil
	}
	availIdx, fault := d.readGuestU16(q.avail + 2)
	if fault != nil {
		return nil, fault
	}
	var frames [][]byte
	for q.lastAvail != availIdx {
		head, fault := d.readGuestU16(q.avail + 4 + 2*uint64(q.lastAvail%q.num))
		if fault != nil {
			return frames, fault
		}
		payload, fault := d.readDescriptorChainLocked(q, head, false)
		if fault != nil {
			return frames, fault
		}
		if len(payload) >= virtioNetHeaderSize {
			frame := append([]byte(nil), payload[virtioNetHeaderSize:]...)
			if len(frame) != 0 {
				frames = append(frames, frame)
			}
		}
		if fault := d.addUsedLocked(q, head, 0); fault != nil {
			return frames, fault
		}
		q.lastAvail++
	}
	if len(frames) != 0 {
		d.raiseVringInterruptLocked()
	}
	return frames, nil
}

func (d *virtioNetDevice) pumpRXLocked() bool {
	q := &d.queues[virtioNetQueueRX]
	if !q.ready || q.num == 0 {
		return false
	}
	delivered := false
	for len(d.pendingRX) != 0 {
		availIdx, fault := d.readGuestU16(q.avail + 2)
		if fault != nil || q.lastAvail == availIdx {
			break
		}
		head, fault := d.readGuestU16(q.avail + 4 + 2*uint64(q.lastAvail%q.num))
		if fault != nil {
			break
		}
		packet := make([]byte, virtioNetHeaderSize+len(d.pendingRX[0]))
		copy(packet[virtioNetHeaderSize:], d.pendingRX[0])
		ok, fault := d.writeDescriptorChainLocked(q, head, packet)
		if fault != nil || !ok {
			break
		}
		if fault := d.addUsedLocked(q, head, uint32(len(packet))); fault != nil {
			break
		}
		d.pendingRX = d.pendingRX[1:]
		q.lastAvail++
		delivered = true
	}
	if delivered {
		d.raiseVringInterruptLocked()
	}
	return delivered
}

func (d *virtioNetDevice) raiseVringInterruptLocked() {
	d.interruptStatus |= virtioMMIOIntVring
}

func (d *virtioNetDevice) readDescriptorChainLocked(q *virtioNetQueue, head uint16, requireWrite bool) ([]byte, *riscv.MemFault) {
	var out []byte
	id := head
	for n := uint16(0); n < q.num; n++ {
		desc, fault := d.readDescLocked(q, id)
		if fault != nil {
			return out, fault
		}
		if requireWrite && desc.flags&virtqDescFWrite == 0 {
			return out, nil
		}
		if !requireWrite && desc.flags&virtqDescFWrite == 0 && desc.len != 0 {
			if uint64(len(out))+uint64(desc.len) > virtioNetMaxFrameLen {
				return out, nil
			}
			buf := make([]byte, desc.len)
			if fault := d.mem.ReadBytes(desc.addr, buf); fault != nil {
				return out, fault
			}
			out = append(out, buf...)
		}
		if desc.flags&virtqDescFNext == 0 {
			return out, nil
		}
		id = desc.next
	}
	return out, nil
}

func (d *virtioNetDevice) writeDescriptorChainLocked(q *virtioNetQueue, head uint16, packet []byte) (bool, *riscv.MemFault) {
	id := head
	off := 0
	for n := uint16(0); n < q.num; n++ {
		desc, fault := d.readDescLocked(q, id)
		if fault != nil {
			return false, fault
		}
		if desc.flags&virtqDescFWrite == 0 {
			return false, nil
		}
		count := int(desc.len)
		if rem := len(packet) - off; count > rem {
			count = rem
		}
		if count > 0 {
			if fault := d.mem.WriteBytes(desc.addr, packet[off:off+count]); fault != nil {
				return false, fault
			}
			off += count
		}
		if off == len(packet) {
			return true, nil
		}
		if desc.flags&virtqDescFNext == 0 {
			return false, nil
		}
		id = desc.next
	}
	return false, nil
}

func (d *virtioNetDevice) readDescLocked(q *virtioNetQueue, id uint16) (virtqDesc, *riscv.MemFault) {
	var raw [16]byte
	if q.num == 0 || id >= q.num {
		return virtqDesc{}, nil
	}
	if fault := d.mem.ReadBytes(q.desc+uint64(id)*16, raw[:]); fault != nil {
		return virtqDesc{}, fault
	}
	return virtqDesc{
		addr:  binary.LittleEndian.Uint64(raw[0:8]),
		len:   binary.LittleEndian.Uint32(raw[8:12]),
		flags: binary.LittleEndian.Uint16(raw[12:14]),
		next:  binary.LittleEndian.Uint16(raw[14:16]),
	}, nil
}

func (d *virtioNetDevice) addUsedLocked(q *virtioNetQueue, id uint16, length uint32) *riscv.MemFault {
	usedIdx, fault := d.readGuestU16(q.used + 2)
	if fault != nil {
		return fault
	}
	entry := q.used + 4 + 8*uint64(usedIdx%q.num)
	if fault := d.writeGuestU32(entry, uint32(id)); fault != nil {
		return fault
	}
	if fault := d.writeGuestU32(entry+4, length); fault != nil {
		return fault
	}
	return d.writeGuestU16(q.used+2, usedIdx+1)
}

func (d *virtioNetDevice) readGuestU16(addr uint64) (uint16, *riscv.MemFault) {
	var raw [2]byte
	if fault := d.mem.ReadBytes(addr, raw[:]); fault != nil {
		return 0, fault
	}
	return binary.LittleEndian.Uint16(raw[:]), nil
}

func (d *virtioNetDevice) writeGuestU16(addr uint64, value uint16) *riscv.MemFault {
	var raw [2]byte
	binary.LittleEndian.PutUint16(raw[:], value)
	return d.mem.WriteBytes(addr, raw[:])
}

func (d *virtioNetDevice) writeGuestU32(addr uint64, value uint32) *riscv.MemFault {
	var raw [4]byte
	binary.LittleEndian.PutUint32(raw[:], value)
	return d.mem.WriteBytes(addr, raw[:])
}

func loadLittleEndianFromU32(value uint32, width uint64) uint64 {
	var raw [4]byte
	binary.LittleEndian.PutUint32(raw[:], value)
	if width > uint64(len(raw)) {
		width = uint64(len(raw))
	}
	return loadLittleEndian(raw[:], 0, width)
}
