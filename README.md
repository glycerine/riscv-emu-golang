emu_net: RISCV64 emulator and network in Golang (Go)
==============

![emu_net](emunet_grid2.png)

* News 2026 June 25: sshd on by default, new rekey utility

The "make linux" guest Alpine Linux OS now starts sshd automatically.

We now work on go1.27rc1 after working around a 
tailscale bug by turning off jsonv2. 
That means doing `GOEXPERIMENT=nojsonv2 go install ./cmd/emu` to build.
https://github.com/tailscale/tailscale/issues/20254 was filed.

The default Makefile target builds a new utility called 'rekey'.
Invoking rekey from the host command line will generate fresh 
keys for the host and ssh login. To ssh into the container,
you would append the following to your host ~/.ssh/config file,
changing the IP address to the IP of the booted node. That is
replacing 100.99.208.124 below with whatever tailnet 100.x.y.z IP the
node says it has after it boots up; do "ifconfig" at the guest
shell to see.

~~~
# addition for your ~/.ssh/config file:
Host emu 100.99.208.124
   Hostname 100.99.208.124
   User root
   IdentityFile ~/.ssh/id_ed25519_emunet
~~~

The rekey binary also allows you to repack the initramfs on windows
without needing a working cpio archive set of tools (which cygwin
_might_ have, but windows does not). This helps if you ever
want to "apk add" to install other apk Alpine packages. 
You need to either "make repack" (on darwin or linux) or 
run rekey to repack on Windows.

The repack will generate new host and user keys. It
writes them into the guest filesystem and to 
your host ~/.ssh/id_ed25519_emunet file, re-packs
the file system initial ramdisk into a initramfs.cpio.gz, then re-builds
emul so you can still start with just a single "emul" afterwards.
The entire container image (bios, kernel, and initramfs.cpio.gz)
is embedded in the emul binary, so it is very portable.

Use "reboot" to exit the guest OS. Since we run
the Alpine /sbin/init to manage sshd, it no longer
suffices to simply exit the guest shell.

* News 2026 June 21:

The emu command now builds on Windows. Tested and working using msys2 for the cgo.

Installing emu with "make" now also installs emul, short for
"emu linux". This does the same thing that the "make linux"
target does, but embeds the kernel in the command line binary
and sets all the flags for you. There are no flags to emul.
You can `go install github.com/glycerine/riscv-emu-golang/cmd/emul@latest`
and type just `emul` to boot a linux guest running inside
your Go process. See cmd/emul/emul.go. This also serves as
a simple example of how to invoke guest Linux programatically.

* News 2026 June 19:

Real local grid network via the local NAT/DHCP works. Multiple
emu on the same local host can communicate with themselves
and the tsnet; so they can reach the internet to install
packages and configure applications before restarting
in hermitic deterministic mode if desired for DST testing.

Idle emu yield to the host OS, so sit at 1% cpu when
doing nothing (under real network / non-deterministic mode).

Our interpeter's floating point implimentation is verified green on 
John R. Hauser's Berkeley TestFloat/SoftFloat test suite;
https://www.jhauser.us/arithmetic/TestFloat.html
which is vendored in. "make test-softfloat" to run.

* News 2026 June 17:

As a verification of robustness, the CPU emulator can now boot a regular (e.g. Ubuntu) Linux kernel. See the "make linux" target. 

* News 2026 June 16: 

v0.1.0 and beyond emulate a deterministic Linux OS "personality" and thus
gives deterministic simulation testing (DST) a proper fighting chance.
It is not really Linux; it just implements the Linux system calls ABI that
a compiled Go program needs to run; kind of like gvisor. It has stacked
note handling inspired by plan 9.

We are only about 40x slower than actual hardware. The `emu` command
line tool will run your RISCV64 ELF binary inside the DST sandbox.
It is single threaded, and all entropy and randomness and scheduling can be controlled.
You could think of `emu` as "a miniature Antithesis" in one little 
8MB command line tool. It can run compiled Go code.

Also we support JIT-to-ARM64 now too, in addition to JIT to AMD64.

Project Summary
---------------

This repository is a performance-oriented RV64 emulator and native JIT
for amd64/x86_64. It includes a RISC-V CPU interpreter in pure Go. That is, you can run
RV64 binaries and instructions from within your Go programs. 

Inspiration: https://github.com/libriscv/libriscv (a RISC-V emulator in C++).

Specifically, cpu.go is an RV64IMAFDC with Zicsr, Zifencei, Zicond, 
and bitmanip extensions Zba/Zbb/Zbc/Zbs.

The A, F, D, C, and B-family extensions are implemented for RV64; RV128 is not supported.

Zicsr is implemented for the CSRs this emulator cares about, 
not as a full privileged-architecture model. The atomics are processed but
are no-ops.

--------
Author: Jason E. Aten, Ph.D.

License: BSD 3-Clause, same as Go.

---------
![the emu!](emu_head_small.png)

## notes

note: macOS uses a different RISC-V toolchain from linux.

### Prerequisites (one-time)

brew install riscv64-elf-gcc cmake

### Extract

tar -xzf riscv-emulator.tar.gz
cd riscv

### Unit tests — works immediately

go test -v ./...

### One-time setup (clones libriscv ~400MB, builds static libs, compiles guest ELF)

make bench-setup

### Full benchmark comparison

make bench

### Quick targets (no libriscv needed)

make bench-ours       ### our GuestMemory benchmarks only

make test             ### unit tests

### benchmark results 2026 April 20

JIT-ed, we are ballpark of libriscv now. Sometimes a little slower
slower, sometimes faster. We are not measuring system call latency.
This is a pure compute benchmarks at the moment.

* linux: make bench

~~~
$ make bench

══════════════════════════════════════════════════════════════════
  JIT ALLOCATOR COMPARISON — 2026-04-20 16:29  [linux]
  cpu: AMD Ryzen Threadripper 3960X 24-Core Processor
══════════════════════════════════════════════════════════════════

  Strategy                                     MIPS
  ──────────────────────────────────────────── ──────────
  Go interpreter (no JIT):                     407.8 MIPS
  Go JIT — ELS allocator (native):           4189 MIPS
  Go JIT — Fixed Static Mapping (native):    3987 MIPS
  Go JIT — TCC backend:                      2215 MIPS
  libriscv — JIT (TCC):                      2960 MIPS
  libriscv — interpreter (no JIT):           843.6 MIPS
  native x86-64 (-O3 -march=native):           22954 MIPS  (110.0 ms)
~~~

### darwin: make bench

~~~
$ make bench

══════════════════════════════════════════════════════════════════
  JIT ALLOCATOR COMPARISON — 2026-04-20 18:29  [macos]
  cpu: Intel(R) Core(TM) i7-1068NG7 CPU @ 2.30GHz
══════════════════════════════════════════════════════════════════

  Strategy                                     MIPS
  ──────────────────────────────────────────── ──────────
  Go interpreter (no JIT):                     430.5 MIPS
  Go JIT — ELS allocator (native):           3316 MIPS
  Go JIT — Fixed Static Mapping (native):    3357 MIPS
  Go JIT — TCC backend:                      1966 MIPS
  libriscv — JIT (TCC):                      3562 MIPS
  libriscv — interpreter (no JIT):           569.8 MIPS
  native x86-64 (-O3 -march=native):           18035 MIPS  (140.0 ms)
~~~

latest

~~~
Profile-driven optimization pass is complete. Summary:

Results (benchstat, 20 samples, same machine state):

┌─────────────┬───────────────┬────────────┬────────┬───────┐
│    bench    │ Phase 2b base │ Opt 1+2+3  │   Δ    │   p   │
├─────────────┼───────────────┼────────────┼────────┼───────┤
│ coremark    │    886.4 MIPS │ 938.1 MIPS │ +5.84% │ 0.001 │
├─────────────┼───────────────┼────────────┼────────┼───────┤
│ dhrystone   │         759.2 │      785.5 │ +3.46% │ 0.000 │
├─────────────┼───────────────┼────────────┼────────┼───────┤
│ bench_guest │          3233 │       3264 │  +1.0% │ 0.180 │
└─────────────┴───────────────┴────────────┴────────┴───────┘

What landed (user committed as ddaed1c + f5ec0ee):
- Opt 1 soleSegment fast-path: skip findSegment + blk.segment null-chain when len(aotSegments)==1
- Opt 2 hygiene: drop the redundant defensive findSegment inside nextExecuteSegment
- Opt 3 pre-computed vaddrSize on DecodedExecuteSegment — removes vaddrEnd-vaddrBegin subtraction per CallAOT

ExecRegion finesse (your question): The check is only on block-cache miss, not the steady-state hot path — so for coremark's tight inner loop it contributes ~0 MIPS. Opt 2 still cleaned up the redundant re-scan inside nextExecuteSegment.

Regression sweep: root tests / ir / bench / chain-ref all green. fuzz-oracle / fuzz-rvc / fuzz-amo / fuzz-bitmanip 60s each — all PASS. fuzz-fd fails on a pre-existing NaN-payload seed corpus entry unrelated to this work (verified by running against HEAD~3; same failure).

Coremark reached 938 MIPS — within 0.2% of the ≥940 target and within 2.2% of Phase 2a's 959. Bonus win on dhrystone.


~/ris (master) $ go test -v -run=xxx -bench='^BenchmarkJITAOT_' -benchtime=1x ./bench/
goos: darwin
goarch: amd64
pkg: riscv/bench
cpu: Intel(R) Core(TM) i7-1068NG7 CPU @ 2.30GHz
BenchmarkJITAOT_CoreMark
BenchmarkJITAOT_CoreMark-8     	       1	 379747218 ns/op	      1026 MIPS	 1839696 B/op	    1129 allocs/op
BenchmarkJITAOT_Dhrystone
BenchmarkJITAOT_Dhrystone-8    	       1	 340511799 ns/op	       828.2 MIPS	  210296 B/op	      55 allocs/op
BenchmarkJITAOT_BenchGuest
BenchmarkJITAOT_BenchGuest-8   	       1	 750860305 ns/op	      3363 MIPS	   15016 B/op	      10 allocs/op
PASS
ok  	riscv/bench	1.621s

~~~

### libriscv assembly dumps to debug_libriscv_dir

~~~
The libriscv dump facility is implemented and working end-to-end.

  What landed:

  - New files: xendor/libriscv/lib/libriscv/tr_dump.{hpp,cpp} — env-gated diagnostic dumper
  - Edited: tr_translate.cpp (two hooks), lib/CMakeLists.txt, bench/hellobench/main.go, jit_vizjit.go
  (exported GetVizJitTag), .gitignore

  Output format — mirrors GoCPU's VizJit, one file per block:
  ~/ris/debug_libriscv_dir/<tag>.libriscv.asm.pc_0x<basepc:08x>.asm
  with sections:
  1. Header (run tag, entry PC, byte range, symbol)
  2. == Guest RISC-V == — raw hex per instruction
  3. == libriscv bintr C == — the f_<pc> function body extracted from the generated C
  4. == Host x86-64 (from TCC) == — Intel-syntax disassembly via llvm-mc, trimmed at the last ret

  Usage — single-switch activation:
  GOCPU_VIZJIT=~/ris/debug_vizjit_dir \
    go run -tags libriscv ./bench/hellobench/ -only=libriscv
  hellobench auto-sets LIBRISCV_DUMP_DIR to a sibling path and propagates GoCPU's 16-hex run tag, so
  diff ~/ris/debug_vizjit_dir/<tag>.gocpu.asm.pc_<X>.asm
  ~/ris/debug_libriscv_dir/<tag>.libriscv.asm.pc_<X>.asm just works.

  Zero cost when LIBRISCV_DUMP_DIR is unset — TestLibriscvSmokeTest still passes unchanged.
~~~

wedged. rolled back. on rv8inspired branch now, a bit slower
after some of this. but tests were faster for a while; top level was 25 sec,
now back to 67s.
  
~~~
with cgo, on darwin now:

ok  	riscv	119.905s  (about 2x slow down from pre-CGO and smaller memory sizes)
ok  	riscv/ir	0.323s
ok  	riscv/bench	18.235s

darwin benchmarks:

  Go JIT — Fixed Static Mapping (native):    3172 MIPS
  Go interpreter (no JIT):                    427 MIPS

linux:  cpu: AMD Ryzen Threadripper 3960X 24-Core Processor

  Go JIT — Fixed Static Mapping (native):    3394 MIPS
  Go interpreter (no JIT):                     391.6 MIPS

still on default rv8, about to compare to ABJIT

  cpu: Intel(R) Core(TM) i7-1068NG7 CPU @ 2.30GHz
══════════════════════════════════════════════════════════════════

  Strategy                                     MIPS
  ──────────────────────────────────────────── ──────────
  Go JIT — Fixed Static Mapping (native):    3415 MIPS
  Go interpreter (no JIT):                     461.7 MIPS
  libriscv — JIT (TCC):                      3842 MIPS
  libriscv — interpreter (no JIT):           902.0 MIPS
  native x86-64 (-O3 -march=native):           18035 MIPS  (140.0 ms)
  wazero wasm aot-and-run                    8628.9 MIPS

~~~

######### performance note

the main client caller should do

`runtime.LockOSThread()` before invoking the emulator.
This pins the goroutine to one thread and thus avoids
re-scheduling and keep caches hot.

also scheduler affinity.

~~~
$ make bench

darwin

  Go JIT — rv8 Fixed Static Mapping (native): 3462 MIPS
  Go JIT — abjit (native):                    3367 MIPS
  Go interpreter (no JIT):                     464.7 MIPS

linux
cpu: AMD Ryzen Threadripper 3960X 24-Core Processor

  Go JIT — rv8 Fixed Static Mapping (native): 3218 MIPS
  Go JIT — abjit (native):                    3348 MIPS
  Go interpreter (no JIT):                     378.8 MIPS
  libriscv — JIT (TCC):                       3409 MIPS
  libriscv — interpreter (no JIT):            795.8 MIPS
  native x86-64 (-O3 -march=native):         21041 MIPS  (120.0 ms)
  wazero wasm aot-and-run                    18384.9 MIPS
~~~

~~~
make bench-cpu

goos: darwin
goarch: amd64
pkg: riscv/bench
cpu: Intel(R) Core(TM) i7-1068NG7 CPU @ 2.30GHz

BenchmarkCPU_FullExecution_JIT_Rv8-8     	       1	 837820510 ns/op	      3014 MIPS	 2369224 B/op	   17590 allocs/op
BenchmarkCPU_FullExecution_JIT_ABJIT-8   	       1	 829903924 ns/op	      3042 MIPS	 2356184 B/op	   17311 allocs/op

goos: linux
goarch: amd64
pkg: riscv/bench
cpu: AMD Ryzen Threadripper 3960X 24-Core Processor 

BenchmarkCPU_FullExecution_JIT_Rv8-48                  1         985023960 ns/op
              2563 MIPS  2389736 B/op      17726 allocs/op
BenchmarkCPU_FullExecution_JIT_ABJIT-48                1         843590039 ns/op
              2993 MIPS  2374776 B/op      17441 allocs/op
~~~

without any fake IC 

~~~
for 2524935201 riscv instuctions:

darwin 9399 MIPS (269 msec) 
Linux  7129 MIPS (354 msec)
~~~

------
~~~
PLAN105_test_lazy_jit_and_aot.md

Plan: Close RISC-V Test ELF Coverage Gap (Lazy JIT + AOT)

Context

The standard RISC-V test ELFs (rv64ui, rv64um, rv64ua, rv64uc) are run through three
 modes, but there's a gap:

┌──────────────────────────────┬───────────────────┬───────────────────────────────┐
│          Test group          │      Runner       │             Mode              │
├──────────────────────────────┼───────────────────┼───────────────────────────────┤
│ TestRISCVTests_UI etc.       │ RunWithOS         │ Interpreter                   │
├──────────────────────────────┼───────────────────┼───────────────────────────────┤
│ TestRISCVTests_UI_JIT etc.   │ runJITWithOS →    │ AOT (auto-installed by        │
│                              │ RunJIT            │ RunJIT:715)                   │
├──────────────────────────────┼───────────────────┼───────────────────────────────┤
│ TestRISCVTests_Lockstep_UI   │ StepBlock loop    │ Lazy JIT (per-block, no       │
│ etc.                         │                   │ RunJIT dispatch)              │
└──────────────────────────────┴───────────────────┴───────────────────────────────┘

Missing: Lazy JIT via RunJIT (DisableAutoAOT=true). This exercises the full RunJIT
dispatch loop with lazy compilation — the 2-slot JALR IC, lazy block cache, and
interpreter fallback. The lockstep tests use StepBlock (single-step), not RunJIT.

Changes

1. Add runRISCVTestJITLazy helper (riscv_test.go)

Clone runRISCVTestJIT but set DisableAutoAOT=true:

func runRISCVTestJITLazy(t *testing.T, elfPath string) {
    // Same as runRISCVTestJIT but:
    jit := NewJIT()
    jit.DisableAutoAOT = true
    // ... install OS, RunJIT, check exit code ...
}

2. Add TestRISCVTests_*_JIT_Lazy test functions (riscv_test.go)

For each active instruction category (UI, UM, UA, UC):

func TestRISCVTests_UI_JIT_Lazy(t *testing.T) { ... runRISCVTestJITLazy ... }
func TestRISCVTests_UM_JIT_Lazy(t *testing.T) { ... }
func TestRISCVTests_UA_JIT_Lazy(t *testing.T) { ... }
func TestRISCVTests_UC_JIT_Lazy(t *testing.T) { ... }

UF and UD remain skipped (fflags issue, same as existing _JIT tests).

3. Rename existing _JIT tests for clarity (optional)

The existing _JIT tests are actually AOT. Renaming to _JIT_AOT makes the coverage
matrix self-documenting. This is optional — the user may prefer to keep the names
stable.

Coverage Matrix After Changes

┌────────────────┬─────────────┬────────────────┬─────────────┬───────────────────┐
│      Test      │ Interpreter │   Lazy JIT     │    AOT      │     Lockstep      │
│                │             │    (RunJIT)    │  (RunJIT)   │    (StepBlock)    │
├────────────────┼─────────────┼────────────────┼─────────────┼───────────────────┤
│ UI (integer)   │     yes     │      new       │     yes     │        yes        │
├────────────────┼─────────────┼────────────────┼─────────────┼───────────────────┤
│ UM (mul/div)   │     yes     │      new       │     yes     │        yes        │
├────────────────┼─────────────┼────────────────┼─────────────┼───────────────────┤
│ UA (atomics)   │     yes     │      new       │     yes     │        yes        │
├────────────────┼─────────────┼────────────────┼─────────────┼───────────────────┤
│ UC             │     yes     │      new       │     yes     │        yes        │
│ (compressed)   │             │                │             │                   │
├────────────────┼─────────────┼────────────────┼─────────────┼───────────────────┤
│ UF (float)     │     yes     │      skip      │    skip     │       skip        │
├────────────────┼─────────────┼────────────────┼─────────────┼───────────────────┤
│ UD (double)    │     yes     │      skip      │    skip     │       skip        │
└────────────────┴─────────────┴────────────────┴─────────────┴───────────────────┘

Performance Comparison

The lazy tests will naturally be slower than AOT (every JALR goes through 2-slot IC
or Go round-trip vs decoder cache). The test log can report timing to quantify the
gap without a separate benchmark.

Verification

cd ~/ris && go test -v -run 'TestRISCVTests_UI_JIT_Lazy' .
cd ~/ris && go test -v -run 'TestRISCVTests_U._JIT_Lazy' .
cd ~/ris && go test -count=1 .  ### full suite, no regressions

Critical Files

┌───────────────┬──────────────────────────────────────────────────────────┐
│     File      │                          Change                          │
├───────────────┼──────────────────────────────────────────────────────────┤
│ riscv_test.go │ Add runRISCVTestJITLazy + 4 new _JIT_Lazy test functions │
└───────────────┴──────────────────────────────────────────────────────────┘

### outcome after implementing:

Interesting — lazy JIT is faster, not slower:

┌──────────┬────────────┬──────────────────┬─────────────┐
│ Category │ AOT (_JIT) │ Lazy (_JIT_Lazy) │    Ratio    │
├──────────┼────────────┼──────────────────┼─────────────┤
│ UI       │      27.6s │             1.5s │  18x faster │
├──────────┼────────────┼──────────────────┼─────────────┤
│ UM       │       3.8s │            0.15s │  25x faster │
├──────────┼────────────┼──────────────────┼─────────────┤
│ UA       │      28.1s │            0.19s │ 148x faster │
├──────────┼────────────┼──────────────────┼─────────────┤
│ UC       │      0.72s │            0.03s │  24x faster │
└──────────┴────────────┴──────────────────┴─────────────┘

The AOT tests use Size1MB memory, and the AOT compilation overhead (scanning +
compiling all blocks upfront for each tiny test ELF) dominates. The lazy path only
compiles blocks as they're reached — much less work for these small test binaries.

All four _JIT_Lazy test suites pass with all subtests green. The coverage gap is
closed: RISC-V test ELFs now run through interpreter, AOT JIT, lazy JIT (RunJIT), and
lockstep.
~~~

with IC instruction counter back. We use R15 for this.

Update: we shifted to R15 having a count-down (budget) of
remaining guest RISCV64 instructions still available;
so that we can adjust the budget without re-compiling the
JIT-ed to native code. With count-up, the limit is
baked into the JIT-ed code. With count-down, we know
to stop at 0 (or slightly before for fused native instructions).
A special return code handles fused instructions which
can require fallback to interpreter single steps if a
budget would split/land in the middle of a fused instruction.
~~~
(goivy-venv) jaten@jbook ~/ris (master) $ make bench

══════════════════════════════════════════════════════════════════
  JIT COMPARISON (rv8 vs abjit) — 2026-04-28 02:22  [macos]
  cpu: Intel(R) Core(TM) i7-1068NG7 CPU @ 2.30GHz
══════════════════════════════════════════════════════════════════

  Strategy                                     MIPS
  ──────────────────────────────────────────── ──────────
  Go JIT — rv8 Fixed Static Mapping (native): 3853 MIPS
  Go JIT — abjit (native):                   3824 MIPS
  Go interpreter (no JIT):                     475.2 MIPS
  libriscv — JIT (TCC):                      3927 MIPS
  libriscv — interpreter (no JIT):           930.8 MIPS
  native x86-64 (-O3 -march=native):           18035 MIPS  (140.0 ms)
  wazero wasm aot-and-run                    9008.0 MIPS
~~~

~~~
latest test suite at 43e5e8810e6df1cd607dc703a8b15790f418a494

atg

darwin ok  	riscv	324.063s
linux: ok   riscv   380.452s
~~~

# run with a different idle sleep than the default 1ms

When guest OS Linux wants to low-power idle sleep, by
default we sleep on the Go side for 1 millisecond. But here one
can experiment with 25ms
~~~
make linux EMU_IDLE='-idle 25ms'
~~~
To turn off WFI sleep all together, you can do:
~~~
make linux EMU_IDLE='-idle 0'
~~~

# installing apks from the network and having them persist across reboots

Inside the guest alpine linux, having 
booted up with "make linux", to install 
local apks and also update apks on host
for next boot:
~~~
ROOT=/host/Users/jaten/ris/xendor/alpine-minirootfs-3.24.1-riscv64
APKDIR=/host/Users/jaten/ris/xendor/linux/alpine-nettools/apks

apk add --root "$ROOT" --no-network \
  "$APKDIR/libcap2-2.78-r0.apk" \
  "$APKDIR/zstd-libs-1.5.7-r2.apk" \
  "$APKDIR/libelf-0.195-r0.apk" \
  "$APKDIR/libmnl-1.0.5-r2.apk" \
  "$APKDIR/iproute2-minimal-7.0.0-r0.apk"
~~~

verify, still in guest:

~~~
apk --root "$ROOT" info -e iproute2-minimal
apk --root "$ROOT" info -W /sbin/ip
~~~

after that, on host, repack; this is archived in git repo.

~~~
cd ~/ris && make repack-initramfs
~~~

## windows notes

in guest linux, to use DNS 1.1.1.1 instead of 100.100.100.100
which is the tailscale dns that was having trouble with IPV6 AAAA lookups, try:
~~~
# printf 'nameserver 1.1.1.1\n' > /etc/resolv.conf
~~~
