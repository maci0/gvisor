#!/bin/bash
# gVisor macOS port benchmark suite
# Usage: ./cmd/sentrydarwin/bench.sh [rootfs_path]
#
# Outputs structured results in TSV format for easy parsing.
# Each line: benchmark_name<TAB>value<TAB>unit

set -e
SENTRY="${SENTRY:-./sentrydarwin}"
ROOTFS="${1:-_tmp/alpine-rootfs}"

if [ ! -x "$SENTRY" ]; then
    echo "ERROR: $SENTRY not found" >&2; exit 1
fi
if [ ! -d "$ROOTFS" ]; then
    echo "ERROR: $ROOTFS not found" >&2; exit 1
fi

run() {
    "$SENTRY" --rootfs "$ROOTFS" /bin/sh -c "$1" 2>/dev/null
}

echo "=== gVisor macOS HVF Benchmarks ==="
echo "date: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
echo "sentry: $SENTRY"
echo "rootfs: $ROOTFS"
echo ""
echo -e "benchmark\tvalue\tunit"
echo -e "---------\t-----\t----"

# 1. Syscall latency: getpid (fast-path, handled at EL1)
result=$(run "python3 -u -c '
import ctypes, time
libc = ctypes.CDLL(None)
N = 50000
t = time.monotonic()
for _ in range(N): libc.getpid()
elapsed = time.monotonic() - t
print(f\"{elapsed*1e6/N:.2f}\")
'")
echo -e "getpid_latency\t${result}\tus/call"

# 2. Syscall latency: write (requires VM exit)
result=$(run "python3 -u -c '
import os, time
N = 10000
fd = os.open(\"/dev/null\", os.O_WRONLY)
data = b\"x\"
t = time.monotonic()
for _ in range(N): os.write(fd, data)
elapsed = time.monotonic() - t
os.close(fd)
print(f\"{elapsed*1e6/N:.2f}\")
'")
echo -e "write_latency\t${result}\tus/call"

# 3. Syscall latency: clock_gettime (fast-path)
result=$(run "python3 -u -c '
import time
N = 50000
t = time.monotonic()
for _ in range(N): time.monotonic()
elapsed = time.monotonic() - t
print(f\"{elapsed*1e6/N:.2f}\")
'")
echo -e "clock_gettime_latency\t${result}\tus/call"

# 4. Fork+exec throughput (pure shell, no python)
result=$(run "
i=0; while [ \$i -lt 500 ]; do /bin/true; i=\$((i+1)); done
echo 500
")
echo -e "fork_exec_count\t${result}\t(500x /bin/true)"

# 5. Pipe throughput (4K blocks)
result=$(run "python3 -u -c '
import os, time
r, w = os.pipe()
N = 25000
data = b\"\\x00\" * 4096
pid = os.fork()
if pid == 0:
    os.close(r)
    for _ in range(N): os.write(w, data)
    os.close(w)
    os._exit(0)
os.close(w)
t = time.monotonic()
total = 0
while True:
    chunk = os.read(r, 65536)
    if not chunk: break
    total += len(chunk)
elapsed = time.monotonic() - t
os.close(r)
os.waitpid(pid, 0)
print(f\"{total/1024/1024/elapsed:.0f}\")
'")
echo -e "pipe_throughput\t${result}\tMB/s"

# 6. File I/O: sequential write
result=$(run "python3 -u -c '
import os, time
N = 10000
data = b\"A\" * 4096
fd = os.open(\"/tmp/bench_write\", os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o644)
t = time.monotonic()
for _ in range(N): os.write(fd, data)
elapsed = time.monotonic() - t
os.close(fd)
os.unlink(\"/tmp/bench_write\")
print(f\"{N*4096/1024/1024/elapsed:.1f}\")
'")
echo -e "seq_write_throughput\t${result}\tMB/s"

# 7. mmap anonymous fault rate
result=$(run "python3 -u -c '
import mmap, time, os
SIZE = 16 * 1024 * 1024  # 16MB
t = time.monotonic()
m = mmap.mmap(-1, SIZE)
for i in range(0, SIZE, 4096): m[i] = 65  # touch each page
elapsed = time.monotonic() - t
pages = SIZE // 4096
print(f\"{pages/elapsed:.0f}\")
m.close()
'")
echo -e "anon_fault_rate\t${result}\tpages/sec"

# 8. Thread creation
result=$(run "python3 -u -c '
import threading, time, os
N = 100
def noop(): pass
t = time.monotonic()
threads = [threading.Thread(target=noop) for _ in range(N)]
for th in threads: th.start()
for th in threads: th.join()
elapsed = time.monotonic() - t
print(f\"{elapsed*1000/N:.2f}\")
'")
echo -e "thread_create_latency\t${result}\tms/thread"

# 9. Context switch (two threads ping-pong via pipe)
result=$(run "python3 -u -c '
import os, threading, time
r1, w1 = os.pipe()
r2, w2 = os.pipe()
N = 5000
def pong():
    for _ in range(N):
        os.read(r1, 1)
        os.write(w2, b\"p\")
t = threading.Thread(target=pong)
t.start()
start = time.monotonic()
for _ in range(N):
    os.write(w1, b\"p\")
    os.read(r2, 1)
elapsed = time.monotonic() - start
t.join()
print(f\"{elapsed*1e6/N:.2f}\")
for fd in (r1,w1,r2,w2): os.close(fd)
'")
echo -e "ctx_switch_latency\t${result}\tus/switch"

# 10. TCP loopback throughput
result=$(run "python3 -u -c '
import socket, threading, time, os
SIZE = 1024 * 1024  # 1MB
data = b\"X\" * 65536
def server(s):
    c, _ = s.accept()
    total = 0
    while total < SIZE:
        chunk = c.recv(65536)
        if not chunk: break
        total += len(chunk)
    c.close()
s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind((\"127.0.0.1\", 0))
port = s.getsockname()[1]
s.listen(1)
t = threading.Thread(target=server, args=(s,))
t.start()
c = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
c.connect((\"127.0.0.1\", port))
start = time.monotonic()
sent = 0
while sent < SIZE:
    n = c.send(data[:min(65536, SIZE - sent)])
    sent += n
c.close()
t.join()
elapsed = time.monotonic() - start
s.close()
print(f\"{SIZE/1024/1024/elapsed:.1f}\")
'")
echo -e "tcp_loopback_throughput\t${result}\tMB/s"

echo ""
echo "=== Done ==="
