package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"strings"
	"testing"
	"time"

	riscv "github.com/glycerine/riscv-emu-golang"
)

func TestRunEmuxDefaultRunsGoHelloFixture(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code, err := runEmux(EmuxConfig{
		RunPath:           "../../testvectors/jea9linux/go/elf/hello.elf",
		MemorySize:        riscv.Size16GB,
		InstructionBudget: 1 << 20,
		Stdin:             strings.NewReader(""),
		Stdout:            &stdout,
		Stderr:            &stderr,
	})
	if err != nil {
		t.Fatalf("runEmux: %v; stderr=%q", err, stderr.String())
	}
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%q", code, stderr.String())
	}
	if got, want := stdout.String(), "hello jea9linux go\n"; got != want {
		t.Fatalf("stdout = %q, want %q; stderr=%q", got, want, stderr.String())
	}
}

func TestRunEmuxReturnsGuestExitCodeAndStderr(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code, err := runEmux(EmuxConfig{
		RunPath:           "../../testvectors/jea9linux/go/elf/nilpanic.elf",
		MemorySize:        riscv.Size16GB,
		InstructionBudget: 1 << 20,
		Stdin:             strings.NewReader(""),
		Stdout:            &stdout,
		Stderr:            &stderr,
	})
	if err != nil {
		t.Fatalf("runEmux: %v; stdout=%q stderr=%q", err, stdout.String(), stderr.String())
	}
	if code != 2 {
		t.Fatalf("exit code = %d, want 2; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "panic: runtime error") {
		t.Fatalf("stderr = %q, want Go panic text", stderr.String())
	}
}

func TestRunEmuxSeedControlsGetrandom(t *testing.T) {
	first := runEmuxFixtureOutput(t, 1234)
	second := runEmuxFixtureOutput(t, 1234)
	third := runEmuxFixtureOutput(t, 5678)

	if first != second {
		t.Fatalf("same seed output differs: %q != %q", first, second)
	}
	if first == third {
		t.Fatalf("different seeds produced matching output: %q", first)
	}
}

func TestRunEmuxJea9LinuxFixtureModes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		jitlazy bool
		jitaot  bool
	}{
		{name: "interpreter"},
		{name: "lazy-jit", jitlazy: true},
		{name: "aot-jit", jitaot: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code, err := runEmux(EmuxConfig{
				RunPath:           "../../testvectors/jea9linux/elf/write_stdout.elf",
				MemorySize:        riscv.Size64MB,
				InstructionBudget: 1 << 20,
				JITLazy:           tc.jitlazy,
				JITAOT:            tc.jitaot,
				Stdin:             strings.NewReader(""),
				Stdout:            &stdout,
				Stderr:            &stderr,
			})
			if err != nil {
				t.Fatalf("runEmux: %v; stderr=%q", err, stderr.String())
			}
			if code != 0 {
				t.Fatalf("exit code = %d, want 0; stderr=%q", code, stderr.String())
			}
			if got, want := stdout.String(), "jea9linux stdout\n"; got != want {
				t.Fatalf("stdout = %q, want %q", got, want)
			}
		})
	}
}

func TestEmuxJITFlagsAreMutuallyExclusive(t *testing.T) {
	cfg := EmuxConfig{
		RunPath: "../../testvectors/jea9linux/elf/write_stdout.elf",
		JITLazy: true,
		JITAOT:  true,
	}
	if err := cfg.ValidateConfig(); err == nil {
		t.Fatal("ValidateConfig accepted both -jitlazy and -jitaot")
	}
}

func TestEmuxDefaultFlagsRunGoTimeNowFixtureCompletes(t *testing.T) {
	cfg, stdout, stderr := parseEmuxConfigForTest(t,
		"-run", "../../testvectors/jea9linux/go/elf/timenow.elf",
	)

	type result struct {
		code int
		err  error
	}
	done := make(chan result, 1)
	go func() {
		code, err := runEmux(cfg)
		done <- result{code: code, err: err}
	}()

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("runEmux: %v; stdout=%q stderr=%q", got.err, stdout.String(), stderr.String())
		}
		if got.code != 0 {
			t.Fatalf("exit code = %d, want 0; stdout=%q stderr=%q", got.code, stdout.String(), stderr.String())
		}
		if strings.TrimSpace(stdout.String()) == "" {
			t.Fatalf("stdout is empty; stderr=%q", stderr.String())
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("default emux timenow run did not complete; stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestEmuxConfigDefaultsPreserveExplicitZeroClock(t *testing.T) {
	cfg := EmuxConfig{}.withDefaults()
	if cfg.MemorySize != defaultEmuxMemorySize {
		t.Fatalf("MemorySize = %d, want %d", cfg.MemorySize, defaultEmuxMemorySize)
	}
	if cfg.InstructionBudget != defaultEmuxInstructionBudget {
		t.Fatalf("InstructionBudget = %d, want %d", cfg.InstructionBudget, defaultEmuxInstructionBudget)
	}
	if cfg.ClockMode != defaultEmuxClockMode {
		t.Fatalf("ClockMode = %q, want %q", cfg.ClockMode, defaultEmuxClockMode)
	}
	if cfg.MonotonicStartNS != defaultEmuxMonotonicStartNS {
		t.Fatalf("MonotonicStartNS = %d, want %d", cfg.MonotonicStartNS, defaultEmuxMonotonicStartNS)
	}

	explicitZero := EmuxConfig{MonotonicStartSet: true}.withDefaults()
	if explicitZero.MonotonicStartNS != 0 {
		t.Fatalf("explicit MonotonicStartNS = %d, want zero preserved", explicitZero.MonotonicStartNS)
	}
}

func TestParseEmuxJITModeFlags(t *testing.T) {
	lazy, _, _ := parseEmuxConfigForTest(t,
		"-run", "../../testvectors/jea9linux/elf/write_stdout.elf",
		"-jitlazy",
	)
	if !lazy.JITLazy || lazy.JITAOT {
		t.Fatalf("-jitlazy parsed as JITLazy=%v JITAOT=%v", lazy.JITLazy, lazy.JITAOT)
	}

	aot, _, _ := parseEmuxConfigForTest(t,
		"-run", "../../testvectors/jea9linux/elf/write_stdout.elf",
		"-jitaot",
	)
	if !aot.JITAOT || aot.JITLazy {
		t.Fatalf("-jitaot parsed as JITLazy=%v JITAOT=%v", aot.JITLazy, aot.JITAOT)
	}

	interp, _, _ := parseEmuxConfigForTest(t,
		"-run", "../../testvectors/jea9linux/elf/write_stdout.elf",
	)
	if interp.JITLazy || interp.JITAOT {
		t.Fatalf("default parsed as JITLazy=%v JITAOT=%v", interp.JITLazy, interp.JITAOT)
	}
	if interp.AllowAllHostFiles {
		t.Fatal("default parsed with AllowAllHostFiles enabled")
	}

	allhost, _, _ := parseEmuxConfigForTest(t,
		"-run", "../../testvectors/jea9linux/elf/write_stdout.elf",
		"-allhost",
	)
	if !allhost.AllowAllHostFiles {
		t.Fatal("-allhost did not enable AllowAllHostFiles")
	}
}

func BenchmarkRunEmuxGoHelloInterpreter(b *testing.B) {
	benchmarkRunEmuxGoHello(b, EmuxConfig{})
}

func BenchmarkRunEmuxGoHelloLazyJIT(b *testing.B) {
	benchmarkRunEmuxGoHello(b, EmuxConfig{JITLazy: true})
}

func BenchmarkRunEmuxGoHelloAOTJIT(b *testing.B) {
	benchmarkRunEmuxGoHello(b, EmuxConfig{JITAOT: true})
}

func benchmarkRunEmuxGoHello(b *testing.B, mode EmuxConfig) {
	b.Helper()
	b.ReportAllocs()
	var totalStats EmuxJITStats
	for i := 0; i < b.N; i++ {
		var stdout, stderr bytes.Buffer
		var stats EmuxJITStats
		cfg := mode
		cfg.RunPath = "../../testvectors/jea9linux/go/elf/hello.elf"
		cfg.MemorySize = riscv.Size16GB
		cfg.InstructionBudget = 1 << 20
		cfg.Stdin = strings.NewReader("")
		cfg.Stdout = &stdout
		cfg.Stderr = &stderr
		cfg.JITStats = &stats

		code, err := runEmux(cfg)
		if err != nil {
			b.Fatalf("runEmux: %v; stderr=%q", err, stderr.String())
		}
		if code != 0 {
			b.Fatalf("exit code = %d, want 0; stderr=%q", code, stderr.String())
		}
		if got, want := stdout.String(), "hello jea9linux go\n"; got != want {
			b.Fatalf("stdout = %q, want %q; stderr=%q", got, want, stderr.String())
		}

		totalStats.DispatchOK += stats.DispatchOK
		totalStats.DispatchCompile += stats.DispatchCompile
		totalStats.DispatchInterp += stats.DispatchInterp
		totalStats.ChainPatchedJalr += stats.ChainPatchedJalr
		totalStats.JalrICMisses += stats.JalrICMisses
		totalStats.JalrICDeopts += stats.JalrICDeopts
		totalStats.AOTSegmentsInstalled += stats.AOTSegmentsInstalled
		totalStats.AOTBlocksInstalled += stats.AOTBlocksInstalled
		totalStats.AOTCompileFailures += stats.AOTCompileFailures
		totalStats.AOTDecoderCacheLookups += stats.AOTDecoderCacheLookups
		totalStats.AOTDecoderCacheHits += stats.AOTDecoderCacheHits
		totalStats.AOTDecoderCacheMisses += stats.AOTDecoderCacheMisses
		totalStats.AOTDecoderCacheOutside += stats.AOTDecoderCacheOutside
	}
	if totalStats.DispatchOK != 0 || totalStats.DispatchCompile != 0 || totalStats.DispatchInterp != 0 {
		b.ReportMetric(float64(totalStats.DispatchOK)/float64(b.N), "dispatch_ok/op")
		b.ReportMetric(float64(totalStats.DispatchCompile)/float64(b.N), "compile/op")
		b.ReportMetric(float64(totalStats.DispatchInterp)/float64(b.N), "interp_fallback/op")
		b.ReportMetric(float64(totalStats.ChainPatchedJalr)/float64(b.N), "jalr_patch/op")
		b.ReportMetric(float64(totalStats.JalrICMisses)/float64(b.N), "jalr_miss/op")
		b.ReportMetric(float64(totalStats.JalrICDeopts)/float64(b.N), "jalr_deopt/op")
	}
	if totalStats.AOTSegmentsInstalled != 0 || totalStats.AOTCompileFailures != 0 {
		b.ReportMetric(float64(totalStats.AOTSegmentsInstalled)/float64(b.N), "aotseg/op")
		b.ReportMetric(float64(totalStats.AOTBlocksInstalled)/float64(b.N), "aotblock/op")
		b.ReportMetric(float64(totalStats.AOTCompileFailures)/float64(b.N), "aotfail/op")
	}
	if totalStats.AOTDecoderCacheLookups != 0 {
		b.ReportMetric(float64(totalStats.AOTDecoderCacheLookups)/float64(b.N), "aotdc_lookup/op")
		b.ReportMetric(float64(totalStats.AOTDecoderCacheHits)/float64(b.N), "aotdc_hit/op")
		b.ReportMetric(float64(totalStats.AOTDecoderCacheMisses)/float64(b.N), "aotdc_miss/op")
	}
	if totalStats.AOTDecoderCacheOutside != 0 {
		b.ReportMetric(float64(totalStats.AOTDecoderCacheOutside)/float64(b.N), "aotdc_outside/op")
	}
}

func TestParseClockModeAndSeedBytes(t *testing.T) {
	for _, tc := range []struct {
		name string
		want riscv.Jea9LinuxClockMode
	}{
		{name: "idle-jump", want: riscv.Jea9ClockIdleJump},
		{name: "idlejump", want: riscv.Jea9ClockIdleJump},
		{name: "ic-tick", want: riscv.Jea9ClockICTick},
		{name: "ictick", want: riscv.Jea9ClockICTick},
	} {
		got, err := parseClockMode(tc.name)
		if err != nil {
			t.Fatalf("parseClockMode(%q): %v", tc.name, err)
		}
		if got != tc.want {
			t.Fatalf("parseClockMode(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
	if _, err := parseClockMode("host-time"); err == nil {
		t.Fatal("parseClockMode(host-time) returned nil error")
	}
	if _, err := parseClockMode("manual"); err == nil {
		t.Fatal("parseClockMode(manual) returned nil error")
	}

	const seed = uint64(0x0102030405060708)
	if got := binary.LittleEndian.Uint64(seedBytes(seed)); got != seed {
		t.Fatalf("seedBytes round trip = %#x, want %#x", got, seed)
	}
}

func parseEmuxConfigForTest(t *testing.T, args ...string) (EmuxConfig, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	fs := flag.NewFlagSet("emux-test", flag.ContinueOnError)
	var flagErrors bytes.Buffer
	fs.SetOutput(&flagErrors)

	cfg := &EmuxConfig{}
	cfg.DefineFlags(fs)
	if err := fs.Parse(args); err != nil {
		t.Fatalf("parse flags: %v; output=%q", err, flagErrors.String())
	}
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "monotonic-ns" {
			cfg.MonotonicStartSet = true
		}
	})
	if err := cfg.ValidateConfig(); err != nil {
		t.Fatalf("validate flags: %v", err)
	}
	cfg.Args = append([]string{cfg.RunPath}, fs.Args()...)
	cfg.Env = []string{}

	var stdout, stderr bytes.Buffer
	cfg.Stdin = strings.NewReader("")
	cfg.Stdout = &stdout
	cfg.Stderr = &stderr
	return *cfg, &stdout, &stderr
}

func runEmuxFixtureOutput(t *testing.T, seed uint64) string {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code, err := runEmux(EmuxConfig{
		RunPath:           "../../testvectors/jea9linux/go/elf/cryptorand.elf",
		MemorySize:        riscv.Size16GB,
		InstructionBudget: 1 << 20,
		Seed:              seed,
		Stdin:             strings.NewReader(""),
		Stdout:            &stdout,
		Stderr:            &stderr,
	})
	if err != nil {
		t.Fatalf("runEmux: %v; stderr=%q", err, stderr.String())
	}
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	return stdout.String()
}
