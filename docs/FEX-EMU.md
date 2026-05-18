# FEX-Emu on gVisor macOS

FEX-Emu is an x86_64 emulator that translates Linux x86_64 binaries to
ARM64 at runtime. Running FEX inside gVisor on macOS enables x86_64
workloads on Apple Silicon without a full x86 virtual machine.

**Status:** FEX loads all shared libraries and starts the interpreter.
The original string table corruption crash is fixed. A remaining L3
permission fault loop on COW break blocks full execution.

| Milestone | Status |
|-----------|--------|
| FEX ELF segments load (p_align=0x10000) | Working |
| Shared libraries (libstdc++, libc, libm, libgcc_s, ld.so) | Working |
| glibc ld.so loads (MAP_FIXED fix) | Working |
| String table crash (0x4700312e6f7331d9) | Fixed |
| x86_64 code translation | Blocked (L3 permission fault loop) |

## Root Cause: The page4KRound Problem

### Original Crash

FEX-Emu crashed with a deterministic SIGSEGV at fault address
`0x4700312e6f7331d9`. The bytes at that address decode to the ASCII
string fragment `1so.1` -- a shared library name from a `.dynstr`
section being interpreted as a pointer. The crash happened every run
at the same address.

### Why It Happened

gVisor's 4K guest page support included a function `page4KRound` that
rounded mmap addresses up to the next 4K boundary. This rounding was
applied even to `MAP_FIXED` requests, which violates the MAP_FIXED
contract: the caller specifies the exact address and expects it to be
used without modification.

When glibc's `ld.so` (the dynamic linker) loads shared libraries, it
uses MAP_FIXED to place ELF segments at precise offsets relative to the
library's base address. The `p_vaddr` values in ELF program headers
determine these offsets. If the address is rounded, segments land at
wrong offsets relative to each other, corrupting cross-segment references
like string tables.

### The VMA Overlap Problem

Here is what happens when `page4KRound` shifts a MAP_FIXED address:

```
ELF program headers for libfoo.so:
  LOAD  p_vaddr=0x0000  p_memsz=0x3A00  p_flags=R    (segment 1: headers + .dynstr)
  LOAD  p_vaddr=0x4000  p_memsz=0x1800  p_flags=R+X  (segment 2: .text)
  LOAD  p_vaddr=0x6000  p_memsz=0x0C00  p_flags=R+W  (segment 3: .data + .got)

ld.so picks base = 0x7f00100000

Expected layout (correct):                Actual layout (with page4KRound):

0x7f00100000 ┌──────────┐ seg1 R         0x7f00100000 ┌──────────┐ seg1 R
             │ .dynstr  │                              │ .dynstr  │
0x7f00103A00 └──────────┘                0x7f00103A00 └──────────┘
             │ (gap)    │                              │ (gap)    │
0x7f00104000 ┌──────────┐ seg2 R+X       0x7f00105000 ┌──────────┐ seg2 R+X  <-- SHIFTED!
             │ .text    │                              │ .text    │
0x7f00105800 └──────────┘                0x7f00106800 └──────────┘
             │ (gap)    │                              │ (gap)    │
0x7f00106000 ┌──────────┐ seg3 R+W       0x7f00107000 ┌──────────┐ seg3 R+W  <-- SHIFTED!
             │ .got     │                              │ .got     │
0x7f00106C00 └──────────┘                0x7f00107C00 └──────────┘

GOT entry for "puts" at expected .got offset points to 0x7f00104XXX (.text)
But .text is now at 0x7f00105000, so the GOT points into the gap or
wrong segment. When ld.so resolves the GOT, it reads from the wrong
offset -- string table data ("1so.1") gets interpreted as a code pointer.
```

The 0x1000 shift cascades: every segment after the first lands at the
wrong offset relative to segment 1's string tables and symbol tables.
Relocations computed against the base address point to stale or
misaligned data.

### The Fix

`MAP_FIXED` now passes the caller's 4K-aligned address directly to the
kernel without additional rounding. Only non-fixed mappings are aligned
by the VMA layer. This matches Linux kernel behavior: MAP_FIXED means
"use this exact address."

**Changed behavior:**

| Request type | Before (broken) | After (fixed) |
|-------------|-----------------|---------------|
| mmap(0x7f00104000, MAP_FIXED) | Rounded to 0x7f00105000 | Used as-is: 0x7f00104000 |
| mmap(0, len, 0) | Rounded to 4K | Aligned to GuestPageSize (4K) |

## Setup

### Build an Ubuntu rootfs with FEX-Emu

```bash
docker run --platform linux/arm64 --name fex-build -d ubuntu:24.04 sleep 3600
docker exec fex-build apt-get update -qq
docker exec fex-build apt-get install -y -qq software-properties-common
docker exec fex-build add-apt-repository -y ppa:fex-emu/fex
docker exec fex-build apt-get install -y -qq fex-emu-armv8.0 binutils
docker export fex-build | tar -C _tmp/ubuntu-rootfs -xf -
docker rm -f fex-build
```

### Create an x86_64 test binary

```bash
docker run --platform linux/amd64 --name x86build -d gcc:latest sleep 60
docker exec x86build bash -c 'cat > /tmp/h.c << '\''EOF'\''
#include <unistd.h>
int main() { write(1, "hello x86\n", 10); return 0; }
EOF
gcc -static -o /tmp/hello_x86 /tmp/h.c'
docker cp x86build:/tmp/hello_x86 _tmp/ubuntu-rootfs/usr/local/bin/hello_x86
docker rm -f x86build
```

### Run FEX-Emu on gVisor

```bash
./sentrydarwin --rootfs _tmp/ubuntu-rootfs /usr/bin/FEXInterpreter /usr/local/bin/hello_x86
```

### Build the sentry

```bash
bazel build --config=hvf //cmd/sentrydarwin
cp bazel-bin/cmd/sentrydarwin/sentrydarwin_/sentrydarwin .
codesign -s - --entitlements cmd/sentrydarwin/entitlements.plist -f ./sentrydarwin
```

## Current Status

### What Works

- **FEX ELF loading:** FEX's own binary segments load correctly. The
  `p_align=0x10000` alignment means no 4K sub-page overlaps occur for
  FEX itself.
- **Shared library loading:** All runtime libraries load and relocate:
  libstdc++, libc, libm, libgcc_s, libpthread, ld-linux-aarch64.so.1.
- **glibc ld.so:** The dynamic linker loads correctly with the MAP_FIXED
  fix. Previously, the `page4KRound` shift corrupted relocations.
- **Syscalls:** 106 syscalls complete successfully during FEX startup
  (brk, mmap, mprotect, munmap, openat, close, faccessat, fstat,
  set_robust_list). `rseq` returns ENOSYS (gVisor does not implement
  restartable sequences), which FEX handles gracefully.

### Remaining Issue: L3 Permission Fault Loop

When FEX begins translating x86_64 code, it writes to COW (copy-on-write)
pages. If the backing IPA for a file-backed private page is not aligned
to 16K, the COW break triggers an L3 permission fault loop:

1. Guest writes to a COW page (AP[2]=1, read-only).
2. `HandleUserFault` performs the COW break: allocates a new physical
   page, copies data, updates the L3 PTE to AP[2]=0 (read-write).
3. The guest retries the write.
4. The write faults again with the same L3 permission fault.
5. Steps 2-4 repeat indefinitely.

**Root cause:** Apple Silicon's HVF implementation does not correctly
handle 4K granule page table permission upgrades (read-only to
read-write) via in-place AP bit modification when the IPA is not
aligned to the host's native 16K page boundary. The stage-2 TLB
retains the old read-only permission.

**Workaround under investigation:** Instead of changing AP bits
in-place for COW breaks, allocate a new IPA, copy the page data, and
remap the L3 PTE to the new IPA. This avoids the in-place permission
upgrade entirely. The cost is one extra page copy and IPA allocation
per COW break on affected pages.

## Debugging Notes

### Useful strace output

Run with `--strace` to see FEX's syscall trace:

```bash
./sentrydarwin --strace --rootfs _tmp/ubuntu-rootfs \
  /usr/bin/FEXInterpreter /usr/local/bin/hello_x86 2>&1 | head -50
```

Look for `mmap` calls with `MAP_FIXED` (flag 0x10) -- these are the
ELF segment placements that were previously broken by `page4KRound`.

### Verifying library loading

A successful FEX startup shows all libraries loaded via `openat`:

```
openat(AT_FDCWD, "/lib/aarch64-linux-gnu/libstdc++.so.6")
openat(AT_FDCWD, "/lib/aarch64-linux-gnu/libm.so.6")
openat(AT_FDCWD, "/lib/aarch64-linux-gnu/libgcc_s.so.1")
openat(AT_FDCWD, "/lib/aarch64-linux-gnu/libc.so.6")
openat(AT_FDCWD, "/lib/aarch64-linux-gnu/libpthread.so.0")
```

If any library fails to load, check that the Ubuntu rootfs was built
with the correct architecture (`--platform linux/arm64`).
