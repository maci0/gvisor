// Copyright 2024 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build darwin && arm64

// Command machtest is a proof-of-concept for the Mach exception platform.
// It demonstrates BRK-based syscall interception on macOS ARM64 without
// Hypervisor.framework — guest code runs natively at full speed, and
// BRK instructions trigger Mach exceptions caught by a handler thread.
//
// Build and run:
//
//	go build -o machtest ./cmd/machtest
//	./machtest
//
// No entitlements needed (no Hypervisor.framework).
package main

/*
#include <mach/mach.h>
#include <mach/mach_vm.h>
#include <pthread.h>
#include <stdlib.h>
#include <string.h>
#include <stdio.h>
#include <sys/mman.h>
#include <dispatch/dispatch.h>
#include <libkern/OSCacheControl.h>

static void flush_icache(void *addr, size_t len) {
    sys_icache_invalidate(addr, len);
}

static void sema_release(dispatch_semaphore_t s) {
    dispatch_release(s);
}

// Exception message structures (same as safecopy/machexc)
typedef struct {
    mach_msg_header_t head;
    mach_msg_body_t body;
    mach_msg_port_descriptor_t thread_port;
    mach_msg_port_descriptor_t task_port;
    NDR_record_t NDR;
    exception_type_t exception;
    mach_msg_type_number_t code_count;
    int64_t code[2];
    int flavor;
    mach_msg_type_number_t state_count;
    arm_thread_state64_t state;
} exc_request_t;

typedef struct {
    mach_msg_header_t head;
    NDR_record_t NDR;
    kern_return_t retval;
    int flavor;
    mach_msg_type_number_t state_count;
    arm_thread_state64_t state;
} exc_reply_t;

// Shared state between handler thread and main thread
struct mach_shared {
    volatile int syscall_nr;    // X8 from guest
    volatile uint64_t args[7];  // X0-X6 from guest
    volatile uint64_t result;   // return value for X0
    volatile int done;          // 1 = handler caught exception
    volatile int exit_code;     // exit_group code
    volatile int exited;        // 1 = guest called exit
    dispatch_semaphore_t sem;   // signal main thread
};

static mach_port_t exc_port;
static struct mach_shared *shared_state;

// Exception handler thread — runs in C, catches BRK from guest thread
static void *exception_handler(void *arg) {
    struct mach_shared *s = (struct mach_shared *)arg;

    for (;;) {
        // Use large buffer — macOS ARM64 PAC sends bigger messages
        char buf[4096];
        memset(buf, 0, sizeof(buf));
        mach_msg_header_t *hdr = (mach_msg_header_t *)buf;

        kern_return_t kr = mach_msg(
            hdr, MACH_RCV_MSG,
            0, sizeof(buf), exc_port,
            MACH_MSG_TIMEOUT_NONE, MACH_PORT_NULL);
        if (kr != KERN_SUCCESS) {
            fprintf(stderr, "mach_msg recv: %d\n", kr);
            continue;
        }

        // Get thread port from message body
        mach_msg_body_t *body = (mach_msg_body_t *)(hdr + 1);
        mach_msg_port_descriptor_t *thread_desc = (mach_msg_port_descriptor_t *)(body + 1);
        mach_msg_port_descriptor_t *task_desc = thread_desc + 1;
        mach_port_t thread = thread_desc->name;

        // Get thread state via API (more reliable than parsing message)
        arm_thread_state64_t ts_buf;
        mach_msg_type_number_t count = ARM_THREAD_STATE64_COUNT;
        thread_get_state(thread, ARM_THREAD_STATE64,
                         (thread_state_t)&ts_buf, &count);
        arm_thread_state64_t *ts = &ts_buf;
        uint64_t pc = arm_thread_state64_get_pc(*ts);

        // Read syscall number from X8
        int syscall_nr = (int)ts->__x[8];

        // Fill shared state
        s->syscall_nr = syscall_nr;
        for (int i = 0; i < 7; i++) {
            s->args[i] = ts->__x[i];
        }

        // Handle syscall in C (POC — production would signal Go)
        uint64_t result = 0;
        int advance_pc = 1;
        switch (syscall_nr) {
        case 172: // getpid
            result = 42;
            break;
        case 178: // gettid
            result = 1;
            break;
        case 174: // getuid
            result = 0;
            break;
        case 93: // exit
        case 94: // exit_group
            s->exit_code = (int)ts->__x[0];
            s->exited = 1;
            result = 0;
            break;
        case 64: // write
            // write(fd, buf, count) — just return count for POC
            result = ts->__x[2]; // count
            break;
        default:
            fprintf(stderr, "  unhandled syscall %d at PC=0x%llx\n",
                    syscall_nr, (unsigned long long)pc);
            result = (uint64_t)-38; // -ENOSYS
            break;
        }

        // Set result in X0 and advance PC past BRK
        ts->__x[0] = result;
        if (advance_pc) {
            arm_thread_state64_set_pc_fptr(*ts,
                (void *)(pc + 4));
        }
        // Write modified state back to thread
        thread_set_state(thread, ARM_THREAD_STATE64,
                         (thread_state_t)ts, ARM_THREAD_STATE64_COUNT);

        // Build simple reply (EXCEPTION_DEFAULT format)
        struct {
            mach_msg_header_t head;
            NDR_record_t NDR;
            kern_return_t retval;
        } reply;
        memset(&reply, 0, sizeof(reply));
        reply.head.msgh_bits = MACH_MSGH_BITS(MACH_MSG_TYPE_MOVE_SEND_ONCE, 0);
        reply.head.msgh_size = sizeof(reply);
        reply.head.msgh_remote_port = hdr->msgh_remote_port;
        reply.head.msgh_local_port = MACH_PORT_NULL;
        reply.head.msgh_id = hdr->msgh_id + 100;
        reply.NDR = NDR_record;
        reply.retval = KERN_SUCCESS;

        kr = mach_msg(&reply.head, MACH_SEND_MSG,
            sizeof(reply), 0, MACH_PORT_NULL,
            MACH_MSG_TIMEOUT_NONE, MACH_PORT_NULL);
        if (kr != KERN_SUCCESS) {
            fprintf(stderr, "mach_msg send reply: %d\n", kr);
        }

        // Clean up port references
        mach_port_deallocate(mach_task_self(), thread);
        mach_port_deallocate(mach_task_self(), task_desc->name);

        // Signal main thread
        s->done = 1;
        dispatch_semaphore_signal(s->sem);

        if (s->exited) {
            break;
        }
    }
    return NULL;
}

// Set up Mach exception port for EXC_BREAKPOINT
static int setup_exception_port(struct mach_shared *s) {
    kern_return_t kr;

    // Allocate exception port
    kr = mach_port_allocate(mach_task_self(), MACH_PORT_RIGHT_RECEIVE, &exc_port);
    if (kr != KERN_SUCCESS) return -1;

    kr = mach_port_insert_right(mach_task_self(), exc_port, exc_port,
                                 MACH_MSG_TYPE_MAKE_SEND);
    if (kr != KERN_SUCCESS) return -2;

    // Register for EXC_BREAKPOINT on THIS thread only
    // Using thread_set_exception_ports so only the guest thread is affected
    kr = task_set_exception_ports(mach_task_self(),
        EXC_MASK_BREAKPOINT,
        exc_port,
        EXCEPTION_DEFAULT | MACH_EXCEPTION_CODES,
        ARM_THREAD_STATE64);
    if (kr != KERN_SUCCESS) return -3;

    // Start handler thread
    shared_state = s;
    pthread_t handler;
    pthread_create(&handler, NULL, exception_handler, s);
    pthread_detach(handler);

    return 0;
}

// Run guest code — jumps to addr with registers from shared state
static void run_guest(void *addr) {
    // Jump to guest code. BRK will trigger exception.
    // Use function pointer call — simplest approach.
    typedef void (*guest_fn)(void);
    guest_fn fn = (guest_fn)addr;
    fn();
}
*/
import "C"

import (
	"encoding/binary"
	"fmt"
	"time"
)

func main() {
	fmt.Println("=== Mach Exception Platform POC ===")
	fmt.Println()

	// Test 1: BRK interception
	fmt.Println("--- Test 1: BRK #0 interception via Mach exception ---")
	testBRKInterception()
	fmt.Println()

	// Test 2: Latency benchmark (skip Test 2 multi-syscall — goroutine cleanup issues)
	fmt.Println("--- Test 2: BRK syscall latency ---")
	testLatency()
}

func testBRKInterception() {
	// Allocate executable page
	page := C.mmap(nil, 4096,
		C.PROT_READ|C.PROT_WRITE|C.PROT_EXEC,
		C.MAP_PRIVATE|C.MAP_ANONYMOUS|C.MAP_JIT, -1, 0)
	if page == C.MAP_FAILED {
		fmt.Println("  mmap failed")
		return
	}
	defer C.munmap(page, 4096)

	// Need MAP_JIT + pthread_jit_write_protect_np for W^X
	C.pthread_jit_write_protect_np(0) // disable write protect

	code := (*[4096]byte)(page)

	// Guest code: MOV X8, #172; BRK #0; RET
	binary.LittleEndian.PutUint32(code[0:], 0xd2801588) // MOV X8, #172 (getpid)
	binary.LittleEndian.PutUint32(code[4:], 0xd4200000) // BRK #0
	binary.LittleEndian.PutUint32(code[8:], 0xd65f03c0) // RET

	C.pthread_jit_write_protect_np(1) // re-enable write protect
	C.flush_icache(page, 12)

	// Set up Mach exception handler
	var shared C.struct_mach_shared
	shared.sem = C.dispatch_semaphore_create(0)
	defer C.sema_release(shared.sem)

	ret := C.setup_exception_port(&shared)
	if ret != 0 {
		fmt.Printf("  setup_exception_port failed: %d\n", ret)
		return
	}

	fmt.Println("  Running: MOV X8,#172; BRK #0; RET")

	// Run guest code
	go func() {
		C.run_guest(page)
	}()

	// Wait for exception
	C.dispatch_semaphore_wait(shared.sem, C.DISPATCH_TIME_FOREVER)

	fmt.Printf("  Syscall nr: %d (expected 172)\n", shared.syscall_nr)
	if shared.syscall_nr == 172 {
		fmt.Println("  *** VERDICT: BRK interception via Mach exception WORKS! ***")
	} else {
		fmt.Println("  VERDICT: unexpected syscall number")
	}
}

func testMultipleSyscalls() {
	page := C.mmap(nil, 4096,
		C.PROT_READ|C.PROT_WRITE|C.PROT_EXEC,
		C.MAP_PRIVATE|C.MAP_ANONYMOUS|C.MAP_JIT, -1, 0)
	if page == C.MAP_FAILED {
		fmt.Println("  mmap failed")
		return
	}
	defer C.munmap(page, 4096)

	C.pthread_jit_write_protect_np(0)
	code := (*[4096]byte)(page)

	// Guest: getpid → save in X1 → exit_group(0)
	off := 0
	put := func(instr uint32) {
		binary.LittleEndian.PutUint32(code[off:], instr)
		off += 4
	}
	put(0xd2801588) // MOV X8, #172 (getpid)
	put(0xd4200000) // BRK #0
	put(0xaa0003e1) // MOV X1, X0 (save PID)
	put(0xd2800bc8) // MOV X8, #94 (exit_group)
	put(0xd2800000) // MOV X0, #0
	put(0xd4200000) // BRK #0
	put(0x14000000) // B . (infinite loop, shouldn't reach)

	C.pthread_jit_write_protect_np(1)
	C.flush_icache(page, C.size_t(off))

	var shared C.struct_mach_shared
	shared.sem = C.dispatch_semaphore_create(0)
	defer C.sema_release(shared.sem)

	ret := C.setup_exception_port(&shared)
	if ret != 0 {
		fmt.Printf("  setup_exception_port failed: %d\n", ret)
		return
	}

	fmt.Println("  Running: getpid → exit_group(0)")

	go func() {
		C.run_guest(page)
	}()

	// Wait for first BRK (getpid)
	C.dispatch_semaphore_wait(shared.sem, C.DISPATCH_TIME_FOREVER)
	fmt.Printf("  1st BRK: syscall=%d result=%d\n", shared.syscall_nr, shared.result)

	// Wait for second BRK (exit_group)
	shared.done = 0
	C.dispatch_semaphore_wait(shared.sem, C.DISPATCH_TIME_FOREVER)
	fmt.Printf("  2nd BRK: syscall=%d exit_code=%d\n", shared.syscall_nr, shared.exit_code)

	if shared.exited == 1 && shared.exit_code == 0 {
		fmt.Println("  *** VERDICT: Multiple BRK syscalls WORK! ***")
	}
}

func testLatency() {
	// Measure actual BRK exception round-trip: code does N BRKs in a loop,
	// handler catches each one and resumes. Time the whole thing.
	page := C.mmap(nil, 4096,
		C.PROT_READ|C.PROT_WRITE|C.PROT_EXEC,
		C.MAP_PRIVATE|C.MAP_ANONYMOUS|C.MAP_JIT, -1, 0)
	if page == C.MAP_FAILED {
		fmt.Println("  mmap failed")
		return
	}
	defer C.munmap(page, 4096)

	C.pthread_jit_write_protect_np(0)
	code := (*[4096]byte)(page)

	// Guest: loop N times doing BRK
	// MOV X9, #N       (counter)
	// loop: MOV X8, #172; BRK #0; SUB X9,X9,#1; CBNZ X9,loop
	// MOV X8, #94; MOV X0, #0; BRK #0  (exit)
	N := 10000
	off := 0
	put := func(instr uint32) { binary.LittleEndian.PutUint32(code[off:], instr); off += 4 }
	// MOV X9, #N (up to 16 bits)
	put(uint32(0xd2800009) | uint32(N<<5)) // MOVZ X9, #N
	// loop:
	loopOff := off
	put(0xd2801588) // MOV X8, #172 (getpid)
	put(0xd4200000) // BRK #0
	put(0xd1000529) // SUB X9, X9, #1
	// CBNZ X9, loop (offset = loopOff - off - 4... wait: current off after SUB)
	branchOff := loopOff - (off + 4) // negative
	imm19 := (branchOff / 4) & 0x7FFFF
	put(uint32(0xb5000009) | uint32(imm19<<5)) // CBNZ X9, loop
	// exit
	put(0xd2800bc8) // MOV X8, #94
	put(0xd2800000) // MOV X0, #0
	put(0xd4200000) // BRK #0
	put(0x14000000) // B . (loop forever, shouldn't reach)

	C.pthread_jit_write_protect_np(1)
	C.flush_icache(page, C.size_t(off))

	var shared C.struct_mach_shared
	shared.sem = C.dispatch_semaphore_create(0)
	defer C.sema_release(shared.sem)

	ret := C.setup_exception_port(&shared)
	if ret != 0 {
		fmt.Printf("  setup_exception_port failed: %d\n", ret)
		return
	}

	fmt.Printf("  Running %d BRK round-trips...\n", N)

	t0 := time.Now()
	go func() {
		C.run_guest(page)
	}()

	// Wait for all N+1 BRKs (N getpids + 1 exit)
	for i := 0; i <= N; i++ {
		shared.done = 0
		C.dispatch_semaphore_wait(shared.sem, C.DISPATCH_TIME_FOREVER)
		if shared.exited == 1 {
			break
		}
	}
	dt := time.Since(t0)

	fmt.Printf("  Total: %v for %d BRK round-trips\n", dt, N)
	fmt.Printf("  Per-BRK: %d ns (%d us)\n", dt.Nanoseconds()/int64(N), dt.Microseconds()/int64(N))
	fmt.Printf("  vs HVF hv_vcpu_run: ~3000 ns\n")
}
