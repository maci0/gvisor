//go:build darwin && arm64

package hvf

// In-VM Syscall Handling for Sentry-as-Ring0
//
// When the sentry runs at EL1, guest SVC from EL0 traps to our
// el0_sync vector (VBAR_EL1 + 0x400). The handler can process
// the syscall entirely at EL1 and ERET back to EL0.
//
// Syscall flow (ring0):
//   1. Guest at EL0 executes SVC #0
//   2. CPU traps to EL1, saves PC in ELR_EL1, PSTATE in SPSR_EL1
//   3. el0_sync handler saves registers to per-vCPU save area
//   4. Handler reads ESR_EL1 to confirm EC=0x15 (SVC from AArch64)
//   5. Syscall number in X8, args in X0-X5
//   6. Sentry processes syscall (e.g., read, write, mmap, etc.)
//   7. Result placed in X0
//   8. ERET back to EL0 (restores ELR_EL1→PC, SPSR_EL1→PSTATE)
//
// Only host I/O operations (file ops, network, mmap backing) need
// HVC exit to the VMM. Pure sentry operations (e.g., getpid, clock_gettime
// from vDSO, signal handling, futex) stay entirely in-VM.
//
// Estimated syscall latency:
//   Current (HVC exit): ~11µs per syscall
//   Ring0 (in-VM):      ~100ns per syscall (100x improvement)

// SyscallHandler is the function signature for in-VM syscall handling.
// When the sentry runs at EL1, the el0_sync assembly vector calls
// this handler after saving registers.
//
// Parameters are the syscall number and arguments from the guest's
// registers (X8 for number, X0-X5 for arguments). Returns the
// syscall result to place in X0 before ERET.
type SyscallHandler func(nr, a0, a1, a2, a3, a4, a5 uint64) uint64

// syscallHandler is the registered handler for in-VM syscalls.
// Set during sentry initialization when running in ring0 mode.
var syscallHandler SyscallHandler

// RegisterSyscallHandler sets the function that handles guest syscalls
// when the sentry runs at EL1. This must be called before any guest
// code executes.
func RegisterSyscallHandler(h SyscallHandler) {
	syscallHandler = h
}

// handleGuestSyscall is called from the el0_sync assembly vector
// when a guest SVC is caught at EL1. It dispatches to the registered
// syscall handler.
//
// This function is called with interrupts masked (DAIF set) and must
// not block or cause stack growth (nosplit). The actual syscall
// processing happens in the registered handler, which may relax
// these constraints.
//
//go:nosplit
func handleGuestSyscall(nr, a0, a1, a2, a3, a4, a5 uint64) uint64 {
	if syscallHandler != nil {
		return syscallHandler(nr, a0, a1, a2, a3, a4, a5)
	}
	// No handler registered — should not happen in production.
	// Return -ENOSYS.
	return ^uint64(0) - 37 // -ENOSYS (38 on Linux)
}

