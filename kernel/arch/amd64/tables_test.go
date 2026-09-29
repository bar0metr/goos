package amd64

import (
	"encoding/binary"
	"testing"
)

func TestIDTEntryLayoutAndAddress(t *testing.T) {
	if binary.Size(IDTEntry{}) != 16 {
		t.Fatalf("IDT entry size = %d", binary.Size(IDTEntry{}))
	}
	const handler = uintptr(0xffff800012345678)
	entry := NewIDTEntry(handler, interruptGate)
	if entry.Handler() != handler || entry.Selector != kernelCodeSelector || entry.Attributes != interruptGate {
		t.Fatalf("unexpected IDT entry: %#v", entry)
	}
}

func TestBreakpointGateIsUserCallable(t *testing.T) {
	if breakpointGate&0x60 != 0x60 || userCallGate&0x60 != 0x60 || interruptGate&0x60 != 0 {
		t.Fatal("invalid descriptor privilege levels")
	}
}

func TestLinuxSyscallSelectorsAndFlagMask(t *testing.T) {
	if uint16((linuxSTAR>>32)&0xffff) != kernelCodeSelector || uint16((linuxSTAR>>48)&0xffff) != userDataSelector-8 {
		t.Fatalf("STAR = %#x", linuxSTAR)
	}
	const required = uint64(1<<9 | 1<<10 | 1<<8 | 1<<18)
	if linuxFMASK != required {
		t.Fatalf("FMASK = %#x", linuxFMASK)
	}
}

func TestTSSDescriptorEncodesBaseAndLimit(t *testing.T) {
	const base = uintptr(0x123456789abc)
	low, high := tssDescriptor(base, 103)
	decodedBase := uintptr((low>>16)&0xffffff) |
		uintptr((low>>56)&0xff)<<24 |
		uintptr(high&0xffffffff)<<32
	decodedLimit := uint32(low&0xffff) | uint32((low>>48)&0xf)<<16
	if decodedBase != base || decodedLimit != 103 || byte(low>>40) != 0x89 {
		t.Fatalf("invalid TSS descriptor: low=%#x high=%#x", low, high)
	}
}

func TestInitialFPUAreaMasksExceptionsAndClearsState(t *testing.T) {
	var area [FPUAreaSize]byte
	for index := range area {
		area[index] = 0xa5
	}
	InitialFPUArea(&area)
	if area[0] != 0x7f || area[1] != 0x03 || area[24] != 0x80 || area[25] != 0x1f || area[26] != 0 || area[27] != 0 {
		t.Fatalf("control words %x %x", area[0:2], area[24:28])
	}
	for index, value := range area {
		if index != 0 && index != 1 && index != 24 && index != 25 && value != 0 {
			t.Fatalf("byte %d kept %#x", index, value)
		}
	}
}
