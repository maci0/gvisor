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

// Package machexc provides Mach exception port handling for safecopy
// fault recovery on macOS ARM64.
//
// macOS ARM64 sigreturn uses PAC-signed return addresses, preventing
// signal handlers from modifying mcontext registers to redirect
// execution. Mach exception ports bypass this limitation: a separate
// thread receives EXC_BAD_ACCESS exceptions and uses
// thread_get_state/thread_set_state to modify the faulting thread's
// registers directly.
package machexc

/*
#include <mach/mach.h>
#include <mach/mach_types.h>
#include <pthread.h>
#include <stdint.h>
#include <string.h>

// Maximum number of fault ranges we track.
#define MAX_FAULT_RANGES 16

typedef struct {
    uint64_t begin;
    uint64_t end;
    uint64_t handler;
} fault_range_t;

static fault_range_t fault_ranges[MAX_FAULT_RANGES];
static int num_fault_ranges = 0;
static mach_port_t exception_port = MACH_PORT_NULL;

static void add_fault_range(uint64_t begin, uint64_t end, uint64_t handler) {
    if (num_fault_ranges < MAX_FAULT_RANGES) {
        fault_ranges[num_fault_ranges].begin = begin;
        fault_ranges[num_fault_ranges].end = end;
        fault_ranges[num_fault_ranges].handler = handler;
        num_fault_ranges++;
    }
}

static uint64_t find_handler(uint64_t pc) {
    for (int i = 0; i < num_fault_ranges; i++) {
        if (pc >= fault_ranges[i].begin && pc < fault_ranges[i].end) {
            return fault_ranges[i].handler;
        }
    }
    return 0;
}

// exc_server_thread is the Mach exception handler thread.
// It receives EXC_BAD_ACCESS messages, looks up the faulting PC
// in the registered fault ranges, and redirects execution to the
// appropriate fault handler via thread_set_state.
static void *exc_server_thread(void *arg) {
    (void)arg;

    // Buffer for receiving exception messages.
    // mach_msg_header_t + exception body + thread state.
    struct {
        mach_msg_header_t head;
        mach_msg_body_t body;
        mach_msg_port_descriptor_t thread_port;
        mach_msg_port_descriptor_t task_port;
        NDR_record_t ndr;
        exception_type_t exception;
        mach_msg_type_number_t code_count;
        int64_t code[2];
        int flavor;
        mach_msg_type_number_t state_count;
        arm_thread_state64_t state;
        mach_msg_trailer_t trailer;
    } request;

    struct {
        mach_msg_header_t head;
        NDR_record_t ndr;
        kern_return_t ret_code;
        int flavor;
        mach_msg_type_number_t state_count;
        arm_thread_state64_t state;
    } reply;

    for (;;) {
        // Receive exception message.
        mach_msg_return_t mr = mach_msg(
            &request.head,
            MACH_RCV_MSG | MACH_RCV_LARGE,
            0,
            sizeof(request),
            exception_port,
            MACH_MSG_TIMEOUT_NONE,
            MACH_PORT_NULL);

        if (mr != MACH_MSG_SUCCESS) {
            continue;
        }

        // Extract the faulting thread and its state.
        mach_port_t thread = request.thread_port.name;

        // Get thread state.
        arm_thread_state64_t ts;
        mach_msg_type_number_t count = ARM_THREAD_STATE64_COUNT;
        kern_return_t kr = thread_get_state(
            thread,
            ARM_THREAD_STATE64,
            (thread_state_t)&ts,
            &count);

        if (kr != KERN_SUCCESS) {
            // Can't get state — forward to default handler by not replying.
            mach_port_deallocate(mach_task_self(), thread);
            mach_port_deallocate(mach_task_self(), request.task_port.name);
            continue;
        }

        // Check if the PC is in a registered fault range.
        uint64_t pc = arm_thread_state64_get_pc(ts);
        uint64_t handler = find_handler(pc);

        if (handler == 0) {
            // Not our fault — forward to default handler.
            // Reply with KERN_FAILURE to let the kernel deliver SIGSEGV/SIGBUS.
            memset(&reply, 0, sizeof(reply));
            reply.head.msgh_bits = MACH_MSGH_BITS(MACH_MSGH_BITS_REMOTE(request.head.msgh_bits), 0);
            reply.head.msgh_size = sizeof(reply);
            reply.head.msgh_remote_port = request.head.msgh_remote_port;
            reply.head.msgh_local_port = MACH_PORT_NULL;
            reply.head.msgh_id = request.head.msgh_id + 100;
            reply.ndr = NDR_record;
            reply.ret_code = KERN_FAILURE;
            reply.flavor = ARM_THREAD_STATE64;
            reply.state_count = ARM_THREAD_STATE64_COUNT;
            reply.state = ts;
            mach_msg(&reply.head, MACH_SEND_MSG, sizeof(reply), 0,
                     MACH_PORT_NULL, MACH_MSG_TIMEOUT_NONE, MACH_PORT_NULL);
            mach_port_deallocate(mach_task_self(), thread);
            mach_port_deallocate(mach_task_self(), request.task_port.name);
            continue;
        }

        // Redirect execution: set PC to fault handler, R0 to fault address,
        // R1 to signal number (SIGSEGV=11).
        uint64_t fault_addr = (uint64_t)request.code[1];
        arm_thread_state64_set_pc_fptr(ts, (void *)handler);
        ts.__x[0] = fault_addr;
        ts.__x[1] = 11; // SIGSEGV

        kr = thread_set_state(thread, ARM_THREAD_STATE64,
                              (thread_state_t)&ts, ARM_THREAD_STATE64_COUNT);

        // Reply with success + modified state.
        memset(&reply, 0, sizeof(reply));
        reply.head.msgh_bits = MACH_MSGH_BITS(MACH_MSGH_BITS_REMOTE(request.head.msgh_bits), 0);
        reply.head.msgh_size = sizeof(reply);
        reply.head.msgh_remote_port = request.head.msgh_remote_port;
        reply.head.msgh_local_port = MACH_PORT_NULL;
        reply.head.msgh_id = request.head.msgh_id + 100;
        reply.ndr = NDR_record;
        reply.ret_code = KERN_SUCCESS;
        reply.flavor = ARM_THREAD_STATE64;
        reply.state_count = ARM_THREAD_STATE64_COUNT;
        reply.state = ts;

        mach_msg(&reply.head, MACH_SEND_MSG, sizeof(reply), 0,
                 MACH_PORT_NULL, MACH_MSG_TIMEOUT_NONE, MACH_PORT_NULL);

        mach_port_deallocate(mach_task_self(), thread);
        mach_port_deallocate(mach_task_self(), request.task_port.name);
    }

    return NULL;
}

static int install_exc_handler(void) {
    // Allocate exception port.
    kern_return_t kr = mach_port_allocate(
        mach_task_self(),
        MACH_PORT_RIGHT_RECEIVE,
        &exception_port);
    if (kr != KERN_SUCCESS) return -1;

    kr = mach_port_insert_right(
        mach_task_self(),
        exception_port,
        exception_port,
        MACH_MSG_TYPE_MAKE_SEND);
    if (kr != KERN_SUCCESS) return -2;

    // Set as task exception port for EXC_BAD_ACCESS.
    kr = task_set_exception_ports(
        mach_task_self(),
        EXC_MASK_BAD_ACCESS,
        exception_port,
        EXCEPTION_STATE_IDENTITY | MACH_EXCEPTION_CODES,
        ARM_THREAD_STATE64);
    if (kr != KERN_SUCCESS) return -3;

    // Start handler thread.
    pthread_t thr;
    pthread_attr_t attr;
    pthread_attr_init(&attr);
    pthread_attr_setdetachstate(&attr, PTHREAD_CREATE_DETACHED);
    int err = pthread_create(&thr, &attr, exc_server_thread, NULL);
    pthread_attr_destroy(&attr);
    if (err != 0) return -4;

    return 0;
}
*/
import "C"

import "fmt"

// AddFaultRange registers a memory range [begin, end) with a fault
// handler. If a fault occurs with PC in [begin, end), execution is
// redirected to the handler address.
func AddFaultRange(begin, end, handler uintptr) {
	C.add_fault_range(C.uint64_t(begin), C.uint64_t(end), C.uint64_t(handler))
}

// Install sets up the Mach exception port and starts the handler thread.
func Install() error {
	ret := C.install_exc_handler()
	if ret != 0 {
		return fmt.Errorf("install_exc_handler failed: %d", int(ret))
	}
	return nil
}
