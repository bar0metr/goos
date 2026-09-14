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
