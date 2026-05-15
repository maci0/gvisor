// EL1 syscall dispatcher for HVF VM.
// Compiled as bare-metal PIC:
//   clang -target arm64-apple-none -ffreestanding -nostdlib -fPIC -O2
//         -mno-implicit-float -c dispatch.c -o dispatch.o
//
// Loaded into a TTBR1-mapped page and called via BLR from the
// EL1 exception vector handler.
//
// Calling convention:
//   X0 = state page pointer (TTBR1 kernel VA)
//   X1 = syscall number (X8 from guest)
//   X2 = arg0 (X0 from guest)
//   X3 = arg1 (X1 from guest)
//   X4 = arg2 (X2 from guest)
//   Returns: X0 = result, X9 = 0 (handled) or 1 (exit to host)

typedef unsigned long u64;

// State page offsets — must match spOffset* constants in vcpu_arm64.go
#define SP_PID      0x200
#define SP_TID      0x208
#define SP_BRK      0x210
#define SP_UID      0x218
#define SP_GID      0x220
#define SP_PPID     0x228
#define SP_PGID     0x230
#define SP_SID      0x238
#define SP_SIGMASK  0x128
#define SP_SIGDIRTY 0x130

static inline u64 load(const void *base, int offset) {
    return *(volatile u64 *)((const char *)base + offset);
}

static inline void store(void *base, int offset, u64 val) {
    *(volatile u64 *)((char *)base + offset) = val;
}

// Signal: handled, ERET back to EL0
static inline void signal_handled(void) {
    __asm__ volatile("mov x9, #0");
}

// Signal: exit to host via HVC #9
static inline void signal_exit(void) {
    __asm__ volatile("mov x9, #1");
}

u64 dispatch(void *state, u64 nr, u64 a0, u64 a1, u64 a2) {
    u64 ret;

    switch (nr) {
    // Identity syscalls — return cached values from state page
    case 172: ret = load(state, SP_PID); break;     // getpid
    case 173: ret = load(state, SP_PPID); break;    // getppid
    case 174: ret = load(state, SP_UID); break;     // getuid
    case 175: ret = load(state, SP_UID); break;     // geteuid
    case 176: ret = load(state, SP_GID); break;     // getgid
    case 177: ret = load(state, SP_GID); break;     // getegid
    case 178: ret = load(state, SP_TID); break;     // gettid
    case 96:  ret = load(state, SP_TID); break;     // set_tid_address

    // sched_yield — no-op for single-task
    case 124: ret = 0; break;

    // getpgid(0) — return cached PGID
    case 155:
        if (a0 == 0) { ret = load(state, SP_PGID); break; }
        signal_exit(); return 0;

    // getsid(0) — return cached SID
    case 156:
        if (a0 == 0) { ret = load(state, SP_SID); break; }
        signal_exit(); return 0;

    default:
        signal_exit();
        return 0;
    }

    signal_handled();
    return ret;
}
