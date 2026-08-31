//go:build breadcrumb

package riscv

import (
	"bufio"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/glycerine/blake3"
)

const breadcrumbEnabled = true

var ErrBreadcrumbConfig = errors.New("riscv: invalid breadcrumb configuration")

var breadcrumbTracers sync.Map // map[*CPU]*BreadcrumbTracer

var BreadcrumbDefaultPath string = "crumbs.txt"

type breadcrumbControl uint32

const (
	breadcrumbControlStart             breadcrumbControl = 1
	breadcrumbControlPause             breadcrumbControl = 2
	breadcrumbControlResetStart        breadcrumbControl = 3
	breadcrumbControlFlush             breadcrumbControl = 4
	breadcrumbControlArmNextUserReset  breadcrumbControl = 5
	breadcrumbControlArmNextUserResume breadcrumbControl = 6
	breadcrumbControlArmNextASReset    breadcrumbControl = 7
	breadcrumbControlArmNextASResume   breadcrumbControl = 8
	breadcrumbControlPendingNone       breadcrumbControl = 0
	breadcrumbStatusIdle               uint32            = 0
	breadcrumbStatusActive             uint32            = 1
	breadcrumbStatusPaused             uint32            = 2
	breadcrumbStatusError              uint32            = 3
	breadcrumbArmNone                  uint8             = 0
	breadcrumbArmNextUser              uint8             = 1
	breadcrumbArmNextAddressSpace      uint8             = 2
)

type BreadcrumbTracer struct {
	w                  *bufio.Writer
	outfile            *os.File
	path               string
	h                  *blake3.Hasher
	ownOutfile         bool
	interval           uint64
	startAt            uint64
	stopAt             uint64
	afterAt            uint64
	afterInterval      uint64
	nextCheckpoint     uint64
	lastPC             uint64
	active             bool
	guestControl       bool
	includePrivileged  bool
	filterAddressSpace bool
	targetSATP         uint64
	targetSATPValid    bool
	targetPID          uint32
	seq                uint64
	eligibleSeq        uint64
	epoch              uint64
	pendingControl     breadcrumbControl
	armMode            uint8
	armSawPrivileged   bool
	armSourceSATP      uint64
	armSourceSATPValid bool
	tripMode           uint32
	tripSeq            uint64
	tripAttempt        uint64
	tripSignal         uint32
	tripPID            uint32
	tripPending        bool
	tripHitSeq         uint64
	tripHitAttempt     uint64
	tripHitPC          uint64
	tripNotify         func()
	closed             bool
	lastErr            error
	buf                [8]byte
}

type breadcrumbIODevice struct {
	tracer *BreadcrumbTracer
}

func NewBreadcrumbTracer(cfg BreadcrumbConfig) (*BreadcrumbTracer, error) {
	if cfg.Outfile == nil && cfg.Path == "" {
		return nil, fmt.Errorf("%w: Path or Outfile is required", ErrBreadcrumbConfig)
	}
	if cfg.Interval == 0 {
		cfg.Interval = defaultBreadcrumbInterval
	}
	if cfg.StopAt != 0 && cfg.StartAt > cfg.StopAt {
		return nil, fmt.Errorf("%w: StopAt %d is before StartAt %d", ErrBreadcrumbConfig, cfg.StopAt, cfg.StartAt)
	}
	if cfg.AfterInterval != 0 && cfg.AfterAt == 0 {
		return nil, fmt.Errorf("%w: AfterInterval requires AfterAt", ErrBreadcrumbConfig)
	}
	if cfg.AfterAt != 0 && cfg.AfterInterval == 0 {
		return nil, fmt.Errorf("%w: AfterAt requires AfterInterval", ErrBreadcrumbConfig)
	}

	b := &BreadcrumbTracer{
		outfile:            cfg.Outfile,
		path:               cfg.Path,
		h:                  blake3.New(32, nil),
		ownOutfile:         cfg.Outfile == nil,
		interval:           cfg.Interval,
		startAt:            cfg.StartAt,
		stopAt:             cfg.StopAt,
		afterAt:            cfg.AfterAt,
		afterInterval:      cfg.AfterInterval,
		nextCheckpoint:     firstBreadcrumbCheckpoint(cfg.Interval, cfg.AfterAt, cfg.AfterInterval),
		active:             !cfg.StartPaused && !cfg.GuestControl,
		guestControl:       cfg.GuestControl,
		includePrivileged:  cfg.IncludePrivileged,
		filterAddressSpace: cfg.GuestControl,
		tripSignal:         3, // Linux SIGQUIT: Go prints goroutine stacks by default.
	}
	if !(cfg.Outfile == nil && cfg.Path != "" && cfg.StartPaused && cfg.GuestControl) {
		if err := b.ensureOutput(); err != nil {
			_ = b.closeOutput()
			return nil, err
		}
	}
	return b, nil
}

func validateBreadcrumbConfig(c *EmuConfig) error {
	if !c.Breadcrumb.requested() {
		return nil
	}
	if c.JITLazy || c.JITAOT {
		return fmt.Errorf("%w: JIT paths are not instrumented; use the interpreted CPU path", ErrBreadcrumbConfig)
	}
	cfg := c.Breadcrumb.normalizedForEmu(c.BiosPath != "")
	if cfg.Outfile == nil && cfg.Path == "" {
		return fmt.Errorf("%w: -breadcrumb requires an output path", ErrBreadcrumbConfig)
	}
	if cfg.StopAt != 0 && cfg.StartAt > cfg.StopAt {
		return fmt.Errorf("%w: -breadcrumb-stop %d is before -breadcrumb-start %d", ErrBreadcrumbConfig, cfg.StopAt, cfg.StartAt)
	}
	if cfg.AfterInterval != 0 && cfg.AfterAt == 0 {
		return fmt.Errorf("%w: -breadcrumb-after-interval requires -breadcrumb-after-at", ErrBreadcrumbConfig)
	}
	if cfg.AfterAt != 0 && cfg.AfterInterval == 0 {
		return fmt.Errorf("%w: -breadcrumb-after-at requires -breadcrumb-after-interval", ErrBreadcrumbConfig)
	}
	return nil
}

func prepareBreadcrumbTracer(cfg *EmuConfig, bios bool) (*BreadcrumbTracer, error) {
	if !cfg.Breadcrumb.requested() {
		return nil, nil
	}
	bc := cfg.Breadcrumb.normalizedForEmu(bios)
	cfg.Breadcrumb = bc
	return NewBreadcrumbTracer(bc)
}

func (c *CPU) SetBreadcrumbTracer(b *BreadcrumbTracer) {
	if b == nil {
		breadcrumbTracers.Delete(c)
		return
	}
	breadcrumbTracers.Store(c, b)
}

func (c *CPU) BreadcrumbTracer() *BreadcrumbTracer {
	v, ok := breadcrumbTracers.Load(c)
	if !ok {
		return nil
	}
	b, _ := v.(*BreadcrumbTracer)
	return b
}

func breadcrumbRecordPC(cpu *CPU, attempt, retired, pc, satp uint64, priv PrivilegeMode) error {
	b := cpu.BreadcrumbTracer()
	if b == nil {
		return nil
	}
	return b.recordPC(attempt, retired, pc, priv, satp)
}

func breadcrumbBeforeAttempt(cpu *CPU, attempt, retired, pc, satp uint64, priv PrivilegeMode) (bool, error) {
	b := cpu.BreadcrumbTracer()
	if b == nil || !b.shouldTripBeforeAttempt(attempt, priv, satp) {
		return false, nil
	}
	if err := b.recordPC(attempt, retired, pc, priv, satp); err != nil {
		return false, err
	}
	if b.tripHitAttempt != attempt || b.tripHitPC != pc {
		return false, nil
	}
	if err := breadcrumbInjectBreakpoint(cpu, pc); err != nil {
		return false, err
	}
	return true, nil
}

func breadcrumbAfterAttempt(cpu *CPU, attempt, retired, pc, satp uint64, priv PrivilegeMode) error {
	b := cpu.BreadcrumbTracer()
	if b == nil {
		return nil
	}
	return b.afterAttempt(attempt, retired, pc, priv, satp, cpu.priv, cpu.satp)
}

func breadcrumbFlush(cpu *CPU) error {
	b := cpu.BreadcrumbTracer()
	if b == nil {
		return nil
	}
	return b.Flush()
}

func (b *BreadcrumbTracer) RecordPC(attempt, retired, pc uint64, priv PrivilegeMode) error {
	return b.recordPC(attempt, retired, pc, priv, 0)
}

func (b *BreadcrumbTracer) recordPC(attempt, retired, pc uint64, priv PrivilegeMode, satp uint64) error {
	if b == nil || b.closed {
		return nil
	}
	if !b.active {
		return nil
	}
	if !b.includePrivileged && priv != PrivUser {
		return nil
	}
	if !b.addressSpaceAllowed(priv, satp) {
		return nil
	}
	b.eligibleSeq++
	if b.startAt != 0 && b.eligibleSeq < b.startAt {
		return nil
	}
	if b.stopAt != 0 && b.eligibleSeq > b.stopAt {
		return nil
	}
	binary.LittleEndian.PutUint64(b.buf[:], pc)
	if _, err := b.h.Write(b.buf[:]); err != nil {
		b.lastErr = err
		return err
	}
	b.seq++
	b.lastPC = pc
	if b.seq >= b.nextCheckpoint {
		if err := b.writeCheckpoint(attempt, retired, pc, priv, satp); err != nil {
			return err
		}
	}
	if err := b.maybeTrip(attempt, retired, pc, satp); err != nil {
		return err
	}
	if b.stopAt != 0 && b.eligibleSeq == b.stopAt {
		if err := b.fireTrip(attempt, retired, pc, satp, "stop_at"); err != nil {
			return err
		}
		b.active = false
	}
	return nil
}

func (b *BreadcrumbTracer) Activate(attempt, retired, pc uint64, reset bool) error {
	return b.activate(attempt, retired, pc, 0, PrivUser, reset)
}

func (b *BreadcrumbTracer) activate(attempt, retired, pc, satp uint64, postPriv PrivilegeMode, reset bool) error {
	if b == nil || b.closed {
		return nil
	}
	mode := "mode=resume"
	if reset {
		if err := b.beginResetTrace(); err != nil {
			return err
		}
		mode = "mode=reset"
	}
	if postPriv == PrivUser {
		b.captureTargetAddressSpace(satp)
	}
	b.active = true
	b.armMode = breadcrumbArmNone
	b.armSawPrivileged = false
	return b.writeEvent("activation", attempt, retired, pc, b.withTargetExtra(mode))
}

func (b *BreadcrumbTracer) Pause(attempt, retired, pc uint64) error {
	if b == nil || b.closed {
		return nil
	}
	b.active = false
	return b.writeEvent("pause", attempt, retired, pc, "")
}

func (b *BreadcrumbTracer) Flush() error {
	if b == nil || b.closed {
		return nil
	}
	return b.flushOutput(false)
}

func (b *BreadcrumbTracer) Sync() error {
	if b == nil || b.closed {
		return nil
	}
	return b.flushOutput(true)
}

func (b *BreadcrumbTracer) flushOutput(sync bool) error {
	if b.w == nil {
		return nil
	}
	if err := b.w.Flush(); err != nil {
		b.lastErr = err
		return err
	}
	if sync && b.ownOutfile && b.outfile != nil {
		if err := b.outfile.Sync(); err != nil {
			b.lastErr = err
			return err
		}
	}
	return nil
}

func (b *BreadcrumbTracer) Close() error {
	if b == nil || b.closed {
		return nil
	}
	b.closed = true
	return b.closeOutput()
}

func (b *BreadcrumbTracer) afterAttempt(attempt, retired, pc uint64, priv PrivilegeMode, satp uint64, postPriv PrivilegeMode, postSATP uint64) error {
	if err := b.recordPC(attempt, retired, pc, priv, satp); err != nil {
		return err
	}
	if err := b.applyPendingControl(attempt, retired, pc, satp, postPriv, postSATP); err != nil {
		return err
	}
	return b.observePostPriv(attempt, retired, pc, postPriv, postSATP)
}

func (b *BreadcrumbTracer) applyPendingControl(attempt, retired, pc, satp uint64, postPriv PrivilegeMode, postSATP uint64) error {
	cmd := b.pendingControl
	if cmd == breadcrumbControlPendingNone {
		return nil
	}
	b.pendingControl = breadcrumbControlPendingNone
	switch cmd {
	case breadcrumbControlStart:
		return b.activate(attempt, retired, pc, postSATP, postPriv, false)
	case breadcrumbControlPause:
		return b.Pause(attempt, retired, pc)
	case breadcrumbControlResetStart:
		return b.activate(attempt, retired, pc, postSATP, postPriv, true)
	case breadcrumbControlFlush:
		return b.Flush()
	case breadcrumbControlArmNextUserReset:
		return b.armNextUser(attempt, retired, pc, postPriv, true)
	case breadcrumbControlArmNextUserResume:
		return b.armNextUser(attempt, retired, pc, postPriv, false)
	case breadcrumbControlArmNextASReset:
		return b.armNextAddressSpace(attempt, retired, pc, satp, postPriv, true)
	case breadcrumbControlArmNextASResume:
		return b.armNextAddressSpace(attempt, retired, pc, satp, postPriv, false)
	default:
		b.lastErr = fmt.Errorf("%w: unknown breadcrumb control %d", ErrBreadcrumbConfig, cmd)
		return nil
	}
}

func (b *BreadcrumbTracer) armNextUser(attempt, retired, pc uint64, postPriv PrivilegeMode, reset bool) error {
	if reset {
		if err := b.beginResetTrace(); err != nil {
			return err
		}
	}
	b.active = false
	b.armMode = breadcrumbArmNextUser
	b.armSawPrivileged = postPriv != PrivUser
	mode := "mode=resume"
	if reset {
		mode = "mode=reset"
	}
	return b.writeEvent("armed-next-user", attempt, retired, pc, mode)
}

func (b *BreadcrumbTracer) armNextAddressSpace(attempt, retired, pc, sourceSATP uint64, postPriv PrivilegeMode, reset bool) error {
	if reset {
		if err := b.beginResetTrace(); err != nil {
			return err
		}
	}
	b.active = false
	b.armMode = breadcrumbArmNextAddressSpace
	b.armSawPrivileged = postPriv != PrivUser
	b.armSourceSATP = sourceSATP
	b.armSourceSATPValid = true
	mode := "mode=resume"
	if reset {
		mode = "mode=reset"
	}
	return b.writeEvent("armed-next-address-space", attempt, retired, pc,
		fmt.Sprintf("%s source_satp=0x%016x", mode, sourceSATP))
}

func (b *BreadcrumbTracer) observePostPriv(attempt, retired, pc uint64, postPriv PrivilegeMode, postSATP uint64) error {
	if b == nil || b.armMode == breadcrumbArmNone {
		return nil
	}
	if postPriv != PrivUser {
		b.armSawPrivileged = true
		return nil
	}
	if !b.armSawPrivileged {
		return nil
	}
	if b.armMode == breadcrumbArmNextAddressSpace && b.armSourceSATPValid && postSATP == b.armSourceSATP {
		return nil
	}
	mode := "mode=armed-next-user"
	if b.armMode == breadcrumbArmNextAddressSpace {
		mode = "mode=armed-next-address-space"
	}
	b.active = true
	b.armMode = breadcrumbArmNone
	b.armSawPrivileged = false
	b.armSourceSATP = 0
	b.armSourceSATPValid = false
	b.captureTargetAddressSpace(postSATP)
	return b.writeEvent("activation", attempt, retired, pc, b.withTargetExtra(mode))
}

func (b *BreadcrumbTracer) queueControl(cmd breadcrumbControl) {
	if b == nil || b.closed {
		return
	}
	b.pendingControl = cmd
}

func (b *BreadcrumbTracer) setInterval(v uint64) {
	if b == nil || v == 0 {
		return
	}
	b.interval = v
	if b.nextCheckpoint == 0 || b.nextCheckpoint < b.seq {
		b.nextCheckpoint = b.seq + v
	}
}

func (b *BreadcrumbTracer) setAfterAt(v uint64) {
	if b == nil {
		return
	}
	b.afterAt = v
	if b.seq == 0 {
		b.nextCheckpoint = firstBreadcrumbCheckpoint(b.interval, b.afterAt, b.afterInterval)
	}
}

func (b *BreadcrumbTracer) setAfterInterval(v uint64) {
	if b == nil || v == 0 {
		return
	}
	b.afterInterval = v
	if b.seq == 0 {
		b.nextCheckpoint = firstBreadcrumbCheckpoint(b.interval, b.afterAt, b.afterInterval)
	}
}

func (b *BreadcrumbTracer) status() uint32 {
	if b == nil {
		return breadcrumbStatusError
	}
	if b.lastErr != nil {
		return breadcrumbStatusError
	}
	if b.active {
		return breadcrumbStatusActive
	}
	if b.armMode != breadcrumbArmNone || b.pendingControl != breadcrumbControlPendingNone {
		return breadcrumbStatusPaused
	}
	if b.guestControl {
		return breadcrumbStatusIdle
	}
	return breadcrumbStatusPaused
}

func (b *BreadcrumbTracer) resetEpoch() {
	b.h = blake3.New(32, nil)
	b.seq = 0
	b.eligibleSeq = 0
	b.nextCheckpoint = firstBreadcrumbCheckpoint(b.interval, b.afterAt, b.afterInterval)
	b.epoch++
	b.targetSATP = 0
	b.targetSATPValid = false
	b.armSourceSATP = 0
	b.armSourceSATPValid = false
	b.tripPending = false
	b.tripHitSeq = 0
	b.tripHitAttempt = 0
	b.tripHitPC = 0
}

func (b *BreadcrumbTracer) beginResetTrace() error {
	if b == nil || b.closed {
		return nil
	}
	if err := b.reopenOutputForNewTrace(); err != nil {
		return err
	}
	b.resetEpoch()
	return nil
}

func (b *BreadcrumbTracer) reopenOutputForNewTrace() error {
	if b == nil || !b.ownOutfile || b.path == "" {
		return nil
	}
	if err := b.closeOutput(); err != nil {
		return err
	}
	if err := rotateExistingBreadcrumbPath(b.path); err != nil {
		b.lastErr = err
		return err
	}
	return nil
}

func (b *BreadcrumbTracer) ensureOutput() error {
	if b == nil || b.closed || b.w != nil {
		return nil
	}
	if b.outfile == nil {
		if b.path == "" {
			err := fmt.Errorf("%w: Path or Outfile is required", ErrBreadcrumbConfig)
			b.lastErr = err
			return err
		}
		out, err := os.Create(b.path)
		if err != nil {
			b.lastErr = err
			return err
		}
		b.outfile = out
		b.ownOutfile = true
	}
	b.w = bufio.NewWriter(b.outfile)
	return b.writeHeader()
}

func (b *BreadcrumbTracer) closeOutput() error {
	if b == nil {
		return nil
	}
	var err error
	if b.w != nil {
		if flushErr := b.w.Flush(); flushErr != nil {
			err = flushErr
		}
		b.w = nil
	}
	if b.ownOutfile && b.outfile != nil {
		if closeErr := b.outfile.Close(); err == nil {
			err = closeErr
		}
		b.outfile = nil
	}
	if err != nil {
		b.lastErr = err
	}
	return err
}

func rotateExistingBreadcrumbPath(path string) error {
	if path == "" {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if info.IsDir() {
		return fmt.Errorf("%w: breadcrumb path %q is a directory", ErrBreadcrumbConfig, path)
	}
	for i := 1; ; i++ {
		suffix := fmt.Sprintf("%02d", i)
		if i > 99 {
			suffix = fmt.Sprintf("%d", i)
		}
		rotated := fmt.Sprintf("%s.%s", path, suffix)
		if _, err := os.Stat(rotated); err != nil {
			if !os.IsNotExist(err) {
				return err
			}
			if err := os.Rename(path, rotated); err != nil && !os.IsNotExist(err) {
				return err
			}
			return nil
		}
	}
}

func (b *BreadcrumbTracer) writeHeader() error {
	scope := "user"
	if b.includePrivileged {
		scope = "all"
	}
	_, err := fmt.Fprintf(b.w, "# riscv-breadcrumb-v1 kind=pc-hash digest=blake3-256 endian=little scope=%s interval=%d start_at=%d stop_at=%d after_at=%d after_interval=%d guest_control=%t filter_address_space=%t\n",
		scope, b.interval, b.startAt, b.stopAt, b.afterAt, b.afterInterval, b.guestControl, b.filterAddressSpace)
	if err != nil {
		b.lastErr = err
	}
	return err
}

func (b *BreadcrumbTracer) writeEvent(name string, attempt, retired, pc uint64, extra string) error {
	if b == nil || b.closed {
		return nil
	}
	if err := b.ensureOutput(); err != nil {
		return err
	}
	if extra != "" {
		extra = " " + extra
	}
	_, err := fmt.Fprintf(b.w, "# %s epoch=%d seq=%d attempt=%d retired=%d pc=0x%016x%s scope=%s\n",
		name, b.epoch, b.seq, attempt, retired, pc, extra, b.scope())
	if err != nil {
		b.lastErr = err
	}
	return err
}

func (b *BreadcrumbTracer) writeCheckpoint(attempt, retired, pc uint64, priv PrivilegeMode, satp uint64) error {
	if b == nil || b.closed {
		return nil
	}
	if err := b.ensureOutput(); err != nil {
		return err
	}
	digest := b.h.Sum(nil)
	// attempt, retired, epoch and satp are not repeatable, so omit them.
	//_, err := fmt.Fprintf(b.w, "seq=%d attempt=%d retired=%d epoch=%d priv=%s pc=0x%016x satp=0x%016x hash=%s\n", b.seq, attempt, retired, b.epoch, breadcrumbPrivName(priv), pc, satp, hex.EncodeToString(digest))
	_, err := fmt.Fprintf(b.w, "seq=%d pc=0x%016x hash=%s\n", b.seq, pc, hex.EncodeToString(digest))
	if err != nil {
		b.lastErr = err
		return err
	}
	for b.seq >= b.nextCheckpoint {
		b.nextCheckpoint += b.nextInterval()
	}
	return nil
}

func (b *BreadcrumbTracer) maybeTrip(attempt, retired, pc, satp uint64) error {
	if b == nil || b.tripMode == 0 || b.tripPending {
		return nil
	}
	switch b.tripMode {
	case 1:
		if b.tripSeq == 0 || b.seq != b.tripSeq {
			return nil
		}
	case 2:
		if b.tripAttempt == 0 || attempt != b.tripAttempt {
			return nil
		}
	default:
		return nil
	}
	return b.fireTrip(attempt, retired, pc, satp, breadcrumbTripReason(b.tripMode))
}

func (b *BreadcrumbTracer) shouldTripBeforeAttempt(attempt uint64, priv PrivilegeMode, satp uint64) bool {
	if b == nil || b.closed || !b.active || b.tripPending {
		return false
	}
	if !b.includePrivileged && priv != PrivUser {
		return false
	}
	if !b.addressSpaceWouldBeAllowed(priv, satp) {
		return false
	}
	eligibleSeq := b.eligibleSeq + 1
	if b.startAt != 0 && eligibleSeq < b.startAt {
		return false
	}
	if b.stopAt != 0 && eligibleSeq > b.stopAt {
		return false
	}
	if b.stopAt != 0 && eligibleSeq == b.stopAt {
		return true
	}
	switch b.tripMode {
	case 1:
		return b.tripSeq != 0 && b.seq+1 == b.tripSeq
	case 2:
		return b.tripAttempt != 0 && attempt == b.tripAttempt
	default:
		return false
	}
}

func (b *BreadcrumbTracer) addressSpaceWouldBeAllowed(priv PrivilegeMode, satp uint64) bool {
	if !b.filterAddressSpace || priv != PrivUser {
		return true
	}
	if !b.targetSATPValid {
		return true
	}
	return satp == b.targetSATP
}

func breadcrumbInjectBreakpoint(cpu *CPU, pc uint64) error {
	if cpu == nil {
		return ErrEbreak
	}
	cpu.pc = pc
	if cpu.trapToPrivilegedAt(pc, CauseBreakpoint, 0, 4) {
		return nil
	}
	cpu.setTrap(CauseBreakpoint, 4)
	return ErrEbreak
}

func (b *BreadcrumbTracer) fireTrip(attempt, retired, pc, satp uint64, reason string) error {
	if b == nil || b.tripPending {
		return nil
	}
	b.tripPending = true
	b.tripHitSeq = b.seq
	b.tripHitAttempt = attempt
	b.tripHitPC = pc
	b.tripMode = 0
	if reason == "" {
		reason = "manual"
	}
	if err := b.writeEvent("trip", attempt, retired, pc, b.withTargetExtra(fmt.Sprintf("reason=%s signal=%d pid=%d satp=0x%016x", reason, b.tripSignal, b.signalPID(), satp))); err != nil {
		return err
	}
	if err := b.Sync(); err != nil {
		return err
	}
	if b.tripNotify != nil {
		b.tripNotify()
	}
	return nil
}

func breadcrumbTripReason(mode uint32) string {
	switch mode {
	case 1:
		return "trip_seq"
	case 2:
		return "trip_attempt"
	default:
		return "manual"
	}
}

func (b *BreadcrumbTracer) signalPID() uint32 {
	if b == nil {
		return 0
	}
	if b.tripPID != 0 {
		return b.tripPID
	}
	return b.targetPID
}

func (b *BreadcrumbTracer) addressSpaceAllowed(priv PrivilegeMode, satp uint64) bool {
	if !b.filterAddressSpace || priv != PrivUser {
		return true
	}
	if !b.targetSATPValid {
		b.captureTargetAddressSpace(satp)
		return true
	}
	return satp == b.targetSATP
}

func (b *BreadcrumbTracer) captureTargetAddressSpace(satp uint64) {
	if b == nil || !b.filterAddressSpace || b.targetSATPValid {
		return
	}
	b.targetSATP = satp
	b.targetSATPValid = true
}

func (b *BreadcrumbTracer) withTargetExtra(extra string) string {
	if b == nil || !b.filterAddressSpace {
		return extra
	}
	valid := 0
	if b.targetSATPValid {
		valid = 1
	}
	suffix := fmt.Sprintf(" target_satp_valid=%d target_satp=0x%016x target_pid=%d", valid, b.targetSATP, b.targetPID)
	if extra == "" {
		return suffix[1:]
	}
	return extra + suffix
}

func (b *BreadcrumbTracer) nextInterval() uint64 {
	if b.afterAt != 0 && b.afterInterval != 0 && b.nextCheckpoint >= b.afterAt {
		return b.afterInterval
	}
	return b.interval
}

func (b *BreadcrumbTracer) scope() string {
	if b.includePrivileged {
		return "all"
	}
	return "user"
}

func firstBreadcrumbCheckpoint(interval, afterAt, afterInterval uint64) uint64 {
	if interval == 0 {
		interval = defaultBreadcrumbInterval
	}
	if afterAt != 0 && afterInterval != 0 && afterAt < interval {
		return afterAt
	}
	return interval
}

func breadcrumbPrivName(priv PrivilegeMode) string {
	switch priv {
	case PrivUser:
		return "user"
	case PrivSupervisor:
		return "supervisor"
	case PrivMachine:
		return "machine"
	default:
		return fmt.Sprintf("priv%d", priv)
	}
}

func (m *biosMMIO) enableBreadcrumb(b *BreadcrumbTracer) {
	if b == nil {
		return
	}
	b.tripNotify = m.markExternalInterruptDirty
	m.breadcrumb = &breadcrumbIODevice{tracer: b}
}

func (m *biosMMIO) closeBreadcrumb() error {
	if m == nil || m.breadcrumb == nil || m.breadcrumb.tracer == nil {
		return nil
	}
	return m.breadcrumb.tracer.Close()
}

func (d *breadcrumbIODevice) Load(off, width uint64) uint64 {
	if d == nil || d.tracer == nil {
		return 0
	}
	switch off {
	case breadcrumbRegMagic:
		return uint64(breadcrumbMagic)
	case breadcrumbRegVersion:
		return 1
	case breadcrumbRegStatus:
		return uint64(d.tracer.status())
	case breadcrumbRegControl:
		return uint64(d.tracer.pendingControl)
	case breadcrumbRegInterval:
		return d.tracer.interval
	case breadcrumbRegAfterAt:
		return d.tracer.afterAt
	case breadcrumbRegAfterInterval:
		return d.tracer.afterInterval
	case breadcrumbRegTripSeq:
		return d.tracer.tripSeq
	case breadcrumbRegTripAttempt:
		return d.tracer.tripAttempt
	case breadcrumbRegTripSignal:
		return uint64(d.tracer.tripSignal)
	case breadcrumbRegTripPID:
		return uint64(d.tracer.signalPID())
	case breadcrumbRegTripMode:
		return uint64(d.tracer.tripMode)
	case breadcrumbRegIRQStatus:
		if d.tracer.tripPending {
			return 1
		}
		return 0
	case breadcrumbRegTripHitSeq:
		return d.tracer.tripHitSeq
	case breadcrumbRegTripHitAttempt:
		return d.tracer.tripHitAttempt
	case breadcrumbRegTripHitPC:
		return d.tracer.tripHitPC
	case breadcrumbRegTargetPID:
		return uint64(d.tracer.targetPID)
	case breadcrumbRegTargetSATP:
		return d.tracer.targetSATP
	case breadcrumbRegTargetStatus:
		var status uint32
		if d.tracer.filterAddressSpace {
			status |= 1
		}
		if d.tracer.targetSATPValid {
			status |= 2
		}
		return uint64(status)
	default:
		return 0
	}
}

func (d *breadcrumbIODevice) Store(off, width, value uint64) *MemFault {
	if d == nil || d.tracer == nil {
		return nil
	}
	switch off {
	case breadcrumbRegControl:
		d.tracer.queueControl(breadcrumbControl(uint32(value)))
	case breadcrumbRegInterval:
		d.tracer.setInterval(value)
	case breadcrumbRegAfterAt:
		d.tracer.setAfterAt(value)
	case breadcrumbRegAfterInterval:
		d.tracer.setAfterInterval(value)
	case breadcrumbRegTripSeq:
		d.tracer.tripSeq = value
	case breadcrumbRegTripAttempt:
		d.tracer.tripAttempt = value
	case breadcrumbRegTripSignal:
		d.tracer.tripSignal = uint32(value)
	case breadcrumbRegTripPID:
		d.tracer.tripPID = uint32(value)
	case breadcrumbRegTripMode:
		d.tracer.setTripMode(uint32(value))
	case breadcrumbRegIRQStatus:
		if value&1 != 0 {
			d.tracer.tripPending = false
		}
	case breadcrumbRegTargetPID:
		d.tracer.targetPID = uint32(value)
	case breadcrumbRegTargetSATP:
		d.tracer.targetSATP = value
		d.tracer.targetSATPValid = true
	case breadcrumbRegTargetStatus:
		d.tracer.filterAddressSpace = value&1 != 0
		if value&2 == 0 {
			d.tracer.targetSATPValid = false
		}
	}
	return nil
}

func (d *breadcrumbIODevice) InterruptPending() bool {
	return d != nil && d.tracer != nil && d.tracer.tripPending
}

func (b *BreadcrumbTracer) setTripMode(mode uint32) {
	switch mode {
	case 0, 1, 2:
		b.tripMode = mode
		if mode != 0 {
			b.tripPending = false
			b.tripHitSeq = 0
			b.tripHitAttempt = 0
			b.tripHitPC = 0
		}
	}
}
