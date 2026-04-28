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

//go:build ignore
// +build ignore

// This program generates a minimal static ARM64 Linux ELF binary that
// writes "Hello from gVisor on macOS!\n" to stdout and exits.
//
// Run with: go run testprog.go
package main

import (
	"encoding/binary"
	"os"
)

func main() {
	// ARM64 instructions for:
	//   _start:
	//     MOV X8, #64        // __NR_write
	//     MOV X0, #1         // fd = stdout
	//     ADR X1, msg        // buf = &msg (PC-relative)
	//     MOV X2, #29        // count = len(msg)
	//     SVC #0
	//     MOV X8, #93        // __NR_exit
	//     MOV X0, #0         // status = 0
	//     SVC #0
	//   msg:
	//     .ascii "Hello from gVisor on macOS!\n"

	// Code will be loaded at 0x400000 (standard Linux load address).
	loadAddr := uint64(0x400000)
	msgLen := 29
	msgOffset := 8 * 4 // 8 instructions * 4 bytes

	code := make([]byte, 0)

	// Encode instructions.
	instrs := []uint32{
		0xd2800808,                                           // MOV X8, #64
		0xd2800020,                                           // MOV X0, #1
		adrX1(uint32(msgOffset - 2*4)),                       // ADR X1, msg (PC-relative from this instruction)
		uint32(0xd2800002) | uint32((msgLen&0xFFFF)<<5),      // MOV X2, #msgLen
		0xd4000001,                                           // SVC #0
		uint32(0xd2800008) | uint32((93&0xFFFF)<<5),          // MOV X8, #93
		0xd2800000,                                           // MOV X0, #0
		0xd4000001,                                           // SVC #0
	}

	for _, instr := range instrs {
		b := make([]byte, 4)
		binary.LittleEndian.PutUint32(b, instr)
		code = append(code, b...)
	}

	// Append message.
	msg := "Hello from gVisor on macOS!\n\x00"
	code = append(code, []byte(msg)...)

	// Pad to align.
	for len(code)%16 != 0 {
		code = append(code, 0)
	}

	// Build minimal ELF.
	elf := buildELF(loadAddr, loadAddr, code)

	os.WriteFile("/tmp/hello-arm64", elf, 0755)
}

// adrX1 encodes ADR X1, #offset (PC-relative).
func adrX1(offset uint32) uint32 {
	// ADR Xd, label: encodes PC + offset into Xd.
	// Encoding: [immlo:2][10000][immhi:19][Rd:5]
	// offset is signed, relative to this instruction.
	immlo := offset & 0x3
	immhi := (offset >> 2) & 0x7FFFF
	return (immlo << 29) | (0b10000 << 24) | (immhi << 5) | 1
}

func buildELF(entryPoint, loadAddr uint64, code []byte) []byte {
	// ELF64 header (64 bytes) + 1 program header (56 bytes) + code.
	ehdrSize := 64
	phdrSize := 56
	headerSize := ehdrSize + phdrSize
	fileSize := headerSize + len(code)

	buf := make([]byte, fileSize)

	// ELF header.
	copy(buf[0:4], []byte{0x7f, 'E', 'L', 'F'}) // e_ident[EI_MAG]
	buf[4] = 2                                     // EI_CLASS = ELFCLASS64
	buf[5] = 1                                     // EI_DATA = ELFDATA2LSB
	buf[6] = 1                                     // EI_VERSION = EV_CURRENT
	buf[7] = 0                                     // EI_OSABI = ELFOSABI_NONE
	binary.LittleEndian.PutUint16(buf[16:], 2)     // e_type = ET_EXEC
	binary.LittleEndian.PutUint16(buf[18:], 183)   // e_machine = EM_AARCH64
	binary.LittleEndian.PutUint32(buf[20:], 1)     // e_version = EV_CURRENT
	binary.LittleEndian.PutUint64(buf[24:], entryPoint+uint64(headerSize)) // e_entry
	binary.LittleEndian.PutUint64(buf[32:], uint64(ehdrSize)) // e_phoff
	binary.LittleEndian.PutUint64(buf[40:], 0)     // e_shoff
	binary.LittleEndian.PutUint32(buf[48:], 0)     // e_flags
	binary.LittleEndian.PutUint16(buf[52:], uint16(ehdrSize)) // e_ehsize
	binary.LittleEndian.PutUint16(buf[54:], uint16(phdrSize)) // e_phentsize
	binary.LittleEndian.PutUint16(buf[56:], 1)     // e_phnum
	binary.LittleEndian.PutUint16(buf[58:], 0)     // e_shentsize
	binary.LittleEndian.PutUint16(buf[60:], 0)     // e_shnum
	binary.LittleEndian.PutUint16(buf[62:], 0)     // e_shstrndx

	// Program header (PT_LOAD).
	ph := buf[ehdrSize:]
	binary.LittleEndian.PutUint32(ph[0:], 1)       // p_type = PT_LOAD
	binary.LittleEndian.PutUint32(ph[4:], 5)       // p_flags = PF_R | PF_X
	binary.LittleEndian.PutUint64(ph[8:], 0)       // p_offset
	binary.LittleEndian.PutUint64(ph[16:], loadAddr) // p_vaddr
	binary.LittleEndian.PutUint64(ph[24:], loadAddr) // p_paddr
	binary.LittleEndian.PutUint64(ph[32:], uint64(fileSize)) // p_filesz
	binary.LittleEndian.PutUint64(ph[40:], uint64(fileSize)) // p_memsz
	binary.LittleEndian.PutUint64(ph[48:], uint64(pageSize)) // p_align

	// Code.
	copy(buf[headerSize:], code)

	return buf
}

const pageSize = 16384
