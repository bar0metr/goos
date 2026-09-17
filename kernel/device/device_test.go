package device

import "testing"

func TestPortObjectRoundTripAndBounds(t *testing.T) {
	object, ok := PortObject(0xc040, 0x40)
	if !ok {
		t.Fatal("valid port range rejected")
	}
	base, size, ok := DecodePortObject(object)
	if !ok || base != 0xc040 || size != 0x40 {
		t.Fatalf("unexpected decoded range %#x %#x", base, size)
	}
	if _, ok := PortObject(0xfff0, 0x20); ok {
		t.Fatal("wrapping port range accepted")
	}
}

func TestDMAAndIRQObjectsRejectInvalidAuthority(t *testing.T) {
	object, ok := DMAObject(1)
	if domain, decoded := DecodeDMAObject(object); !ok || !decoded || domain != 1 {
		t.Fatal("DMA authority did not round trip")
	}
	if _, ok := DMAObject(0); ok {
		t.Fatal("zero DMA domain accepted")
	}
	if _, ok := IRQObject(0xff); ok {
		t.Fatal("missing PCI IRQ accepted")
	}
}
