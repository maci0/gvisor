//go:build darwin && arm64

// Command el1sentry demonstrates a C-compiled syscall dispatcher
// running at EL1 inside an HVF VM. The dispatcher handles simple
// syscalls (getpid, etc.) without VM exit, and falls through to
// HVC for complex ones.
package main

/*
#cgo LDFLAGS: -framework Hypervisor -framework CoreFoundation
#include <Hypervisor/Hypervisor.h>
#include <CoreFoundation/CoreFoundation.h>
#include <stdlib.h>
#include <string.h>

// copy_dispatch copies Go-assembled dispatch code to the dispatch page.
static void copy_dispatch_from(void *dst, const void *src, int size) {
    memcpy(dst, src, size);
}
*/
import "C"

import (
	"encoding/binary"
	"fmt"
	"runtime"
	"sync"
	"time"
	"unsafe"

	"gvisor.dev/gvisor/cmd/el1sentry/el1dispatch"
)

const pageSize = 16384

func main() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	fmt.Println("=== Full Sentry-at-EL1: C Dispatcher ===")
	fmt.Println()
	testCDispatcher()
}

func alloc(n int) unsafe.Pointer {
	var p unsafe.Pointer
	C.posix_memalign(&p, C.size_t(pageSize), C.size_t(n))
	C.memset(p, 0, C.size_t(n))
	return p
}

func runWithTimeout(vcpu C.hv_vcpu_t, sec int) (hung bool, ret C.hv_return_t) {
	var mu sync.Mutex
	go func() {
		time.Sleep(time.Duration(sec) * time.Second)
		mu.Lock()
		hung = true
		mu.Unlock()
		C.hv_vcpus_exit(&vcpu, 1)
	}()
	ret = C.hv_vcpu_run(vcpu)
	mu.Lock()
	defer mu.Unlock()
	return
}

func testCDispatcher() {
	fmt.Println("--- C Dispatcher: multi-syscall in-VM handling ---")
	fmt.Println("  Using Go plan9 assembly dispatcher")

	config := C.hv_vm_config_create()
	defer C.CFRelease(C.CFTypeRef(unsafe.Pointer(config)))
	C.hv_vm_config_set_ipa_size(config, 40)
	C.hv_vm_create(config)
	defer C.hv_vm_destroy()

	var vcpu C.hv_vcpu_t
	var exit *C.hv_vcpu_exit_t
	C.hv_vcpu_create(&vcpu, &exit, nil)
	defer C.hv_vcpu_destroy(vcpu)

	vecMem := alloc(pageSize)
	userMem := alloc(pageSize)
	stateMem := alloc(pageSize)
	codeMem := alloc(pageSize)
	ptL0 := alloc(pageSize)
	ptL1 := alloc(pageSize)
	ptL2 := alloc(pageSize)
	ptL3 := alloc(pageSize)
	kptL0 := alloc(pageSize)
	kptL1 := alloc(pageSize)
	kptL2 := alloc(pageSize)
	kptL3 := alloc(pageSize)
	allPtrs := []unsafe.Pointer{vecMem, userMem, stateMem, codeMem,
		ptL0, ptL1, ptL2, ptL3, kptL0, kptL1, kptL2, kptL3}
	defer func() { for _, p := range allPtrs { C.free(p) } }()

	type mp struct {
		p   unsafe.Pointer
		ipa uint64
		f   C.hv_memory_flags_t
	}
	maps := []mp{
		{vecMem, 0x00000, C.HV_MEMORY_READ | C.HV_MEMORY_EXEC},
		{userMem, 0x04000, C.HV_MEMORY_READ | C.HV_MEMORY_EXEC},
		{stateMem, 0x08000, C.HV_MEMORY_READ | C.HV_MEMORY_WRITE},
		{codeMem, 0x0C000, C.HV_MEMORY_READ | C.HV_MEMORY_EXEC},
		{ptL0, 0x10000, C.HV_MEMORY_READ | C.HV_MEMORY_WRITE},
		{ptL1, 0x14000, C.HV_MEMORY_READ | C.HV_MEMORY_WRITE},
		{ptL2, 0x18000, C.HV_MEMORY_READ | C.HV_MEMORY_WRITE},
		{ptL3, 0x1C000, C.HV_MEMORY_READ | C.HV_MEMORY_WRITE},
		{kptL0, 0x20000, C.HV_MEMORY_READ | C.HV_MEMORY_WRITE},
		{kptL1, 0x24000, C.HV_MEMORY_READ | C.HV_MEMORY_WRITE},
		{kptL2, 0x28000, C.HV_MEMORY_READ | C.HV_MEMORY_WRITE},
		{kptL3, 0x2C000, C.HV_MEMORY_READ | C.HV_MEMORY_WRITE},
	}
	for _, m := range maps {
		C.hv_vm_map(m.p, C.hv_ipa_t(m.ipa), C.size_t(pageSize), m.f)
	}
	defer func() {
		for _, m := range maps {
			C.hv_vm_unmap(C.hv_ipa_t(m.ipa), C.size_t(pageSize))
		}
	}()

	// Copy Go-assembled dispatcher to code page
	code := el1dispatch.Code()
	fmt.Printf("  Go asm dispatch: %d bytes\n", len(code))
	C.copy_dispatch_from(codeMem, unsafe.Pointer(&code[0]), C.int(len(code)))

	// Page tables
	tableDesc := uint64(0x3)
	mkPage := func(ipa uint64, el0 bool) uint64 {
		d := ipa | (1 << 10) | (3 << 8) | 0x3
		if el0 {
			d |= (1 << 6)
		}
		return d
	}
	(*[2048]uint64)(ptL0)[0] = 0x14000 | tableDesc
	(*[2048]uint64)(ptL1)[0] = 0x18000 | tableDesc
	(*[2048]uint64)(ptL2)[0] = 0x1C000 | tableDesc
	(*[2048]uint64)(ptL3)[0] = mkPage(0x00000, false)
	(*[2048]uint64)(ptL3)[1] = mkPage(0x04000, true)

	stateKVA := uint64(0xFFFF000000008000)
	_ = uint64(0xFFFF00000000C000) // codeKVA hardcoded in MOVZ/MOVK
	(*[2048]uint64)(kptL0)[0] = 0x24000 | tableDesc
	(*[2048]uint64)(kptL1)[0] = 0x28000 | tableDesc
	(*[2048]uint64)(kptL2)[0] = 0x2C000 | tableDesc
	(*[2048]uint64)(kptL3)[2] = mkPage(0x08000, false) // state RW
	(*[2048]uint64)(kptL3)[3] = mkPage(0x0C000, false) // code RX

	// State page: set identity values
	sp := (*[pageSize]byte)(stateMem)
	// Dispatch VA is hardcoded as MOVZ/MOVK in vector handler.
	// State page 0x200+ holds persistent syscall state.
	binary.LittleEndian.PutUint64(sp[0x200:], 42)       // pid
	binary.LittleEndian.PutUint64(sp[0x208:], 42)       // tid
	binary.LittleEndian.PutUint64(sp[0x210:], 0x1000000) // brk
	binary.LittleEndian.PutUint64(sp[0x218:], 0)         // uid
	binary.LittleEndian.PutUint64(sp[0x220:], 0)         // gid
	binary.LittleEndian.PutUint64(sp[0x228:], 0)         // ppid
	binary.LittleEndian.PutUint64(sp[0x230:], 42)        // pgid
	binary.LittleEndian.PutUint64(sp[0x238:], 42)        // sid

	// Vectors: el0_sync at 0x400
	vectors := (*[pageSize]byte)(vecMem)
	for i := 0; i < 16; i++ {
		binary.LittleEndian.PutUint32(vectors[i*128:], 0xd4000002|uint32(i)<<5)
	}
	binary.LittleEndian.PutUint32(vectors[0x200:], 0xd5385212)
	binary.LittleEndian.PutUint32(vectors[0x204:], 0xd5386011)
	binary.LittleEndian.PutUint32(vectors[0x208:], 0xd4000082)

	// el0_sync: check SVC, call C dispatcher
	off := 0x400
	put := func(instr uint32) {
		binary.LittleEndian.PutUint32(vectors[off:], instr)
		off += 4
	}
	put(0xd5385212) // MRS X18, ESR_EL1
	put(0xd35afe51) // LSR X17, X18, #26
	put(0x7100563f) // CMP W17, #0x15 (SVC?)
	// B.NE to fault handler (offset calculated below)
	faultBneOff := off
	put(0x54000001) // placeholder B.NE

	// SVC path: TLBI, call C dispatcher
	put(0xd508831f) // TLBI VMALLE1IS
	put(0xd5033b9f) // DSB ISH
	put(0xd5033fdf) // ISB
	put(0xd538d090) // MRS X16, TPIDR_EL1 (state page)
	// Save guest X30, set up dispatcher args
	put(0xAA1E03EF) // MOV X15, X30

	// Args: X0=state, X1=nr(X8), X2=a0(orig X0), X3=a1(orig X1), X4=a2(orig X2)
	// But X0 has guest X0, need to save it first
	put(0xAA0003E9) // MOV X9, X0 (save guest X0)
	put(0xAA1003E0) // MOV X0, X16 (arg0: state page)
	put(0xAA0803E1) // MOV X1, X8 (arg1: syscall nr)
	// X2 already has guest X2 (a2), but we want guest X0 in X2
	put(0xAA0903E2) // MOV X2, X9 (arg2: guest X0 = a0)
	// X3 has guest X3, but we want guest X1 in X3
	// Actually, we already clobbered X0. X1 still has guest X1.
	put(0xAA0103E3) // MOV X3, X1 (arg3: guest X1 = a1)
	// X4 = guest X2... but we used X2 for a0. Need to load from saved state.
	// Actually X2 was saved to X9 (that was X0). X2 still has guest X2.
	// Wait, we did MOV X2, X9 which overwrote X2 with guest X0.
	// Fix: save more regs first.

	// Let me simplify: just pass state page + nr. The C function
	// reads args from state page if needed.
	off = 0x400 + 4*4 // reset to after B.NE
	put(0xd508831f) // TLBI VMALLE1IS
	put(0xd5033b9f) // DSB ISH
	put(0xd5033fdf) // ISB
	put(0xd538d090) // MRS X16, TPIDR_EL1
	put(0xAA1E03EF) // MOV X15, X30 (save LR)
	put(0xAA1003E0) // MOV X0, X16 (arg0: state)
	put(0xAA0803E1) // MOV X1, X8 (arg1: nr)
	put(0xAA0003E2) // MOV X2, X0... wait, X0 is already state.

	// OK the issue is X0 was guest X0 but we overwrote it. Let me just
	// pass state+nr. The C function only uses state page fields.
	off = 0x400 + 4*4
	put(0xd508831f) // TLBI VMALLE1IS
	put(0xd5033b9f) // DSB ISH
	put(0xd5033fdf) // ISB
	put(0xd538d090) // MRS X16, TPIDR_EL1
	put(0xAA1E03EF) // MOV X15, X30
	put(0xAA1003E0) // MOV X0, X16 (state page)
	put(0xAA0803E1) // MOV X1, X8 (syscall nr)
	// Load dispatcher VA from state page offset (but we overwrote sp[0x200] with pid!)
	// Fix: use a different offset for dispatch VA
	// Actually the code page VA should be loaded via a known mechanism.
	// Simplest: hardcode the KVA in a register or use a fixed offset.
	// Let me just use MOV immediate:
	// codeKVA = 0xFFFF00000000C000
	// MOVZ X17, #0xC000; MOVK X17, #0, lsl#16; MOVK X17, #0, lsl#32; MOVK X17, #0xFFFF, lsl#48
	put(0xd2980011) // MOVZ X17, #0xC000
	put(0xf2a00011) // MOVK X17, #0, lsl#16
	put(0xf2c00011) // MOVK X17, #0, lsl#32
	put(0xf2fffff1) // MOVK X17, #0xFFFF, lsl#48
	put(0xD63F0220) // BLR X17
	put(0xAA0F03FE) // MOV X30, X15 (restore LR)
	put(0xB5000049) // CBNZ X9, .+8 (→ HVC)
	put(0xd69f03e0) // ERET

	hvcOff := off
	put(0xd4000122) // HVC #9

	faultOff := off
	put(0xd5385212) // MRS X18, ESR_EL1
	put(0xd4000102) // HVC #8

	// Fix B.NE offset
	imm19 := (faultOff - faultBneOff) / 4
	binary.LittleEndian.PutUint32(vectors[faultBneOff:], 0x54000001|uint32(imm19<<5))
	_ = hvcOff

	binary.LittleEndian.PutUint32(vectors[0x800:], 0xd69f03e0) // ERET

	// User code: 5 SVCs — getpid, gettid, sched_yield, getppid, then write(unknown)
	user := (*[pageSize]byte)(userMem)
	uoff := 0
	uput := func(instr uint32) {
		binary.LittleEndian.PutUint32(user[uoff:], instr)
		uoff += 4
	}
	// getpid (172)
	uput(0xD2800000 | uint32(172<<5) | 8) // MOVZ X8, #172
	uput(0xd4000001)                       // SVC
	uput(0xAA0003F3)                       // MOV X19, X0 (save pid)

	// gettid (178)
	uput(0xD2800000 | uint32(178<<5) | 8)
	uput(0xd4000001)
	uput(0xAA0003F4) // MOV X20, X0

	// sched_yield (124)
	uput(0xD2800000 | uint32(124<<5) | 8)
	uput(0xd4000001)
	uput(0xAA0003F5) // MOV X21, X0

	// getppid (173)
	uput(0xD2800000 | uint32(173<<5) | 8)
	uput(0xd4000001)
	uput(0xAA0003F6) // MOV X22, X0

	// write (64) — unknown, exits to host
	uput(0xD2800000 | uint32(64<<5) | 8)
	uput(0xd4000001)

	// System registers
	sctlr := uint64(0x30D00985)
	tcr := uint64(16) | (0x1 << 8) | (0x1 << 10) | (0x3 << 12) | (0x2 << 14) |
		(uint64(16) << 16) | (0x1 << 24) | (0x1 << 26) | (0x3 << 28) |
		(uint64(0x1) << 30) | (uint64(0x2) << 32) | (uint64(1) << 36)

	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_VBAR_EL1, 0)
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_CPACR_EL1, 3<<20)
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_MAIR_EL1, 0xFF)
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_SCTLR_EL1, C.uint64_t(sctlr))
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_TCR_EL1, C.uint64_t(tcr))
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_TTBR0_EL1, 0x10000)
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_TTBR1_EL1, 0x20000)
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_TPIDR_EL1, C.uint64_t(stateKVA))
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_ELR_EL1, 0x4000)
	C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_SPSR_EL1, 0)
	C.hv_vcpu_set_reg(vcpu, C.HV_REG_PC, 0x800)
	C.hv_vcpu_set_reg(vcpu, C.HV_REG_CPSR, 0x3c4)

	fmt.Println("  Guest: getpid→42, gettid→42, yield→0, getppid→0, write→exit")

	hung, _ := runWithTimeout(vcpu, 5)
	if hung {
		var pc C.uint64_t
		C.hv_vcpu_get_reg(vcpu, C.HV_REG_PC, &pc)
		fmt.Printf("  HUNG at PC=%#x\n", uint64(pc))
		if uint64(pc) >= 0x200 && uint64(pc) < 0x280 {
			var x18, x17 C.uint64_t
			C.hv_vcpu_get_reg(vcpu, C.HV_REG_X18, &x18)
			C.hv_vcpu_get_reg(vcpu, C.HV_REG_X17, &x17)
			fmt.Printf("  Fault: ESR=%#x (EC=%#x) FAR=%#x\n",
				uint64(x18), (uint64(x18)>>26)&0x3f, uint64(x17))
		}
		return
	}

	var x8, x19, x20, x21, x22 C.uint64_t
	C.hv_vcpu_get_reg(vcpu, C.HV_REG_X8, &x8)
	C.hv_vcpu_get_reg(vcpu, C.HV_REG_X19, &x19)
	C.hv_vcpu_get_reg(vcpu, C.HV_REG_X20, &x20)
	C.hv_vcpu_get_reg(vcpu, C.HV_REG_X21, &x21)
	C.hv_vcpu_get_reg(vcpu, C.HV_REG_X22, &x22)

	fmt.Printf("  getpid (X19): %d (want 42)\n", uint64(x19))
	fmt.Printf("  gettid (X20): %d (want 42)\n", uint64(x20))
	fmt.Printf("  yield  (X21): %d (want 0)\n", uint64(x21))
	fmt.Printf("  getppid(X22): %d (want 0)\n", uint64(x22))
	fmt.Printf("  X8 at exit:   %d (want 64=write)\n", uint64(x8))

	ok := uint64(x19) == 42 && uint64(x20) == 42 &&
		uint64(x21) == 0 && uint64(x22) == 0 && uint64(x8) == 64
	if ok {
		fmt.Println("  *** C DISPATCHER WORKS! 4 syscalls handled in-VM, 1 exits ***")
	} else {
		fmt.Println("  PARTIAL: some values incorrect")
	}
	fmt.Println()
}
