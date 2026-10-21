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

func TestClassifyIRQ(t *testing.T) {
	net, _ := IRQObject(19)
	for _, tc := range []struct {
		object, net uint64
		routed      bool
		want        IRQLine
	}{
		{net, net, true, IRQNet},
		{net, net, false, IRQOther},
		{DiskIRQObject, net, true, IRQDisk},
		{DiskIRQObject, 0, false, IRQDisk},
		{InertIRQObject, net, true, IRQOther},
		{InertIRQObject, InertIRQObject, false, IRQOther},
		{4, net, true, IRQOther},
		// A network line on 11 would be the network's, never both.
		{DiskIRQObject, DiskIRQObject, true, IRQNet},
	} {
		if got := ClassifyIRQ(tc.object, tc.net, tc.routed); got != tc.want {
			t.Fatalf("ClassifyIRQ(%d, %d, %v) = %d, want %d", tc.object, tc.net, tc.routed, got, tc.want)
		}
	}
}
