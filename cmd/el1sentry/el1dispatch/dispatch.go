//go:build darwin && arm64

// Package el1dispatch provides a Go-assembled EL1 syscall dispatcher.
package el1dispatch

import (
	"encoding/binary"
	"reflect"
	"unsafe"
)

// dispatch is the EL1 syscall handler (defined in dispatch_arm64.s).
func dispatch()

// dispatchEnd marks the end of the dispatch function.
func dispatchEnd()

// Code returns the raw machine code bytes of the dispatch function.
// Follows the ABI wrapper JMP to find the real function body.
func Code() []byte {
	start := followJMP(reflect.ValueOf(dispatch).Pointer())
	end := followJMP(reflect.ValueOf(dispatchEnd).Pointer())
	size := end - start
	return unsafe.Slice((*byte)(unsafe.Pointer(start)), size)
}

// followJMP checks if addr points to a B (unconditional branch)
// instruction and follows it to the target. Go 1.22+ generates
// ABI wrappers that are just JMP instructions.
func followJMP(addr uintptr) uintptr {
	instr := binary.LittleEndian.Uint32(
		unsafe.Slice((*byte)(unsafe.Pointer(addr)), 4))
	// B imm26: 000101 imm26
	if instr>>26 == 0x05 {
		// Sign-extend 26-bit offset
		off := int32(instr&0x3FFFFFF) << 6 >> 6
		return uintptr(int64(addr) + int64(off)*4)
	}
	return addr
}
