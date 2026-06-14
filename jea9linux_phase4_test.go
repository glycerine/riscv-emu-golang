package riscv

import "testing"

const (
	jea9TestATNull        = uint64(0)
	jea9TestATPHDR        = uint64(3)
	jea9TestATPHENT       = uint64(4)
	jea9TestATPHNUM       = uint64(5)
	jea9TestATPAGESZ      = uint64(6)
	jea9TestATENTRY       = uint64(9)
	jea9TestATUID         = uint64(11)
	jea9TestATEUID        = uint64(12)
	jea9TestATGID         = uint64(13)
	jea9TestATEGID        = uint64(14)
	jea9TestATPLATFORM    = uint64(15)
	jea9TestATHWCAP       = uint64(16)
	jea9TestATCLKTCK      = uint64(17)
	jea9TestATSECURE      = uint64(23)
	jea9TestATRANDOM      = uint64(25)
	jea9TestATHWCAP2      = uint64(26)
	jea9TestATEXECFN      = uint64(31)
	jea9TestATSYSINFOEHDR = uint64(33)
)

func loadTinyELFForStack(t *testing.T) (*CPU, *GuestMemory, *ELF) {
	t.Helper()
	mem, err := NewGuestMemory(Size64MB)
	if err != nil {
		t.Fatal(err)
	}
	elf, err := LoadELFBytes(mem, BuildELF(0x10000, []uint32{instrECALL}))
	if err != nil {
		mem.Free()
		t.Fatalf("LoadELFBytes: %v", err)
	}
	cpu := NewCPU(*mem)
	cpu.SetPC(elf.Entry)
	return cpu, mem, elf
}

func loadPtr(t *testing.T, mem *GuestMemory, addr uint64) uint64 {
	t.Helper()
	v, f := mem.Load64(addr)
	if f != nil {
		t.Fatalf("Load64(0x%x): %v", addr, f)
	}
	return v
}

func guestString(t *testing.T, cpu *CPU, ptr uint64) string {
	t.Helper()
	s, errno := readLinuxCString(cpu, ptr, 1024)
	if errno != 0 {
		t.Fatalf("readLinuxCString(0x%x) errno %d", ptr, errno)
	}
	return s
}

func parseAuxv(t *testing.T, mem *GuestMemory, addr uint64) map[uint64]uint64 {
	t.Helper()
	aux := make(map[uint64]uint64)
	for i := 0; i < 64; i++ {
		tag := loadPtr(t, mem, addr+uint64(i*16))
		val := loadPtr(t, mem, addr+uint64(i*16+8))
		if tag == jea9TestATNull {
			return aux
		}
		aux[tag] = val
	}
	t.Fatal("auxv did not terminate")
	return nil
}

func TestJea9Linux_InitStackArgvEnvAuxv(t *testing.T) {
	cpu, mem, elf := loadTinyELFForStack(t)
	defer mem.Free()

	j := NewJea9Linux(Jea9LinuxOptions{EntropySeed: []byte("stack seed")})
	err := j.InitELFStack(cpu, elf, Jea9LinuxStartOptions{
		Args:     []string{"prog", "alpha"},
		Env:      []string{"A=B", "C=D"},
		StackTop: 0x03F00000,
	})
	if err != nil {
		t.Fatalf("InitELFStack: %v", err)
	}

	sp := cpu.Reg(2)
	if sp == 0 || sp%16 != 0 {
		t.Fatalf("sp = 0x%x, want nonzero 16-byte aligned", sp)
	}
	if got := cpu.PC(); got != elf.Entry {
		t.Fatalf("PC = 0x%x, want entry 0x%x", got, elf.Entry)
	}
	if argc := loadPtr(t, mem, sp); argc != 2 {
		t.Fatalf("argc = %d, want 2", argc)
	}
	argv0 := loadPtr(t, mem, sp+8)
	argv1 := loadPtr(t, mem, sp+16)
	argvNull := loadPtr(t, mem, sp+24)
	if argvNull != 0 {
		t.Fatalf("argv null = 0x%x, want 0", argvNull)
	}
	if got := guestString(t, cpu, argv0); got != "prog" {
		t.Fatalf("argv0 = %q", got)
	}
	if got := guestString(t, cpu, argv1); got != "alpha" {
		t.Fatalf("argv1 = %q", got)
	}
	env0 := loadPtr(t, mem, sp+32)
	env1 := loadPtr(t, mem, sp+40)
	envNull := loadPtr(t, mem, sp+48)
	if envNull != 0 {
		t.Fatalf("env null = 0x%x, want 0", envNull)
	}
	if got := guestString(t, cpu, env0); got != "A=B" {
		t.Fatalf("env0 = %q", got)
	}
	if got := guestString(t, cpu, env1); got != "C=D" {
		t.Fatalf("env1 = %q", got)
	}

	aux := parseAuxv(t, mem, sp+56)
	if aux[jea9TestATPAGESZ] != GuestPageSize {
		t.Fatalf("AT_PAGESZ = %d, want %d", aux[jea9TestATPAGESZ], GuestPageSize)
	}
	if aux[jea9TestATENTRY] != elf.Entry {
		t.Fatalf("AT_ENTRY = 0x%x, want 0x%x", aux[jea9TestATENTRY], elf.Entry)
	}
	if aux[jea9TestATPHENT] != uint64(elf.Header.PhEntSize) {
		t.Fatalf("AT_PHENT = %d, want %d", aux[jea9TestATPHENT], elf.Header.PhEntSize)
	}
	if aux[jea9TestATPHNUM] != uint64(elf.Header.PhNum) {
		t.Fatalf("AT_PHNUM = %d, want %d", aux[jea9TestATPHNUM], elf.Header.PhNum)
	}
	if _, ok := aux[jea9TestATSYSINFOEHDR]; ok {
		t.Fatal("AT_SYSINFO_EHDR/VDSO must be omitted")
	}
	randomPtr := aux[jea9TestATRANDOM]
	if randomPtr == 0 {
		t.Fatal("AT_RANDOM missing")
	}
	random := readGuestBytes(t, mem, randomPtr, 16)
	if string(random) == string(make([]byte, 16)) {
		t.Fatal("AT_RANDOM points at all-zero bytes")
	}
}

func TestJea9Linux_InitStackATRandomDeterministic(t *testing.T) {
	var got [2][]byte
	for i := range got {
		cpu, mem, elf := loadTinyELFForStack(t)
		defer mem.Free()
		j := NewJea9Linux(Jea9LinuxOptions{EntropySeed: []byte("same stack seed")})
		if err := j.InitELFStack(cpu, elf, Jea9LinuxStartOptions{StackTop: 0x03F00000}); err != nil {
			t.Fatalf("InitELFStack: %v", err)
		}
		sp := cpu.Reg(2)
		aux := parseAuxv(t, mem, sp+32) // argc=1 default argv, argv null, env null
		got[i] = readGuestBytes(t, mem, aux[jea9TestATRANDOM], 16)
	}
	if string(got[0]) != string(got[1]) {
		t.Fatalf("AT_RANDOM differs for same seed: %x != %x", got[0], got[1])
	}
}

func TestJea9Linux_InitStackATRandomSeparateFromSysRandom(t *testing.T) {
	cpu, mem, elf := loadTinyELFForStack(t)
	defer mem.Free()

	seed := []byte("separate random streams")
	j := NewJea9Linux(Jea9LinuxOptions{EntropySeed: seed})
	if err := j.InitELFStack(cpu, elf, Jea9LinuxStartOptions{StackTop: 0x03F00000}); err != nil {
		t.Fatalf("InitELFStack: %v", err)
	}
	sp := cpu.Reg(2)
	aux := parseAuxv(t, mem, sp+32)
	atRandom := readGuestBytes(t, mem, aux[jea9TestATRANDOM], 16)

	var afterStack, withoutStack [16]byte
	j.fillRandom(afterStack[:])
	NewJea9Linux(Jea9LinuxOptions{EntropySeed: seed}).fillRandom(withoutStack[:])
	if afterStack != withoutStack {
		t.Fatalf("InitELFStack consumed syscall random stream: got %x want %x", afterStack, withoutStack)
	}
	if string(atRandom) == string(afterStack[:]) {
		t.Fatalf("AT_RANDOM reused first syscall random bytes: %x", atRandom)
	}
}

func TestJea9Linux_InitStackAuxvLinuxPersonalityDefaults(t *testing.T) {
	cpu, mem, elf := loadTinyELFForStack(t)
	defer mem.Free()

	j := NewJea9Linux(Jea9LinuxOptions{EntropySeed: []byte("auxv defaults")})
	err := j.InitELFStack(cpu, elf, Jea9LinuxStartOptions{
		Args:     []string{"argv0"},
		ExecPath: "/bin/argv0",
		StackTop: 0x03F00000,
	})
	if err != nil {
		t.Fatalf("InitELFStack: %v", err)
	}

	sp := cpu.Reg(2)
	aux := parseAuxv(t, mem, sp+32) // argc=1, argv null, env null
	for tag, want := range map[uint64]uint64{
		jea9TestATUID:    0,
		jea9TestATEUID:   0,
		jea9TestATGID:    0,
		jea9TestATEGID:   0,
		jea9TestATSECURE: 0,
		jea9TestATHWCAP:  0,
		jea9TestATHWCAP2: 0,
		jea9TestATCLKTCK: 100,
	} {
		if got, ok := aux[tag]; !ok || got != want {
			t.Fatalf("aux tag %d = (%d,%v), want %d present", tag, got, ok, want)
		}
	}
	if got := guestString(t, cpu, aux[jea9TestATPLATFORM]); got != "riscv64" {
		t.Fatalf("AT_PLATFORM string = %q, want riscv64", got)
	}
	if got := guestString(t, cpu, aux[jea9TestATEXECFN]); got != "/bin/argv0" {
		t.Fatalf("AT_EXECFN string = %q, want /bin/argv0", got)
	}
}

func TestJea9Linux_InitStackInputErrors(t *testing.T) {
	cpu, mem, elf := loadTinyELFForStack(t)
	defer mem.Free()

	j := NewJea9Linux(Jea9LinuxOptions{})
	if err := j.InitELFStack(cpu, nil, Jea9LinuxStartOptions{}); err == nil {
		t.Fatal("InitELFStack with nil ELF returned nil error")
	}
	if err := j.InitELFStack(cpu, &ELF{}, Jea9LinuxStartOptions{}); err == nil {
		t.Fatal("InitELFStack with empty ELF returned nil error")
	}
	if err := j.InitELFStack(cpu, elf, Jea9LinuxStartOptions{
		Args:     []string{"this string cannot fit above a tiny stack top"},
		StackTop: 16,
	}); err == nil {
		t.Fatal("InitELFStack with tiny StackTop returned nil error")
	}
}
