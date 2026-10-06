package pci

import "testing"

// fakeConfig models configuration space with BARs that size like hardware:
// writing all ones to a BAR reads back the size mask, and any other write is
// stored as the new base.
type fakeConfig struct {
	values map[uint32]uint32
	sizes  map[uint32]uint32
	writes int
}

func key(bus, slot, function, offset uint8) uint32 {
	return uint32(bus)<<24 | uint32(slot)<<16 | uint32(function)<<8 | uint32(offset)
}

func newFake() *fakeConfig {
	return &fakeConfig{values: map[uint32]uint32{}, sizes: map[uint32]uint32{}}
}

func (f *fakeConfig) Read32(bus, slot, function, offset uint8) uint32 {
	if value, ok := f.values[key(bus, slot, function, offset)]; ok {
		return value
	}
	return 0xffffffff
}

func (f *fakeConfig) Write32(bus, slot, function, offset uint8, value uint32) {
	f.writes++
	k := key(bus, slot, function, offset)
	if size, sized := f.sizes[k]; sized && value == 0xffffffff {
		f.values[k] = ^(size - 1) | 1
		return
	}
	f.values[k] = value
}

// add places a legacy virtio function with an I/O BAR of size bytes.
func (f *fakeConfig) add(bus, slot, function uint8, device uint16, bar, size uint32, line, pin uint8) {
	f.values[key(bus, slot, function, offsetID)] = uint32(device)<<16 | uint32(VirtioVendor)
	f.values[key(bus, slot, function, offsetBAR0)] = bar
	f.sizes[key(bus, slot, function, offsetBAR0)] = size
	f.values[key(bus, slot, function, offsetInterrupt)] = uint32(pin)<<8 | uint32(line)
}

func TestFindLegacyVirtioBlock(t *testing.T) {
	f := newFake()
	f.add(3, 4, 0, LegacyBlockDevice, 0xc041, 0x40, 11, 1)
	device, ok := FindLegacyVirtioBlock(f)
	if !ok || device.Bus != 3 || device.Slot != 4 || device.IOBase != 0xc040 || device.IOSize != 0x40 || device.IRQ != 11 || device.Pin != 1 {
		t.Fatalf("unexpected device: %+v", device)
	}
	if f.values[key(3, 4, 0, offsetBAR0)] != 0xc041 {
		t.Fatalf("BAR not restored after sizing: %#x", f.values[key(3, 4, 0, offsetBAR0)])
	}
}

func TestFindLegacyVirtioNetIndependentOfBlock(t *testing.T) {
	f := newFake()
	f.add(0, 2, 0, LegacyBlockDevice, 0xc001, 0x80, 11, 1)
	f.add(0, 4, 0, LegacyNetDevice, 0xc0c1, 0x20, 10, 1)
	net, problem := FindLegacyVirtioNet(f)
	if problem != ProblemNone || net.Slot != 4 || net.IOBase != 0xc0c0 || net.IOSize != 0x20 || net.IRQ != 10 {
		t.Fatalf("net = %+v, %d", net, problem)
	}
	block, ok := FindLegacyVirtioBlock(f)
	if !ok || block.Slot != 2 || block.Overlaps(net) {
		t.Fatalf("block = %+v, overlap %v", block, block.Overlaps(net))
	}
	if f.values[key(0, 4, 0, offsetBAR0)] != 0xc0c1 {
		t.Fatal("net BAR not restored")
	}
	// Discovery without a NIC still finds the disk.
	g := newFake()
	g.add(0, 2, 0, LegacyBlockDevice, 0xc001, 0x80, 11, 1)
	if _, problem := FindLegacyVirtioNet(g); problem != ProblemAbsent {
		t.Fatalf("absent NIC reported %d", problem)
	}
	if _, ok := FindLegacyVirtioBlock(g); !ok {
		t.Fatal("block lost without a NIC")
	}
}

func TestFindLegacyVirtioNetRejectsSecondFunction(t *testing.T) {
	f := newFake()
	f.add(0, 4, 0, LegacyNetDevice, 0xc0c1, 0x20, 10, 1)
	f.add(0, 5, 0, LegacyNetDevice, 0xc0e1, 0x20, 10, 1)
	if _, problem := FindLegacyVirtioNet(f); problem != ProblemDuplicate {
		t.Fatalf("second NIC gave %d", problem)
	}
}

func TestBARAndInterruptRejection(t *testing.T) {
	cases := []struct {
		name      string
		bar, size uint32
		line, pin uint8
		want      Problem
	}{
		{"memory BAR", 0xfebf0000, 0x1000, 10, 1, ProblemBAR},
		{"zero base", 0x1, 0x20, 10, 1, ProblemBAR},
		{"too small", 0xc0c1, 0x10, 10, 1, ProblemBAR},
		{"beyond 16-bit space", 0x10001, 0x20, 10, 1, ProblemBAR},
		{"misaligned", 0xc0d1, 0x20, 10, 1, ProblemBAR},
		{"no line", 0xc0c1, 0x20, 0, 1, ProblemInterrupt},
		{"line 255", 0xc0c1, 0x20, 0xff, 1, ProblemInterrupt},
		{"no pin", 0xc0c1, 0x20, 10, 0, ProblemInterrupt},
		{"pin 5", 0xc0c1, 0x20, 10, 5, ProblemInterrupt},
	}
	for _, test := range cases {
		f := newFake()
		f.add(0, 4, 0, LegacyNetDevice, test.bar, test.size, test.line, test.pin)
		if _, problem := FindLegacyVirtioNet(f); problem != test.want {
			t.Errorf("%s: got %d want %d", test.name, problem, test.want)
		}
		if f.values[key(0, 4, 0, offsetBAR0)] != test.bar {
			t.Errorf("%s: BAR not restored", test.name)
		}
	}
}

func TestSixteenBitIOBAR(t *testing.T) {
	f := newFake()
	f.add(0, 4, 0, LegacyNetDevice, 0xc0c1, 0x20, 10, 1)
	// A function that decodes 16 bits reads the upper half of a sized BAR as
	// zero.
	f.sizes[key(0, 4, 0, offsetBAR0)] = 0
	f.values[key(0, 4, 0, offsetBAR0)] = 0xc0c1
	sized := &sixteenBit{fakeConfig: f}
	device, problem := FindLegacyVirtioNet(sized)
	if problem != ProblemNone || device.IOSize != 0x20 {
		t.Fatalf("16-bit BAR: %+v %d", device, problem)
	}
}

type sixteenBit struct{ *fakeConfig }

func (s *sixteenBit) Write32(bus, slot, function, offset uint8, value uint32) {
	if offset == offsetBAR0 && value == 0xffffffff {
		s.values[key(bus, slot, function, offset)] = 0x0000ffe1
		return
	}
	s.fakeConfig.Write32(bus, slot, function, offset, value)
}

func TestOverlapAndCommand(t *testing.T) {
	a := Device{IOBase: 0xc000, IOSize: 0x40}
	if !a.Overlaps(Device{IOBase: 0xc020, IOSize: 0x20}) || a.Overlaps(Device{IOBase: 0xc040, IOSize: 0x20}) {
		t.Fatal("overlap test wrong")
	}
	if got := EnableCommand(0x0400 | 0x0100); got != 0x0100|CommandIO|CommandBusMaster {
		t.Fatalf("command %#x", got)
	}
}
