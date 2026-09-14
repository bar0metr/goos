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
	if breakpointGate&0x60 != 0x60 || interruptGate&0x60 != 0 {
		t.Fatal("invalid descriptor privilege levels")
	}
}
