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
// +build darwin,arm64

// Command hvfrun runs static Linux ARM64 ELF binaries on macOS using
// Hypervisor.framework. It loads the ELF, maps segments into a VM,
// and emulates Linux syscalls.
package main

/*
#cgo LDFLAGS: -framework Hypervisor
#include <Hypervisor/Hypervisor.h>
#include <stdlib.h>
#include <string.h>
*/
import "C"

import (
	"debug/elf"
	"encoding/binary"
	"fmt"
	"os"
	"runtime"
	"unsafe"
)

const (
	pageSize = 16384

	// Memory layout:
	// 0x0000_0000 - 0x0000_3FFF: unmapped (null guard)
	// 0x0000_4000 - 0x0000_7FFF: exception vectors + ERET stub
	// 0x0040_0000 - ...:         ELF segments (loaded from binary)
	// 0x7FFF_0000 - 0x7FFF_FFFF: stack (1 page, grows down)
	// 0x8000_0000 - 0x8000_3FFF: brk/heap (1 page initially)

	vectorsAddr = 0x4000
	stackPages  = 64 // 64 * 16K = 1MB stack
	stackSize   = stackPages * pageSize
	stackBase   = 0x7F000000
	stackTop    = stackBase + stackSize - 16
	heapBase = 0x80000000
	heapSize = pageSize

	// mmap region for anonymous mappings (3GB range for Go runtime).
	mmapBase = 0x200000000  // 8GB
	mmapEnd  = 0x300000000  // 12GB
)

// guestMapping tracks a region of guest memory with its host pointer.
type guestMapping struct {
	guestAddr uint64
	size      uint64
	hostPtr   unsafe.Pointer
}

var guestMappings []guestMapping
var mmapNext = uint64(mmapBase) // Next available mmap address

// readGuestMemory reads bytes from guest memory into a Go byte slice.
func readGuestMemory(guestAddr uint64, size int) ([]byte, bool) {
	for _, m := range guestMappings {
		if guestAddr >= m.guestAddr && guestAddr+uint64(size) <= m.guestAddr+m.size {
			offset := guestAddr - m.guestAddr
			src := unsafe.Add(m.hostPtr, int(offset))
			buf := make([]byte, size)
			copy(buf, unsafe.Slice((*byte)(src), size))
			return buf, true
		}
	}
	return nil, false
}

func check(ret C.hv_return_t, msg string) {
	if ret != C.HV_SUCCESS {
		fmt.Fprintf(os.Stderr, "hvfrun: %s: error %d\n", msg, ret)
		os.Exit(1)
	}
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "Usage: hvfrun <static-elf-binary> [args...]\n")
		os.Exit(1)
	}

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	// Parse ELF.
	elfFile, err := elf.Open(os.Args[1])
	if err != nil {
		fmt.Fprintf(os.Stderr, "hvfrun: failed to open ELF: %v\n", err)
		os.Exit(1)
	}
	defer elfFile.Close()

	if elfFile.Machine != elf.EM_AARCH64 {
		fmt.Fprintf(os.Stderr, "hvfrun: not an ARM64 binary (machine=%v)\n", elfFile.Machine)
		os.Exit(1)
	}

	// Create VM.
	check(C.hv_vm_create(nil), "hv_vm_create")
	defer C.hv_vm_destroy()

	// Map exception vectors.
	vectorsMem := setupVectors()
	defer C.free(vectorsMem)

	// Map stack (multi-page).
	stackMem := mapPages(stackBase, stackSize, C.HV_MEMORY_READ|C.HV_MEMORY_WRITE)
	defer C.free(stackMem)
	guestMappings = append(guestMappings, guestMapping{stackBase, uint64(stackSize), stackMem})

	// Map heap (for brk).
	heapMem := mapPage(heapBase, C.HV_MEMORY_READ|C.HV_MEMORY_WRITE)
	defer C.free(heapMem)

	// Load ELF segments.
	var allocated []unsafe.Pointer
	defer func() {
		for _, p := range allocated {
			C.free(p)
		}
	}()
	for _, prog := range elfFile.Progs {
		if prog.Type != elf.PT_LOAD {
			continue
		}
		if prog.Memsz == 0 {
			continue
		}

		// Align to page boundaries.
		startAddr := prog.Vaddr & ^uint64(pageSize - 1)
		endAddr := (prog.Vaddr + prog.Memsz + uint64(pageSize) - 1) & ^uint64(pageSize - 1)
		mapSize := endAddr - startAddr

		var mem unsafe.Pointer
		C.posix_memalign(&mem, C.size_t(pageSize), C.size_t(mapSize))
		C.memset(mem, 0, C.size_t(mapSize))
		allocated = append(allocated, mem)

		// Read segment data.
		data := make([]byte, prog.Filesz)
		if _, err := prog.ReadAt(data, 0); err != nil {
			fmt.Fprintf(os.Stderr, "hvfrun: failed to read segment: %v\n", err)
			os.Exit(1)
		}

		// Copy into mapped page at the correct offset.
		offset := prog.Vaddr - startAddr
		dst := unsafe.Add(mem, int(offset))
		C.memcpy(dst, unsafe.Pointer(&data[0]), C.size_t(len(data)))

		// Map with appropriate permissions.
		perm := C.uint64_t(0)
		if prog.Flags&elf.PF_R != 0 {
			perm |= C.HV_MEMORY_READ
		}
		if prog.Flags&elf.PF_W != 0 {
			perm |= C.HV_MEMORY_WRITE
		}
		if prog.Flags&elf.PF_X != 0 {
			perm |= C.HV_MEMORY_EXEC
		}
		// Always allow read for debugging.
		perm |= C.HV_MEMORY_READ

		check(C.hv_vm_map(mem, C.hv_ipa_t(startAddr), C.size_t(mapSize), perm),
			fmt.Sprintf("hv_vm_map(0x%x, %d)", startAddr, mapSize))
		guestMappings = append(guestMappings, guestMapping{startAddr, mapSize, mem})
		fmt.Fprintf(os.Stderr, "hvfrun: loaded segment 0x%x-0x%x (flags=%v)\n",
			startAddr, endAddr, prog.Flags)
	}

	// Set up initial stack with argv, envp, and auxv.
	sp := setupStack(stackMem, elfFile)

	// Set up identity-mapped page tables for MMU.
	// ARM64 with 16K granule needs: L0 (1 entry per 128TB), L1 (1 entry per 64GB).
	// We use L1 block descriptors (1GB blocks) for a simple 1:1 mapping.
	ptMem := setupPageTables()
	defer C.free(ptMem)

	// Create and configure vCPU.
	var vcpu C.hv_vcpu_t
	var exit *C.hv_vcpu_exit_t
	check(C.hv_vcpu_create(&vcpu, &exit, nil), "hv_vcpu_create")
	defer C.hv_vcpu_destroy(vcpu)

	// Mask virtual timer to prevent IRQ loops.
	C.hv_vcpu_set_vtimer_mask(vcpu, true)

	check(C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_VBAR_EL1,
		C.uint64_t(vectorsAddr)), "VBAR_EL1")
	check(C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_CPACR_EL1, 3<<20), "CPACR_EL1")
	check(C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_SP_EL1,
		C.uint64_t(stackTop)), "SP_EL1")

	// MMU: 16K granule, T0SZ=28 (36-bit VA), L2 blocks.
	check(C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_TTBR0_EL1,
		C.uint64_t(ptGuestAddr)), "TTBR0_EL1")

	tcr := uint64(28)        // T0SZ = 28 (36-bit VA, start at L2)
	tcr |= 0x1 << 8          // IRGN0 = WB-WA
	tcr |= 0x1 << 10         // ORGN0 = WB-WA
	tcr |= 0x3 << 12         // SH0 = Inner shareable
	tcr |= 0x2 << 14         // TG0 = 16KB granule
	tcr |= uint64(0x2) << 32 // IPS = 40-bit PA
	tcr |= 1 << 23           // EPD1 = disable TTBR1 walks
	check(C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_TCR_EL1,
		C.uint64_t(tcr)), "TCR_EL1")

	check(C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_MAIR_EL1, 0xFF), "MAIR_EL1")

	// SCTLR_EL1: QEMU-compatible base + MMU + caches enabled.
	check(C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_SCTLR_EL1, 0x30901185), "SCTLR_EL1")

	// Run at EL1 directly (MMU page tables use AP[2:1]=00 = EL1 only).
	// Guest SVC instructions will trap to EL1 vector 0x200 (sync, current EL).
	entryPC := elfFile.Entry
	C.hv_vcpu_set_reg(vcpu, C.HV_REG_PC, C.uint64_t(entryPC))
	C.hv_vcpu_set_reg(vcpu, C.HV_REG_CPSR, 0x3c5) // EL1h, DAIF masked
	// Set SP to user stack (at EL1 we use SP_EL1)
	check(C.hv_vcpu_set_sys_reg(vcpu, C.HV_SYS_REG_SP_EL1,
		C.uint64_t(sp)), "SP_EL1")

	fmt.Fprintf(os.Stderr, "hvfrun: entry=0x%x sp=0x%x\n", entryPC, sp)
	fmt.Fprintf(os.Stderr, "hvfrun: starting execution...\n")

	// Syscall emulation loop.
	currentBrk := uint64(heapBase)
	for {
		check(C.hv_vcpu_run(vcpu), "hv_vcpu_run")

		if exit.reason == C.HV_EXIT_REASON_VTIMER_ACTIVATED {
			C.hv_vcpu_set_vtimer_mask(vcpu, true)
			continue
		}
		if exit.reason != C.HV_EXIT_REASON_EXCEPTION {
			var pc C.uint64_t
			C.hv_vcpu_get_reg(vcpu, C.HV_REG_PC, &pc)
			fmt.Fprintf(os.Stderr, "hvfrun: unexpected exit reason %d at PC=0x%x\n",
				exit.reason, pc)
			os.Exit(1)
		}

		ec := (exit.exception.syndrome >> 26) & 0x3f

		// Handle stage 2 faults (data/instruction aborts from hypervisor)
		// by demand-paging: map backing memory on first access.
		if ec == 0x24 || ec == 0x25 || ec == 0x20 || ec == 0x21 {
			faultAddr := uint64(exit.exception.virtual_address)
			// Align to page boundary.
			pageAddr := faultAddr & ^uint64(pageSize - 1)

			// Check if this address is in a known reservation.
			if pageAddr >= mmapBase && pageAddr < mmapEnd {
				// Demand-page: map a 32MB chunk around the fault address.
				chunkSize := uint64(32 * 1024 * 1024) // 32MB
				chunkBase := pageAddr & ^(chunkSize - 1)
				if chunkBase < mmapBase {
					chunkBase = mmapBase
				}

				var mem unsafe.Pointer
				C.posix_memalign(&mem, C.size_t(pageSize), C.size_t(chunkSize))
				if mem != nil {
					C.memset(mem, 0, C.size_t(chunkSize))
					ret := C.hv_vm_map(mem, C.hv_ipa_t(chunkBase), C.size_t(chunkSize),
						C.HV_MEMORY_READ|C.HV_MEMORY_WRITE|C.HV_MEMORY_EXEC)
					if ret == C.HV_SUCCESS {
						guestMappings = append(guestMappings, guestMapping{chunkBase, chunkSize, mem})
						continue // Retry the faulting instruction
					}
					C.free(mem)
				}
			}

			// Unrecoverable fault.
			var pc C.uint64_t
			C.hv_vcpu_get_reg(vcpu, C.HV_REG_PC, &pc)
			fmt.Fprintf(os.Stderr, "hvfrun: fatal fault EC=0x%x at PC=0x%x fault_addr=0x%x syndrome=0x%x\n",
				ec, pc, faultAddr, exit.exception.syndrome)
			os.Exit(1)
		}

		if ec != 0x16 { // Not HVC
			var pc C.uint64_t
			C.hv_vcpu_get_reg(vcpu, C.HV_REG_PC, &pc)
			fmt.Fprintf(os.Stderr, "hvfrun: unexpected exception EC=0x%x at PC=0x%x syndrome=0x%x\n",
				ec, pc, exit.exception.syndrome)
			os.Exit(1)
		}

		// Check ESR_EL1 for the original exception.
		var esrEL1 C.uint64_t
		C.hv_vcpu_get_sys_reg(vcpu, C.HV_SYS_REG_ESR_EL1, &esrEL1)
		origEC := (uint64(esrEL1) >> 26) & 0x3f

		// Handle data aborts forwarded from EL1 vectors (demand paging).
		if origEC == 0x25 || origEC == 0x21 { // Data/instruction abort from same EL
			var farEL1, elrEL1 C.uint64_t
			C.hv_vcpu_get_sys_reg(vcpu, C.HV_SYS_REG_FAR_EL1, &farEL1)
			C.hv_vcpu_get_sys_reg(vcpu, C.HV_SYS_REG_ELR_EL1, &elrEL1)
			faultAddr := uint64(farEL1)
			chunkSize := uint64(32 * 1024 * 1024)
			chunkBase := faultAddr & ^(chunkSize - 1)

			var mem unsafe.Pointer
			C.posix_memalign(&mem, C.size_t(pageSize), C.size_t(chunkSize))
			if mem != nil {
				C.memset(mem, 0, C.size_t(chunkSize))
				ret := C.hv_vm_map(mem, C.hv_ipa_t(chunkBase), C.size_t(chunkSize),
					C.HV_MEMORY_READ|C.HV_MEMORY_WRITE|C.HV_MEMORY_EXEC)
				if ret == C.HV_SUCCESS {
					guestMappings = append(guestMappings, guestMapping{chunkBase, chunkSize, mem})
					// Resume at the faulting instruction.
					C.hv_vcpu_set_reg(vcpu, C.HV_REG_PC, elrEL1)
					continue
				}
				C.free(mem)
			}
			fmt.Fprintf(os.Stderr, "hvfrun: demand page failed at 0x%x (from PC=0x%x)\n", faultAddr, elrEL1)
			os.Exit(1)
		}

		if origEC != 0x15 { // Not SVC
			var elr C.uint64_t
			C.hv_vcpu_get_sys_reg(vcpu, C.HV_SYS_REG_ELR_EL1, &elr)
			var far C.uint64_t
			C.hv_vcpu_get_sys_reg(vcpu, C.HV_SYS_REG_FAR_EL1, &far)
			fmt.Fprintf(os.Stderr, "hvfrun: guest exception EC=0x%x at PC=0x%x FAR=0x%x ESR=0x%x\n",
				origEC, elr, far, esrEL1)
			os.Exit(1)
		}

		// Read syscall arguments.
		var regs [9]C.uint64_t // X0-X5, X8
		C.hv_vcpu_get_reg(vcpu, C.HV_REG_X0, &regs[0])
		C.hv_vcpu_get_reg(vcpu, C.HV_REG_X1, &regs[1])
		C.hv_vcpu_get_reg(vcpu, C.HV_REG_X2, &regs[2])
		C.hv_vcpu_get_reg(vcpu, C.HV_REG_X3, &regs[3])
		C.hv_vcpu_get_reg(vcpu, C.HV_REG_X4, &regs[4])
		C.hv_vcpu_get_reg(vcpu, C.HV_REG_X5, &regs[5])
		C.hv_vcpu_get_reg(vcpu, C.HV_REG_X8, &regs[8])

		sysno := uint64(regs[8])
		ret := emulateSyscall(sysno, regs, &currentBrk)

		// Set return value in X0.
		C.hv_vcpu_set_reg(vcpu, C.HV_REG_X0, C.uint64_t(ret))

		// Resume at EL1: set PC to ELR_EL1 (return address from SVC).
		// The SVC handler at vector 0x200 did HVC. ELR_EL1 has the SVC+4 address.
		var elrEL1 C.uint64_t
		C.hv_vcpu_get_sys_reg(vcpu, C.HV_SYS_REG_ELR_EL1, &elrEL1)
		C.hv_vcpu_set_reg(vcpu, C.HV_REG_PC, elrEL1)
		C.hv_vcpu_set_reg(vcpu, C.HV_REG_CPSR, 0x3c5)
	}
}

func emulateSyscall(sysno uint64, regs [9]C.uint64_t, brk *uint64) uint64 {
	switch sysno {
	case 64: // write
		fd := int(regs[0])
		bufAddr := uint64(regs[1])
		count := int(regs[2])
		buf, ok := readGuestMemory(bufAddr, count)
		if !ok {
			return negErrno(14) // EFAULT
		}
		var n int
		var err error
		switch fd {
		case 1:
			n, err = os.Stdout.Write(buf)
		case 2:
			n, err = os.Stderr.Write(buf)
		default:
			return negErrno(9) // EBADF
		}
		if err != nil {
			return negErrno(5) // EIO
		}
		return uint64(n)

	case 93, 94: // exit, exit_group
		code := int(regs[0])
		fmt.Fprintf(os.Stderr, "hvfrun: guest exited with code %d\n", code)
		os.Exit(code)

	case 214: // brk
		addr := uint64(regs[0])
		if addr == 0 {
			return *brk
		}
		// Grow the heap if needed.
		if addr > *brk {
			// Map additional pages.
			newEnd := (addr + uint64(pageSize) - 1) & ^(uint64(pageSize) - 1)
			oldEnd := (*brk + uint64(pageSize) - 1) & ^(uint64(pageSize) - 1)
			if newEnd > oldEnd {
				growSize := newEnd - oldEnd
				var mem unsafe.Pointer
				C.posix_memalign(&mem, C.size_t(pageSize), C.size_t(growSize))
				if mem == nil {
					return *brk
				}
				C.memset(mem, 0, C.size_t(growSize))
				ret := C.hv_vm_map(mem, C.hv_ipa_t(oldEnd), C.size_t(growSize),
					C.HV_MEMORY_READ|C.HV_MEMORY_WRITE)
				if ret != C.HV_SUCCESS {
					C.free(mem)
					return *brk
				}
				guestMappings = append(guestMappings, guestMapping{oldEnd, growSize, mem})
			}
		}
		*brk = addr
		return addr

	case 96: // set_tid_address
		return 1 // Return a fake TID

	case 29: // ioctl
		return negErrno(25) // ENOTTY

	case 66: // writev
		fd := int(regs[0])
		iovAddr := uint64(regs[1])
		iovCnt := int(regs[2])
		totalWritten := 0
		for i := 0; i < iovCnt; i++ {
			// Each iovec is 16 bytes: 8 for base ptr, 8 for len
			iovEntryAddr := iovAddr + uint64(i*16)
			entry, ok := readGuestMemory(iovEntryAddr, 16)
			if !ok {
				if totalWritten > 0 {
					return uint64(totalWritten)
				}
				return negErrno(14) // EFAULT
			}
			base := binary.LittleEndian.Uint64(entry[0:8])
			length := binary.LittleEndian.Uint64(entry[8:16])
			if length == 0 {
				continue
			}
			buf, ok := readGuestMemory(base, int(length))
			if !ok {
				if totalWritten > 0 {
					return uint64(totalWritten)
				}
				return negErrno(14) // EFAULT
			}
			var n int
			switch fd {
			case 1:
				n, _ = os.Stdout.Write(buf)
			case 2:
				n, _ = os.Stderr.Write(buf)
			default:
				return negErrno(9)
			}
			totalWritten += n
		}
		return uint64(totalWritten)

	case 56: // openat
		return negErrno(2) // ENOENT

	case 48: // faccessat
		return negErrno(2) // ENOENT

	case 80: // fstat
		return negErrno(9) // EBADF

	case 99: // set_robust_list
		return 0 // Success

	case 226: // mprotect
		addr := uint64(regs[0])
		length := uint64(regs[1])
		prot := uint64(regs[2])
		fmt.Fprintf(os.Stderr, "hvfrun: mprotect(0x%x, 0x%x, prot=0x%x)\n", addr, length, prot)
		length = (length + uint64(pageSize) - 1) & ^(uint64(pageSize) - 1)

		if prot != 0 {
			// Check if this region is already mapped.
			alreadyMapped := false
			for _, m := range guestMappings {
				if addr >= m.guestAddr && addr+length <= m.guestAddr+m.size {
					alreadyMapped = true
					break
				}
			}
			if !alreadyMapped && length > 0 {
				// Map backing memory for this previously-reserved region.
				var mem unsafe.Pointer
				C.posix_memalign(&mem, C.size_t(pageSize), C.size_t(length))
				if mem == nil {
					return negErrno(12)
				}
				C.memset(mem, 0, C.size_t(length))
				hvPerm := C.uint64_t(C.HV_MEMORY_READ | C.HV_MEMORY_WRITE | C.HV_MEMORY_EXEC)
				ret := C.hv_vm_map(mem, C.hv_ipa_t(addr), C.size_t(length), hvPerm)
				if ret != C.HV_SUCCESS {
					C.free(mem)
					return negErrno(12)
				}
				guestMappings = append(guestMappings, guestMapping{addr, length, mem})
			}
		}
		return 0

	case 222: // mmap
		addr := uint64(regs[0])
		length := uint64(regs[1])
		prot := uint64(regs[2])
		flags := uint64(regs[3])
		// fd := int64(regs[4])
		// offset := uint64(regs[5])

		const mapAnonymous = 0x20
		const mapFixed = 0x10
		const mapPrivate = 0x2
		const mapNoReserve = 0x4000
		_ = prot

		if flags&mapAnonymous == 0 {
			return negErrno(38)
		}

		fmt.Fprintf(os.Stderr, "hvfrun: mmap(0x%x, 0x%x, prot=0x%x, flags=0x%x)\n", addr, length, prot, flags)

		// Align length up to page size.
		length = (length + uint64(pageSize) - 1) & ^(uint64(pageSize) - 1)
		if length == 0 {
			return negErrno(22) // EINVAL
		}

		var mapAddr uint64
		if flags&mapFixed != 0 && addr != 0 {
			mapAddr = addr & ^(uint64(pageSize) - 1)
		} else {
			mapAddr = mmapNext
			mmapNext += length
			if mmapNext > mmapEnd {
				return negErrno(12) // ENOMEM
			}
		}

		// Reject addresses outside our mapped range (64GB = 0x10_0000_0000).
		const maxMappedAddr = uint64(0x1000000000) // 64GB
		if mapAddr+length > maxMappedAddr {
			return negErrno(12) // ENOMEM - Go runtime will try lower addresses
		}

		if prot == 0 {
			// PROT_NONE = address reservation only. Don't allocate or map.
			return mapAddr
		}

		// Allocate and map host memory for this region.
		var mem unsafe.Pointer
		C.posix_memalign(&mem, C.size_t(pageSize), C.size_t(length))
		if mem == nil {
			return negErrno(12) // ENOMEM
		}
		C.memset(mem, 0, C.size_t(length))

		hvPerm := C.uint64_t(C.HV_MEMORY_READ | C.HV_MEMORY_WRITE | C.HV_MEMORY_EXEC)
		ret := C.hv_vm_map(mem, C.hv_ipa_t(mapAddr), C.size_t(length), hvPerm)
		if ret != C.HV_SUCCESS {
			fmt.Fprintf(os.Stderr, "hvfrun: hv_vm_map(0x%x, 0x%x) failed: %d\n", mapAddr, length, ret)
			C.free(mem)
			return negErrno(12) // ENOMEM
		}
		fmt.Fprintf(os.Stderr, "hvfrun: mapped 0x%x-0x%x (prot=0x%x)\n", mapAddr, mapAddr+length, prot)
		guestMappings = append(guestMappings, guestMapping{mapAddr, length, mem})
		return mapAddr

	case 215: // munmap
		// We don't actually unmap, just pretend success.
		return 0

	case 233: // madvise
		return 0

	case 261: // prlimit64
		return 0 // Pretend success

	case 278: // getrandom
		// Fill buffer with pseudo-random data.
		bufAddr := uint64(regs[0])
		count := int(regs[1])
		for _, m := range guestMappings {
			if bufAddr >= m.guestAddr && bufAddr+uint64(count) <= m.guestAddr+m.size {
				offset := bufAddr - m.guestAddr
				dst := unsafe.Add(m.hostPtr, int(offset))
				// Fill with simple pseudo-random bytes.
				b := unsafe.Slice((*byte)(dst), count)
				for i := range b {
					b[i] = byte(i * 7)
				}
				return uint64(count)
			}
		}
		return negErrno(14) // EFAULT

	case 113: // clock_gettime
		return 0 // Pretend success (time struct in guest memory not filled)

	case 135: // rt_sigprocmask
		return 0

	case 134: // rt_sigaction
		return 0

	case 132: // sigaltstack
		return 0

	case 131: // tgkill
		return negErrno(38)

	case 178: // gettid
		return 1

	case 172: // getpid
		return 1

	case 174: // getuid
		return 0

	case 175: // geteuid
		return 0

	case 176: // getgid
		return 0

	case 177: // getegid
		return 0

	case 179: // sysinfo
		return negErrno(38)

	case 160: // uname
		return negErrno(38)

	case 198: // socket
		return negErrno(38)

	case 57: // close
		return 0

	case 25: // fcntl
		return negErrno(38)

	case 79: // fstatfs
		return negErrno(38)

	case 17: // getcwd
		return negErrno(38)

	case 165: // getrusage
		return negErrno(38)

	case 220: // clone (clone3)
		return negErrno(38) // Single-threaded for now

	case 98: // futex
		return 0 // Pretend success

	case 101: // nanosleep
		return 0

	case 260: // wait4
		return negErrno(10) // ECHILD

	case 62: // lseek
		return negErrno(38)

	case 63: // read
		return negErrno(9) // EBADF

	case 123: // sched_setaffinity
		return 0

	case 167: // prctl (on arm64... actually this might be sysinfo)
		// Go runtime uses prctl(PR_SET_VMA) for memory naming
		return negErrno(38) // ENOSYS

	default:
		fmt.Fprintf(os.Stderr, "hvfrun: unhandled syscall %d (X0=%d, X1=0x%x, X2=%d)\n",
			sysno, regs[0], regs[1], regs[2])
		return negErrno(38) // ENOSYS
	}
	return 0
}

func negErrno(e uint64) uint64 {
	return ^e + 1 // Two's complement negation: -e
}

// ptGuestAddr is the IPA where the L2 page table is mapped.
const ptGuestAddr = uint64(0xA0000000) // 2.5GB

// setupPageTables creates identity-mapped page tables for the guest MMU.
// Uses ARM64 16K granule, T0SZ=28 (36-bit VA), starting directly at L2.
// L2 block descriptors map 32MB each. Maps first 4GB (128 entries).
//
// CRITICAL: AP[2:1]=00 (EL1-only) - Apple Silicon HVF requires this.
// Setting AP[1]=1 causes permission faults that create infinite loops.
func setupPageTables() unsafe.Pointer {
	var mem unsafe.Pointer
	C.posix_memalign(&mem, C.size_t(pageSize), C.size_t(pageSize))
	C.memset(mem, 0, C.size_t(pageSize))

	pt := unsafe.Slice((*byte)(mem), pageSize)

	// Map full 64GB (2048 entries * 32MB = 64GB) to cover Go runtime's
	// large mmap reservations. Only 16K of page table needed.
	for i := 0; i < 2048; i++ {
		blockAddr := uint64(i) * 32 * 1024 * 1024
		desc := blockAddr
		desc |= 1 << 10 // AF (Access Flag)
		desc |= 3 << 8  // SH = Inner Shareable
		// NO AP[1] - critical for Apple Silicon HVF
		desc |= 0 << 2 // AttrIdx = 0 (Normal WB from MAIR)
		desc |= 0x1    // Block descriptor
		binary.LittleEndian.PutUint64(pt[i*8:], desc)
	}

	check(C.hv_vm_map(mem, C.hv_ipa_t(ptGuestAddr), C.size_t(pageSize),
		C.HV_MEMORY_READ|C.HV_MEMORY_WRITE), "map page tables")

	fmt.Fprintf(os.Stderr, "hvfrun: page tables at IPA 0x%x (2048 x 32MB = 64GB identity map)\n", ptGuestAddr)
	return mem
}

func setupVectors() unsafe.Pointer {
	var mem unsafe.Pointer
	C.posix_memalign(&mem, C.size_t(pageSize), C.size_t(pageSize))
	C.memset(mem, 0, C.size_t(pageSize))

	vectors := make([]byte, pageSize)
	for i := 0; i < 16; i++ {
		hvcInstr := uint32(0xd4000002) | (uint32(i) << 5)
		binary.LittleEndian.PutUint32(vectors[i*128:], hvcInstr)
	}
	// ERET stub at 0x800.
	binary.LittleEndian.PutUint32(vectors[0x800:], 0xd69f03e0)

	C.memcpy(mem, unsafe.Pointer(&vectors[0]), C.size_t(pageSize))
	check(C.hv_vm_map(mem, C.hv_ipa_t(vectorsAddr), C.size_t(pageSize),
		C.HV_MEMORY_READ|C.HV_MEMORY_EXEC), "map vectors")
	return mem
}

func mapPage(addr uint64, perm C.uint64_t) unsafe.Pointer {
	return mapPages(addr, pageSize, perm)
}

func mapPages(addr uint64, size int, perm C.uint64_t) unsafe.Pointer {
	var mem unsafe.Pointer
	C.posix_memalign(&mem, C.size_t(pageSize), C.size_t(size))
	C.memset(mem, 0, C.size_t(size))
	check(C.hv_vm_map(mem, C.hv_ipa_t(addr), C.size_t(size), perm),
		fmt.Sprintf("map pages 0x%x size=%d", addr, size))
	return mem
}

// setupStack builds the initial stack for the ELF binary.
// Linux ABI: SP -> argc, argv[0], ..., argv[argc-1], NULL, envp..., NULL, auxv...
func setupStack(stackMem unsafe.Pointer, ef *elf.File) uint64 {
	stack := make([]byte, stackSize)
	sp := stackSize

	// Place argv strings at the top of the stack.
	var argvAddrs []uint64
	for i := len(os.Args) - 1; i >= 1; i-- {
		arg := os.Args[i] + "\x00"
		sp -= len(arg)
		copy(stack[sp:], arg)
		argvAddrs = append([]uint64{stackBase + uint64(sp)}, argvAddrs...)
	}

	// Align SP to 16 bytes.
	sp &= ^0xF

	// Build the stack frame (grows downward):
	// auxv terminator
	sp -= 16
	binary.LittleEndian.PutUint64(stack[sp:], 0) // AT_NULL
	binary.LittleEndian.PutUint64(stack[sp+8:], 0)

	// AT_PAGESZ
	sp -= 16
	binary.LittleEndian.PutUint64(stack[sp:], 6) // AT_PAGESZ
	binary.LittleEndian.PutUint64(stack[sp+8:], uint64(pageSize))

	// AT_ENTRY
	sp -= 16
	binary.LittleEndian.PutUint64(stack[sp:], 9) // AT_ENTRY
	binary.LittleEndian.PutUint64(stack[sp+8:], ef.Entry)

	// AT_PHDR, AT_PHENT, AT_PHNUM (for dynamic linker, but useful)
	for _, prog := range ef.Progs {
		if prog.Type == elf.PT_PHDR {
			sp -= 16
			binary.LittleEndian.PutUint64(stack[sp:], 3) // AT_PHDR
			binary.LittleEndian.PutUint64(stack[sp+8:], prog.Vaddr)
			break
		}
	}
	sp -= 16
	binary.LittleEndian.PutUint64(stack[sp:], 4) // AT_PHENT
	binary.LittleEndian.PutUint64(stack[sp+8:], 56) // sizeof(Elf64_Phdr)
	sp -= 16
	binary.LittleEndian.PutUint64(stack[sp:], 5) // AT_PHNUM
	binary.LittleEndian.PutUint64(stack[sp+8:], uint64(len(ef.Progs)))

	// envp terminator
	sp -= 8
	binary.LittleEndian.PutUint64(stack[sp:], 0)

	// argv terminator
	sp -= 8
	binary.LittleEndian.PutUint64(stack[sp:], 0)

	// argv pointers
	for i := len(argvAddrs) - 1; i >= 0; i-- {
		sp -= 8
		binary.LittleEndian.PutUint64(stack[sp:], argvAddrs[i])
	}

	// argc
	sp -= 8
	binary.LittleEndian.PutUint64(stack[sp:], uint64(len(argvAddrs)))

	C.memcpy(stackMem, unsafe.Pointer(&stack[0]), C.size_t(stackSize))
	return stackBase + uint64(sp)
}
