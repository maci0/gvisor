#!/bin/bash
# Stress test for gVisor macOS port
SENTRY="./sentrydarwin"
ROOTFS="${1:-_tmp/alpine-rootfs}"
PASS=0
FAIL=0

stress() {
    local name="$1" cmd="$2" expect="$3" count="${4:-10}" timeout="${5:-30}"
    local ok=0 fail=0
    for i in $(seq 1 $count); do
        result=$(perl -e "alarm $timeout; exec @ARGV" -- $SENTRY --rootfs "$ROOTFS" /bin/sh -c "$cmd" 2>/dev/null || true)
        if echo "$result" | grep -q "$expect"; then
            ok=$((ok+1))
        else
            fail=$((fail+1))
        fi
    done
    if [ $fail -eq 0 ]; then
        PASS=$((PASS+1))
        printf "  PASS  %s (%d/%d)\n" "$name" "$ok" "$count"
    else
        FAIL=$((FAIL+1))
        printf "  FAIL  %s (%d/%d passed)\n" "$name" "$ok" "$count"
    fi
}

echo "=== gVisor macOS Stress Tests ==="
echo "Binary: $SENTRY  Rootfs: $ROOTFS"
echo ""

echo "--- Concurrent fork stress ---"
stress "fork 100x" 'i=0; while [ $i -lt 100 ]; do /bin/true; i=$((i+1)); done; echo ok' "ok" 5 60

echo "--- Pipe stress ---"
stress "pipe 10K" 'seq 1 10000 | wc -l' "10000" 5 30

echo "--- Multi-process ---"
stress "5 concurrent shells" 'for i in 1 2 3 4 5; do (echo $i) & done; wait; echo done' "done" 10 15

echo "--- Python repeated ---"
stress "python 20x" 'i=0; while [ $i -lt 20 ]; do python3 -c "pass" 2>/dev/null; i=$((i+1)); done; echo py_ok' "py_ok" 3 60

echo "--- jq repeated ---"  
stress "jq 50x" 'i=0; while [ $i -lt 50 ]; do echo "{}" | jq . > /dev/null; i=$((i+1)); done; echo jq_ok' "jq_ok" 3 60

echo "--- Large output ---"
stress "seq 100K" 'seq 1 100000 | tail -1' "100000" 3 30

echo "--- Signal handling ---"
stress "SIGTERM 10x" 'i=0; while [ $i -lt 10 ]; do trap ":" TERM; kill -TERM $$; i=$((i+1)); done; echo sig_ok' "sig_ok" 5 15

echo ""
echo "Results: $PASS passed, $FAIL failed"
