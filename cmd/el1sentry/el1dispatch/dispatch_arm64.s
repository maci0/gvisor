// EL1 syscall dispatcher in Go plan9 assembly.
// Position-independent (only relative branches + register-indirect loads).
// Copied to TTBR1 dispatch page at runtime and called via BLR from EL1.
//
// Calling convention:
//   R0 = state page pointer (TTBR1 kernel VA)
//   R1 = syscall number (X8 from guest)
//   Returns: R0 = result, R9 = 0 (handled) or 1 (exit to host)
//
// State page layout:
//   0x200: pid    0x208: tid    0x210: brk
//   0x218: uid    0x220: gid    0x228: ppid
//   0x230: pgid   0x238: sid

#include "textflag.h"

// dispatch is the EL1 syscall handler. Must be NOSPLIT (no stack growth).
TEXT ·dispatch(SB), NOSPLIT|NOFRAME, $0
	CMP	$172, R1	// getpid
	BEQ	handle_getpid
	CMP	$173, R1	// getppid
	BEQ	handle_getppid
	CMP	$174, R1	// getuid
	BEQ	handle_getuid
	CMP	$175, R1	// geteuid
	BEQ	handle_getuid	// same as getuid
	CMP	$176, R1	// getgid
	BEQ	handle_getgid
	CMP	$177, R1	// getegid
	BEQ	handle_getgid	// same as getgid
	CMP	$178, R1	// gettid
	BEQ	handle_gettid
	CMP	$96, R1		// set_tid_address
	BEQ	handle_gettid	// returns tid
	CMP	$124, R1	// sched_yield
	BEQ	handle_yield
	CMP	$155, R1	// getpgid
	BEQ	handle_getpgid
	CMP	$156, R1	// getsid
	BEQ	handle_getsid

	// Unknown syscall: exit to host
	MOVD	$1, R9
	RET

handle_getpid:
	MOVD	0x200(R0), R0	// pid
	MOVD	$0, R9
	RET

handle_getppid:
	MOVD	0x228(R0), R0	// ppid
	MOVD	$0, R9
	RET

handle_getuid:
	MOVD	0x218(R0), R0	// uid
	MOVD	$0, R9
	RET

handle_getgid:
	MOVD	0x220(R0), R0	// gid
	MOVD	$0, R9
	RET

handle_gettid:
	MOVD	0x208(R0), R0	// tid
	MOVD	$0, R9
	RET

handle_yield:
	MOVD	$0, R0
	MOVD	$0, R9
	RET

handle_getpgid:
	// Only handle getpgid(0)
	CBNZ	R2, unknown	// R2 = arg0 (pid param)
	MOVD	0x230(R0), R0	// pgid
	MOVD	$0, R9
	RET

handle_getsid:
	// Only handle getsid(0)
	CBNZ	R2, unknown
	MOVD	0x238(R0), R0	// sid
	MOVD	$0, R9
	RET

unknown:
	MOVD	$1, R9
	RET

// dispatchEnd marks the end of the dispatch function.
// Used to calculate code size: dispatchEnd - dispatch.
TEXT ·dispatchEnd(SB), NOSPLIT|NOFRAME, $0
	RET
