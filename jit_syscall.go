package riscv

// JITEcallHandler handles a JIT ECALL note after compiled code has written
// guest-visible state back to CPU memory and returned to the JIT dispatcher.
type JITEcallHandler func(cpu *CPU, n Note) NoteDisposition

func (j *JIT) deliverEcall(cpu *CPU, n Note) NoteDisposition {
	if j.ecallHandler != nil {
		j.personalityEcallCount++
		if d := j.ecallHandler(cpu, n); d != NoteForward {
			return d
		}
	}
	return cpu.Notes.Deliver(cpu, n)
}

func (j *JIT) PersonalityEcallCount() uint64 {
	return j.personalityEcallCount
}
