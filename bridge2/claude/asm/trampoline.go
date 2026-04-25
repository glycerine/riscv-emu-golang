package sandbox

// SwitchStack atomically switches RSP to newSP and returns the previous
// stack pointer. The caller MUST update g.stack.lo / g.stack.hi (via
// unsafe access to the g struct) BEFORE calling this, otherwise the
// Go GC will panic when it scans the goroutine stack and finds RSP
// outside [g.stack.lo, g.stack.hi].
//
// Implemented in trampoline_amd64.s / trampoline_arm64.s.
//
//go:nosplit
func SwitchStack(newSP uintptr) (oldSP uintptr)

// GetG returns the address of the current goroutine's internal g struct.
// On amd64 (Go 1.17+) this is R14. On arm64 it is R28.
// The layout of g is runtime-internal and version-dependent — pin your
// Go version if you're using field offsets directly.
//
//go:nosplit
func GetG() uintptr

// CPURelax emits a spin-wait hint (PAUSE on x86, YIELD on arm64).
// Use inside polling loops to reduce power and avoid memory order violations.
//
//go:nosplit
func CPURelax()

// StoreFenceRelease emits a store-release memory fence.
// On x86 this is SFENCE. On arm64 this is DMB ISHST.
// Use after writing a ring slot, before publishing the head pointer.
//
//go:nosplit
func StoreFenceRelease()

// LoadFenceAcquire emits a load-acquire memory fence.
// On x86 this is LFENCE. On arm64 this is DMB ISHLD.
// Use when polling the ring head or a work item's state field.
//
//go:nosplit
func LoadFenceAcquire()
