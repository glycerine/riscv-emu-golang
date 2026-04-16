2026 April 16

A few weeks ago Aaron Balke wrote this post describing his JIT call strategy: I have made a
local copy for you to review reference/aabalke_gojit_Revived_Jit_Compiler_in_Go.html . How is his
strategy different from ours? Does it have advantages or disadvantages compared to ours. I think ours
 avoids ever having a direct call from the JIT compiled code call into Go code, and thus allows Go
calls to be of arbitrary nature rather than requiring nosplit.

⏺ Great read. Here's the comparison:

  Balke's strategy (gojit)

  - Code generation: Emits x86-64 machine code bytes directly (no C layer)
  - JIT calls Go: Yes, via a gocall label embedded inside the Go asm trampoline. The JIT code pushes a
   fake return address onto SP, then JMPs to the gocall label which does CALL CX; JMP (SP). This
  tricks the Go runtime into believing the call came from a real Go function, satisfying stack-shape
  expectations
  - ABI plumbing required:
    - callJITImplAddr to get the ABI0 address (bypassing the ABIInternal wrapper the Go compiler
  inserts)
    - Runtime byte-scan of the trampoline to find the offset of CALL CX (varies by Go version/OS)
    - NO_LOCAL_POINTERS + careful frame size accounting
  - Restrictions on callee: Called Go functions must behave well with a spoofed frame. Anything that
  deeply inspects the stack (certain signal handlers, some GC paths) can still hit "unexpected return
  pc"

  Our strategy (ris)

  - Code generation: Emit C source, TCC compiles to native
  - JIT calls Go: Never. Full stop.
  - How we handle Go-side work: The block returns a status code struct. The Go dispatch loop (RunJIT /
   StepBlock) handles it:
    - jitEcall / jitEbreak → deliver through NoteChain
    - jitLoadFault / jitStoreFault → deliver fault note
    - Unknown status / untranslatable instruction → interpreter fallback
  - Calling external C functions: Yes — tcc_add_symbol injects plain C function pointers (sqrtf, sqrt,
   potentially fflags capture). These are not Go functions; they're ordinary System V ABI C functions.
   No Go runtime involvement.

  Differences

  ┌──────────────────────────┬─────────────────────────┬──────────────────────────────────────────┐
  │          Aspect          │          Balke          │                    Us                    │
  ├──────────────────────────┼─────────────────────────┼──────────────────────────────────────────┤
  │ Code-gen target          │ raw bytes               │ C → TCC                                  │
  ├──────────────────────────┼─────────────────────────┼──────────────────────────────────────────┤
  │ JIT → Go call            │ Yes, via gocall trick   │ Never                                    │
  ├──────────────────────────┼─────────────────────────┼──────────────────────────────────────────┤
  │ ABI0/ABIInternal         │ Required                │ N/A                                      │
  │ handling                 │                         │                                          │
  ├──────────────────────────┼─────────────────────────┼──────────────────────────────────────────┤
  │ Restrictions on Go       │ Must survive spoofed    │ None — arbitrary Go code                 │
  │ callees                  │ frame                   │                                          │
  ├──────────────────────────┼─────────────────────────┼──────────────────────────────────────────┤
  │ Go version sensitivity   │ High (label offset      │ Low (no Go call at all)                  │
  │                          │ scan)                   │                                          │
  ├──────────────────────────┼─────────────────────────┼──────────────────────────────────────────┤
  │ Cold compilation cost    │ Fast (copy bytes)       │ ~1ms (TCC compile)                       │
  ├──────────────────────────┼─────────────────────────┼──────────────────────────────────────────┤
  │ Dependencies             │ None                    │ libtcc.a vendored                        │
  ├──────────────────────────┼─────────────────────────┼──────────────────────────────────────────┤
  │ Portability to non-x86   │ Hand-port each ISA      │ C is portable (TCC is x86-specific       │
  │                          │                         │ though)                                  │
  ├──────────────────────────┼─────────────────────────┼──────────────────────────────────────────┤
  │ GC safety                │ Delicate                │ Trivial — JIT block is a pure compute    │
  │                          │                         │ function                                 │
  └──────────────────────────┴─────────────────────────┴──────────────────────────────────────────┘

  Your intuition is correct

  Because we never call into Go from JIT code, our Go-side handlers can do anything a normal Go
  function can do:
  - Allocate on the heap (our NoteChain handlers do this)
  - Call Go APIs, block on channels, spawn goroutines
  - Grow the stack
  - Use panic/recover (our runJITWithOS does — for ExitError)

  Balke's gojit has to keep callee Go functions tight and NOSPLIT-friendly. Our cpu.Notes.Deliver,
  o.Handle, LinuxExit are completely unconstrained because they're only ever called from regular Go
  dispatch code — the JIT never reaches them directly.

  Trade-offs

  We pay:
  - TCC compile latency per new block (~1ms)
  - Each Go-side interaction requires a block exit/re-entry (dispatch overhead, last-block cache
  mitigates)
  - TCC's C dialect limitations (no __int128, no libc — we hit these for MULH and fprintf)

  We gain:
  - No ABI fragility across Go versions
  - No byte-scanning at init time
  - Full C language in the hot path (register caching as locals, structured control flow)
  - Go callees are first-class Go code with no caveats

  For an emulator, the block exit/re-entry cost is small because blocks are short and the last-block
  cache catches the re-entry in ~50 ns. For a use case needing frequent Go callbacks mid-block (e.g.,
  memory-mapped I/O hooks with complex Go logic), Balke's approach might win on latency. For us
  (RISC-V with mostly-pure memory access through a mask), the return-and-redispatch pattern is cleaner
   and faster per-block than paying ABI coordination costs every call.

  One thing worth noting: our approach could steal Balke's technique if we ever needed to call
  arbitrary Go functions from JIT (e.g., for a debugger or tracing API) — TCC can emit a call rax just
   as easily as direct code generation. But for the emulator's hot path, we don't need it.
