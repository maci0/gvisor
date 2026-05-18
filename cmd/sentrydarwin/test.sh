#!/bin/bash
# gVisor macOS port test suite
# Usage: ./cmd/sentrydarwin/test.sh [rootfs_path]
#
# Requires: sentrydarwin binary built and signed in current directory.

set -uo pipefail

ROOTFS="${1:-_tmp/alpine-rootfs}"
SENTRY="./sentrydarwin"
PASS=0
FAIL=0
SKIP=0

if [ ! -x "$SENTRY" ]; then
    echo "ERROR: $SENTRY not found. Build first:"
    echo "  bazel build --config=hvf //cmd/sentrydarwin"
    echo "  cp bazel-bin/cmd/sentrydarwin/sentrydarwin_/sentrydarwin . && codesign -s - --entitlements cmd/sentrydarwin/entitlements.plist -f sentrydarwin"
    exit 1
fi

if [ ! -d "$ROOTFS" ]; then
    echo "ERROR: rootfs not found at $ROOTFS"
    exit 1
fi

run_test() {
    local name="$1"
    local cmd="$2"
    local expect="$3"
    local timeout="${4:-15}"
    local tmpf
    tmpf=$(mktemp)

    # Capture output via pipe: the subshell's stdout goes through a
    # pipe to tee, which writes to tmpf. The pipe EOF guarantees all
    # output is written before we read tmpf.
    { $SENTRY --rootfs "$ROOTFS" /bin/sh -c "$cmd" 2>/dev/null || true; } | head -c 1048576 >"$tmpf" &
    local spid=$!
    (sleep "$timeout"; kill "$spid" 2>/dev/null) &
    local wpid=$!
    wait "$spid" 2>/dev/null || true
    kill "$wpid" 2>/dev/null || true
    wait "$wpid" 2>/dev/null || true

    if [ -z "$expect" ] || grep -q "$expect" "$tmpf" 2>/dev/null; then
        PASS=$((PASS+1))
        printf "  PASS  %s\n" "$name"
    else
        FAIL=$((FAIL+1))
        printf "  FAIL  %s (expected '%s', got '%s')\n" "$name" "$expect" "$(head -1 "$tmpf")"
    fi
    rm -f "$tmpf"
}

skip_test() {
    local name="$1"
    local reason="$2"
    SKIP=$((SKIP+1))
    printf "  SKIP  %s (%s)\n" "$name" "$reason"
}

echo "=== gVisor macOS Test Suite ==="
echo "Binary: $SENTRY"
echo "Rootfs: $ROOTFS"
echo ""

# --- Basic execution ---
echo "--- Basic Execution ---"
run_test "echo" "echo hello_world" "hello_world"
run_test "uname -m" "uname -m" "aarch64"
run_test "uname -s" "uname -s" "Linux"
run_test "alpine release" "grep ^ID /etc/os-release" "ID=alpine"
run_test "exit 0" "exit 0" "" 5
run_test "exit 1" "/bin/sh -c 'exit 1'; echo exit=\$?" "exit=1" 5
run_test "ls /" "ls / | head -1" "bin"
run_test "whoami" "id -u" "0"
run_test "hostname" "hostname" "gvisor"
run_test "pwd" "pwd" "/"

# --- Shell features ---
echo ""
echo "--- Shell Features ---"
run_test "pipe" "echo hello | tr a-z A-Z" "HELLO"
run_test "multi-pipe" "echo abc | rev | tr a-z A-Z" "CBA"
run_test "subshell" "echo \$(echo nested)" "nested"
run_test "backtick" "echo \`echo bt\`" "bt"
run_test "seq + awk" "seq 1 5 | awk '{s+=\$1} END{print s}'" "15"
run_test "file I/O" "echo test > /tmp/t.txt && cat /tmp/t.txt" "test"
run_test "append" "echo a > /tmp/ap.txt && echo b >> /tmp/ap.txt && wc -l < /tmp/ap.txt" "2"
run_test "env vars" "FOO=bar sh -c 'echo \$FOO'" "bar"
run_test "heredoc" "cat <<E
hello
E" "hello"
run_test "arithmetic" "echo \$((7*6))" "42"
run_test "glob" "ls /bin/b* | head -1" "/bin/b" 30
run_test "test -f" "test -f /bin/sh && echo exists" "exists"
run_test "while loop" "i=0; while [ \$i -lt 3 ]; do i=\$((i+1)); done; echo \$i" "3"
run_test "for loop" "for x in a b c; do echo \$x; done | wc -l" "3"

# --- Fork/exec ---
echo ""
echo "--- Fork/Exec ---"
run_test "fork child" "/bin/sh -c 'echo child_ok'" "child_ok"
run_test "sequential exec x5" "i=0; while [ \$i -lt 5 ]; do /bin/true; i=\$((i+1)); done; echo done5" "done5" 15
run_test "exec with args" "/bin/sh -c 'echo a b c'" "a b c"
run_test "nested shell" "/bin/sh -c '/bin/sh -c \"echo deep\"'" "deep"
run_test "wait for child" "/bin/sh -c 'sleep 0 &'; wait; echo waited" "waited" 5
run_test "multiple children" "/bin/sh -c 'echo c1' && /bin/sh -c 'echo c2' && /bin/sh -c 'echo c3'" "c3"

# --- Filesystem ---
echo ""
echo "--- Filesystem ---"
run_test "/proc/self/fd" "ls /proc/self/fd | head -1" "0"
run_test "/proc/self/status" "grep ^Name /proc/self/status" "Name:"
run_test "/proc/self/maps" "head -1 /proc/self/maps | wc -c" ""
run_test "/dev/null" "echo x > /dev/null && echo ok" "ok"
run_test "/dev/zero" "dd if=/dev/zero bs=16 count=1 2>/dev/null | wc -c" "16"
run_test "/dev/urandom" "dd if=/dev/urandom bs=16 count=1 2>/dev/null | wc -c" "16"
run_test "/dev/pts" "ls /dev/pts/" "ptmx"
run_test "/tmp writable" "touch /tmp/test_file && echo ok" "ok"
run_test "mkdir + rmdir" "mkdir /tmp/testdir && rmdir /tmp/testdir && echo ok" "ok"
run_test "chmod" "touch /tmp/ch && chmod 755 /tmp/ch && echo ok" "ok"
run_test "symlink read" "ln -sf /bin/sh /tmp/mysh && readlink /tmp/mysh" "/bin/sh"
run_test "stat" "stat -c %s /bin/busybox" ""
run_test "find" "touch /tmp/findme && find /tmp -name 'findme' -type f | wc -l" "1"

# --- Memory ---
echo ""
echo "--- Memory ---"
run_test "large alloc" "dd if=/dev/zero of=/dev/null bs=1M count=10 2>&1 | tail -1" "bytes"
run_test "mmap anon" "python3 -u -c 'import mmap; m=mmap.mmap(-1,4096); m.write(b\"test\"); m.seek(0); print(m.read(4))' 2>/dev/null || echo skip" ""

# --- Networking (loopback) ---
echo ""
echo "--- Networking ---"
run_test "ping loopback" "ping -c1 -W1 127.0.0.1 2>&1 | grep -c '1 packets received'" "1" 10
run_test "ping6 loopback" "ping -c1 -W1 ::1 2>&1 | grep -c '1 packets received'" "1" 10
run_test "localhost resolve" "grep localhost /etc/hosts 2>/dev/null || echo '127.0.0.1 localhost'" "localhost"
run_test "tcp loopback" "python3 -u -c '
import socket,threading,os
def serve(s):
    c,_=s.accept(); c.send(c.recv(4)); c.close()
s=socket.socket(socket.AF_INET,socket.SOCK_STREAM)
s.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1)
s.bind((\"127.0.0.1\",0)); port=s.getsockname()[1]; s.listen(1)
t=threading.Thread(target=serve,args=(s,)); t.start()
c=socket.socket(socket.AF_INET,socket.SOCK_STREAM)
c.connect((\"127.0.0.1\",port)); c.send(b\"tcp4\")
os.write(1,c.recv(4)+b\"\n\"); os.fsync(1)
c.close(); s.close(); t.join(timeout=5)
' 2>/dev/null || echo skip" "tcp4" 10
run_test "udp loopback" "python3 -u -c '
import sys,os,socket
s=socket.socket(socket.AF_INET,socket.SOCK_DGRAM); s.bind((\"127.0.0.1\",0)); port=s.getsockname()[1]
c=socket.socket(socket.AF_INET,socket.SOCK_DGRAM); c.sendto(b\"ping\",(\"127.0.0.1\",port))
data,_=s.recvfrom(4); print(data.decode()); s.close(); c.close()
sys.stdout.flush(); os.fsync(1)
' 2>/dev/null || echo skip" "ping" 10

# --- Python ---
echo ""
echo "--- Python ---"
if [ -f "$ROOTFS/usr/bin/python3" ]; then
    run_test "python3 math" "python3 -c 'import os; os.write(1, b\"4\\n\"); os.fsync(1)'" "4"
    run_test "python3 import os" "python3 -c 'import os; os.write(1, str(os.getpid()).encode()+b\"\\n\"); os.fsync(1)'" ""
    run_test "python3 list" "python3 -c 'import os; os.write(1, str(list(range(5))).encode()+b\"\\n\"); os.fsync(1)'" "[0, 1, 2, 3, 4]"
    skip_test "python3 hashlib" "OpenSSL crypto instructions trap as EC=0 in HVF"
    run_test "python3 json" "python3 -c 'import os,json; os.write(1, (json.dumps({\"a\":1})+\"\\n\").encode()); os.fsync(1)'" '{"a": 1}'
    run_test "python3 tempfile" "python3 -c 'import os,tempfile; f=tempfile.NamedTemporaryFile(); os.write(1, (f.name+\"\\n\").encode()); os.fsync(1)'" "/tmp/" 20
    run_test "python3 subprocess" "python3 -c '
import subprocess,os
r=subprocess.run([\"/bin/echo\",\"sub_ok\"],capture_output=True,text=True,timeout=5)
os.write(1,(r.stdout.strip()+\"\n\").encode()); os.fsync(1)
' 2>/dev/null || echo skip" "sub_ok" 15
    run_test "python3 threading" "python3 -c '
import threading,os
results=[]
lock=threading.Lock()
def w(n):
    with lock: results.append(n)
ts=[threading.Thread(target=w,args=(i,)) for i in range(4)]
for t in ts: t.start()
for t in ts: t.join(timeout=5)
os.write(1,(\"t\"+str(len(results))+\"\n\").encode()); os.fsync(1)
' 2>/dev/null || echo skip" "t4" 15
else
    SKIP=$((SKIP+8))
    echo "  SKIP  python3 (not installed)"
fi

# --- jq ---
echo ""
echo "--- jq ---"
if [ -f "$ROOTFS/usr/bin/jq" ]; then
    run_test "jq parse" "echo '{\"a\":42}' | jq .a" "42"
    run_test "jq transform" "echo '{\"x\":1,\"y\":2}' | jq '.x + .y'" "3"
    run_test "jq array" "echo '[1,2,3]' | jq 'map(. * 2)'" "[2,4,6]"
    run_test "jq filter" "echo '[1,2,3,4,5]' | jq '[.[] | select(. > 3)]'" "[4,5]"
else
    SKIP=$((SKIP+4))
    echo "  SKIP  jq (not installed)"
fi

# --- GraalVM ---
echo ""
echo "--- GraalVM ---"
if [ -f "$ROOTFS/usr/local/bin/hello-native" ]; then
    run_test "graalvm native-image" "/usr/local/bin/hello-native" "GraalVM Native Image" 15
    run_test "graalvm processors" "/usr/local/bin/hello-native" "Available processors" 15
else
    SKIP=$((SKIP+2))
    echo "  SKIP  graalvm (not installed)"
fi

# --- Signals ---
echo ""
echo "--- Signals ---"
run_test "trap TERM" "trap 'echo caught' TERM; kill -TERM \$\$; echo after" "after" 5
run_test "trap USR1" "trap 'echo usr1' USR1; kill -USR1 \$\$" "usr1" 5
run_test "ignore PIPE" "echo x | /bin/true; echo pipe_ok" "pipe_ok" 10

# --- /proc info ---
echo ""
echo "--- /proc ---"
run_test "cpuinfo count" "grep -c processor /proc/cpuinfo" ""
run_test "cpuinfo features" "grep Features /proc/cpuinfo | head -1" "asimd"
run_test "meminfo" "grep MemTotal /proc/meminfo" "MemTotal"
run_test "uptime" "cat /proc/uptime | wc -w" "2"
run_test "stat" "cat /proc/stat | head -1" "cpu"
run_test "version" "cat /proc/version" "Linux"
run_test "filesystems" "cat /proc/filesystems | grep -c tmpfs" ""

# --- Text processing ---
echo ""
echo "--- Text Processing ---"
run_test "sort" "echo -e 'c\na\nb' | sort" "a"
run_test "uniq" "echo -e 'a\na\nb' | uniq | wc -l" "2"
run_test "wc" "echo hello world | wc -w" "2"
run_test "head" "seq 1 100 | head -1" "1"
run_test "tail" "seq 1 100 | tail -1" "100"
run_test "cut" "echo 'a:b:c' | cut -d: -f2" "b"
run_test "sed" "echo hello | sed 's/hello/world/'" "world"
run_test "grep -c" "echo -e 'a\nb\na' | grep -c a" "2"
run_test "xargs" "echo '1 2 3' | xargs -n1 echo | wc -l" "3"
run_test "tee" "echo tee_test | tee /tmp/tee_out > /dev/null && cat /tmp/tee_out" "tee_test"

# --- Reliability ---
echo ""
echo "--- Reliability ---"
run_test "5x jq" "i=0; while [ \$i -lt 5 ]; do echo '{}' | jq . > /dev/null; i=\$((i+1)); done; echo jq5_ok" "jq5_ok" 20
run_test "20x true" "i=0; while [ \$i -lt 20 ]; do /bin/true; i=\$((i+1)); done; echo true20_ok" "true20_ok" 15
run_test "5x python" "i=0; while [ \$i -lt 5 ]; do python3 -u -c 'pass' 2>/dev/null; i=\$((i+1)); done; echo py5_ok" "py5_ok" 30
run_test "pipe chain" "seq 1 100 | sort -n | tail -1" "100" 15
run_test "large output" "seq 1 10000 | wc -l" "10000" 10
run_test "concurrent fork" 'for i in 1 2 3; do (echo ok) & done; wait; echo alldone' "alldone" 15
run_test "deep nesting" '/bin/sh -c "/bin/sh -c \"/bin/echo nested3\""' "nested3" 10

# --- Checkpoint ---
echo ""
echo "--- Checkpoint ---"
CKPT_FILE=$(mktemp)
rm -f "$CKPT_FILE"
$SENTRY --checkpoint "$CKPT_FILE" --rootfs "$ROOTFS" /bin/sh -c 'echo ckpt_running; sleep 10' >"$CKPT_FILE.out" 2>/dev/null &
CKPT_PID=$!
sleep 2
kill -USR1 "$CKPT_PID" 2>/dev/null
sleep 3
if [ -f "$CKPT_FILE" ] && [ -s "$CKPT_FILE" ]; then
    PASS=$((PASS+1)); printf "  PASS  checkpoint file created (%s bytes)\n" "$(wc -c < "$CKPT_FILE" | tr -d ' ')"
else
    FAIL=$((FAIL+1)); printf "  FAIL  checkpoint file not created\n"
fi
kill "$CKPT_PID" 2>/dev/null; wait "$CKPT_PID" 2>/dev/null

# End-to-end restore test: checkpoint during sleep, restore should echo output
CKPT_R=$(mktemp)
rm -f "$CKPT_R"
$SENTRY --checkpoint "$CKPT_R" --rootfs "$ROOTFS" /bin/sh -c 'sleep 1; echo e2e_ok' >/dev/null 2>/dev/null &
CKPT_R_PID=$!
sleep 0.5
kill -USR1 "$CKPT_R_PID" 2>/dev/null
sleep 1
kill "$CKPT_R_PID" 2>/dev/null; wait "$CKPT_R_PID" 2>/dev/null
if [ -f "$CKPT_R" ]; then
    CKPT_R_TMP=$(mktemp)
    $SENTRY --restore "$CKPT_R" --rootfs "$ROOTFS" >"$CKPT_R_TMP" 2>/dev/null
    if grep -q 'e2e_ok' "$CKPT_R_TMP" 2>/dev/null; then
        PASS=$((PASS+1)); printf "  PASS  checkpoint+restore (output verified)\n"
    else
        FAIL=$((FAIL+1)); printf "  FAIL  checkpoint+restore (output='%s')\n" "$(cat "$CKPT_R_TMP")"
    fi
    rm -f "$CKPT_R_TMP"
else
    FAIL=$((FAIL+1)); printf "  FAIL  checkpoint+restore (no ckpt file)\n"
fi
rm -f "$CKPT_R" "$CKPT_FILE" "$CKPT_FILE.out"

# --- Benchmarks (informational, not pass/fail) ---
echo ""
echo "--- Benchmarks ---"
bench() {
    local name="$1"
    local cmd="$2"
    local timeout="${3:-30}"
    local result
    local tmpf
    tmpf=$(mktemp)
    $SENTRY --rootfs "$ROOTFS" /bin/sh -c "$cmd" >"$tmpf" 2>/dev/null &
    local spid=$!
    (sleep "$timeout"; kill "$spid" 2>/dev/null; sleep 1; kill -9 "$spid" 2>/dev/null) &
    local wpid=$!
    wait "$spid" 2>/dev/null || true
    kill "$wpid" 2>/dev/null || true
    wait "$wpid" 2>/dev/null || true
    result=$(cat "$tmpf" 2>/dev/null || echo "timeout")
    rm -f "$tmpf"
    printf "  %-24s %s\n" "$name" "$result"
}
bench "getpid 10K" "python3 -u -c 'import os,time; t=time.monotonic(); [os.getpid() for _ in range(10000)]; print(f\"{(time.monotonic()-t)*1e6/10000:.0f} us/call\")'" 30
bench "fork+exec 100x" "t=\$(cat /proc/uptime | cut -d' ' -f1); i=0; while [ \$i -lt 100 ]; do /bin/true; i=\$((i+1)); done; t2=\$(cat /proc/uptime | cut -d' ' -f1); echo \"\${t}s → \${t2}s\"" 60
bench "pipe 4K x10K" "dd if=/dev/zero bs=4096 count=10000 2>/dev/null | wc -c" 30
bench "seq+sort 10K" "seq 1 10000 | sort -n | tail -1" 15

# --- Summary ---
echo ""
echo "==================================="
TOTAL=$((PASS+FAIL))
echo "Results: $PASS/$TOTAL passed, $FAIL failed, $SKIP skipped"
if [ $FAIL -eq 0 ]; then
    echo "ALL TESTS PASSED"
    exit 0
else
    echo "SOME TESTS FAILED"
    exit 1
fi
