# gVisor macOS Port

> **Status: Functional — full interactive shell with devpts PTY and job control**
>
> gVisor runs on macOS Apple Silicon via Hypervisor.framework. Alpine
> Linux boots with full shell, networking, package management, and
> multi-CPU support. 68+ packages installed and tested. Dynamically-linked
> binaries (jq, curl, etc.) run reliably with 1000+ sequential exec cycles.
> Interactive shell uses devpts PTY with proper line discipline, echo,
> signals (Ctrl+C/Ctrl+Z), and job control (bg/fg).

| Feature | Status |
|---------|--------|
| HVF platform (Hypervisor.framework) | Working |
| ARM64 page tables (4K default, per-MM, COW) | Working |
| Fork/exec with copy-on-write | Working (shadow pages fix PA coherency) |
| Signal delivery (SIGURG, SIGINT, etc.) | Working |
| Multi-CPU (14 vCPUs, GOMAXPROCS=22) | Working |
| Gofer filesystem (host dir passthrough) | Working |
| Symlink resolution (busybox) | Working |
| Alpine Linux 3.21.3 | Working |
| TCP/UDP (loopback + internet) | Working |
| Host networking (proxy, default) | Working (no root, no daemon) |
| Host networking (utun + userspace proxy) | Working (requires root) |
| Host networking (vmnet via socket_vmnet) | Working (rootless, needs daemon) |
| DNS resolution (host system DNS) | Working |
| HTTP/HTTPS downloads | Working (up to 17MB verified) |
| Package install (`apk add`) | Working (68+ packages) |
| Installed packages | Working — 50/50 test pass rate |
| Direct TLBI at EL1 | Working (TLB coherency via VMALLE1IS) |
| VDSO (clock_gettime fast path) | Working (~5ns/call via CNTVCT_EL0) |
| directfs mode (bypass lisafs RPC) | Working (`--directfs` flag) |
| ICMP ping (unprivileged) | Working (SOCK_DGRAM, no raw socket) |
| safecopy (Mach exception ports) | Working (bypasses PAC sigreturn) |
| devpts PTY (interactive shell, job control) | Working (bg/fg/Ctrl+Z/Ctrl+C) |
| MRS ID register emulation (trap-and-emulate) | Working |
| EL0 cache maintenance (DC CIVAC, CTR_EL0) | Working (SCTLR_EL1 UCI/UCT) |
| 4K guest pages (default) | Working (IPA granule 4K, TG0=4K) |
| Split page model (4K guest / 16K host) | Working (74/77 Alpine tests) |
| GraalVM Native Image (Java AOT) | Working |
| FEX-Emu (x86_64 emulation) | Partial — [COW fault loop](docs/FEX-EMU.md) |
| Java / JVM (HotSpot JIT) | Blocked — [upstream JDK bug](docs/MRS-TRAPPING.md) |

## Quick Start

```bash
# Build (requires pure=false for CGO/Hypervisor.framework)
bazel build //cmd/sentrydarwin --@io_bazel_rules_go//go/config:pure=false

# Sign with Hypervisor entitlement
cp bazel-bin/cmd/sentrydarwin/sentrydarwin_/sentrydarwin ./sentrydarwin
codesign --entitlements cmd/sentrydarwin/entitlements.plist -f --sign - ./sentrydarwin

# Download Alpine Linux rootfs
mkdir alpine-rootfs
curl -sL https://dl-cdn.alpinelinux.org/alpine/v3.21/releases/aarch64/alpine-minirootfs-3.21.3-aarch64.tar.gz \
  | tar xz -C alpine-rootfs

# Run
./sentrydarwin --rootfs alpine-rootfs /bin/sh -c 'ls /; cat /etc/os-release'

# With host networking (no root needed)
./sentrydarwin --net --rootfs alpine-rootfs /bin/sh -c 'ping -c2 8.8.8.8'
```

Or build with Bazel (handles CGo and dependencies):

```bash
bazel build --config=hvf //cmd/sentrydarwin
cp bazel-bin/cmd/sentrydarwin/sentrydarwin_/sentrydarwin .
codesign -s - --entitlements cmd/sentrydarwin/entitlements.plist -f sentrydarwin
```

### Flags

| Flag | Description |
|------|-------------|
| `--rootfs <dir>` | Host directory to use as guest root filesystem (via gofer) |
| `--net`, `--net=proxy` | Enable host networking via userspace proxy (default, no root) |
| `--net=utun` | Enable host networking via utun (requires root) |
| `--net=vmnet` | Enable host networking via socket_vmnet (no root, needs daemon) |
| `--guest-ip <ip>` | Guest IP address for vmnet mode (default: 192.168.105.100) |
| `--vmnet-socket <path>` | socket_vmnet Unix socket path (default: auto-detect) |
| `--strace` | Enable system call tracing |
| `--directfs` | Enable directfs mode (bypass lisafs RPC for host file access) |
| `--cpus <n>` | Number of vCPUs (0 = auto-detect, default) |
| `--mach-memory` | Experimental: use Mach anonymous memory for MemoryFile |
| `--keep-root` | Keep root privileges after utun setup (default: drop to SUDO_UID) |
| `--page4k` | Use 4K guest pages (default: on). Linux ARM64 standard. |
| `--profile <file>` | Write per-Switch() timing stats to file on exit. |
| `--page16k` | Use 16K guest pages (macOS native). Disables 4K mode. |

## Usage Examples

### Basic commands

```console
$ sentrydarwin --rootfs alpine-rootfs /bin/sh -c 'uname -a'
Linux gvisor-darwin 4.4.0 #1 SMP Sun Jan 10 15:06:54 PST 2016 aarch64 Linux

$ sentrydarwin --rootfs alpine-rootfs /bin/sh -c 'cat /etc/alpine-release'
3.21.3

$ sentrydarwin --rootfs alpine-rootfs /bin/sh -c 'id'
uid=0(root) gid=0(root)

$ sentrydarwin --rootfs alpine-rootfs /bin/sh -c 'ls /'
bin dev etc home lib media mnt opt proc root run sbin srv sys tmp usr var
```

### Shell scripts with fork, exec, and pipes

```console
$ sentrydarwin --rootfs alpine-rootfs /bin/sh -c '
    echo "Files in /bin: $(ls /bin | wc -l)"
    echo "Hello" | tr a-z A-Z
    seq 1 5 | awk "{s+=\$1} END{print \"Sum:\", s}"
'
Files in /bin: 82
HELLO
Sum: 15
```

### File I/O

```console
$ sentrydarwin --rootfs alpine-rootfs /bin/sh -c '
    echo "gVisor on macOS" > /tmp/test.txt
    cat /tmp/test.txt
    wc -c /tmp/test.txt
'
gVisor on macOS
16 /tmp/test.txt
```

### Host networking (no root needed)

```console
$ sentrydarwin --net --rootfs alpine-rootfs /bin/sh -c 'ping -c 2 8.8.8.8'
PING 8.8.8.8 (8.8.8.8): 56 data bytes
64 bytes from 8.8.8.8: seq=0 ttl=110 time=3.957 ms
64 bytes from 8.8.8.8: seq=1 ttl=110 time=4.123 ms
```

### Syscall tracing

```console
$ sentrydarwin --strace --rootfs alpine-rootfs /bin/sh -c 'echo hello' 2>&1 | grep -E "write|exit"
[   1:   1] sh X write(0x1, ..., 0x6) = 6
[   1:   1] sh E exit_group(0x0)
```

### Running Go binaries directly (no rootfs needed)

```console
$ sentrydarwin ./my-static-linux-arm64-binary arg1 arg2
```

## Architecture

![HVF Architecture](g3doc/architecture_guide/macos/hvf-architecture.png "HVF platform architecture on macOS.")

The port replaces gVisor's KVM platform with a Hypervisor.framework (HVF) platform. The guest runs at EL1 inside an HVF virtual machine. The sentry intercepts syscalls via exception vectors and handles them in Go, just like on Linux.

## Platform: Hypervisor.framework (HVF)

### Source Files

| File | Purpose |
|------|---------|
| `pkg/sentry/platform/hvf/hvf.go` | Platform type, constructor, `platform.Register("hvf")` |
| `pkg/sentry/platform/hvf/machine.go` | vCPU pool management, shared VM resources |
| `pkg/sentry/platform/hvf/context.go` | `Switch()` loop: run vCPU, handle exits |
| `pkg/sentry/platform/hvf/address_space.go` | Per-MM address spaces, `MapFile`/`Unmap` |
| `pkg/sentry/platform/hvf/pagetable.go` | ARM64 4-level page tables (L0-L3), COW support |
| `pkg/sentry/platform/hvf/vcpu_arm64.go` | Register save/restore, exception vectors, PSTATE translation |
| `pkg/sentry/platform/hvf/ipa_allocator.go` | IPA space management, page table page recycling |

### Guest Execution Model

```
┌──────────────────────────────────────────────────────────────┐
│                    macOS Host Process                         │
│                                                              │
│  ┌─── Sentry (Go) ──────────────────────────────────────┐   │
│  │  Switch() loop:                                       │   │
│  │    loadRegisters → hv_vcpu_run → saveRegisters        │   │
│  │    Syscall dispatch (mm, fs, net, signals)             │   │
│  └──────────────────────┬───────────────────────────────┘   │
│                          │ HVF API                           │
│  ┌─── HVF VM (ARM64) ──┴───────────────────────────────┐   │
│  │                                                       │   │
│  │  EL1: Exception Vectors (0x400)                       │   │
│  │    ├─ ESR_EL1 → EC=0x15 (SVC)?                       │   │
│  │    ├─ Fast-path table (172-178): MOVZ+ERET  ~0.1µs   │   │
│  │    ├─ Extended (124,155,156,96): ERET       ~0.1µs   │   │
│  │    └─ Other: HVC #9 → VM exit              ~4µs     │   │
│  │                                                       │   │
│  │  EL1: Dispatch Code Page (TTBR1, optional)            │   │
│  │    └─ Go asm / C compiled handlers via BLR            │   │
│  │                                                       │   │
│  │  EL1: State Page (TTBR1, per-vCPU)                    │   │
│  │    ├─ GP regs (X0-X30)     0x000                      │   │
│  │    ├─ ESR/SP/TPIDR/PC      0x100                      │   │
│  │    ├─ Signal mask           0x128                      │   │
│  │    └─ Persistent state      0x200 (pid,tid,brk,uid..) │   │
│  │                                                       │   │
│  │  EL0: Guest Application                               │   │
│  │    └─ Linux ARM64 ELF (musl/glibc)                    │   │
│  │                                                       │   │
│  │  Memory:                                              │   │
│  │    TTBR0: Per-process guest pages (4K/16K granule)    │   │
│  │    TTBR1: Kernel pages — vectors, state, dispatch     │   │
│  │           AP[1]=0 (EL1-only), global (no nG)          │   │
│  └───────────────────────────────────────────────────────┘   │
└──────────────────────────────────────────────────────────────┘
```

**Execution flow:**
1. Host loads guest registers via HVF API (loadGPRegs, ~800ns)
2. ERET stub: TLBI ASIDE1IS + ERET → drops to EL0
3. Guest runs until SVC/fault
4. EL1 handler: reads ESR_EL1, dispatches fast-path or HVC exit
5. Host saves registers (saveGPRegs, ~800ns; FP save only on faults)

**Key ARM64 state:**
- TTBR0: per-process page tables (ASID-tagged, rotated each Switch)
- TTBR1: shared kernel page table (global, vectors + state + dispatch)
- TPIDR_EL1: per-vCPU state page kernel VA
- SP_EL1: scratch stack at end of state page
- VBAR_EL1: IPA 0 (vectors page)
- ESR_EL1: readable from EL1 after SVC (EC=0x15)
- PAN: auto-set on EL0→EL1 exception (use AP[1]=0 or STTR/LDTR)

**EL1 capabilities (proven by el1memtest):**
- Data read/write via TTBR0 and TTBR1 (AP[1]=0 pages)
- ESR_EL1 readable (EC=0x15 for SVC, EC=0x24 for data abort)
- STTR/LDTR for user memory access (bypasses PAN)
- Code execution from TTBR1 pages via BLR
- ELR_EL1/SPSR_EL1/FAR_EL1 return 0 (must use API for these)

**State page layout** (16K per vCPU, at `kernelVABase + 0x4000 + id*16K`):

| Offset | Field | Description |
|--------|-------|-------------|
| 0x000 | X0-X30 | GP registers (31 × 8 = 248 bytes) |
| 0x100 | ESR_EL1 | Exception syndrome |
| 0x108 | SP_EL0 | User stack pointer |
| 0x110 | TPIDR_EL0 | User TLS pointer |
| 0x128 | sig_mask | Signal mask (synced via SignalMasker) |
| 0x130 | sig_dirty | Non-zero if EL1 modified mask |
| 0x200 | pid/tid/brk/uid/gid | Persistent per-task values |

**Vectors page layout** (16K at IPA 0, shared across all vCPUs):

```
0x000-0x07F  Current-EL SP0 sync       HVC #0
0x080-0x0FF  Current-EL SP0 IRQ        HVC #1
0x100-0x17F  Current-EL SP0 FIQ        HVC #2
0x180-0x1FF  Current-EL SP0 SError     HVC #3
0x200-0x27F  Current-EL SPx sync       MRS X18,ESR; MRS X17,FAR; HVC #4
0x280-0x37F  Current-EL SPx IRQ/FIQ/SE HVC #5/#6/#7
0x400-0x47F  Lower-EL sync (el0_sync)  ESR dispatch + fast-path table
0x480-0x5FF  Lower-EL IRQ/FIQ/SError   HVC #9/#10/#11
0x600-0x67F  Extended handler           sched_yield, getpgid, getsid,
                                        set_tid_address, slow→HVC #9
0x800        Bare ERET                  (sigreturn entry point)
0x804        Sigreturn trampoline       MOV X8,#139; SVC #0
0x810        ERET stub                  TLBI ASIDE1IS + ERET
0x828        Full TLBI stub             TLBI VMALLE1IS + ERET (ASID wrap)
```

**IPA space layout:**

```
0x00000-0x03FFF  Vectors page (16K, RX)
0x04000-0x0FFFF  Reserved
0x10000-0xFFFFF  Page table pages (PT allocator, RW)
0x1000000+       Data pages (IPA allocator, RWX)
                 Dispatch code page, state pages,
                 guest memory, shadow copies
```

### Page Tables

Sigreturn trampoline at IPA 0x804:
```asm
 0x804:  MOV X8, #139     // __NR_rt_sigreturn
 0x808:  SVC #0            // trap to sentry
```

### Copy-on-Write Fork

When `fork()` is called:

1. A new `addressSpace` with new `guestPageTable` is created
2. Parent's PMAs are marked `needCOW=true`, write permissions removed
3. Parent's writable page table entries are unmapped via `unmapASLocked`
4. Child's PMA `internalMappings` are cleared to force slow-path IO
5. Child's page table starts empty (except vectors at VA 0)

On first access by the child:
- Guest faults (no L3 entry) -> `HandleUserFault` -> `mapASLocked` -> `MapFile`
- The IPA allocator returns the existing IPA for the shared host page
- Page table entry created with AP[2]=1 (read-only) if COW

On write to a COW page:
- Permission fault -> `HandleUserFault` -> `copy-on-write break`
- New physical page allocated, data copied
- Page table entry updated with AP[2]=0 (writable)

## Gofer Filesystem

The gofer provides host directory passthrough via the lisafs protocol. An in-process lisafs server runs in a goroutine, connected to the sentry via a Unix socketpair.

![Gofer Architecture](g3doc/architecture_guide/macos/gofer-architecture.png "In-process gofer connected via Unix socketpair.")

### Linux API → macOS Mapping

Every Linux-specific syscall/API used by gVisor was mapped to a macOS equivalent. Compat files use `_darwin.go` / `_linux.go` suffixes.

#### Filesystem & File Operations

| Linux API | macOS Replacement | Notes |
|-----------|-------------------|-------|
| `/proc/self/fd/N` | `fcntl(F_GETPATH)` + `open()` | FD reopen; falls back to `dup()` for dirs |
| `O_PATH` | `O_RDONLY\|O_NONBLOCK` or `O_SYMLINK` | No O_PATH on macOS; O_SYMLINK must NOT combine with O_NOFOLLOW |
| `AT_EMPTY_PATH` | `fchown(fd)` directly | macOS doesn't support empty path |
| `readlinkat(fd, "")` | `fcntl(F_GETPATH)` + `readlink()` | Empty name not supported |
| `getdents64(2)` | `getdirentries(2)` | Different dirent format, header size=21 |
| `fallocate(2)` | `ENOTSUP` | No equivalent on macOS |
| `mknodat(2)` | `open(O_CREAT\|O_EXCL)` | Special files return ENOTSUP |
| `statx(2)` | `fstat(2)` | statx doesn't exist on macOS |
| `dup3(2)` | `dup(2)` + `fcntl(F_SETFD)` | Can't atomically dup to specific FD |
| `tee(2)` / `splice(2)` | `ENOTSUP` | Not available on macOS |
| `O_LARGEFILE` | Stripped (`darwinOpenMask`) | Maps to `O_EVTONLY` on macOS (!) |
| `STATX_*` constants | Hardcoded protocol values | Same wire format as Linux |

#### Memory & Page Management

| Linux API | macOS Replacement | Notes |
|-----------|-------------------|-------|
| `memfd_create(2)` | `tmpfile()` + `dup()` + `unlink()` | Anonymous file via temp file |
| `fallocate(PUNCH_HOLE)` | `ENOSYS` → `manuallyZero` | Caller falls back to zeroing |
| `madvise(MADV_POPULATE_WRITE)` | `ENOSYS` | Not supported |
| `madvise(MADV_HUGEPAGE)` | No-op | No THP on macOS |
| `mmap(MAP_SHARED)` (host files) | `mmap(MAP_PRIVATE)` | HVF rejects MAP_SHARED of quarantined files |
| `mmap(MAP_FIXED_NOREPLACE)` | `mmap(MAP_FIXED)` | Caller handles conflicts |
| `membarrier(2)` | Unsupported | Not available on macOS |
| Allocation alignment | `posix_memalign(16K)` | 16K pages for HVF direct mapping |

#### Synchronization & Events

| Linux API | macOS Replacement | Notes |
|-----------|-------------------|-------|
| `futex(2)` | Polling + `usleep` | Spin-then-sleep pattern |
| `eventfd(2)` | `pipe(2)` + tracking map | Read/write ends tracked |
| `epoll(2)` | `kqueue(2)` | EVFILT_READ/WRITE + EV_CLEAR |
| `ppoll(2)` | `poll(2)` | Converted timeout |
| `POLLRDHUP` | `0` (ignored) | Use POLLHUP instead |

#### Networking & Sockets

| Linux API | macOS Replacement | Notes |
|-----------|-------------------|-------|
| `accept4(2)` | `accept(2)` + `fcntl()` | Set CLOEXEC/NONBLOCK after |
| `recvmmsg(2)` | `recvmsg(2)` loop | One message at a time |
| `sendmmsg(2)` | `sendmsg(2)` loop | One message at a time |
| `SO_DOMAIN` | `getsockname(2)` | Extract address family |

#### System Info & Process

| Linux API | macOS Replacement | Notes |
|-----------|-------------------|-------|
| `seccomp(2)` | `ENOSYS` | Not supported on macOS |
| `/proc/sys/vm/mmap_min_addr` | Hardcoded `4096` | No /proc on macOS |
| `gettid(2)` | `SYS_THREAD_SELFID` | macOS thread ID syscall |
| `CLOCK_MONOTONIC=1` | `CLOCK_MONOTONIC=6` | Clock ID values differ |
| VDSO `clock_gettime` | Cross-compiled ELF VDSO (CNTVCT_EL0) | ~5ns/call via userspace counter read |
| Cgroups | Not supported | No cgroups on macOS |
| `ENODATA` (errno 61) | `ENOATTR` (errno 93) | xattr error mapping |

#### Signal Handling

| Linux API | macOS Replacement | Notes |
|-----------|-------------------|-------|
| safecopy signal handler | Mach exception ports | Bypasses PAC-signed sigreturn limitation |
| `ucontext_t.uc_mcontext` | Pointer dereference | macOS: pointer at offset 0x30 (not embedded) |
| `SIGBUS=7` | `SIGBUS=10` | Signal numbers differ |

#### Gofer Communication

| Linux API | macOS Replacement | Notes |
|-----------|-------------------|-------|
| `SOCK_SEQPACKET` | `SOCK_STREAM` | macOS AF_UNIX doesn't support SEQPACKET |
| SCM_RIGHTS (zero-length) | 1-byte dummy message | SOCK_STREAM requires data |
| `F_ADD_SEALS` (memfd) | No-op | macOS shm_open doesn't support seals |
| `SOCK_CLOEXEC` | `fcntl(F_SETFD)` | Not available as socket type flag |
| flipcall FD donation | Disabled | Hangs on macOS; use socket RPC path |

### Symlink Handling

macOS returns `ELOOP` when opening symlinks with `O_NOFOLLOW` (Linux uses `O_PATH|O_NOFOLLOW` to open the symlink itself). The gofer handles this in `Walk()` and `WalkStat()`:

1. `open(name, O_RDONLY|O_NOFOLLOW)` -> `ELOOP`
2. Fall back to `fstatat(dirfd, name, AT_SYMLINK_NOFOLLOW)` to get symlink stat
3. Return symlink stat to sentry; sentry resolves via `Readlink` + re-walk

`Readlink()` on the symlink control FD falls back to `readlink(node.FilePath())` since the control FD is a dup of the parent directory (not the symlink itself).

Note: `fstatat` must use the Go `unix.Fstatat` wrapper, not the raw `SYS_FSTATAT` syscall, because the raw syscall returns incorrect `Stat_t` fields on macOS ARM64.

### ParseDirents Fix

macOS `unix.Dirent` has a 1024-byte `Name` field, making `sizeof(Dirent)` = 1048 bytes. But actual dirents from `Getdirentries` are ~32 bytes each. The minimum size check was changed from `sizeof(Dirent)` to the header size (21 bytes) to avoid rejecting valid entries.

## flipcall / fdchannel

The lisafs protocol uses flipcall channels for parallel RPCs. These required platform-specific adaptations:

| Component | Linux | macOS | Reason |
|-----------|-------|-------|--------|
| FD channel socket | `SOCK_SEQPACKET` | `SOCK_STREAM` | macOS doesn't support SEQPACKET for AF_UNIX |
| SCM_RIGHTS data | Zero-length message | 1-byte dummy | SOCK_STREAM requires data for control message delivery |
| Memfd seals | `F_ADD_SEALS` applied | No-op | macOS shm_open doesn't support seals |
| CLOEXEC | `SOCK_CLOEXEC` flag | `fcntl(F_SETFD)` | SOCK_CLOEXEC not available as socket type flag |

The `Endpoint` struct has an `iov` field initialized by platform-specific `initIov()`. On macOS, this sets up a 1-byte iov pointing to a static `iovDummy` buffer.

## Network Stack

### Loopback

gVisor's netstack provides TCP/UDP/ICMP via a loopback interface (always available):

- IPv4 127.0.0.1/8, IPv6 ::1/128
- TCP, UDP, ICMP, ARP, raw sockets
- SACK, TTL=64, moderate receive buffer

### Host Networking (proxy — default)

The `--net` or `--net=proxy` flag enables host networking via a pure userspace
proxy. No root, no daemon, no TUN device — all traffic is forwarded through
host sockets.

**Source**: `pkg/tcpip/link/proxynet/endpoint.go`

```
Guest app → netstack → proxynet endpoint → host TCP/UDP/ICMP sockets → internet
```

- Guest IP: 10.0.2.15/24 (QEMU-style user networking)
- TCP: proxied via host `net.Dial`, full state machine with backpressure
- UDP: proxied via host `net.Dial`, per-flow sessionization
- ICMP: proxied via unprivileged SOCK_DGRAM
- DNS: forwarded to host's `/etc/resolv.conf` nameserver (default 8.8.8.8)
- No inbound connections (outbound-only, like QEMU `-net user`)

### Host Networking (utun)

The `--net` flag creates a macOS utun device and wires it into netstack for guest-to-host connectivity. Requires root.

![utun Networking](g3doc/architecture_guide/macos/utun-networking.png "Host networking via macOS utun interface.")

**Source**: `pkg/tcpip/link/utun/endpoint.go`

- Creates utun via `socket(AF_SYSTEM, SOCK_DGRAM, SYSPROTO_CONTROL)` + `CTLIOCGINFO` + `connect`
- Each packet has a 4-byte AF protocol header (AF_INET=2, AF_INET6=30)
- Dynamic IP assignment: `utunN` gets `192.168.(100+N).0/30` to avoid conflicts between multiple gVisor instances
- `configureUtun()` calls `ifconfig` to set up the host endpoint
- Read loop dispatches inbound packets to netstack via `DeliverNetworkPacket`
- Write path prepends AF header and calls `unix.Write(fd, ...)`

### Host Networking (vmnet via socket_vmnet)

The `--net=vmnet` flag connects to a [socket_vmnet](https://github.com/lima-vm/socket_vmnet) daemon for rootless host networking. No root required for the gVisor process.

**Source**: `pkg/tcpip/link/vmnet/endpoint.go`

**Setup:**

```bash
# Install socket_vmnet (one-time)
brew install socket_vmnet
sudo brew services start socket_vmnet

# Run gVisor with vmnet networking (no sudo needed)
./sentrydarwin --net=vmnet --rootfs alpine-rootfs /bin/sh -c 'ping -c1 8.8.8.8'
```

- Connects to socket_vmnet Unix socket (auto-detected from Homebrew paths)
- Exchanges length-prefixed L2 Ethernet frames (not L3 like utun)
- vmnet.framework provides NAT, DHCP gateway, and DNS
- Static guest IP (default 192.168.105.100, override with `--guest-ip`)
- Gateway derived from guest IP (last octet → .1)
- ARP handled by netstack (CapabilityResolutionRequired)
- No userspace proxy needed (vmnet handles NAT natively)

**Networking mode comparison:**

| | proxy (default) | utun | vmnet |
|--|----------------|------|-------|
| Root required | No | Yes | No (daemon is root) |
| External daemon | No | No | socket_vmnet |
| Layer | Userspace | L3 (IP) | L2 (Ethernet) |
| NAT | Host sockets | pfctl + proxy | vmnet.framework |
| Setup | None | Automatic | `brew install socket_vmnet` |
| Inbound connections | No | Yes | Yes |
| ICMP ping | Yes | Yes | Yes |

## Other macOS Adaptations

### Errno Mapping

macOS errno `ENOATTR` (93) has no Linux equivalent. It maps to Linux `ENODATA` (61) for extended attribute operations. Added in `pkg/syserr/host_darwin.go`. The `GetFilePrivileges` VFS function also checks for raw `ENOATTR` via `isErrNoData()` since in-process gofer errors bypass the lisafs protocol's errno translation.

### Page Size: Split Model (4K Guest / 16K Host)

macOS uses 16K host pages, but Linux ARM64 guests expect 4K. The port
uses a split page size model with two constants:

| Constant | Value | Layer | Purpose |
|----------|-------|-------|---------|
| `GuestPageSize` | 4096 | Syscall-facing (VMA) | mmap alignment, mprotect granularity, AT_PAGESZ |
| `PageSize` | 16384 | Host-facing (PMA, MemoryFile) | Physical allocation, HVF mapping, file I/O |

On darwin, `GuestPageSize` is set to 4K. All VMA operations (mmap
addresses, mprotect ranges, munmap boundaries) align to 4K. The PMA
layer and MemoryFile continue to allocate in 16K chunks internally.

**How the layers interact:**

```
Syscall layer (4K)          PMA layer (16K)           MemoryFile (16K)
┌──────────────┐           ┌──────────────┐          ┌──────────────┐
│ mmap addr    │──align──→ │ allocate     │──round──→│ fr.Start     │
│ aligned to   │  to 4K    │ rounds to    │  to 16K  │ aligned to   │
│ GuestPageSize│           │ PageSize     │          │ GuestPageSize│
└──────────────┘           └──────────────┘          └──────────────┘
```

**MAP_FIXED behavior:**

`MAP_FIXED` passes the 4K-aligned address directly to the kernel
without additional rounding. This is critical for programs like
FEX-Emu and glibc's `ld.so` that rely on precise ELF segment
placement. Non-fixed mappings are aligned to `GuestPageSize` by the
VMA layer.

**HandleUserFault fallback:**

When a page fault occurs, `HandleUserFault` first tries to map a full
16K range (the enclosing `PageSize`-aligned region). If this range
crosses a VMA boundary, it falls back to mapping a single 4K page at
the faulting address. This handles cases where adjacent VMAs have
different permissions or protection flags.

**PMA clamping:**

The PMA layer clamps all allocations to the containing VMA's bounds.
A 16K physical allocation that would extend past the VMA end is
truncated. This prevents one VMA's physical pages from leaking into
an adjacent VMA's address range.

**MemoryFile:**

`MemoryFile` accepts allocations aligned to `GuestPageSize` (4K). The
backing file offset arithmetic uses 4K granularity for VMA-facing
operations, while the underlying `mmap` and HVF mappings operate at
16K boundaries.

**Test results:** 74/77 Alpine tests pass with the split model,
matching the baseline pass rate.

**Stage-2 and stage-1 configuration:**

The HVF platform uses `hv_vm_config_set_ipa_granule(HV_IPA_GRANULE_4KB)`
for 4K stage-2 and `TCR_EL1.TG0=0x0` for 4K stage-1 page tables.
Sub-16K `PROT_NONE` guard pages (from musl/glibc thread stacks) are
handled by skipping VMA creation -- the absence of a page table entry
provides equivalent fault behavior.

Use `--page16k` to revert to 16K guest pages if needed.

### MAP_PRIVATE for Host Files

macOS quarantine (`com.apple.provenance` xattr) prevents `MAP_SHARED` of downloaded files. The gofer's `MapInternal` uses `MAP_PRIVATE` for host file mappings via `pkg/sentry/fsutil/mmap_compat_darwin.go`.

### MemoryFile Allocation

`pgalloc.MemoryFile` uses `MAP_SHARED` for its backing store (required for HVF memory coherency). Set in `pkg/sentry/pgalloc/pgalloc_darwin.go`.

### Gofer Sentry Package

The sentry-side gofer package (`pkg/sentry/fsimpl/gofer/`) has Linux-specific code in `directfs_inode.go` which uses `O_PATH`, `/proc/self/fd`, `AT_EMPTY_PATH`, `dup3`, `statx`, `fallocate`, `tee`. On macOS:

- `directfs_inode.go` is excluded via `//go:build linux`
- `directfs_inode_darwin.go` provides a full macOS directfs implementation with 10 adaptations:
  - `O_PATH` → `tryOpen()` with `O_RDONLY|O_NONBLOCK` then `O_SYMLINK` fallback
  - `O_SYMLINK` must NOT combine with `O_NOFOLLOW` (ELOOP on macOS)
  - `AT_EMPTY_PATH` in `fchown` → `unix.Fchown(fd)` directly
  - `Mknodat` → `Openat(O_CREAT|O_EXCL)` for regular files
  - `readlinkat(fd, "")` → `fcntl(F_GETPATH)` + `unix.Readlink(path)`
  - `unix.STATX_*` / `UTIME_OMIT` → `linux.STATX_*` / `linux.UTIME_OMIT`
  - `Stat_t.Mode` (uint16) casts to uint32
  - `Statfs_t.Namelen` → hardcoded 255 (NAME_MAX)
  - `Statfs_t.Bsize` (uint32) cast to int64
  - `doRevalidationDirectfs` uses `tryOpen` instead of `O_PATH`
- `gofer_compat_darwin.go` provides `statxSizeFast` (fstat), `dupFD` (dup+fcntl), `fallocateFile` (ENOTSUP), `teeFile` (ENOTSUP), `goferStatDevMinor/Major` (int32 cast)
- directfs mode is enabled with `--directfs` flag; bypasses lisafs RPC serialization

## Signal Forwarding & PTY

Host signals (SIGINT, SIGTERM, SIGHUP, SIGWINCH) are forwarded to the guest.
In PTY mode (interactive shell), SIGINT is delivered to the foreground
process group via the devpts TTY, and SIGWINCH updates the PTY window size.

The host terminal is set to raw mode (ISIG disabled) so all signal
characters (Ctrl+C, Ctrl+Z, Ctrl+\) pass through to the PTY's line
discipline, which handles echo, signal generation, and job control natively.

### devpts PTY Architecture

Interactive shells use a devpts PTY pair:

```
Host stdin → pumpPTY → PTY master → line discipline → PTY slave → shell
Shell → PTY slave → line discipline → PTY master → pumpPTY → host stdout
```

The PTY line discipline handles:
- Echo (ECHO flag)
- Signal generation (ISIG: Ctrl+C→SIGINT, Ctrl+Z→SIGTSTP, Ctrl+\→SIGQUIT)
- Line editing (ICANON: backspace, line buffering)
- Output processing (ONLCR: \n→\r\n)
- Job control (foreground/background process groups via TIOCSPGRP)

Non-interactive commands (`-c` flag) use host FDs directly (no PTY).

## Build

The `.bazelrc` config `hvf` enables cgo (required for Hypervisor.framework):

```
build:hvf --@io_bazel_rules_go//go/config:pure=false
```

The binary must be signed with the Hypervisor entitlement:

```xml
<key>com.apple.security.hypervisor</key>
<true/>
```

```bash
codesign --force --sign - --entitlements hvf.plist sentrydarwin
```

## Test Results

### Package Tests (Alpine 3.21, vmnet networking)

| Package | Version | Test | Result |
|---------|---------|------|--------|
| python3 | 3.12.13 | import, compute, HTTP server, hashlib, json | Pass |
| curl | - | HTTP 200, HTTPS 200 + TLS verify | Pass |
| wget | - | HTTP + HTTPS downloads (17MB+) | Pass |
| git | 2.47.3 | version, clone | Pass |
| openssh | 9.9p2 | client installed, version | Pass |
| openssl | 3.3.7 | SHA256 hash | Pass |
| bash | 5.2.37 | variable expansion, subshells | Pass |
| lua | 5.4 | math.pi | Pass |
| strace | - | syscall tracing (write) | Pass |
| nginx | 1.26.3 | installed, version | Pass |
| sqlite | - | CREATE, INSERT, SELECT | Pass |
| tar + gzip | - | create + extract archive | Pass |
| awk + sed + grep | - | text processing pipeline | Pass |
| bc | - | arbitrary precision (355/113) | Pass |
| file | - | ELF binary identification | Pass |
| jq | - | JSON parse + transform | Pass |
| bind-tools (dig) | - | DNS A record lookup | Pass |
| build-base (gcc) | - | compile + run C program | Pass |
| tree | 2.2.1 | directory listing | Pass |

Total: **19/19 packages**, 114 packages installed, 314 MiB.

### Automated Test Suite

Run: `./cmd/sentrydarwin/test.sh [rootfs_path]`

**88 tests** (86-88 pass consistently, 0-2 timing-sensitive flakes) on Alpine
3.21 with python3, jq, and GraalVM native-image. Multi-threaded programs,
TCP loopback, and subprocess.run all supported. 2 tests skipped (hashlib
crypto traps, GraalVM not installed).

| Category | Tests | Coverage |
|----------|-------|----------|
| Basic Execution | 10 | echo, uname, exit codes, id, hostname, pwd |
| Shell Features | 14 | pipes, subshell, heredoc, arithmetic, glob, loops |
| Fork/Exec | 6 | child, nested shell, wait, multiple children |
| Filesystem | 13 | /proc, /dev/null+zero+urandom+pts, mkdir, chmod, symlink, find |
| Memory | 2 | large alloc (10MB), mmap anon |
| Networking | 5 | ping IPv4+IPv6, TCP+UDP loopback (python), DNS resolve |
| Python | 8 | math, os, hashlib, json, tempfile, subprocess, threading |
| jq | 4 | parse, transform, array map, filter select |
| GraalVM | 2 | native-image hello, processor detection |
| Signals | 3 | trap TERM, trap USR1, SIGPIPE ignore |
| /proc | 7 | cpuinfo, meminfo, uptime, stat, version, filesystems |
| Text Processing | 10 | sort, uniq, wc, head, tail, cut, sed, grep, xargs, tee |
| Reliability | 5 | 20× jq, 50× true, 10× python, 1000-element pipe, 10K lines |

### Additional Manual Tests

| Test Suite | Result |
|------------|--------|
| Interactive shell (devpts PTY) | echo, Ctrl+C, Ctrl+Z, bg, fg, job control |
| Sequential exec (jq 1000×) | 1000/1000 passed (shadow pages) |
| Multi-CPU | 14 vCPUs, GOMAXPROCS=22 |
| directfs mode | 7/7 passed |
| safecopy fault recovery | 200× rapid reads, no crashes |

### Network Tests

| Test | Result |
|------|--------|
| Loopback TCP/UDP/ICMP | Pass |
| utun host networking (root) | DNS + HTTP via userspace proxy |
| vmnet host networking (rootless) | DNS + HTTP + HTTPS + ICMP |
| ICMP ping (8.8.8.8) | Pass |
| DNS resolution (nslookup, dig) | Pass |
| HTTPS + TLS verification | Pass |
| Python HTTP server + client | Pass |
| apk package install from repos | Pass (114 packages) |

## Implementation Checklist

- [x] Darwin compilation support (build tags, stubs, platform-specific code)
- [x] HVF platform (Hypervisor.framework, vCPU pool, exception vectors, ELF loader)
- [x] Memory management (16K pages, FPSIMD, direct HVF mapping, CopyOut coherency)
- [x] Multi-vCPU and signals (PSTATE translation, SIGURG preemption, up to 64 CPUs)
- [x] Network stack and rootfs passthrough (TCP/UDP/ICMP, busybox, Alpine)
- [x] Fork with COW page tables (per-MM, AP[2] permissions, ASID rotation, page recycling)
- [x] Gofer filesystem (lisafs porting, symlinks, flipcall channels, ParseDirents)
- [x] CLI and polish (signal forwarding, macsc wrapper, bazel config:hvf)
- [x] Host networking with NAT (utun, pfctl, interactive terminal)
- [x] Documentation and diagrams (README, g3doc, SVG/PNG)
- [x] Fix libcrypto hang (safecopy macOS compat, Translate clamp, GOMAXPROCS)
- [x] Fix dynamically-linked binary crashes (fallocateDecommit, MapInternal clamp)
- [x] Fix multi-vCPU TLB coherency (IPA stage-2 unmap, epoch kick, BBM, DSB ISH)
- [x] Fix sequential exec crash (shadow pages for file-backed guest memory)
- [x] VDSO clock_gettime (~5ns/call via cross-compiled ELF + CNTVCT_EL0)
- [x] safecopy via Mach exception ports (bypass PAC sigreturn limitation)
- [x] ICMP ping without raw sockets (SOCK_DGRAM + IP header stripping)
- [x] directfs mode for macOS (bypass lisafs RPC, O_PATH→O_SYMLINK adaptation)
- [x] Populate /proc/cpuinfo with Apple Silicon features
- [x] Userspace TCP/UDP proxy (replaces broken pfctl NAT return path)
- [x] Host DNS resolver integration (reads /etc/resolv.conf)
- [x] Package installation from Alpine repos (`apk add`)
- [x] Drop root privileges after utun/pfctl setup
- [x] Investigate TLB race root cause (HVF ARM64 lacks guest TLBI API + ASID TLB)
- [x] Interactive shell with TTY support (echo, Ctrl+C, Ctrl+D, ONLCR)
- [x] socket_vmnet rootless networking (L2 Ethernet via Unix socket)
- [x] Fix fchown EPERM for symlink/file creation on macOS gofer
- [x] GCC compilation inside guest (build-base package)
- [x] Python HTTP server + HTTPS client verified
- [x] 19/19 Alpine packages tested (python3, curl, git, gcc, nginx, sqlite, etc.)
- [x] Fix ptrace SYSEMU x0 clobbering on ARM64 (restore OrigR0 before ptrace stop)
- [x] gVisor-in-gVisor via PTRACE_SYSEMU (nested sentry inside HVF sentry)
- [x] 48-bit VA (256TB) via 4-level page tables (L0→L1→L2→L3, T0SZ=16)
- [x] MRS ID register patching in shadow pages (HVF hangs on ID reg reads)
- [x] runsc --version runs inside macOS sentry
- [x] Proxy networking mode (--net=proxy, zero root, zero daemon, pure userspace)
- [x] Controlling TTY for shell process (shared stdio FDs, job control)
- [x] devpts PTY for interactive shell (line discipline, echo, ONLCR, signals)
- [x] Job control via PTY (bg, fg, Ctrl+Z, Ctrl+C via foreground process group)
- [x] SIGWINCH forwarding to PTY (terminal resize)
- [x] MRS sysreg emulation PC advance fix (was infinite loop)
- [x] SCTLR_EL1 UCI/UCT for EL0 cache maintenance and CTR_EL0 access
- [x] Corrected ID register emulation values (PFR0, ISAR0/1, MIDR match Apple M4)
- [x] ICANON signal char processing fix (Ctrl+C works when read buffer has pending data)
- [x] 4K guest pages as default (IPA granule 4K, TG0=4K, AT_PAGESZ=4096)
- [x] MRS trapping report and standalone reproducer (docs/MRS-TRAPPING.md, cmd/mrstest)
- [x] EL2+TID3 investigation (TID3 ALLOWED but EL2 changes HVC routing)
- [x] GraalVM Native Image runs (AOT bypasses HotSpot assembler, 10M loop in 2ms)
- [ ] Java / JVM HotSpot (blocked by upstream bitmask immediate encoding bug)
- [x] Syscall speedup investigation: 7 approaches tested (docs/SYSCALL-SPEEDUP.md)
- [x] EL2 ESR_EL1 bridge tested — dead end (HVC from EL2 loops, ESR_EL1 hangs from EL2)
- [x] State page register batching (TTBR1): 15% speedup but 30% TLB regression
- [x] State page via vectors page (TTBR0): complete hang (RWX interaction)
- [x] Per-vCPU state page infrastructure in place (allocation, TTBR1 mapping, TPIDR_EL1)
- [x] Apple Radar template for ESR_EL1 access (docs/SYSCALL-SPEEDUP.md)
- [x] MRS ESR_EL1 from EL1 confirmed working (mrstest Test 7, May 2026)
- [x] 4K IPA granule via hv_vm_config_set_ipa_granule (stage-2 4K)
- [x] 4K guest page tables (TCR TG0=4K, parameterized L0-L3 walker)
- [x] Code review fixes: 17 bugs (vCPU race, PSTATE, IPA overflow, terminal, etc.)
- [x] Fix python hang (O(mapped) unmap for large 48-bit VA ranges)
- [x] In-VM ESR_EL1 dispatch (el0_sync classifies SVC vs fault at EL1)
- [x] 4K pages as default (matching Linux ARM64)
- [ ] State page via separate TTBR0 page (next approach for register batching)
- [x] In-VM fast-path syscalls via ERET (11 syscalls, 40x faster, 0.1µs)
- [x] Split page size model (GuestPageSize=4K, PageSize=16K, 74/77 Alpine tests)
- [x] MAP_FIXED fix for FEX-Emu/glibc ld.so (no page4KRound on Fixed)
- [x] FEX-Emu shared library loading (libstdc++, libc, libm, libgcc_s, ld.so)
- [ ] FEX-Emu COW fault loop (L3 permission fault on non-16K-aligned IPA)
- [ ] COW fork race at high concurrency (TLB coherency, needs Apple API)


## Performance

### sysbench (Apple M4 Pro)

> **Note:** 11 fast-path syscalls handled at EL1 via ERET (~0.1µs, no VM
> exit): getpid, getppid, getuid, geteuid, getgid, getegid, gettid,
> set_tid_address, sched_yield, getpgid(0), getsid(0). Other syscalls
> exit via HVC #9 (~4µs round-trip). Lazy FP save skips 32 SIMD registers
> on syscall exits (42% faster save). Dispatch code page infrastructure
> ready for Go-assembled handlers (el1sentry POC proven). EL1 data access
> + ESR_EL1 + STTR/LDTR all confirmed working.

| Benchmark | Native macOS | gVisor (lisafs) | gVisor (directfs) | Overhead |
|-----------|-------------|----------------|-------------------|----------|
| sysbench cpu (1T) | 10,450,465 ev/s | 6,389 ev/s | 6,351 ev/s | ~1,636x |
| sysbench cpu (4T) | 35,588,252 ev/s | 25,404 ev/s | 25,628 ev/s | ~1,401x |
| sysbench memory | 7,660 MiB/s | 6,012 MiB/s | 5,990 MiB/s | ~1.3x |
| sysbench mutex | 0.20s | 0.41s | 0.41s | ~2x |
| fileio seqwr | 2,429 MiB/s | 802 MiB/s | 774 MiB/s | ~3x |
| fileio seqrd | 10,186 MiB/s | 1,784 MiB/s | 1,787 MiB/s | ~6x |
| fileio rndrd | 8,670 MiB/s | 1,590 MiB/s | 1,630 MiB/s | ~5x |
| fsyncs/s | 198,992/s | 65,725/s | 63,405/s | ~3x |
| Pipe (dd 4K×100K) | 3,213 MB/s | 393 MB/s | 393 MB/s | ~8x |

lisafs and directfs show nearly identical performance because the
bottleneck is HVF VM exit overhead (~3-4µs per syscall), not the gofer
RPC layer. The sysbench CPU benchmark uses LuaJIT which makes frequent
syscalls in its inner loop — the ~1,600x overhead reflects HVC exit cost,
not raw computation speed. Memory bandwidth is near-native (~1.3x),
confirming guest computation runs efficiently at EL0. File I/O is 3-6x
native, limited by syscall overhead per I/O operation.

### Micro-benchmarks

| Metric | gVisor/macOS | Native macOS | Overhead |
|--------|-------------|-------------|----------|
| clock_gettime (VDSO) | 5 ns | 3 ns | ~1.7x |
| getpid (fast-path) | **100 ns** | 32 ns | **~3x** |
| getpid (VM exit) | 4,150 ns | 32 ns | ~130x |
| Pipe throughput (4K) | 393 MB/s | 3,213 MB/s | ~8x |

### In-VM fast-path syscalls

11 syscalls handled entirely at EL1 — no VM exit:
getpid, getppid, getuid, geteuid, getgid, getegid, gettid (table
dispatch at 0x400), sched_yield, getpgid(0), getsid(0), set_tid_address
(extended handler at 0x600).
**16M calls/second** for register-only syscalls.

### Switch() hot path breakdown (non-fast-path syscalls)

Use `--profile <file>` to collect per-Switch() timing stats.

| Component | Time | % of total | Notes |
|-----------|------|-----------|-------|
| loadRegisters | ~800 ns | 12% | Batched CGO (31 regs in 1 call) |
| hv_vcpu_run | ~4,700 ns | 73% | Apple HVF VM exit floor |
| saveRegisters | ~800 ns | 13% | Batched CGO, lazy FP (skip on SVC) |
| **Total** | **~6,400 ns** | 100% | Non-fast-path syscalls |

Fast-path syscalls bypass the entire Switch() loop (~0.1µs via ERET).
Lazy FP save: skip 32 SIMD register reads on syscall exits (save FP
only on fault exits that may trigger signal delivery).

### Optimization approaches tested

| Approach | Result | Status |
|----------|--------|--------|
| In-VM ERET fast-path | 11 syscalls at ~0.1µs | **Deployed** |
| Batched CGO register save/load | ~500ns per batch vs ~4.5µs individual | **Deployed** |
| Lazy FP save (skip on SVC) | 42% faster save path | **Deployed** |
| Go asm dispatch page (el1sentry) | C/Go compiled code at EL1 via BLR | **Proven (POC)** |
| hv_vcpu_run_until(FOREVER) | Eliminates vtimer exits | **Deployed** |
| Selective TLBI (ASIDE1IS) | ~50ns per flush, no measurable speedup | **Deployed** |
| State page STP save chain | ~3% faster best-case, net-negative under load | Disabled (TLB cost) |
| In-VM LDP load chain | 3µs slower (TTBR1 TLB cold misses) | Rejected |
| BRK/SIGTRAP signal handler | 3.1-3.6µs — same as HVF VM exit | Rejected |
| Mach exception platform | 14µs per BRK — 5x worse than HVF | Rejected |

See [docs/SYSCALL-SPEEDUP.md](docs/SYSCALL-SPEEDUP.md) for full
profiling results, research, and alternative approaches.

### Benchmarking with sysbench

```bash
# Install sysbench (requires network)
./sentrydarwin --net=vmnet --rootfs alpine-rootfs /bin/sh -c '
  apk add --no-cache -X http://dl-cdn.alpinelinux.org/alpine/edge/community sysbench
'

# CPU (single-threaded)
./sentrydarwin --rootfs alpine-rootfs /bin/sh -c 'sysbench cpu --time=5 run'

# CPU (multi-threaded)
./sentrydarwin --rootfs alpine-rootfs /bin/sh -c 'sysbench cpu --time=5 --threads=4 run'

# Memory throughput
./sentrydarwin --rootfs alpine-rootfs /bin/sh -c 'sysbench memory --time=5 run'

# Pipe throughput (measures syscall overhead)
./sentrydarwin --rootfs alpine-rootfs /bin/sh -c \
  'dd if=/dev/zero bs=4096 count=10000 2>/dev/null | dd of=/dev/null bs=4096'

# Fork/exec latency
./sentrydarwin --rootfs alpine-rootfs /bin/sh -c \
  'i=0; while [ $i -lt 100 ]; do /bin/true; i=$((i+1)); done; echo "100 fork+exec done"'
```

Run the benchmarks above and compare against native `sysbench` on the host.

## Java (GraalVM Native Image)

GraalVM native-image AOT-compiles Java applications to standalone ARM64
binaries, bypassing HotSpot's JIT assembler entirely. This avoids the
16K page size `logical_immediate_encode` bug that blocks standard JVMs.

```console
$ sentrydarwin --rootfs alpine-rootfs /usr/local/bin/hello-native
GraalVM Native Image: Hello from gVisor macOS!
Java version: 23.0.2
OS: Linux aarch64
Available processors: 14
Max memory: 1638 MB
10M loop: 2ms (sum=49999995000000)
```

### Building native-image binaries

Use podman with the GraalVM container:

```bash
# Write your Java app
cat > App.java << 'EOF'
public class App {
    public static void main(String[] args) {
        System.out.println("Hello from native-image!");
    }
}
EOF

# Build native binary (ARM64 Linux)
podman run --rm --platform linux/arm64 \
  -w /work -v .:/work --entrypoint bash \
  ghcr.io/graalvm/native-image-community:23 \
  -c 'javac App.java && native-image -o app App'

# Copy glibc runtime from container (one-time)
podman run --rm --platform linux/arm64 \
  -v ./alpine-rootfs:/out --entrypoint bash \
  ghcr.io/graalvm/native-image-community:23 \
  -c 'cp /lib/ld-linux-aarch64.so.1 /out/lib/ && \
      mkdir -p /out/lib64 && \
      cp /lib64/libc.so.6 /lib64/libz.so.1 /lib64/libgcc_s.so.1 /out/lib64/'

# Run in gVisor
cp app alpine-rootfs/usr/local/bin/
./sentrydarwin --rootfs alpine-rootfs /usr/local/bin/app
```

Note: native-image binaries are dynamically linked against glibc. Copy
`ld-linux-aarch64.so.1` and `libc.so.6` from the GraalVM container into
the Alpine rootfs. See [docs/MRS-TRAPPING.md](docs/MRS-TRAPPING.md) for
details on why standard HotSpot JVMs don't work.

## FEX-Emu (x86_64 Emulation)

FEX-Emu translates x86_64 Linux binaries to ARM64 at runtime. It runs
inside gVisor on macOS, enabling x86_64 workloads on Apple Silicon
without a full x86 VM.

**Status:** FEX loads all shared libraries and starts the interpreter.
The original string table corruption crash (`0x4700312e6f7331d9`) is
fixed. A remaining L3 permission fault loop blocks full execution --
see [docs/FEX-EMU.md](docs/FEX-EMU.md) for root cause analysis.

**What works:**

- FEX's own ELF segments load correctly (`p_align=0x10000`, no 4K overlap)
- All shared libraries load: libstdc++, libc, libm, libgcc_s, libpthread, ld.so
- glibc's `ld.so` loads correctly with the `MAP_FIXED` fix
- 106 syscalls complete successfully during startup

### Setup

```bash
# Build an Ubuntu rootfs with FEX-Emu
docker run --platform linux/arm64 --name fex-build -d ubuntu:24.04 sleep 3600
docker exec fex-build apt-get update -qq
docker exec fex-build apt-get install -y -qq software-properties-common
docker exec fex-build add-apt-repository -y ppa:fex-emu/fex
docker exec fex-build apt-get install -y -qq fex-emu-armv8.0 binutils
docker export fex-build | tar -C _tmp/ubuntu-rootfs -xf -
docker rm -f fex-build

# Create a static x86_64 test binary
docker run --platform linux/amd64 --name x86build -d gcc:latest sleep 60
docker exec x86build bash -c 'cat > /tmp/h.c << '\''EOF'\''
#include <unistd.h>
int main() { write(1, "hello x86\n", 10); return 0; }
EOF
gcc -static -o /tmp/hello_x86 /tmp/h.c'
docker cp x86build:/tmp/hello_x86 _tmp/ubuntu-rootfs/usr/local/bin/hello_x86
docker rm -f x86build

# Run FEX-Emu on gVisor
./sentrydarwin --rootfs _tmp/ubuntu-rootfs /usr/bin/FEXInterpreter /usr/local/bin/hello_x86
```

### Remaining Issue

COW break for non-16K-aligned file-backed private pages triggers an L3
permission fault loop. When the guest writes to a COW page whose
backing IPA is not 16K-aligned, the AP bit update (read-only to
read-write) does not take effect in the HVF stage-2 TLB. The guest
re-faults on the same address indefinitely. This is an Apple Silicon
HVF limitation with 4K granule page table permission upgrades.

**Workaround under investigation:** IPA remapping (allocate new IPA,
copy data, remap) instead of in-place AP bit changes for COW breaks
on non-aligned pages.

See [docs/FEX-EMU.md](docs/FEX-EMU.md) for the full investigation,
including the original crash root cause and the `page4KRound` fix.

## Nested Virtualization (gVisor-in-gVisor)

gVisor can run inside itself on macOS using the ptrace platform. The outer
sentry uses HVF to run the guest at EL0. The inner sentry uses
`PTRACE_SYSEMU` to intercept the nested guest's syscalls.

```
macOS host
  └─ sentrydarwin (HVF platform, EL0 guest)
       └─ inner sentry (PTRACE_SYSEMU)
            └─ nested guest binary
```

**Tested:** a minimal ptrace-based sentry running inside sentrydarwin
successfully intercepts and emulates syscalls (write, exit_group) for a
static ARM64 binary. PTRACE_TRACEME, PTRACE_SETOPTIONS, PTRACE_SYSEMU,
PTRACE_GETREGSET, and PTRACE_PEEKDATA all work correctly.

**Fix required:** ARM64 `PTRACE_SYSEMU` clobbers `x0` with `-ENOSYS`
before the tracer sees it. Fixed by restoring `x0` from `OrigR0` before
entering the ptrace stop (see `pkg/sentry/kernel/ptrace.go`).

**Nested test results** (macOS → HVF sentry → ptrace sentry → guest):

| Test | Result |
|------|--------|
| Static binary (hello) | Pass |
| Busybox echo, uname, cat, ls, wc, id | Pass |
| jq (dynamic linking, JSON) | Pass |
| Shell pipe (`echo \| tr`) | Pass |
| Shell for loop | Pass |
| Python3 (`sum(range(100))`) | Pass |
| awk computation | Pass |

**Reliability by GOMAXPROCS** (10 runs each):

| GOMAXPROCS | Pass rate |
|------------|-----------|
| 1 | 10/10 (100%) |
| 4 | 10/10 (100%) |
| 8 | 9/10 (90%) |
| 22 | hangs (TLB race) |

Reliable up to GOMAXPROCS=4. Above 8, the TLB coherency race under
heavy goroutine concurrency causes Go runtime crashes.

### Running upstream `runsc` (status: blocked on page size)

The sentry emulates ARM64 ID register reads (MRS trap, EC=0x18) which
allows the upstream `runsc` binary to get past its init code. However,
`runsc` is compiled for 4K pages (`PageShift=12`) while our sentry
operates with 16K pages (`PageShift=14`). The Go runtime inside `runsc`
calls `mmap(addr, 4096)` which fails with EINVAL because our sentry's
minimum mapping granularity is 16K.

**Fixed:** Three issues resolved:

1. **MRS instruction hang** — HVF traps `MRS ID_AA64MMFR0_EL1` at EL0
   and never returns. Fixed by scanning shadow-copied code pages for
   MRS instructions and patching them to MOV with the correct values.

2. **64GB VA limit** — Go runtime arena hints require addresses above
   64GB. Fixed by expanding guest page tables from 2-level (36-bit VA)
   to 4-level (48-bit VA, 256TB) via TCR_EL1 T0SZ=16.

3. **Address space detection** — runsc probes mmap to detect TASK_SIZE.
   Fixed by only rounding 4K-aligned addresses (not arbitrary ones).

```
$ ./sentrydarwin --rootfs alpine-rootfs /bin/sh -c 'runsc --version'
runsc version 0.0.0
spec: 1.1.0-rc.1

$ ./sentrydarwin --rootfs alpine-rootfs /bin/sh -c 'runsc help' | head -3
Usage: runsc <flags> <subcommand> <subcommand args>
runsc is the gVisor container runtime.

$ ./sentrydarwin --rootfs alpine-rootfs /bin/sh -c 'runsc spec'
# generates config.json successfully
```

**Remaining blocker for `runsc run`:** `clone(CLONE_VM|CLONE_VFORK)` in
the gofer forkExec path crashes with `exitsyscall: syscall frame is no
longer valid`. Go 1.26's runtime validates stack frame integrity after
syscall return, and our sentry's vfork stop/resume cycle invalidates
the saved syscall SP. Needs sentry-level fix for vfork task scheduling.

## Limitations

- **Host networking (utun)**: requires root (drops privileges after setup). Use `--net=vmnet` with socket_vmnet for rootless networking.
- **Shadow page memory**: File-backed guest pages (shared libraries, executables) use 2x memory due to anonymous shadow copies. Negligible for most workloads.
- **VDSO sub-millisecond timing**: `CNTVCT_EL0` reads inside the guest may return the same value within a single HVF execution slice. Programs that measure sub-millisecond intervals via the VDSO (e.g., `ping` RTT) show `time=0.000 ms` after the first packet. Wall clock and second-resolution timing work correctly.
- **Java / JVM**: Blocked by upstream HotSpot AArch64 assembler bug (`logical_immediate_encode` fails for 16K-derived values). Tested JDK 17, 21, 26 — all crash identically during stub generation. See [docs/MRS-TRAPPING.md](docs/MRS-TRAPPING.md).
- **Page size**: Guest uses a split model -- `GuestPageSize=4K` for VMA alignment (syscall-facing), `PageSize=16K` for PMA/MemoryFile (host-facing). HandleUserFault tries 16K ranges first and falls back to 4K at VMA boundaries. PMA allocations clamp to VMA bounds. Sub-16K PROT_NONE guard pages are handled by skipping VMA creation. Use `--page16k` for macOS-native 16K pages. See [Split Page Size Model](#page-size-split-model-4k-guest--16k-host).
- **FEX-Emu COW faults**: Non-16K-aligned file-backed private pages trigger an L3 permission fault loop during COW break. Apple Silicon HVF does not flush stale stage-2 TLB entries when AP bits change on sub-16K-aligned IPAs. Workaround: IPA remapping instead of in-place AP changes. See [docs/FEX-EMU.md](docs/FEX-EMU.md).
- **Stale vmnet packets**: When using `--net=vmnet`, ICMP replies from previous sessions may appear briefly. Clears after vmnet bridge ARP entries expire (~30s).

## TLB Coherency (HVF ARM64 Limitation)

### Problem

When multiple vCPUs run concurrently and the sentry modifies page tables (via `MapFile`/`Unmap`), other vCPUs inside `hv_vcpu_run` may retain stale TLB entries pointing to old page mappings. If the backing page is freed, zeroed, and recycled before the stale TLB entry expires, the vCPU reads corrupted data (typically zeroed or recycled stack frames), causing guest crashes.

The failure manifests as Go runtime panics: `SIGSEGV at unknown pc` (corrupted return address from stale stack data) or `split stack overflow` (stack metadata corruption).

### Root Cause

**HVF ARM64 does not implement ASID-tagged TLB.** This was confirmed by testing with `nG=0` (Global pages, no ASID tagging) vs `nG=1` (non-Global, ASID-tagged) — identical failure rates (~90%). The ARM64 `nG` bit and ASID field in `TTBR0_EL1` have no effect on HVF's TLB behavior.

Additionally, **HVF ARM64 provides no guest TLB invalidation API**. The x86 HVF has `hv_vcpu_invalidate_tlb()`, but the ARM64 API (`hv_vcpu.h`) has no equivalent. The only exit mechanism is `hv_vcpus_exit()`, which is asynchronous — the vCPU may execute 1-2 more instructions with stale TLB before actually exiting.

### Mitigations Applied

The initial mitigations (quarantine, zero-page remap, generation tracking, kickAllVCPUs) were replaced by direct TLBI at EL1. Current mitigations:

| Mitigation | Effect | Code |
|-----------|--------|------|
| Direct TLBI VMALLE1IS | Flush all stage-1 TLB on every guest entry | vectors offset 0x810 stub |
| Break-before-make | Clear old PTE before writing new one | `mapPage()` |
| DSB ISH barrier | Full inner-shareable barrier (not just store) | `ptBarrier()` |
| ASID rotation | Per-vCPU incrementing 16-bit ASID | `Switch()` in `context.go` |

Combined result: **100%** on all real-world workloads. ~1-2% failure rate remains on pathological stress tests (14 concurrent goroutines × deep recursion).

### Approaches Investigated and Rejected

| Approach | Result | Why |
|----------|--------|-----|
| `hv_vcpu_invalidate_tlb` | N/A | ARM64 HVF doesn't have this API |
| TLBI via guest stub (HVC exit) | Broken | Mini `hv_vcpu_run` for TLBI corrupts `ESR_EL1` state even with full save/restore |
| TLBI via guest stub (ERET exit) | Broken | ERET continues guest execution instead of returning to sentry |
| mprotect host TLB shootdown | Broken | mprotect on MAP_SHARED MemoryFile corrupts other mappings |
| Zero-page IPA remap | 83% (worse) | Silent zero reads cause harder-to-detect corruption |
| Unmap-before-MapFile | 62% (much worse) | Lock contention from kicking vCPUs during page fault handling |
| TTBR0 toggle (null + real) | Deadlock | Null TTBR causes infinite page fault retry |
| Global pages (nG=0) | 90% (same) | Confirms HVF ignores ASID entirely |
| kickAllVCPUs + epoch wait | 93-94% | Async hv_vcpus_exit can't guarantee immediate exit |
| kickAllVCPUs + RCU quarantine | 93% (worse) | Kick causes vCPU churn that widens race window |

### Impact

- **Pathological stress test** (14 concurrent goroutines × deep recursion forcing rapid stack growth): ~1-2% failure rate
- **Real-world workloads** (shell, apk, Go binaries, fork+exec, HTTP server): 0% failure rate (30/30, 20/20 in repeated tests)
- **Package installation** (`apk add tree less nano`): works reliably
- **Concurrent TCP** (20 connections, loopback): works reliably

### TLBI Guest Stub Investigation

Attempted to flush guest TLB by running a small TLBI stub (TLBI VMALLE1IS + DSB ISH + ISB) inside the guest via a mini `hv_vcpu_run` call before each guest entry. Every viable exit mechanism was tested:

| Exit Method | Result | Reason |
|------------|--------|--------|
| HVC #N | Corrupts ESR_EL1 | HVC modifies exception state visible to subsequent guest exceptions |
| ERET | Doesn't exit | ERET returns to guest code; hv_vcpu_run continues execution |
| WFI | Hangs | No pending interrupt; vCPU sleeps indefinitely |
| WFI + pending IRQ | Hangs | DAIF mask (I-bit) blocks IRQ delivery even with `hv_vcpu_set_pending_interrupt` |
| SMC #0 | Undefined at EL1 | HVF traps SMC as EC=0x17; corrupts vCPU resume state |
| LDR from unmapped IPA | Routes through EL1 vectors | Stage-2 fault at EL1 taken by guest exception vector, not directly to hypervisor |

**Conclusion:** No viable mechanism exists to execute guest-mode TLBI and cleanly return to the host on HVF ARM64. The hypervisor provides no clean "run N instructions and stop" primitive.

### Resolution Path

1. **Sentry-as-Ring0** (see [architecture section](#sentry-as-ring0-with-host-vmm-kvm-style-architecture)): run sentry at EL1 inside the VM, execute TLBI directly — eliminates the problem entirely
2. **Apple API additions**: `hv_vcpu_invalidate_tlb()` for ARM64, or ASID-tagged TLB in HVF's guest MMU, or synchronous `hv_vcpus_exit()`

## Sequential Exec Crash (Root Cause) — FIXED

> **Status: Fixed** via shadow pages in `ipaAllocator.mapPageShadow`.
> File-backed guest pages are copied into anonymous memory before being
> passed to `hv_vm_map`. Anonymous pages have stable physical addresses
> that macOS will not relocate. Tested: 1000× `jq` iterations, zero crashes.

### Symptom (before fix)

After ~24 invocations of `echo | jq .a` in a single sentry session, all
subsequent jq invocations crash with SIGSEGV. The threshold scales with
per-exec memory footprint:

| Binary | Threshold | Per-exec memory |
|--------|-----------|-----------------|
| `file` (+ 10MB magic.mgc) | ~3 | ~12MB |
| `jq` (+ libonig) | ~24 | ~3MB |
| `curl` (+ 13 libs) | ~31 | ~2MB |
| `nano` (+ ncurses) | ~99 | ~0.7MB |
| `tree` (musl only) | 100+ | ~0.2MB |
| `grep` (musl + pcre2) | 100+ | ~0.3MB |

Cumulative MemoryFile consumption at crash: ~65MB.

### Root Cause

**macOS silently changes the physical page backing mmap'd host VAs
without notifying HVF's stage-2 page tables.**

The guest page at VA `base+0` (file offset 0) contains musl text from
file offset `0x6C000` (27 pages shifted). The page content is from the
CORRECT file (musl) but the WRONG offset. This was confirmed by reading
the crash instruction from guest memory:

```
At PC base+0xc90:  guest reads 0xb9400001 (LDR W1,[X0,#0])
File offset 0xc90: contains 0x000000d6 (.hash table data)
File offset 0x6cc90: contains 0xb9400001 (musl .text code)
Shift: 0x6cc90 - 0xc90 = 0x6C000 = 27 × 16K pages
```

The ELF loader's `mapSegment` correctly passes file offset 0 to `MapFile`.
Our `ipaAllocator.mapPage` correctly maps the host VA for file offset 0.
But HVF's stage-2 IPA→PA translation points to the physical page that
backs file offset `0x6C000` instead of `0`. macOS changed the physical
page assignment without updating HVF.

This affects BOTH `MAP_PRIVATE` (mmapFile direct path) and `MAP_SHARED`
(MemoryFile page cache path). Neither `hv_vm_unmap+remap`, `mlock`, nor
page touches before mapping prevent it. This is a macOS/HVF kernel-level
coherency bug. Fixed by shadow-copying file-backed pages into anonymous
memory before passing to `hv_vm_map`. See [Shadow Pages Fix](#shadow-pages-fix) below.

### Investigation Summary

Extensive experiments ruled out all other causes:

| Hypothesis | Test | Result |
|-----------|------|--------|
| PT page reuse / stage-2 TLB | Disabled reuse | Same crash |
| Data IPA reuse | Forced immediate reuse | Same crash |
| hv_vm_unmap accumulation | Disabled unmap | Same crash |
| pgalloc.Allocate failure | Logged all calls | Never fails |
| mmap failure | Logged all calls | Never fails |
| LoadTaskImage failure | Logged | Never fails |
| Chunk boundary crossing | 4GB chunks | Same crash |
| MemoryFile page recycling | Disabled recycling entirely | Same crash (18) |
| msync after zeroing | Added MS_SYNC | Same crash |
| Stage-2 flush on IPA reuse | Added unmap+remap | Same crash |
| Pre-allocated 512MB file | ftruncate at start | Same crash |
| ASLR | Disabled (mmapRand=0) | Same crash |
| Go GC | GOGC=off | Same crash |
| Memory pressure | Purged to 7GB free | Same crash |
| Go data race | Built with -race | No races found |
| Multi-vCPU | --cpus 1 | Same crash |
| FD limits | defaults to infinity | N/A |

Key experiments that narrowed it down:

- **Crash instruction dump**: `0xb9400001` (LDR W1,[X0,#0]) at PC, exists at musl file offset 0x6cc90 — page shifted by 0x6C000
- **MapFile logging**: file offsets are CORRECT at map time (fr=[0x0,0x4000))
- **Both MAP_PRIVATE and MAP_SHARED crash**: forcePageCache (MemoryFile) has identical crash at same offset
- **mlock makes it WORSE** (0/200): interferes with page management
- **hv_vm_unmap+remap doesn't help**: macOS returns same stale physical page
- **Anonymous page copy breaks writes**: read/write desynchronized

### Mechanism

1. `hv_vm_map(hostVA, IPA)` creates a stage-2 entry mapping IPA to the
   physical page currently backing hostVA
2. macOS page management (compressor, deduplication, or swap) silently
   changes the physical page backing hostVA — assigning a physical page
   that belongs to a DIFFERENT virtual page (shifted by 27 pages)
3. HVF's stage-2 entry is NOT updated — it still points to the old
   physical page (which now belongs to a different file offset)
4. Guest reads through IPA → stale stage-2 → wrong physical page →
   content from file offset 0x6C000 instead of 0

This is a macOS/HVF kernel coherency bug. The macOS VM subsystem does
not notify Hypervisor.framework when it changes the physical backing
of pages that were passed to `hv_vm_map`.

### Shadow Pages Fix

The fix is a shadow page approach in `ipaAllocator`: every file-backed
guest page is copied into anonymous memory (`posix_memalign` + `memcpy`)
before being passed to `hv_vm_map`. Anonymous pages have stable physical
addresses that macOS will not relocate.

A `sentryOwnedFile` marker interface (implemented by `pgalloc.MemoryFile`)
distinguishes sentry-managed pages (mapped directly, since the sentry
writes to them and those writes must be visible to the guest) from
file-backed pages (shadow-copied to prevent PA relocation).

**Result:** 1000× `echo {} | jq .n` → zero crashes (was ~315 before fix).

**Cost:** ~2× memory for file-backed guest pages, one `memcpy` per new page.

| File | Change |
|------|--------|
| `pkg/sentry/platform/hvf/ipa_allocator.go` | `mapPageShadow()`: allocate anon page, memcpy, map to HVF |
| `pkg/sentry/platform/hvf/address_space.go` | `MapFile`: use `mapPageShadow` for non-MemoryFile pages |
| `pkg/sentry/pgalloc/pgalloc_darwin.go` | `IsSentryOwned()` marker on MemoryFile |

### Prior Approaches (before shadow pages)

Many approaches were tested before the shadow page fix was found:

| Approach | Result |
|----------|--------|
| hv_vm_unmap + hv_vm_map (re-resolve PA) | Same crash |
| mlock (pin physical pages) | Worse (0/200) |
| Touch page before hv_vm_map | Same crash |
| MAP_SHARED for mmapFile | HVF rejects (quarantine) |
| forcePageCache (copy to MemoryFile) | Same crash |
| Anonymous copy in mapPage (all pages) | Breaks sentry writes |
| Mach anonymous MemoryFile only | Crash at 0 (wrong layer) |
| Retain mmapFile chunk mappings | Same crash |
| MAP_FIXED re-mmap on every access | Same crash |

The key insight was that earlier anonymous copy attempts failed because
they copied ALL pages (including MemoryFile pages that the sentry writes
to). The shadow page fix only copies file-backed pages, leaving
MemoryFile pages directly mapped.

### Systematic Investigation

Many approaches were tested systematically. Most failed because they
either shadow-copied ALL pages (breaking sentry writes) or only fixed
one layer (MemoryFile but not gofer files). The eventual fix (see
[Shadow Pages Fix](#shadow-pages-fix)) succeeded by shadow-copying only
file-backed pages while leaving MemoryFile pages directly mapped.
Earlier failed attempts:

| Approach | Result | Why it failed |
|----------|--------|---------------|
| Anonymous copy in `ipaAllocator.mapPage` | Crash | Broke guest↔host write sync |
| Anonymous copy in `MmapCachedFile.MapInternal` | 8 | Crash on MemoryFile pages too |
| mlock on anonymous + MemoryFile pages | 0 | mlock makes it worse |
| `hv_vm_protect` re-resolve on IPA reuse | 7 | protect doesn't re-resolve PA |
| `hv_vm_unmap` + `hv_vm_map` on every reuse | 7 | Re-mapping gets same stale PA |
| No IPA reuse, no host VA cache | 8 | Rules out IPA/TLB staleness |
| Touch pages before `hv_vm_map` | 8 | Page fault doesn't fix PA |
| Pre-map MemoryFile chunks at startup (QEMU-style) | 8 | macOS changes PAs even for active mappings |
| Combined: pre-map chunks + anon copy MmapCachedFile | 8 | Both paths affected |
| Disable `fallocateDecommit` (no-op punchhole) | 0 | Broke page management |
| `--ring0` TCR swap entry stub | 0/13 | Breaks fork; no improvement without pipe |

**Key finding:** QEMU avoids this bug because it maps guest RAM once at
VM startup as a single large anonymous mmap and never unmaps individual
pages. gVisor's demand-paging model (per-page `hv_vm_map`/`hv_vm_unmap`)
triggers macOS memory management patterns that cause PA relocation.
Even QEMU-style pre-mapping of MemoryFile chunks did not help, suggesting
macOS relocates PAs even for continuously-mapped regions under memory pressure.
The final fix used the QEMU insight (anonymous memory is stable) but applied
it selectively to file-backed pages only.

## Future Ideas

### Mach Exception Backend (alternative to HVF)

The current platform uses Hypervisor.framework which runs the guest at EL1
inside a virtual machine. An alternative approach using **Mach exception
handling** was tested and **rejected** — it's 5x slower than HVF.

**machtest POC results (cmd/machtest):**
- BRK #0 → EXC_BREAKPOINT → Mach handler → thread_get_state/set_state → resume
- Mechanism works: handler reads X8, sets X0, advances PC correctly
- **Latency: 14µs per BRK round-trip** (10K iterations)
- **vs HVF: 3µs per hv_vcpu_run** — Mach is **~5x slower**

**Why it's slower:**
- Mach IPC: exception message → mach_msg → handler thread (~4µs)
- Register access: thread_get_state + thread_set_state via kernel IPC (~4µs)
- Resume: kernel restores thread context from message (~4µs)
- HVF keeps registers in CPU — no serialization/deserialization needed

This would be similar to how gVisor's `ptrace` platform works on Linux
(`PTRACE_SYSEMU`), but using macOS Mach primitives instead. However,
the 14µs latency makes it worse than HVF for all syscalls. HVF with
ERET fast-path (0.1µs for 7 syscalls, 4µs for others) is optimal.

gVisor on
macOS competitive with native Linux performance.

### Sentry-as-Ring0 with Host VMM (KVM-style architecture)

*Idea from Konstantin Bogomolov (bogomolov@google.com)*

**Implementation status:** Phase 1 (EL0/EL1 separation) is complete. Guest apps now run at EL0 with proper exception vectors, dual-TTBR page tables, and AP bits for EL0 access. VMM process skeleton and HVC exit handler are implemented. Direct TLBI and host syscall proxy stubs are ready. Full integration (actually booting sentry Go runtime at EL1) is the next milestone.

The current HVF platform runs the sentry entirely in host userspace.
Every guest syscall requires a full VM exit cycle:

```
Guest SVC → EL1 vector → HVC → hypervisor exit → sentry (Go) → VM re-enter
```

This round-trip (~4µs per syscall) dominates overhead. On the Linux KVM
platform, gVisor avoids this by running the sentry itself as the guest's
ring 0 (kernel mode), handling most syscalls without exiting the VM.

**Architecture:**

```
┌─────────────────────────── HVF VM ───────────────────────────┐
│                                                              │
│  ┌──────────────┐                                            │
│  │ Guest App    │  EL0 (user mode)                           │
│  │ (Linux ELF)  │                                            │
│  └──────┬───────┘                                            │
│         │ SVC #0 (syscall)                                   │
│         ▼                                                    │
│  ┌──────────────┐                                            │
│  │ Sentry       │  EL1 (kernel mode)                         │
│  │ (Go runtime) │  Handles syscalls directly, no VM exit     │
│  │              │  Manages page tables, signals, scheduling  │
│  └──────┬───────┘                                            │
│         │ HVC (host I/O needed)                              │
└─────────┼────────────────────────────────────────────────────┘
          ▼
┌──────────────────┐
│ VMM Process      │  Host macOS userspace
│ (thin host stub) │  Handles: file I/O, network, mmap
│                  │  Communicates via shared memory + HVC
└──────────────────┘
```

**How it works:**

1. The sentry (written in Go) runs at **EL1 inside the HVF VM** as the guest kernel
2. The guest application runs at **EL0** inside the same VM
3. Guest `SVC #0` traps to EL1 → sentry handles the syscall **in-VM** (no exit)
4. For syscalls needing host resources (file I/O, network, mmap backing), the sentry
   issues `HVC` → exits to a thin **VMM process** on the host that performs the
   operation and returns the result via shared memory
5. The VMM is minimal: just a syscall proxy + memory allocator

**Why this is fast:**

- Most syscalls (`getpid`, `clock_gettime`, `read` from page cache, `write` to pipe,
  `mmap` anonymous, signal operations) never exit the VM
- Only host I/O syscalls (`openat`, `read` from gofer, `write` to host FD) need the
  HVC→VMM round-trip
- Estimated syscall overhead: **~100ns** (EL0→EL1 trap) vs current **~4µs** (full VM exit)
- ~100x improvement for syscall-heavy workloads

**Why this fixes the TLB issue:**

- The sentry controls page tables from EL1 inside the VM
- It can execute `TLBI` instructions directly (EL1 has TLBI privileges)
- No need for external TLB flush mechanisms — the sentry IS the kernel
- Page table updates + TLBI + DSB happen atomically from the sentry's perspective

**Feasibility on HVF (confirmed):**

- EL0/EL1 separation works: guest runs at EL0 with ERET, SVC traps through lower-EL vector (HVC #8)
- Dual-TTBR works: TCR_EL1 with EPD1=0 enables both TTBR0 (guest) and TTBR1 (kernel) page tables
- AP[1] bit correctly grants EL0 access to mapped pages
- HVC #0 instruction can be used from EL1 for VMM communication (cgo inline asm stub ready)
- TLBI VMALLE1IS can be emitted via cgo inline asm (execution requires actual EL1 context)
- Go's runtime at EL1 requires: custom init (HVC instead of SVC), HVC-backed mmap, in-VM scheduler
- The `bluepill` mechanism from gVisor's KVM platform provides prior art

**Comparison with KVM platform:**

| Aspect | KVM (Linux) | HVF Ring0 (macOS) |
|--------|------------|-------------------|
| Sentry execution | KVM guest ring 0 | HVF guest EL1 |
| Guest app | KVM guest ring 3 | HVF guest EL0 |
| Syscall path | `SYSCALL` → ring 0 (no exit) | `SVC` → EL1 (no exit) |
| Host I/O | `SYSCALL` to host kernel | `HVC` to VMM process |
| TLB management | Direct `INVLPG` | Direct `TLBI` |
| Context switch | `SYSRET`/`SYSENTER` | `ERET`/`SVC` |
| Prior art | `pkg/sentry/platform/kvm/` | New implementation needed |

**Ring0 investigation status: architecture designed, partially implemented.**

| Component | File(s) | Status |
|-----------|---------|--------|
| EL0 guest execution | `vcpu_arm64.go`, `pagetable.go` | Production — SPSR=EL0t, SP_EL0, AP[1] |
| Direct TLBI at EL1 | `vcpu_arm64.go` (0x810 stub) | Production — VMALLE1IS on every entry |
| TLB quarantine removal | `ipa_allocator.go` | Production — direct TLBI replaces quarantine |
| 40-bit IPA | `hvf.go` | Production — `hv_vm_config_set_ipa_size(40)` |
| Dual-TTBR page tables | `kernel_pagetable.go` | Done — TCR_EL1 EPD1=0 |
| Flat L2 block PT | N/A | Infeasible — ARM64 16K granule has no L2 block entries; full L3 tables need 32MB |
| Ring0 vector table | N/A | Blocked — without flat PTs, faults reach EL1 and ESR_EL1 is needed to tell SVC from fault |
| Stage-2 MapFile | N/A | Blocked — requires flat PTs so page faults route through stage-2 instead of EL1 |
| VMM process | `cmd/vmm/main.go` | Skeleton — HVC exit handler |

### Ring0 Architecture (Not Feasible)

The intended architecture was:

1. **Flat L2 block page tables** (identity VA=IPA, all RWX) eliminate
   stage-1 faults. Page faults only occur at stage-2 and exit to HVF.
2. **SVC is the only exception reaching EL1**, so the handler doesn't
   need ESR_EL1 to dispatch syscalls.
3. **Fast-path dispatcher** handles identity syscalls (getpid, getuid,
   etc.) at EL1 via CMP+ERET (~100ns instead of ~4µs HVC exit).

**Why it doesn't work:**

**ARM64 16K granule does not support L2 block entries.** With T0SZ=28
(36-bit VA), the page table walk starts at L2. At L2 with 16K granule,
only table entries (pointing to L3 pages) are valid — block entries are
architecturally undefined. A flat identity mapping of 64GB would require
2048 L3 tables × 16KB = 32MB of page tables, which is prohibitive.

Without flat page tables, the EL1 handler receives both SVCs and stage-1
page faults at the 0x400 vector. Distinguishing them requires ESR_EL1,
which HVF traps at EL1 after EL0→EL1 exceptions.

### Ring0 Status

**ESR_EL1 reads from EL1 WORK** (confirmed May 2026, mrstest Test 7).
The earlier "hang" report was incorrect — it tested MRS at EL0 (which
traps to the el0_sync handler), not MRS from the EL1 handler itself.
Test 7 proves the el0_sync handler CAN read ESR_EL1 (returns EC=0x15
SVC syndrome), ELR_EL1, and FAR_EL1 after EL0→EL1 exceptions.

**ERET fast-path achieved** for 11 syscalls (~0.1µs, 40x faster):
7 table-dispatch (getpid, gettid, getuid, getgid, geteuid, getegid,
clock_gettime) + 4 extended (sched_yield, getpgid, getsid,
set_tid_address). EL1 data access through TTBR1 kernel page tables
enables reading per-vCPU state pages and dispatch code.

**What would enable more fast-path syscalls:**
1. Apple fixing EL1 data access through stage-1 (LDR/STR from TTBR0/1)
2. Apple allowing MSR SCTLR_EL1 from guest (MMU toggle for IPA access)
3. Mach exception platform (bypasses HVF entirely, ~500ns/syscall)

### What's Now Available for Ring0

1. **ESR_EL1 readable from EL1** — confirmed May 2026 (mrstest Test 7)
2. **4K IPA granule** — `hv_vm_config_set_ipa_granule(HV_IPA_GRANULE_4KB)`
   available in SDK, enables L2 block entries with TG0=4K
3. **Per-process page tables** — already working (TTBR0 per-MM)

**Still needed:**
- Per-vCPU stage-2 (for flat identity PT isolation) — not available
- Go runtime at EL1 (custom init, HVC-based syscalls)

**In-VM sysreg access (confirmed May 2026, mrstest Test 7):**

| Register | MRS at EL1 (no prior exception) | MRS at EL1 (after EL0→EL1 trap) |
|----------|-------------------------------|----------------------------------|
| TPIDR_EL1 | Works | Works |
| Memory STR | Works | Works |
| ESR_EL1 | Works | **Works** (EC=0x15 SVC syndrome) |
| ELR_EL1 | Works | Returns 0 (needs MMU-on test) |
| FAR_EL1 | Works | Returns 0 (needs MMU-on test) |

ESR_EL1 is readable from the EL1 exception handler. This enables
in-VM syscall dispatch without VM exit. The earlier "hang" report was
from mrstest Test 1 which tested MRS at EL0 (different issue).

**Path to ~100x syscall speedup:**

1. Add ESR_EL1 read + SVC dispatch in el0_sync handler (0x400)
2. Handle getpid/clock_gettime/etc at EL1, ERET back (~100ns)
3. HVC only for faults and host I/O (~4µs, but rare)

### EL2 Exploration (Nested Virtualization)

HVF supports EL2 mode (`hv_vm_config_set_el2_enabled`) since macOS 15.0. Probed
on M4 Pro (macOS 26.4) using `cmd/el2probe/main.go`:

| Finding | Result |
|---------|--------|
| EL2 supported | Yes |
| All 20 EL2 sysregs (HCR_EL2, ESR_EL2, etc.) | Readable/writable via API |
| Code execution at EL2h | Works (confirmed with MOV+HVC) |
| HCR_EL2 bit VM [0] | Allowed |
| HCR_EL2 bit RW [31] | Allowed |
| HCR_EL2 bit IMO/FMO/AMO [2,3,5] | Allowed |
| HCR_EL2 bit TWI/TWE [13,14] | Allowed |
| HCR_EL2 bit TSC [19] | Allowed |
| HCR_EL2 bit TVM [26] | Allowed |
| **HCR_EL2 bit TID3 [18]** | **Allowed** (traps ID register reads) |
| **HCR_EL2 bit TGE [27]** | **Silently dropped** |
| **HCR_EL2 bit E2H [34]** | **Silently dropped** |

**Key finding:** TID3 is ALLOWED — Apple accepts the ID register trap bit.
However, enabling EL2 changes HVC routing: EL1 HVC goes to guest EL2
(VBAR_EL2), not HVF. Would need full EL2 vector table to use TID3.
TID3 also only traps EL1→EL2, not EL0→EL2 (EL0 reads are already
UNDEFINED at EL1). See [docs/MRS-TRAPPING.md](docs/MRS-TRAPPING.md)
for full analysis and standalone reproducer (`cmd/mrstest`).

**Conclusion:** Apple blocks TGE and E2H. The sentry-at-EL2 architecture
is impossible. TID3 works but EL2 mode changes exception routing in ways
that break the current architecture. The existing EL0/EL1 approach with
HVC exits + host-side sysreg emulation remains the best option.

## Network Test Results

Tested with `--net` flag (utun + userspace TCP/UDP/ICMP proxy):

| Test | Result |
|------|--------|
| DNS resolution (8.8.8.8) | Pass |
| HTTP download (example.com) | Pass |
| HTTPS download (example.com) | Pass |
| Public IP detection (ifconfig.me) | Pass |
| APK index update (dl-cdn.alpinelinux.org) | Pass |
| 3 concurrent HTTPS downloads | Pass |
| APK package install (many files) | Pass (shadow pages fix) |
| ICMP ping | Pass (unprivileged SOCK_DGRAM) |
