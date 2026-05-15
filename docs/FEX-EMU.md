# FEX-Emu on gVisor macOS

## Status: SIGSEGV during JIT initialization

FEX-Emu runs inside gVisor (ARM64 ELF, loads shared libraries, starts
JIT engine) but crashes with SIGSEGV during x86_64 code translation.

## Setup

```bash
# Ubuntu 24.04 ARM64 rootfs with FEX PPA:
docker create --platform linux/arm64 --name fex ubuntu:24.04 sleep infinity
docker start fex
docker exec fex bash -c '
  apt-get update -qq
  apt-get install -y -qq software-properties-common
  add-apt-repository -y ppa:fex-emu/fex
  apt-get update -qq
  apt-get install -y -qq fex-emu-armv8.2
'
docker export fex | tar xf - -C _tmp/ubuntu-rootfs
docker stop fex; docker rm fex

# Copy x86_64 test binary
cp hello_x86 _tmp/ubuntu-rootfs/usr/local/bin/

# Run
./sentrydarwin --rootfs _tmp/ubuntu-rootfs /usr/bin/FEXInterpreter /usr/local/bin/hello_x86
```

## Crash details

```
Signal 11 (SIGSEGV), fault addr: 0x4700312e6f7331d9
```

The fault address decodes to bytes containing "1so.1" — a string
fragment from a shared library name being interpreted as a pointer.
The crash is deterministic (same address every run).

## FEX syscalls observed (106 total, all successful):
- brk, mmap (anonymous + file-backed), mprotect, munmap
- openat, close, faccessat, fstat
- set_robust_list
- rseq → ENOSYS (errno=38) — gVisor doesn't implement rseq

## Root cause hypothesis

FEX's JIT engine generates ARM64 code that translates x86_64
instructions. During ELF loading of the x86_64 binary, FEX reads
metadata from mapped pages. One of these reads returns a corrupted
pointer (the "1so.1" string fragment), suggesting:

1. A `MAP_FIXED` mmap overlapped a previous mapping that FEX
   still references — gVisor's MAP_FIXED handling may differ
   from Linux in how it handles partial overlaps with existing VMAs
2. The x86_64 ELF's load addresses conflict with FEX's own JIT
   code allocation region
3. `mprotect` on overlapping ranges may not split VMAs the same
   way as Linux

## Edge cases to investigate

1. **rseq (nr=293)** — implement as no-op (return 0) instead of ENOSYS
2. **MAP_FIXED overlap** — verify gVisor correctly unmaps existing
   mappings when MAP_FIXED is used with overlapping ranges
3. **mprotect VMA splitting** — verify mprotect on partial VMA
   ranges correctly splits the VMA
4. **PROT_EXEC anonymous pages** — FEX creates JIT code pages,
   verify gVisor handles anonymous RWX correctly
5. **48-bit VA space** — FEX may need the full 48-bit VA space
   for its x86_64 address space emulation (guest uses low 47 bits)
