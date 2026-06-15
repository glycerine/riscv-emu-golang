package riscv

// machine.go — Machine bundles a CPU and its (optional) JIT for the
// fork API. Existing code that uses *CPU + *JIT directly continues
// to work; the Machine type is purely additive, needed only when
// callers want Clone semantics.
//
// Clone() copies guest memory copy-on-write via the OS (mach_vm_remap
// on darwin, memfd + MAP_PRIVATE on linux) and eagerly copies CPU
// register state. JIT compiled code is not shared: AOT segments are
// mutable decoder-cache tables owned by one JIT, so clones get a fresh
// JIT with the same configuration and warm their own native code.

// Machine is a forkable sandbox: a CPU with embedded GuestMemory,
// plus an optional JIT and optional OS. The zero value is not useful;
// construct via NewMachine or obtain a fork via (*Machine).Clone.
type Machine struct {
	CPU *CPU
	JIT *JIT // nil ⇒ interpreter-only sandbox

	// OS records the most recently installed OS personality (via
	// InstallOS). Clone reads this field to auto-install the same OS
	// on the child, since the child's NoteChain is fresh. nil when
	// no OS has been installed — the Machine runs in bare-metal mode.
	OS *OS

	// ownedMem is the *GuestMemory for a Machine whose memory was
	// allocated by Clone (via CowClone). Close frees it. For Machines
	// constructed by wrapping an externally-allocated CPU (via
	// NewMachine), ownedMem is nil and the caller is responsible for
	// freeing the guest memory.
	ownedMem *GuestMemory
}

// NewMachine wraps an existing CPU (with any embedded GuestMemory it
// already has) and optional JIT into a Machine. The Machine does NOT
// take ownership of the CPU's memory in this constructor — a
// subsequent Close() will not call Free. Use this when the caller
// already has a working CPU+JIT and wants to Clone it.
func NewMachine(cpu *CPU, jit *JIT) *Machine {
	return &Machine{CPU: cpu, JIT: jit}
}

// Clone returns a child Machine whose guest memory is a copy-on-write
// view of this Machine's and whose CPU register/CSR state equals this
// Machine's at fork time. If the parent has a JIT, the child gets a
// fresh JIT with the same configuration but no compiled code. The
// parent and child are fully independent after the call: writes on
// either side don't cross the fence.
//
// The child's NoteChain starts fresh. If the parent had an OS
// installed via InstallOS, the same *OS is auto-installed on the
// child — the common case, with no footgun. Any other handlers the
// parent pushed (closures capturing the parent *CPU, debug spies,
// etc.) are NOT copied; caller re-pushes them on the child if needed.
//
// The returned Machine owns its guest memory; Close will release it.
func (m *Machine) Clone() (*Machine, error) {
	childMem, err := m.CPU.mem.CowClone()
	if err != nil {
		return nil, err
	}
	childCPU := &CPU{
		mem:             *childMem, // value-copy base+mask+size+execRegions into the new CPU
		pc:              m.CPU.pc,
		x:               m.CPU.x,
		f:               m.CPU.f,
		fcsr:            m.CPU.fcsr,
		riscvInstrBegun: m.CPU.riscvInstrBegun,
		resvAddr:        m.CPU.resvAddr,
		resvValid:       m.CPU.resvValid,
		watchAddr:       m.CPU.watchAddr,
		mtvec:           m.CPU.mtvec,
		mepc:            m.CPU.mepc,
		mcause:          m.CPU.mcause,
		mstatus:         m.CPU.mstatus,
		mtval:           m.CPU.mtval,
		// Notes: zero value (empty NoteChain). Caller reinstalls handlers.
	}
	var childJIT *JIT
	if m.JIT != nil {
		childJIT = m.JIT.CloneConfig()
	}
	child := &Machine{
		CPU:      childCPU,
		JIT:      childJIT,
		ownedMem: childMem,
	}
	if m.OS != nil {
		child.InstallOS(m.OS)
	}
	return child, nil
}

// InstallOS pushes the given OS's Handle onto the Machine's CPU
// NoteChain AND records it on m.OS so Clone() can auto-install it
// on any children.
//
// The same *OS is safely shared between parent and child — OS is
// stateless (its syscall table is immutable after construction and
// handlers mutate only the per-call *CPU passed in).
//
// Call once after NewMachine (or once at any point before Clone).
// Calling again with a different *OS pushes the new handler on top
// (innermost-first delivery); the newest OS wins and is the one that
// Clone propagates to children.
func (m *Machine) InstallOS(os *OS) {
	m.OS = os
	m.CPU.Notes.Push(os.Handle)
}

// Close releases resources this Machine owns: any compiled code in
// the JIT, then the guest memory if the Machine owns it (i.e., was
// produced by Clone).
//
// After Close, the Machine's CPU and JIT must not be used.
// Idempotent — subsequent calls are no-ops.
func (m *Machine) Close() {
	if m.JIT != nil {
		m.JIT.Close()
	}
	if m.ownedMem != nil {
		m.ownedMem.Free()
		m.ownedMem = nil
	}
}
