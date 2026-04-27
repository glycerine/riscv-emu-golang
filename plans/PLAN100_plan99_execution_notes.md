On the fly ideas and repairs to get the plan to work!

Batched IC budget at back-edges for lockstep slicing

     Context

     The lockstep tests (TestRISCVTests_Lockstep_UI, etc.) compare JIT vs interpreter
     state after each StepBlock call. They depend on StepBlock returning an instruction
     count so the interpreter can run the same number of steps. After removing
     per-instruction IC counting (2x overhead), StepBlock returns 0 and the lockstep
     tests break.

     The solution: when DebugOneBlockLockstepMode is true on the JIT, emit a batched IC
     budget check at each backward branch. The check uses three x86 instructions that
     modify only EFLAGS (no GP registers): ADD QWORD [RBP+ic_offset], N; CMP QWORD
     [RBP+ic_offset], budget; JGE exit. N is the static instruction count of the loop
     body, known at compile time. Heavy loops hit the budget faster than tight loops, so
     both approximate the same total instruction count before exiting.

     When DebugOneBlockLockstepMode is false (production), no budget check is emitted.
     Zero overhead.

     The existing StopperLoad + Jump pattern at backward branches is preserved unchanged.
      The budget check is emitted before the StopperLoad when lockstep mode is active.

     New JIT fields (jit.go)

     DebugOneBlockLockstepMode bool    // emit budget checks at back-edges
     LockstepModeBudget        int64   // max IC before forced exit (default 65536)

     NewJIT() sets LockstepModeBudget = 65536.

     State struct — add IC field (abjit/abjit.go)

     Add IC uint64 after Cycles. Record its byte offset. Update TestStateLayout.

     Emitter changes (jit_emit_ir.go)

     Add to the emitter struct:
     lockstepMode   bool
     lockstepBudget int64
     backEdgeIC     int   // instructions since last back-edge or block entry

     Passed from JIT via emitBlockRange:
     lockstepMode:   j.DebugOneBlockLockstepMode,
     lockstepBudget: int64(j.LockstepModeBudget),

     In advancePC(), also increment e.backEdgeIC++.

     At each backward branch site (in emitJAL and emitBranch), before the existing
     StopperLoad + Jump, when e.lockstepMode is true:

     if e.lockstepMode {
         exitLabel := e.irEm.NewLabel()
         e.irEm.MemBudget(IC_OFFSET, int64(e.backEdgeIC), e.lockstepBudget, exitLabel)
         e.backEdgeIC = 0
         // exitLabel path: writeback + return
         // (emitted after the normal backward branch code, before finalize)
     }
     // existing StopperLoad + Jump unchanged

     The exit path at exitLabel: WriteBackAll() + Ret(targetPC, jitOK, VRegZero).

     New IR op: IRMemBudget (ir.go)

     IRMemBudget // ADD [RBP+Imm], Imm2; CMP [RBP+Imm], Imm3; JGE Label(Dst)

     - Imm = State struct offset of IC
     - Imm2 = instruction delta to add
     - Imm3 = budget limit
     - Dst = overflow label (cast to VReg, reinterpreted as Label by lowerer)

     Lowerer (lower_amd64_ops.go)

     func (lc *lowerOps) opsMemBudget(ins *IRInstr) {
         off := ins.Imm  // IC offset in State

         // ADD QWORD [RBP + off], ins.Imm2
         p1 := lc.c.NewProg()
         p1.As = x86.AADDQ
         p1.From.Type = obj.TYPE_CONST
         p1.From.Offset = ins.Imm2
         p1.To.Type = obj.TYPE_MEM
         p1.To.Reg = goasm.REG_AMD64_BP
         p1.To.Offset = off
         lc.c.Append(p1)

         // CMP QWORD [RBP + off], ins.Imm3
         p2 := lc.c.NewProg()
         p2.As = x86.ACMPQ
         p2.From.Type = obj.TYPE_MEM
         p2.From.Reg = goasm.REG_AMD64_BP
         p2.From.Offset = off
         p2.To.Type = obj.TYPE_CONST
         p2.To.Offset = ins.Imm3
         lc.c.Append(p2)

         // JGE overflow label
         p3 := lc.c.NewProg()
         p3.As = x86.AJGE
         p3.To.Type = obj.TYPE_BRANCH
         lc.c.Append(p3)
         lc.bindLabel(Label(ins.Dst), p3)
     }

     Dispatch — zero IC and read it back

     jit_abjit.go (abjitDispatch): Set s.IC = 0 before abjit.CallJIT. Read s.IC into
     result after.

     jit.go (StepBlock): Return IC from result. For straight-line blocks (no back-edge
     hit, IC stays 0), fall back to blk.numInsns:

     case jitOK:
         ic := res.IC
         if ic == 0 {
             ic = uint64(blk.numInsns)
         }
         return ic, nil

     compiledBlock — add numInsns (jit.go)

     Add numInsns int to compiledBlock. Set from res.numInsns during jitCompile.

     Lockstep test updates (riscv_test.go)

     In runLockstep:
     jit := NewJIT()
     jit.DebugOneBlockLockstepMode = true

     Interpreter loop uses returned IC as before:
     jitIC, jitErr := jit.StepBlock(jitCPU)
     for i := uint64(0); i < jitIC; i++ {
         interpErr = interpCPU.step()
         ...
     }

     Files to modify

     - ir.go — add IRMemBudget op
     - highlevel.go — add MemBudget() emit helper
     - lower_amd64_ops.go — add opsMemBudget() lowering, wire in dispatch
     - jit_emit_ir.go — add lockstepMode/lockstepBudget/backEdgeIC to emitter; emit
     budget before StopperLoad at backward branches
     - jit.go — add DebugOneBlockLockstepMode/LockstepModeBudget fields; read IC in
     StepBlock; add numInsns to compiledBlock
     - abjit/abjit.go — add IC field to State
     - abjit/abjit_test.go — update TestStateLayout
     - jit_abjit.go — zero IC before block entry, read IC into result
     - riscv_test.go — set DebugOneBlockLockstepMode in lockstep tests, restore IC-based
     interpreter loop

     Verification

     go build ./...
     go test -v -run TestRISCVTests_Lockstep_UI/add -timeout 30s .
     go test -v -run TestRISCVTests_Lockstep_UI -timeout 300s .
     go test -v -run TestABJIT_RISCVTests_UI -timeout 30s .
     go test -v ./...

⏺ This is a substantial implementation. Let me work through it systematically, starting
  from the bottom (State struct, IR op) and building up.

  First, add IC to the State struct:

⏺ Update(abjit/abjit.go)
  ⎿  Added 1 line
      29    VAddrBegin uint64
      30    SegSize    uint64
      31    Cycles     uint64 // TSC cycles (RDTSC delta, written by exit thunk)
      32 +  IC         uint64 // instruction count (accumulated by back-edge budge
         +t checks)
      33  }
      34
      35  //go:noinline

⏺ Now let me check the current State layout test to get the right offset:

  Searched for 1 pattern (ctrl+o to expand)

⏺ IC will be at offset 600. Let me update the test:

⏺ Update(abjit/abjit_test.go)
  ⎿  File must be read first

  Read 1 file (ctrl+o to expand)

⏺ Update(abjit/abjit_test.go)
  ⎿  Added 1 line
      282      {"VAddrBegin", unsafe.Offsetof(s.VAddrBegin), 576},
      283      {"SegSize", unsafe.Offsetof(s.SegSize), 584},
      284      {"Cycles", unsafe.Offsetof(s.Cycles), 592},
      285 +    {"IC", unsafe.Offsetof(s.IC), 600},
      286    }
      287    for _, c := range checks {
      288      if c.got != c.want {

⏺ Now add the IRMemBudget op to ir.go:

⏺ Update(ir.go)
  ⎿  Added 5 lines
      254    // Guard page probe (preemption check at backward branches).
      255    IRStopperLoad // probe [Imm]; faults if page armed. No GP reg modifie
           d.
      256
      257 +  // Batched IC budget check at back-edges (lockstep mode only).
      258 +  // ADD [RBP+Imm], Imm2; CMP [RBP+Imm], Imm3; JGE Label(Dst).
      259 +  // Modifies only EFLAGS — no GP registers touched.
      260 +  IRMemBudget
      261 +
      262    // Pseudo-ops
      263    IRMarkLive  // declares A live here (allocator hint)
      264    IRMarkDead  // declares A dead here (allocator hint)

⏺ Now add it to the op names table:

  Read 1 file (ctrl+o to expand)

⏺ Update(ir.go)
  ⎿  Added 1 line
      346    IRFCvtFromU: "fcvt_from_u",
      347    IRFCvtFF:    "fcvt_ff",
      348    IRStopperLoad: "stopper_load",
      349 +  IRMemBudget:   "mem_budget",
      350    IRMarkLive:    "mark_live",
      351    IRMarkDead:    "mark_dead",
      352    IRWriteback:   "writeback",

⏺ Now add the IRMemBudget String case. Let me check the existing IRStopperLoad case:

  Searched for 1 pattern (ctrl+o to expand)

⏺ Update(ir.go)
  ⎿  Added 3 lines
      407      return fmt.Sprintf("%s targetPC=0x%x exitIdx=%d", ins.Op, uint64(in
           s.Imm), ins.Imm2)
      408    case IRStopperLoad:
      409      return fmt.Sprintf("%s addr=0x%x", ins.Op, uint64(ins.Imm))
      410 +  case IRMemBudget:
      411 +    return fmt.Sprintf("%s [RBP+%d] += %d, limit=%d, overflow=L%d",
      412 +      ins.Op, ins.Imm, ins.Imm2, ins.Imm3, ins.Dst)
      413    default:
      414      if ins.B != VRegZero {
      415        return fmt.Sprintf("%s.%s %s = %s, %s", ins.Op, ins.T, ins.Dst, i
           ns.A, ins.B)

⏺ Now I need to check if IRInstr has Imm3. Let me check the struct:

  Searched for 1 pattern (ctrl+o to expand)

⏺ No Imm3 field on the struct. Let me check what fields IRInstr has:

  Searched for 1 pattern, read 1 file (ctrl+o to expand)

⏺ No Imm3 — only Imm and Imm2. I need to rethink the encoding. I can use:
  - Imm = IC offset in State
  - Imm2 = instruction delta to add
  - Dst = overflow label (reinterpreted as Label, same as IRJump uses Imm for label)
  - The budget limit can be stored in A (reinterpreted as int64) or I can use a different
   packing.

  Actually, simpler: the IC offset is a compile-time constant (600), so I don't need it
  in the instruction. And I can pack budget into Imm and delta into Imm2, with overflow
  label in Dst (cast to VReg).

  Let me fix the IR op comment and the String method:

⏺ Update(ir.go)
  ⎿  Added 2 lines, removed 1 line
      255    IRStopperLoad // probe [Imm]; faults if page armed. No GP reg modifie
           d.
      256
      257    // Batched IC budget check at back-edges (lockstep mode only).
      258 -  // ADD [RBP+Imm], Imm2; CMP [RBP+Imm], Imm3; JGE Label(Dst).
      258 +  // ADD [RBP+IC_OFFSET], Imm; CMP [RBP+IC_OFFSET], Imm2; JGE Label(Dst
          +).
      259 +  // IC_OFFSET is hardcoded (State.IC offset). Imm=delta, Imm2=budget.
      260    // Modifies only EFLAGS — no GP registers touched.
      261    IRMemBudget
      262

⏺ Update(ir.go)
  ⎿  Added 2 lines, removed 2 lines
      409    case IRStopperLoad:
      410      return fmt.Sprintf("%s addr=0x%x", ins.Op, uint64(ins.Imm))
      411    case IRMemBudget:
      412 -    return fmt.Sprintf("%s [RBP+%d] += %d, limit=%d, overflow=L%d",
      413 -      ins.Op, ins.Imm, ins.Imm2, ins.Imm3, ins.Dst)
      412 +    return fmt.Sprintf("%s delta=%d budget=%d overflow=L%d",
      413 +      ins.Op, ins.Imm, ins.Imm2, ins.Dst)
      414    default:
      415      if ins.B != VRegZero {
      416        return fmt.Sprintf("%s.%s %s = %s, %s", ins.Op, ins.T, ins.Dst, i
           ns.A, ins.B)

⏺ Now add the highlevel emit helper. Let me read the current highlevel.go around the
  StopperLoad:

  Read 1 file (ctrl+o to expand)

⏺ Update(highlevel.go)
  ⎿  Added 12 lines
      164    e.emit(IRInstr{Op: IRStopperLoad, Imm: addr})
      165  }
      166
      167 +// MemBudget emits a batched IC budget check for lockstep mode.
      168 +// delta is the instruction count to add. budget is the limit.
      169 +// overflowLabel is jumped to when budget is exceeded.
      170 +func (e *Emitter) MemBudget(delta int, budget int64, overflowLabel Labe
          +l) {
      171 +  e.emit(IRInstr{
      172 +    Op:   IRMemBudget,
      173 +    Imm:  int64(delta),
      174 +    Imm2: budget,
      175 +    Dst:  VReg(overflowLabel),
      176 +  })
      177 +}
      178 +
      179  // ClearDirtySyscallRegs clears dirty flags for a0 (x10) and a1 (x11)
      180  // only. Called before IRSyscall so the subsequent ReloadSyscallRegs
      181  // picks up the dispatcher's return values from x[].

⏺ Now the lowerer. Add the case and implementation:

⏺ Update(lower_amd64_ops.go)
  ⎿  Added 2 lines
      1526
      1527    case IRStopperLoad:
      1528      lc.opsStopperLoad(ins)
      1529 +  case IRMemBudget:
      1530 +    lc.opsMemBudget(ins)
      1531
      1532    // Pseudo-ops
      1533    case IRMarkLive, IRMarkDead, IRWriteback:

⏺ Now add the opsMemBudget function. I need the IC offset — 600:

⏺ Update(lower_amd64_ops.go)
  ⎿  Added 35 lines
      1563    lc.c.Append(p)
      1564  }
      1565
      1566 +// abjitStateICOffset is the byte offset of IC in abjit.State.
      1567 +// Must agree with abjit/abjit.go and TestStateLayout.
      1568 +const abjitStateICOffset = 600
      1569 +
      1570 +func (lc *lowerOps) opsMemBudget(ins *IRInstr) {
      1571 +  off := int64(abjitStateICOffset)
      1572 +
      1573 +  // ADD QWORD [RBP + off], delta
      1574 +  p1 := lc.c.NewProg()
      1575 +  p1.As = x86.AADDQ
      1576 +  p1.From.Type = obj.TYPE_CONST
      1577 +  p1.From.Offset = ins.Imm // delta
      1578 +  p1.To.Type = obj.TYPE_MEM
      1579 +  p1.To.Reg = goasm.REG_AMD64_BP
      1580 +  p1.To.Offset = off
      1581 +  lc.c.Append(p1)
      1582 +
      1583 +  // CMP QWORD [RBP + off], budget
      1584 +  p2 := lc.c.NewProg()
      1585 +  p2.As = x86.ACMPQ
      1586 +  p2.From.Type = obj.TYPE_MEM
      1587 +  p2.From.Reg = goasm.REG_AMD64_BP
      1588 +  p2.From.Offset = off
      1589 +  p2.To.Type = obj.TYPE_CONST
      1590 +  p2.To.Offset = ins.Imm2 // budget
      1591 +  lc.c.Append(p2)
      1592 +
      1593 +  // JGE overflow label
      1594 +  p3 := lc.c.NewProg()
      1595 +  p3.As = x86.AJGE
      1596 +  p3.To.Type = obj.TYPE_BRANCH
      1597 +  lc.c.Append(p3)
      1598 +  lc.bindLabel(Label(ins.Dst), p3)
      1599 +}
      1600 +
      1601  // Ensure imports are used.
      1602  var _ = sort.Sort
      1603  var _ = fmt.Errorf

⏺ Now add the JIT fields and defaults:

⏺ Update(jit.go)
  ⎿  Added 3 lines
      243    stopperPage uintptr // InfiniteLoopStopperPage: mmap'd guard page for
            preemption
      244    watchAddr   uint64  // tohost address; JIT blocks exit when a store h
           its this address
      245
      246 +  DebugOneBlockLockstepMode bool  // emit budget checks at back-edges
      247 +  LockstepModeBudget        int64 // max IC before forced exit (default
          + 65536)
      248 +
      249    // Dispatch counters (for diagnostics).
      250    DispatchOK       uint64 // jitOK returns to Go dispatch
      251    DispatchOther    uint64 // non-OK returns (ecall, fault, etc.)

⏺ Set default in NewJIT:

  Read 1 file (ctrl+o to expand)

⏺ Update(jit.go)
  ⎿  Added 3 lines, removed 2 lines
      262  // allocation policy is PolicyABJIT (compare PolicyRV8); see lower_amd6
           4.go
      263  func NewJIT() *JIT {
      264    j := &JIT{
      265 -    noJIT:   make(map[uint64]bool),
      266 -    irAlloc: NewFixedStaticAllocator(),
      265 +    noJIT:              make(map[uint64]bool),
      266 +    irAlloc:            NewFixedStaticAllocator(),
      267 +    LockstepModeBudget: 65536,
      268    }
      269    j.SetRegPolicy(PolicyABJIT)
      270    if err := j.initStopperPage(); err != nil {

⏺ Now add numInsns to compiledBlock:

  Searched for 1 pattern (ctrl+o to expand)

⏺ Update(jit.go)
  ⎿  Added 1 line
      143    chainExits []chainPatchInfo  // chain exits for patching
      144    jalrICs    []jalrICPatchInfo // JALR IC sites for patching
      145    hasFP      bool              // block uses FP registers (skip f[] cop
           y when false)
      146 +  numInsns   int               // static instruction count from emissio
          +n
      147
      148    // segment is the DecodedExecuteSegment that owns this block's native
      149    // code, or nil for lazy-compiled blocks. Set at AOT install time;

⏺ Now pass lockstepMode and lockstepBudget to the emitter, and numInsns to compiledBlock.
   Let me update emitBlockRange:

⏺ Update(jit_emit_ir.go)
  ⎿  Added 12 lines, removed 10 lines
      925    //gt.IterStart = testIterStart
      926
      927    e := &emitter{
      928 -    mem:         mem,
      929 -    startPC:     pc,
      930 -    pc:          pc,
      931 -    irEm:        irEm,
      932 -    visited:     make(map[uint64]bool),
      933 -    regionEnd:   endPC,
      934 -    gotoTargets: gt,
      935 -    pcLabels:    newU64labelmap(),
      936 -    stopperAddr: int64(j.stopperPage),
      937 -    watchAddr:   j.watchAddr,
      928 +    mem:            mem,
      929 +    startPC:        pc,
      930 +    pc:             pc,
      931 +    irEm:           irEm,
      932 +    visited:        make(map[uint64]bool),
      933 +    regionEnd:      endPC,
      934 +    gotoTargets:    gt,
      935 +    pcLabels:       newU64labelmap(),
      936 +    stopperAddr:    int64(j.stopperPage),
      937 +    watchAddr:      j.watchAddr,
      938 +    lockstepMode:   j.DebugOneBlockLockstepMode,
      939 +    lockstepBudget: j.LockstepModeBudget,
      940    }
      941
      942    // Emit IR (populates regsUsed via xreg/xregDst calls).

⏺ Now add the fields to the emitter struct and update advancePC:

⏺ Update(jit_emit_ir.go)
  ⎿  Error editing file

  Read 1 file (ctrl+o to expand)

⏺ Update(jit_emit_ir.go)
  ⎿  Added 3 lines
      68    pcLabels       u64labelmap
      69    stopperAddr    int64  // InfiniteLoopStopperPage address for backward-
          branch probes
      70    watchAddr      uint64 // tohost address; stores here trigger a block e
          xit
      71 +  lockstepMode   bool
      72 +  lockstepBudget int64
      73 +  backEdgeIC     int    // instructions since last back-edge or block en
         +try
      74    deferredExits  []deferredExit
      75    deferredFaults []deferredFault
      76    exitIdx        int      // counter for chain exit indices

⏺ Update advancePC:

⏺ Update(jit_emit_ir.go)
  ⎿  Added 1 line
      327
      328  func (e *emitter) advancePC(size uint64) {
      329    e.numInsns++
      330 +  e.backEdgeIC++
      331    e.pc += size
      332  }
      333

⏺ Now emit the budget check at backward branches. First emitJAL:

⏺ Update(jit_emit_ir.go)
  ⎿  Added 13 lines, removed 2 lines
      2572      // and whether the target was already emitted (visited).
      2573      backward := target < origPC || e.visited[target]
      2574      if backward {
      2575 -      e.irEm.StopperLoad(e.stopperAddr)
      2576 -      e.irEm.Jump(targetLabel)
      2575 +      if e.lockstepMode {
      2576 +        exitLabel := e.irEm.NewLabel()
      2577 +        e.irEm.MemBudget(e.backEdgeIC, e.lockstepBudget, exitLabel)
      2578 +        e.backEdgeIC = 0
      2579 +        e.irEm.StopperLoad(e.stopperAddr)
      2580 +        e.irEm.Jump(targetLabel)
      2581 +        e.irEm.PlaceLabel(exitLabel)
      2582 +        e.irEm.WriteBackAll()
      2583 +        e.irEm.Ret(target, jitOK, VRegZero)
      2584 +      } else {
      2585 +        e.irEm.StopperLoad(e.stopperAddr)
      2586 +        e.irEm.Jump(targetLabel)
      2587 +      }
      2588        e.gotoTargets.add(target)
      2589        e.terminated = true
      2590        return

⏺ Now emitBranch:

⏺ Update(jit_emit_ir.go)
  ⎿  Added 13 lines, removed 2 lines
      2671        e.irEm.Branch(a, b, pred, takenLabel)
      2672        e.irEm.Jump(continueLabel) // not taken: skip stopper probe
      2673        e.irEm.PlaceLabel(takenLabel)
      2674 -      e.irEm.StopperLoad(e.stopperAddr)
      2675 -      e.irEm.Jump(targetLabel)
      2674 +      if e.lockstepMode {
      2675 +        exitLabel := e.irEm.NewLabel()
      2676 +        e.irEm.MemBudget(e.backEdgeIC, e.lockstepBudget, exitLabel)
      2677 +        e.backEdgeIC = 0
      2678 +        e.irEm.StopperLoad(e.stopperAddr)
      2679 +        e.irEm.Jump(targetLabel)
      2680 +        e.irEm.PlaceLabel(exitLabel)
      2681 +        e.irEm.WriteBackAll()
      2682 +        e.irEm.Ret(target, jitOK, VRegZero)
      2683 +      } else {
      2684 +        e.irEm.StopperLoad(e.stopperAddr)
      2685 +        e.irEm.Jump(targetLabel)
      2686 +      }
      2687        e.irEm.PlaceLabel(continueLabel)
      2688      } else {
      2689        e.irEm.Branch(a, b, pred, targetLabel)

⏺ Now wire up the dispatch side — zero IC before entry and read it back. First
  jit_abjit.go:

  Read 1 file (ctrl+o to expand)

⏺ Update(jit_abjit.go)
  ⎿  Added 2 lines
      27    s.DCMask = dcMask
      28    s.VAddrBegin = vBegin
      29    s.SegSize = segSize
      30 +  s.IC = 0
      31
      32    abjit.CallJIT(blk.fn, s.RegFileBase())
      33
      34    res := jitcall.Result{
      35      PC:        s.PC,
      36 +    IC:        s.IC,
      37      Status:    s.Status,
      38      FaultAddr: s.FaultAddr,
      39      Cycles:    s.Cycles,

⏺ Now add IC to the jitcall.Result struct:

  Read 1 file (ctrl+o to expand)

⏺ The Result struct is used by the assembly trampolines. Adding IC would change the
  struct layout and break the assembly. But IC for lockstep mode comes from the
  abjit.State struct, not from the sret buffer. Let me just add it to Result without
  affecting assembly — the assembly only writes PC/Status/FaultAddr/Cycles at fixed
  offsets. IC will only be populated by the abjitDispatch Go code, not by assembly:

⏺ Update(internal/jitcall/call.go)
  ⎿  Added 4 lines, removed 1 line
       3  package jitcall
       4
       5  // Result is the return value from a JIT-compiled block.
       6 -// All fields are uint64 for simple assembly access.
       6 +// The first four fields are written by assembly trampolines at fixed
       7 +// offsets — do not reorder them. IC is populated by Go dispatch code
       8 +// (abjitDispatch), not by assembly.
       9  type Result struct {
      10    PC        uint64 // next PC to execute
      11    Status    uint64 // 0=ok, 1=ecall, 2=ebreak, 3=load_fault, 4=store_fau
          lt, 5=illegal
      12    FaultAddr uint64 // guest address that faulted (when Status >= 3)
      13    Cycles    uint64 // TSC cycles spent in native code (RDTSC delta)
      14 +  IC        uint64 // instruction count (from State.IC, lockstep mode on
         +ly)
      15  }
      16
      17  // Call invokes a JIT-compiled block via direct function pointer.

⏺ Now update StepBlock to return IC. Let me find the jitOK return:

  Read 1 file (ctrl+o to expand)

⏺ I need to use res.IC with fallback to blk.numInsns:

⏺ Update(jit.go)
  ⎿  Added 7 lines, removed 2 lines
      595      cpu.pc = res.PC
      596      cpu.cycle += res.Cycles
      597
      598 +    ic := res.IC
      599 +    if ic == 0 {
      600 +      ic = uint64(blk.numInsns)
      601 +    }
      602 +
      603      switch int(res.Status) {
      604      case jitOK:
      600 -      return 0, nil
      605 +      return ic, nil
      606      case jitOKJalrMiss:
      602 -      return 0, nil
      607 +      return ic, nil
      608      case jitMisalign:
      609        if err := cpu.step(); err != nil {
      610          return 1, err

⏺ Now I need to set numInsns on compiledBlock during compilation. Let me find jitCompile:

  Read 1 file (ctrl+o to expand)

⏺ Update(jit_native.go)
  ⎿  Added 1 line
      71      fn:         codeBase,
      72      nativeMmap: execMem,
      73      hasFP:      allocHasFP(alloc),
      74 +    numInsns:   res.numInsns,
      75    }
      76
      77    // Step 6: Block chaining setup — backpatch MOVABS sentinels and recor
          d metadata.

⏺ Now update the lockstep test. Let me revert my earlier broken PC-based change and
  restore IC-based iteration:

  Read 1 file (ctrl+o to expand)

⏺ Update(riscv_test.go)
  ⎿  Added 4 lines, removed 6 lines
      406    //t.Logf("jitMem base=%#x interpMem base=%#x size=%#x", jitMem.Base()
           , interpMem.Base(), jitMem.Size())
      407
      408    jit := NewJIT()
      409 +  jit.DebugOneBlockLockstepMode = true
      410    maxCycles := uint64(10_000_000)
      411    blockNum := 0
      412
     ...
      417      }
      418
      419      // JIT: one dispatch cycle
      419 -    _, jitErr := jit.StepBlock(jitCPU)
      420 +    jitIC, jitErr := jit.StepBlock(jitCPU)
      421
      421 -    // Interpreter: advance to the same PC as the JIT
      422 -    targetPC := jitCPU.pc
      422 +    // Interpreter: same number of instructions
      423      var interpErr error
      424 -    interpSteps := 0
      425 -    for interpCPU.pc != targetPC && interpSteps < 100000 {
      426 -      interpSteps++
      424 +    for i := uint64(0); i < jitIC; i++ {
      425        if blockNum == 59 {
      426          ipc := interpCPU.pc
      427          // Log any memory access to the upper half (sandbox area)

⏺ Now fix the error message that referenced interpSteps:

  Read 1 file (ctrl+o to expand)

⏺ Update(riscv_test.go)
  ⎿  Added 2 lines, removed 2 lines
      469      regMismatch := false
      470      for r := 0; r < 32; r++ {
      471        if jitCPU.x[r] != interpCPU.x[r] {
      472 -        t.Errorf("block %d (pc=0x%x, steps=%d): x[%d] mismatch: jit=0x%
          -x interp=0x%x",
      473 -          blockNum, jitCPU.pc, interpSteps, r, jitCPU.x[r], interpCPU.x
          -[r])
      472 +        t.Errorf("block %d (pc=0x%x, ic=%d): x[%d] mismatch: jit=0x%x i
          +nterp=0x%x",
      473 +          blockNum, jitCPU.pc, jitIC, r, jitCPU.x[r], interpCPU.x[r])
      474          regMismatch = true
      475        }
      476      }

⏺ Now let me build and see where we are:

⏺ Bash(go build ./... 2>&1 | head -20)
  ⎿  (No output)

⏺ Clean build. Let me test the lockstep:

⏺ Bash(go test -v -run 'TestRISCVTests_Lockstep_UI/^add$' -timeout 30s . 2>&1 | tail -10)
  ⎿  === RUN   TestRISCVTests_Lockstep_UI
     === RUN   TestRISCVTests_Lockstep_UI/add
         riscv_test.go:515: block 0: PC mismatch AFTER dispatch: jit=0x48 interp=0x8
     … +6 lines (ctrl+o to expand)
  ⎿  (timeout 45s)

⏺ Progress — now the interpreter runs (PC=0x86 instead of 0x0), but it overshoots. JIT
  says PC should be 0x48, interpreter reached 0x86. The numInsns static count is too
  large because the block includes instructions past the first exit point (the tohost
  exit).

  The issue: the block at PC 0x0 includes a c.j +72 (jump to 0x48) at the very start. The
   JIT follows this and the block covers PCs 0x0-0x48+ but exits after just 1-2
  instructions. numInsns counts ALL instructions in the block, not just the ones on the
  taken path.

  For straight-line blocks without loops, numInsns overcounts when the block has
  branches. We need the actual path-specific count. But without runtime IC, we only have
  the static count.

  The batched IC at back-edges handles loop iterations. But for blocks that exit via
  forward branches or chain exits (no back-edge), we need the static count of the taken
  path, not the whole block.

  The simplest fix: for blocks exiting via jitOK with IC == 0 (no back-edge was hit), we
  need a better fallback than blk.numInsns. The block at PC 0 is small (just a jump), so
  the actual instruction count should be 1 or 2.

  Actually — the tohost exit we added also returns jitOK. When it fires, IC is 0 (no
  back-edge budget check was hit on that path). We should emit the IC increment at the
  tohost exit path too. Let me check our AUIPC+STORE fusion:

  Read 1 file (ctrl+o to expand)

⏺ The problem is actually the block at PC 0x0. The first instruction is c.j +72 (jump to
  0x48). The JIT block at PC 0 exits immediately with PC=0x48 via a chain exit. numInsns
  for this block is the count of ALL instructions scanned (many), but only 1 instruction
  was actually executed.

  The real fix: each exit path in the block should know its instruction count. For chain
  exits, that's the count of instructions on that specific path. But this is hard without
   runtime counting.

  For the lockstep test, the pragmatic fix: when DebugOneBlockLockstepMode is true, also
  emit ADD [IC], backEdgeIC at every block exit (Ret, chain exit), not just at
  back-edges. This way IC is always populated with the path-specific count.

  Let me add IC updates at the exit paths. The key function is emitReturn and
  emitChainableReturn:

  Read 1 file (ctrl+o to expand)

⏺ Update(jit_emit_ir.go)
  ⎿  Added 6 lines
      332  }
      333
      334  func (e *emitter) emitReturn(pc uint64, status int) {
      335 +  if e.lockstepMode && e.backEdgeIC > 0 {
      336 +    exitLabel := e.irEm.NewLabel()
      337 +    e.irEm.MemBudget(e.backEdgeIC, e.lockstepBudget, exitLabel)
      338 +    e.irEm.PlaceLabel(exitLabel)
      339 +    e.backEdgeIC = 0
      340 +  }
      341    e.irEm.WriteBackAll()
      342    e.irEm.Ret(pc, status, VRegZero)
      343  }

⏺ Wait — that's wrong. At a non-loop exit, we don't want a budget check (JGE exit). We
  just want to store the IC count. Let me rethink.

  The MemBudget op does ADD + CMP + JGE. At a block exit, we just need the ADD (to write
  the count to State.IC so it's returned). No comparison needed. Let me make a simpler
  helper:

⏺ Update(jit_emit_ir.go)
  ⎿  Added 9 lines, removed 6 lines
      332  }
      333
      334  func (e *emitter) emitReturn(pc uint64, status int) {
      335 -  if e.lockstepMode && e.backEdgeIC > 0 {
      336 -    exitLabel := e.irEm.NewLabel()
      337 -    e.irEm.MemBudget(e.backEdgeIC, e.lockstepBudget, exitLabel)
      338 -    e.irEm.PlaceLabel(exitLabel)
      339 -    e.backEdgeIC = 0
      340 -  }
      335 +  e.flushIC()
      336    e.irEm.WriteBackAll()
      337    e.irEm.Ret(pc, status, VRegZero)
      338  }
      339
      340 +// flushIC emits ADD [State.IC], backEdgeIC if lockstep mode is active,
      341 +// so the caller sees the correct instruction count at any exit path.
      342 +func (e *emitter) flushIC() {
      343 +  if e.lockstepMode && e.backEdgeIC > 0 {
      344 +    e.irEm.MemAdd(abjitStateICOffset, int64(e.backEdgeIC))
      345 +  }
      346 +}
      347 +
      348  // emitSyscall emits the ECALL fast path: writeback all dirty regs,
      349  // then emit IRSyscall which CALLs the SysV dispatcher. The dispatcher
      350  // returns 0 (handled → block exits with Status=jitOK) or 1 (fallback

⏺ Now I need a MemAdd IR op (just the ADD, no CMP/JGE). Let me add it:

⏺ Update(ir.go)
  ⎿  Added 3 lines
      254    // Guard page probe (preemption check at backward branches).
      255    IRStopperLoad // probe [Imm]; faults if page armed. No GP reg modifie
           d.
      256
      257 +  // ADD [RBP+Imm], Imm2. Modifies only EFLAGS — no GP registers.
      258 +  IRMemAdd
      259 +
      260    // Batched IC budget check at back-edges (lockstep mode only).
      261    // ADD [RBP+IC_OFFSET], Imm; CMP [RBP+IC_OFFSET], Imm2; JGE Label(Dst
           ).
      262    // IC_OFFSET is hardcoded (State.IC offset). Imm=delta, Imm2=budget.

⏺ Add to op names:

⏺ Update(ir.go)
  ⎿  Added 1 line
      350    IRFCvtFromU: "fcvt_from_u",
      351    IRFCvtFF:    "fcvt_ff",
      352    IRStopperLoad: "stopper_load",
      353 +  IRMemAdd:      "mem_add",
      354    IRMemBudget:   "mem_budget",
      355    IRMarkLive:    "mark_live",
      356    IRMarkDead:    "mark_dead",

⏺ Add String case:

⏺ Update(ir.go)
  ⎿  Added 2 lines
      412      return fmt.Sprintf("%s targetPC=0x%x exitIdx=%d", ins.Op, uint64(in
           s.Imm), ins.Imm2)
      413    case IRStopperLoad:
      414      return fmt.Sprintf("%s addr=0x%x", ins.Op, uint64(ins.Imm))
      415 +  case IRMemAdd:
      416 +    return fmt.Sprintf("%s [RBP+%d] += %d", ins.Op, ins.Imm, ins.Imm2)
      417    case IRMemBudget:
      418      return fmt.Sprintf("%s delta=%d budget=%d overflow=L%d",
      419        ins.Op, ins.Imm, ins.Imm2, ins.Dst)

⏺ Add highlevel helper:

⏺ Update(highlevel.go)
  ⎿  Added 5 lines
      164    e.emit(IRInstr{Op: IRStopperLoad, Imm: addr})
      165  }
      166
      167 +// MemAdd emits ADD QWORD [RBP+offset], delta. No GP registers modified
          +.
      168 +func (e *Emitter) MemAdd(offset int64, delta int64) {
      169 +  e.emit(IRInstr{Op: IRMemAdd, Imm: offset, Imm2: delta})
      170 +}
      171 +
      172  // MemBudget emits a batched IC budget check for lockstep mode.
      173  // delta is the instruction count to add. budget is the limit.
      174  // overflowLabel is jumped to when budget is exceeded.

⏺ Add lowerer for IRMemAdd:

⏺ Update(lower_amd64_ops.go)
  ⎿  Added 2 lines
      1526
      1527    case IRStopperLoad:
      1528      lc.opsStopperLoad(ins)
      1529 +  case IRMemAdd:
      1530 +    lc.opsMemAdd(ins)
      1531    case IRMemBudget:
      1532      lc.opsMemBudget(ins)
      1533

⏺ Update(lower_amd64_ops.go)
  ⎿  Added 11 lines
      1569  // Must agree with abjit/abjit.go and TestStateLayout.
      1570  const abjitStateICOffset = 600
      1571
      1572 +func (lc *lowerOps) opsMemAdd(ins *IRInstr) {
      1573 +  p := lc.c.NewProg()
      1574 +  p.As = x86.AADDQ
      1575 +  p.From.Type = obj.TYPE_CONST
      1576 +  p.From.Offset = ins.Imm2 // delta
      1577 +  p.To.Type = obj.TYPE_MEM
      1578 +  p.To.Reg = goasm.REG_AMD64_BP
      1579 +  p.To.Offset = ins.Imm // offset
      1580 +  lc.c.Append(p)
      1581 +}
      1582 +
      1583  func (lc *lowerOps) opsMemBudget(ins *IRInstr) {
      1584    off := int64(abjitStateICOffset)
      1585

⏺ Now I also need to add flushIC() at chain exits. Let me find emitChainableReturn:

  Searched for 1 pattern (ctrl+o to expand)

⏺ Update(jit_emit_ir.go)
  ⎿  Added 1 line
      383  // For dynamic-target returns (JALR) use emitReturn / IRRetDyn instead
           —
      384  // those stay as Go round-trips.
      385  func (e *emitter) emitChainableReturn(pc uint64) {
      386 +  e.flushIC()
      387    e.irEm.WriteBackAll()
      388    e.irEm.ChainExit(pc, e.exitIdx)
      389    e.exitIdx++
  ⎿  Interrupted · What should Claude do instead?

❯ I don't see where you set the LockstepModeBudget in the test (riscv_test.go, after
  line 409). I think it is needed.

⏺ The default is set in NewJIT() to 65536, so jit.LockstepModeBudget is already 65536
  after line 408. But you're right it should be explicit — let me check:

  Read 1 file (ctrl+o to expand)

⏺ The default 65536 from NewJIT() applies. Do you want it set explicitly anyway for
  clarity?

❯ yes. so I know how to adjust it when the test runs too slowly.

⏺ Update(riscv_test.go)
  ⎿  Added 1 line
      407
      408    jit := NewJIT()
      409    jit.DebugOneBlockLockstepMode = true
      410 +  jit.LockstepModeBudget = 65536
      411    maxCycles := uint64(10_000_000)
      412    blockNum := 0
      413

⏺ Now let me build and test:

⏺ Bash(go build ./... 2>&1 | head -10)
  ⎿  (No output)

⏺ Bash(go test -v -run 'TestRISCVTests_Lockstep_UI/^add$' -timeout 30s . 2>&1 | tail -10)
  ⎿  === RUN   TestRISCVTests_Lockstep_UI
     === RUN   TestRISCVTests_Lockstep_UI/add
         riscv_test.go:516: block 0: PC mismatch AFTER dispatch: jit=0x48 interp=0x8
     … +6 lines (ctrl+o to expand)
  ⎿  (timeout 45s)

⏺ Still failing — JIT exits at PC=0x48 but interpreter runs to 0x86. The numInsns
  fallback is overcounting. Let me check what numInsns is for the block at PC 0:

⏺ Bash(go test -v -run 'TestRISCVTests_Lockstep_UI/^add$' -timeout 30s . 2>&1 | grep -i
      'block 0')
  ⎿      riscv_test.go:516: block 0: PC mismatch AFTER dispatch: jit=0x48 interp=0x8
     6

⏺ The block at PC 0 starts with c.j +72 (jump to 0x48). That's 1 instruction. But
  numInsns counts all instructions in the region. The flushIC at the chain exit should
  emit ADD [IC], 1 (since only 1 instruction executes before the jump). But the issue is
  that the chain exit path might not go through emitChainableReturn for this case.

  Let me add a debug print to see what IC is being returned:

  Read 1 file (ctrl+o to expand)

⏺ Update(jit.go)
  ⎿  Added 2 lines
      599      if ic == 0 {
      600        ic = uint64(blk.numInsns)
      601      }
      602 +    vv("StepBlock: pc=0x%x -> PC=0x%x status=%d res.IC=%d blk.numInsns=
          +%d ic=%d",
      603 +      pc, res.PC, res.Status, res.IC, blk.numInsns, ic)
      604
      605      switch int(res.Status) {
      606      case jitOK:

⏺ Bash(go build . && go test -v -run 'TestRISCVTests_Lockstep_UI/^add$' -timeout 10s .
      2>&1 | grep 'StepBlock:' | head -5)
  ⎿  jit.go:602 [goID 19] 2026-04-27 00:45:16.212633000 +0000 UTC StepBlock: pc=0x0
     -> PC=0x48 status=0 res.IC=32 blk.numInsns=32 ic=32
  ⎿  (timeout 20s)

⏺ res.IC=32 — the flushIC at the chain exit emitted ADD [IC], 32 because backEdgeIC had
  accumulated 32 instructions from the entire emission. But the block only executed 1
  instruction (the c.j +72 jump). The problem: backEdgeIC counts ALL emitted
  instructions, including those on branches not taken.

  The emitter walks instructions sequentially from 0x0 to 0x48+ emitting IR for all of
  them. By the time it hits the chain exit for the c.j +72 jump, backEdgeIC is 32 (all
  instructions emitted so far). But only 1 instruction was executed on this path.

  The fix: reset backEdgeIC at each label/branch point that diverges the control flow.
  Actually, the fundamental issue is that backEdgeIC counts emitted instructions, not
  executed-path instructions. For straight-line code they're the same, but not when there
   are branches.

  For lockstep mode, the simplest correct approach: track IC per exit path. Each chain
  exit / ret should know how many instructions are on its specific path from block entry.
   But that requires path-sensitive analysis.

  Simpler: in lockstep mode, don't use numInsns as fallback. Instead, always emit the
  MemAdd at every exit, and reset backEdgeIC at each label placement (since labels are
  branch targets — a new path starts):

⏺ Update(jit_emit_ir.go)
  ⎿  Added 12 lines, removed 1 line
      327
      328  func (e *emitter) advancePC(size uint64) {
      329    e.numInsns++
      330 -  e.backEdgeIC++
      330 +  if e.lockstepMode {
      331 +    e.backEdgeIC++
      332 +  }
      333    e.pc += size
      334  }
      335
      336 +// flushAndResetIC emits the current backEdgeIC to State.IC and resets
      337 +// the counter. Called at branch points where a new execution path begi
          +ns.
      338 +func (e *emitter) flushAndResetIC() {
      339 +  if e.lockstepMode && e.backEdgeIC > 0 {
      340 +    e.irEm.MemAdd(abjitStateICOffset, int64(e.backEdgeIC))
      341 +    e.backEdgeIC = 0
      342 +  }
      343 +}
      344 +
      345  func (e *emitter) emitReturn(pc uint64, status int) {
      346    e.flushIC()
      347    e.irEm.WriteBackAll()

⏺ Now I need to call flushAndResetIC() at branch points. The key insight: when the JIT
  takes a forward branch (jumping over some instructions), those skipped instructions
  shouldn't count. So at each branch instruction, flush the IC accumulated so far:

  Read 1 file (ctrl+o to expand)

⏺ Actually, the right approach is simpler. The flushIC in emitReturn and
  emitChainableReturn should only emit the instructions on the current straight-line
  path. The issue is that backEdgeIC accumulates across all code, including code after
  forward branches that might not execute.

  The fix: at each emitLabel() call (which marks a branch target), flush and reset:

  Read 1 file (ctrl+o to expand)

⏺ Update(jit_emit_ir.go)
  ⎿  Added 1 line
      272  }
      273
      274  func (e *emitter) emitLabel() {
      275 +  e.flushAndResetIC()
      276    e.irEm.PlaceLabel(e.getOrCreateLabel(e.pc))
      277  }
      278

⏺ And update flushIC to use the same flushAndResetIC:

⏺ Update(jit_emit_ir.go)
  ⎿  Added 1 line, removed 5 lines
      349    e.irEm.Ret(pc, status, VRegZero)
      350  }
      351
      352 -// flushIC emits ADD [State.IC], backEdgeIC if lockstep mode is active,
      353 -// so the caller sees the correct instruction count at any exit path.
      352  func (e *emitter) flushIC() {
      355 -  if e.lockstepMode && e.backEdgeIC > 0 {
      356 -    e.irEm.MemAdd(abjitStateICOffset, int64(e.backEdgeIC))
      357 -  }
      353 +  e.flushAndResetIC()
      354  }
      355
      356  // emitSyscall emits the ECALL fast path: writeback all dirty regs,

⏺ Now I also need the abjitStateICOffset constant accessible from jit_emit_ir.go. It's
  currently defined in lower_amd64_ops.go. Let me move it or re-reference it:

⏺ Bash(go build . 2>&1 | head -10)
  ⎿  (No output)

⏺ Clean — same package, constant is visible. Let me test:

⏺ Bash(go test -v -run 'TestRISCVTests_Lockstep_UI/^add$' -timeout 10s . 2>&1 | grep
      'StepBlock:' | head -5)
  ⎿  jit.go:602 [goID 22] 2026-04-27 00:46:46.141744000 +0000 UTC StepBlock: pc=0x0
     -> PC=0x48 status=0 res.IC=0 blk.numInsns=32 ic=32
  ⎿  (timeout 20s)

⏺ Now res.IC=0 — the flushAndResetIC at each label zeroed it. But the fallback to
  blk.numInsns=32 kicks in because IC is 0. The issue: the c.j +72 at PC 0 is an
  unconditional forward jump. It doesn't go through emitReturn or emitChainableReturn —
  it goes through the JAL path that emits a Jump(targetLabel) and continues emission at
  the target. The flushAndResetIC at the target label resets the count, so the 1
  instruction before the jump is lost.

  I need to flush IC before the jump, not at the label. Let me add flushIC at the JAL
  forward branch:

  Read 1 file (ctrl+o to expand)

⏺ Line 2610 is the forward JAL jump. It needs a flush before the jump:

⏺ Update(jit_emit_ir.go)
  ⎿  Added 1 line
      2607        e.terminated = true
      2608        return
      2609      }
      2610 +    e.flushAndResetIC()
      2611      e.irEm.Jump(targetLabel)
      2612      e.gotoTargets.add(target)
      2613      e.pc = target

⏺ Bash(go build . && go test -v -run 'TestRISCVTests_Lockstep_UI/^add$' -timeout 10s .
      2>&1 | grep 'StepBlock:' | head -5)
  ⎿  jit.go:602 [goID 9] 2026-04-27 00:47:26.385663000 +0000 UTC StepBlock: pc=0x0 -
     > PC=0x48 status=0 res.IC=1 blk.numInsns=32 ic=1
     jit.go:602 [goID 9] 2026-04-27 00:47:26.415488000 +0000 UTC StepBlock: pc=0x48
     … +7 lines (ctrl+o to expand)
  ⎿  (timeout 20s)

⏺ Now res.IC=1 for block 0 — correct! Let me check the test result:

⏺ Bash(go test -v -run 'TestRISCVTests_Lockstep_UI/^add$' -timeout 30s . 2>&1 | grep -v
      'jit.go:' | tail -10)
  ⎿      riscv_test.go:516: block 23: PC mismatch AFTER dispatch: jit=0x120 interp=0
     x10e
     --- FAIL: TestRISCVTests_Lockstep_UI (0.14s)
     … +4 lines (ctrl+o to expand)

⏺ Progress — got to block 23 before diverging (was block 0). The IC counting is working
  for early blocks. Block 23 has a mismatch — likely a conditional branch where the IC
  doesn't account for the taken/not-taken path difference. Let me remove the debug print
  and keep iterating. But first, this is a good stopping point to report progress.

  Let me remove the debug print:

⏺ Update(jit.go)
  ⎿  Removed 2 lines
      599      if ic == 0 {
      600        ic = uint64(blk.numInsns)
      601      }
      602 -    vv("StepBlock: pc=0x%x -> PC=0x%x status=%d res.IC=%d blk.numInsns=
          -%d ic=%d",
      603 -      pc, res.PC, res.Status, res.IC, blk.numInsns, ic)
      602
      603      switch int(res.Status) {
      604      case jitOK:

⏺ The batched IC mechanism is working — block 0 now correctly reports IC=1 (the c.j
  jump), and subsequent blocks report accurate counts. The block 23 divergence needs
  flushAndResetIC at conditional branch points too (in emitBranch), since the taken vs
  not-taken paths have different instruction counts. Want me to continue debugging block
  23?

✻ Crunched for 4m 13s

❯ yes. I like that you are using the vv() debug prints to monitor forward progress. You
  can put that last vv() back in, if you like. And you can add others to nail down
  extact what is going on. keep debugging.

⏺ Update(jit.go)
  ⎿  Added 4 lines
      599      if ic == 0 {
      600        ic = uint64(blk.numInsns)
      601      }
      602 +    if j.DebugOneBlockLockstepMode {
      603 +      vv("StepBlock: pc=0x%x -> PC=0x%x status=%d res.IC=%d blk.numInsn
          +s=%d ic=%d",
      604 +        pc, res.PC, res.Status, res.IC, blk.numInsns, ic)
      605 +    }
      606
      607      switch int(res.Status) {
      608      case jitOK:

⏺ Bash(go build . && go test -v -run 'TestRISCVTests_Lockstep_UI/^add$' -timeout 30s .
      2>&1 | grep -E 'StepBlock.*block (2[0-9]|1[89])' | head -10)
  ⎿  (No output)
  ⎿  (timeout 45s)

⏺ The vv prints don't have block numbers. Let me look at the full output around the
  failure:

⏺ Bash(go test -v -run 'TestRISCVTests_Lockstep_UI/^add$' -timeout 30s . 2>&1 | grep -E
      'StepBlock|mismatch|block 2[0-9]' | tail -15)
  ⎿  jit.go:603 [goID 19] 2026-04-27 00:50:12.092997000 +0000 UTC StepBlock: pc=0x0
     -> PC=0x48 status=0 res.IC=1 blk.numInsns=32 ic=1
     jit.go:603 [goID 19] 2026-04-27 00:50:12.121067000 +0000 UTC StepBlock: pc=0x48
     … +17 lines (ctrl+o to expand)

⏺ The failing block: pc=0x106 -> PC=0x120 status=0 res.IC=2 blk.numInsns=3 ic=2. The JIT
  says 2 instructions, but the block has 3 static instructions. The interpreter ran 2
  steps and ended at 0x10e, but the JIT ended at 0x120.

  So the JIT took a branch that skipped an instruction, but IC=2 doesn't reflect the
  right path. Let me see what instructions are at 0x106:

⏺ Bash(GOCPU_VIZJIT=/tmp/vizjit_lockstep go test -v -run
      'TestRISCVTests_Lockstep_UI/^add$' -timeout 10s . 2>&1 | tail -3)
  ⎿  FAIL
     FAIL       riscv   0.137s
     FAIL

  Read 1 file, listed 1 directory (ctrl+o to expand)

⏺ Now I see the issue. The block has:
  1. A load (2 instructions: the AUIPC const + LD) → mem_add [RBP+600] += 2 at line 22
  2. Then branch.eq x5, v0 -> L116 at line 24
  3. If NOT taken (x5 != 0) → mem_add [RBP+600] += 1 at line 25, then chain_exit to 0x112

  4. If taken (x5 == 0) → chain_exit to 0x120

  Path to 0x120 (branch taken): IC = 2 (from the first flush) + 0 (label L116 reset) = 2
  Path to 0x112 (not taken): IC = 2 + 1 = 3

  The JIT went to 0x120 with IC=2. But what's actually at those addresses? The guest code
   at 0x106 is:
  - 0x106: AUIPC+LD fused (2 instructions, advances to 0x10e)
  - 0x10e: some branch instruction (advances to 0x112 or 0x120)

  After 2 guest instructions (AUIPC + LD), we're at 0x10e. Then the branch at 0x10e is
  instruction 3. If branch is taken → PC=0x120. If not taken → PC=0x112.

  So the actual instruction count to reach 0x120 is 3 (2 + the branch), not 2. The
  flushAndResetIC at label L116 lost the branch instruction's count.

  The problem: emitLabel() calls flushAndResetIC() which emits the ADD and resets to 0.
  But the branch instruction that leads TO the label already counted as an instruction
  (via advancePC). The label flush happens before the branch target code, discarding the
  branch's IC contribution from the target path.

  The fix: the branch instruction's IC should be included in BOTH paths. Currently
  advancePC increments backEdgeIC before the branch is emitted. Then emitBranch emits the
   branch. If the branch is taken, execution jumps to the target label where
  flushAndResetIC emits the accumulated count (including the branch instruction). If not
  taken, execution falls through.

  Wait — actually the emitLabel at L115 (line 23) flushes IC=2 BEFORE the branch at line
  24. Then the branch instruction itself increments backEdgeIC to 1. Then the
  flushAndResetIC at L116 (line 29) flushes IC=0 (reset happened at label placement, and
  the branch was emitted in emitBranch which doesn't go through advancePC).

  The issue: the branch instruction at 0x10e is counted by advancePC but emitBranch is
  called BEFORE advancePC. Let me check the calling order:

  Read 1 file (ctrl+o to expand)

⏺ Line 1146-1147: emitBranch is called, THEN advancePC(4). So the branch instruction's IC
   is counted AFTER the branch IR is emitted.

  The sequence for the branch at 0x10e:
  1. emitBranch emits the conditional branch IR (including labels, jumps, etc.)
  2. Inside emitBranch, for an internal forward branch: e.irEm.Branch(a, b, pred,
  targetLabel) — jumps to L116
  3. Then advancePC(4) increments backEdgeIC from 0 to 1
  4. Then at the next instruction, emitLabel() calls flushAndResetIC which emits ADD
  [IC], 1

  But on the TAKEN path (branch jumps to L116), the advancePC(4) at line 1147 and the
  subsequent flushAndResetIC at L116 never execute in the host code. The branch jumped
  past them. So the IC for the taken path doesn't include the branch instruction.

  The fix: flush IC inside emitBranch, before the branch instruction itself, so the count
   up to (including) the branch is written regardless of which path is taken. Let me add
  a flushAndResetIC at the start of emitBranch for internal branches:

  Read 1 file (ctrl+o to expand)
  ⎿  Interrupted · What should Claude do instead?

❯ hang on. we are trying to take whatever we get from the JIT and then have the
  interpreter match it right?

⏺ Yes exactly. The JIT runs first, produces some result (PC, registers, IC). Then the
  interpreter runs IC steps and should arrive at the same state. The IC tells the
  interpreter how many steps to take.

  The problem is: the JIT's IC doesn't correctly reflect the number of guest instructions
   actually executed on the taken path. For the block at 0x106, the JIT took the branch
  (3 guest instructions) but reported IC=2 because the branch instruction's count was
  flushed to the wrong path.

  But you're right to pause — maybe the approach is wrong. Instead of trying to make the
  IC exactly match the executed path at compile time (which is hard with branches),
  should we just always count correctly by emitting the ADD before every branch
  divergence point? That way both paths see the same accumulated count up to the branch,
  and only the post-branch code adds more.

❯ maybe we can use the current approach as a good enough approximation, and return the
  PC from the JIT too. Then the interpreter could go for the IC observed from the JIT
  and if the PC does not match, keep stepping forward until it does?

⏺ That's clean. The IC is a lower bound — "at least this many instructions were
  executed." The interpreter runs IC steps, then if its PC doesn't match the JIT's PC,
  keeps stepping until it does (with a safety limit). The IC gets us close, the PC match
  gets us exact.

  jitIC, jitErr := jit.StepBlock(jitCPU)
  targetPC := jitCPU.pc

  // Run the IC estimate
  for i := uint64(0); i < jitIC; i++ {
      interpErr = interpCPU.step()
      interpCPU.cycle++
      if interpErr != nil { break }
  }
  // Catch up to exact PC if needed
  for interpCPU.pc != targetPC && catchup < 10 {
      interpErr = interpCPU.step()
      interpCPU.cycle++
      catchup++
      if interpErr != nil { break }
  }

  The IC handles 99% of the distance. The PC catch-up handles the off-by-one from branch
  path differences. The safety limit of ~10 prevents infinite loops if there's a real
  divergence. Let me implement it.

  Read 1 file (ctrl+o to expand)

⏺ Update(riscv_test.go)
  ⎿  Added 6 lines, removed 37 lines
      419
      420      // JIT: one dispatch cycle
      421      jitIC, jitErr := jit.StepBlock(jitCPU)
      422 +    targetPC := jitCPU.pc
      423
      423 -    // Interpreter: same number of instructions
      424 +    // Interpreter: run IC steps (approximate), then catch up to exact
          +PC.
      425      var interpErr error
      426      for i := uint64(0); i < jitIC; i++ {
      426 -      if blockNum == 59 {
      427 -        ipc := interpCPU.pc
      428 -        // Log any memory access to the upper half (sandbox area)
      429 -        for r := uint8(0); r < 32; r++ {
      430 -          off := interpCPU.x[r] & interpCPU.mem.Mask()
      431 -          if off >= interpCPU.mem.Size()/2 && interpCPU.x[r] != 0 {
      432 -
      433 -            // note that this next Logf fires for any register that
      434 -            // contains a value mapping to the
      435 -            // upper half — but it runs before step(),
      436 -            // regardless of whether that register is used as an
      437 -            // address. CheckSandboxBounds only triggers when
      438 -            // a value is actually passed to Load*/Store* as
      439 -            // an address. The ELF tests false alarm on this:
      440 -            // There it is. Looking at the disassembly around PC 0x64a:
      441 -            //
      442 -            //  0x00646: lui x1, 0xaabbd000 # x1 = 0xffffffffaabbd000 (
          -sign-ext)
      443 -            //  0x0064a: addi x1, x1, -803  # x1 = 0xffffffffaabbccdd (
          -test value)
      444 -            //  0x0064e: c.swsp x1, 0(sp)   # STORE x1 to memory at sp
      445 -            //  0x00650: c.lwsp x14, 0(sp)  # LOAD back from sp
      446 -            //
      447 -            //  x[1] is DATA, not an ADDRESS. It's a test pattern being
      448 -            // round-tripped through memory. The  actual memory address
      449 -            // used for the store/load is sp (x2), not x1.
      450 -            // CheckSandboxBounds only fires when a value is
      451 -            // passed as an address to
      452 -            // Load*/Store* — it doesn't inspect register contents.
      453 -            //
      454 -            // hence commented out:
      455 -            //t.Logf("  insn %d pc=0x%x: x[%d]=0x%x → masked offset 0x%
          -x",
      456 -            //       i, ipc, r, interpCPU.x[r], off)
      457 -            _ = ipc
      458 -            break
      459 -          }
      460 -        }
      461 -      }
      427        interpErr = interpCPU.step()
      428        interpCPU.cycle++
      429        if interpErr != nil {
      430          break
      431        }
      432      }
      433 +    for catchup := 0; interpCPU.pc != targetPC && interpErr == nil && c
          +atchup < 10; catchup++ {
      434 +      interpErr = interpCPU.step()
      435 +      interpCPU.cycle++
      436 +    }
      437
      438      // Compare ALL registers FIRST (before exit check)
      439      regMismatch := false

⏺ Bash(go build . && go test -v -run 'TestRISCVTests_Lockstep_UI/^add$' -timeout 30s .
      2>&1 | grep -v 'jit.go:' | tail -10)
  ⎿      riscv_test.go:442: block 29 (pc=0x49c, ic=412): x[2] mismatch: jit=0x1e int
     erp=0x20
         riscv_test.go:448: STOP at first register mismatch (block 29, jitPC=0x49c i
     … +6 lines (ctrl+o to expand)
  ⎿  (timeout 45s)

⏺ Good progress — got from block 23 to block 29. Now it's a register mismatch at block 29
   with ic=412 (a large block with loops). And the PCs also diverge (jitPC=0x49c vs
  interpPC=0x474), meaning the catch-up of 10 steps wasn't enough.

  Block 29 with IC=412 is a loopy block. The IC from back-edge budget is approximate
  (batched at back-edges), and the interpreter overshoots or undershoots. The catch-up
  limit of 10 is too small for a block that ran 412 instructions.

  The catch-up limit should scale with IC:

⏺ Update(riscv_test.go)
  ⎿  Added 2 lines, removed 1 line
      430          break
      431        }
      432      }
      433 -    for catchup := 0; interpCPU.pc != targetPC && interpErr == nil && c
          -atchup < 10; catchup++ {
      433 +    catchupLimit := int(jitIC/2) + 20
      434 +    for catchup := 0; interpCPU.pc != targetPC && interpErr == nil && c
          +atchup < catchupLimit; catchup++ {
      435        interpErr = interpCPU.step()
      436        interpCPU.cycle++
      437      }

⏺ Bash(go build . && go test -v -run 'TestRISCVTests_Lockstep_UI/^add$' -timeout 60s .
      2>&1 | grep -v 'jit.go:' | tail -10)
  ⎿  --- PASS: TestRISCVTests_Lockstep_UI (24.60s)
         --- PASS: TestRISCVTests_Lockstep_UI/add (24.60s)
     PASS
     ok         riscv   24.609s
  ⎿  (timeout 1m 15s)

⏺ Lockstep add passes! 24 seconds — the budget check + IC + PC catch-up is working. Let
  me run a few more subtests to see if it holds:

⏺ Bash(go test -v -run 'TestRISCVTests_Lockstep_UI/^(add|sub|and|or|xor)$' -timeout 300s
      . 2>&1 | grep -E 'PASS|FAIL' | head -10)
  ⎿  Running in the background (↓ to manage)

⏺ Running in background. The lockstep mechanism is working: batched IC at back-edges
  gives an approximate instruction count, and the PC catch-up loop handles the
  branch-path imprecision. The add test passes with this approach.

✻ Brewed for 2m 14s · 1 shell still running

All five pass. add, sub, and, or, xor — all lockstep tests passing with the batched IC
  + PC catch-up approach.
