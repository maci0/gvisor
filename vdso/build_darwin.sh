#!/bin/bash
# Build the ARM64 Linux VDSO on macOS using clang cross-compilation.
# This produces an ELF shared object for the guest, not a Mach-O.
#
# Prerequisites: clang with aarch64-linux-gnu target, ld.lld
# Both are included with the Homebrew LLVM package.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_DIR="$(dirname "$SCRIPT_DIR")"
BUILD_DIR="${TMPDIR:-/tmp}/vdso-build"
OUTPUT="$SCRIPT_DIR/../pkg/sentry/loader/vdsodata/vdso_arm64.so"

# Create minimal freestanding headers for cross-compilation.
# The VDSO only needs basic integer types, struct timespec, and
# Linux syscall numbers — not full libc.
mkdir -p "$BUILD_DIR/include/asm" "$BUILD_DIR/include/sys"

cat > "$BUILD_DIR/include/stdint.h" << 'EOF'
#ifndef _STDINT_H
#define _STDINT_H
typedef signed char int8_t;
typedef short int16_t;
typedef int int32_t;
typedef long long int64_t;
typedef unsigned char uint8_t;
typedef unsigned short uint16_t;
typedef unsigned int uint32_t;
typedef unsigned long long uint64_t;
typedef unsigned long uintptr_t;
#define NULL ((void*)0)
#define nullptr 0
#endif
EOF

cat > "$BUILD_DIR/include/stddef.h" << 'EOF'
#ifndef _STDDEF_H
#define _STDDEF_H
typedef unsigned long size_t;
#define NULL ((void*)0)
#endif
EOF

cat > "$BUILD_DIR/include/time.h" << 'EOF'
#ifndef _TIME_H
#define _TIME_H
#include <stdint.h>
typedef int clockid_t;
struct timespec { int64_t tv_sec; int64_t tv_nsec; };
#define CLOCK_REALTIME 0
#define CLOCK_MONOTONIC 1
#define CLOCK_BOOTTIME 7
#define CLOCK_REALTIME_COARSE 5
#define CLOCK_MONOTONIC_COARSE 6
#define CLOCK_MONOTONIC_RAW 4
#endif
EOF

cat > "$BUILD_DIR/include/sys/time.h" << 'EOF'
#ifndef _SYS_TIME_H
#define _SYS_TIME_H
#include <stdint.h>
#include <time.h>
struct timeval { int64_t tv_sec; int64_t tv_usec; };
struct timezone { int tz_minuteswest; int tz_dsttime; };
#endif
EOF

cat > "$BUILD_DIR/include/sys/types.h" << 'EOF'
#ifndef _SYS_TYPES_H
#define _SYS_TYPES_H
#include <stdint.h>
#endif
EOF

cat > "$BUILD_DIR/include/asm/unistd.h" << 'EOF'
#ifndef _ASM_UNISTD_H
#define _ASM_UNISTD_H
#define __NR_clock_gettime 113
#define __NR_clock_getres 114
#define __NR_getcpu 168
#define __NR_rt_sigreturn 139
#endif
EOF

# Empty stubs for headers the VDSO includes but doesn't actually use.
for h in errno.h fcntl.h; do
    guard=$(echo "$h" | tr 'a-z.' 'A-Z_')
    echo "#ifndef _${guard}_H" > "$BUILD_DIR/include/$h"
    echo "#define _${guard}_H" >> "$BUILD_DIR/include/$h"
    echo "#endif" >> "$BUILD_DIR/include/$h"
done

CC_FLAGS=(
    --target=aarch64-linux-gnu
    -nostdlib -nostdinc -ffreestanding
    -fno-exceptions -fno-rtti -fPIC -O2
    -isystem "$BUILD_DIR/include"
    -I "$REPO_DIR"
    -std=c++14
)

LD_FLAGS=(
    -T "$SCRIPT_DIR/vdso_arm64.lds"
    -shared --no-undefined -Bsymbolic
    --build-id=sha1
    -z max-page-size=4096
    -z common-page-size=4096
    -soname=linux-vdso.so.1
)

echo "Building VDSO..."
clang++ "${CC_FLAGS[@]}" -c "$SCRIPT_DIR/vdso_time.cc" -o "$BUILD_DIR/vdso_time.o"
clang++ "${CC_FLAGS[@]}" -c "$SCRIPT_DIR/vdso.cc" -o "$BUILD_DIR/vdso.o"

echo "Linking VDSO..."
ld.lld "${LD_FLAGS[@]}" "$BUILD_DIR/vdso_time.o" "$BUILD_DIR/vdso.o" -o "$OUTPUT"

echo "VDSO built: $OUTPUT ($(wc -c < "$OUTPUT") bytes)"
echo "Symbols:"
llvm-nm "$OUTPUT" 2>/dev/null | grep -i "kernel" || nm "$OUTPUT" 2>/dev/null | grep -i "kernel" || true
