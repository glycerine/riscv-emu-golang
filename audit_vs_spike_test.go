package riscv

import "testing"

func oneInsnCPU(t *testing.T, insn uint32) (*CPU, *GuestMemory) {
	t.Helper()
	return newTestCPU(t, Size64MB, 0x1000, []uint32{insn})
}

func TestAuditCSRPrivilegeAndCounterChecks(t *testing.T) {
	t.Run("user cannot read machine CSR", func(t *testing.T) {
		cpu, mem := oneInsnCPU(t, ienc(opSYSTEM, 2, 1, 0, 0x300)) // CSRRS x1,mstatus,x0
		defer mem.Free()
		cpu.EnableStrictCSR()
		cpu.SetPrivilegeMode(PrivUser)
		if err := cpu.Step(); err != ErrIllegalInstruction {
			t.Fatalf("Step err = %v, want illegal instruction", err)
		}
	})

	t.Run("counteren gates user counters", func(t *testing.T) {
		cpu, mem := oneInsnCPU(t, ienc(opSYSTEM, 2, 1, 0, 0xC00)) // CSRRS x1,cycle,x0
		defer mem.Free()
		cpu.EnableStrictCSR()
		cpu.SetPrivilegeMode(PrivUser)
		cpu.mcounteren = 0
		cpu.scounteren = 0
		if err := cpu.Step(); err != ErrIllegalInstruction {
			t.Fatalf("Step err = %v, want illegal instruction", err)
		}
	})

	t.Run("CSRRS write intent uses encoded rs1", func(t *testing.T) {
		cpu, mem := oneInsnCPU(t, ienc(opSYSTEM, 2, 1, 5, 0xC00)) // CSRRS x1,cycle,x5
		defer mem.Free()
		cpu.EnableStrictCSR()
		cpu.SetPrivilegeMode(PrivMachine)
		cpu.SetReg(5, 0)
		if err := cpu.Step(); err != ErrIllegalInstruction {
			t.Fatalf("Step err = %v, want illegal instruction for read-only CSR write intent", err)
		}
	})
}

func TestAuditCSRWriteMasks(t *testing.T) {
	cpu, mem := oneInsnCPU(t, ienc(opSYSTEM, 1, 0, 5, 0x300)) // CSRRW x0,mstatus,x5
	defer mem.Free()
	cpu.SetPrivilegeMode(PrivMachine)
	cpu.SetReg(5, ^uint64(0))
	if err := cpu.Step(); err != nil {
		t.Fatalf("Step: %v", err)
	}
	if got := cpu.mstatus &^ (mstatusWritable | statusSD); got != 0 {
		t.Fatalf("mstatus kept unsupported bits 0x%x in 0x%x", got, cpu.mstatus)
	}
	if got := (cpu.mstatus & statusMPP) >> 11; got == 2 {
		t.Fatalf("mstatus.MPP kept reserved privilege value 2")
	}

	if !cpu.writeCSR(0x304, ^uint64(0)) {
		t.Fatalf("write mie failed")
	}
	if cpu.mie != implementedMieMask {
		t.Fatalf("mie = 0x%x, want mask 0x%x", cpu.mie, implementedMieMask)
	}
	if !cpu.writeCSR(0x344, ^uint64(0)) {
		t.Fatalf("write mip failed")
	}
	if cpu.mip != implementedMipMask {
		t.Fatalf("mip = 0x%x, want mask 0x%x", cpu.mip, implementedMipMask)
	}
}

func TestAuditPrivilegedReturnAndFenceChecks(t *testing.T) {
	t.Run("MRET clears MPRV below machine", func(t *testing.T) {
		cpu, mem := oneInsnCPU(t, mretInsn)
		defer mem.Free()
		cpu.SetPrivilegeMode(PrivMachine)
		cpu.mstatus = statusMPIE | statusMPRV | (uint64(PrivSupervisor) << 11)
		if err := cpu.Step(); err != nil {
			t.Fatalf("Step: %v", err)
		}
		if cpu.mstatus&statusMPRV != 0 {
			t.Fatalf("MRET left MPRV set: mstatus=0x%x", cpu.mstatus)
		}
	})

	t.Run("SRET clears MPRV", func(t *testing.T) {
		cpu, mem := oneInsnCPU(t, sretInsn)
		defer mem.Free()
		cpu.EnableStrictCSR()
		cpu.SetPrivilegeMode(PrivSupervisor)
		cpu.mstatus = statusSPIE | statusMPRV
		if err := cpu.Step(); err != nil {
			t.Fatalf("Step: %v", err)
		}
		if cpu.mstatus&statusMPRV != 0 {
			t.Fatalf("SRET left MPRV set: mstatus=0x%x", cpu.mstatus)
		}
	})

	t.Run("SRET obeys TSR", func(t *testing.T) {
		cpu, mem := oneInsnCPU(t, sretInsn)
		defer mem.Free()
		cpu.EnableStrictCSR()
		cpu.SetPrivilegeMode(PrivSupervisor)
		cpu.mstatus = statusTSR
		if err := cpu.Step(); err != ErrIllegalInstruction {
			t.Fatalf("Step err = %v, want illegal instruction", err)
		}
	})

	t.Run("WFI and SFENCE require privilege", func(t *testing.T) {
		for _, insn := range []uint32{0x10500073, 0x12000073} {
			cpu, mem := oneInsnCPU(t, insn)
			cpu.EnableStrictCSR()
			cpu.SetPrivilegeMode(PrivUser)
			err := cpu.Step()
			mem.Free()
			if err != ErrIllegalInstruction {
				t.Fatalf("insn 0x%08x err = %v, want illegal instruction", insn, err)
			}
		}
	})
}

func TestAuditFPStatusRoundingAndSNaN(t *testing.T) {
	t.Run("FS off rejects FP instruction", func(t *testing.T) {
		cpu, mem := oneInsnCPU(t, encFP(0x00, 0, 1, 2, 3, 0)) // FADD.S
		defer mem.Free()
		cpu.mstatus &^= statusFS | statusSD
		if err := cpu.Step(); err != ErrIllegalInstruction {
			t.Fatalf("Step err = %v, want illegal instruction", err)
		}
	})

	t.Run("reserved static rm rejected", func(t *testing.T) {
		cpu, mem := oneInsnCPU(t, encFP(0x00, 0, 1, 2, 3, 5)) // FADD.S rm=5
		defer mem.Free()
		if err := cpu.Step(); err != ErrIllegalInstruction {
			t.Fatalf("Step err = %v, want illegal instruction", err)
		}
	})

	t.Run("reserved dynamic frm rejected", func(t *testing.T) {
		cpu, mem := oneInsnCPU(t, encFP(0x00, 0, 1, 2, 3, 7)) // FADD.S rm=DYN
		defer mem.Free()
		cpu.fcsr = 5 << 5
		if err := cpu.Step(); err != ErrIllegalInstruction {
			t.Fatalf("Step err = %v, want illegal instruction", err)
		}
	})

	t.Run("FCVT.D.S raises NV for signaling NaN", func(t *testing.T) {
		cpu, mem := oneInsnCPU(t, encFP(0x08, 1, 1, 2, 0, 0)) // FCVT.D.S f1,f2
		defer mem.Free()
		cpu.SetFReg(2, boxF32(0x7f800001))
		if err := cpu.Step(); err != nil {
			t.Fatalf("Step: %v", err)
		}
		if cpu.fcsr&fflagNV == 0 {
			t.Fatalf("fcsr = 0x%x, want NV set", cpu.fcsr)
		}
	})
}

func TestAuditReservedDecodeAndMtvecZero(t *testing.T) {
	for _, tc := range []struct {
		name string
		insn uint32
	}{
		{"zicond invalid funct3", renc(opOP, 0, 0x07, 1, 2, 3)},
		{"shadd invalid funct3", renc(opOP, 0, 0x10, 1, 2, 3)},
		{"binv invalid funct3", renc(opOP, 0, 0x34, 1, 2, 3)},
		{"reserved base funct7", renc(opOP, 0, 0x02, 1, 2, 3)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cpu, mem := oneInsnCPU(t, tc.insn)
			defer mem.Free()
			if err := cpu.Step(); err != ErrIllegalInstruction {
				t.Fatalf("Step err = %v, want illegal instruction", err)
			}
		})
	}

	t.Run("machine trap may vector to mtvec zero", func(t *testing.T) {
		cpu, mem := oneInsnCPU(t, ecallInsn)
		defer mem.Free()
		cpu.EnableStrictCSR()
		cpu.SetPrivilegeMode(PrivMachine)
		cpu.mtvec = 0
		if err := cpu.Step(); err != nil {
			t.Fatalf("Step: %v", err)
		}
		if cpu.PC() != 0 || cpu.mepc != 0x1000 || cpu.mcause != CauseEcallM {
			t.Fatalf("trap state pc=0x%x mepc=0x%x mcause=%d", cpu.PC(), cpu.mepc, cpu.mcause)
		}
	})
}
