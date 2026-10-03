package x86

import "testing"

func topologyFixture(leafValue uint32) map[leaf]Registers {
	return map[leaf]Registers{
		{leafValue, 0}: {EAX: 1, EBX: 2, ECX: 1 << 8, EDX: 6},
		{leafValue, 1}: {EAX: 3, EBX: 8, ECX: 2 << 8, EDX: 6},
		{leafValue, 2}: {},
	}
}

func TestDetectTopologyPrefersLeaf1F(t *testing.T) {
	data := topologyFixture(0x1f)
	data[leaf{0x0b, 0}] = Registers{EAX: 2, EBX: 4, ECX: 1 << 8}
	got, ok := DetectTopology(source(data), 0x1f)
	if !ok || !got.Valid || got.ThreadsPerCore != 2 || got.CoresPerPackage != 4 ||
		got.LogicalCPUsPerPackage != 8 || got.SMTShift != 1 || got.PackageShift != 3 || got.BootstrapX2APICID != 6 {
		t.Fatalf("topology: %+v %v", got, ok)
	}
	ids, ok := got.ID(0b10110)
	if !ok || ids.Package != 2 || ids.Core != 3 || ids.Thread != 0 {
		t.Fatalf("ids: %+v %v", ids, ok)
	}
}

func TestDetectTopologyFallsBackToLeaf0B(t *testing.T) {
	got, ok := DetectTopology(source(topologyFixture(0x0b)), 0x0b)
	if !ok || got.ThreadsPerCore != 2 || got.CoresPerPackage != 4 {
		t.Fatalf("topology: %+v %v", got, ok)
	}
}

func TestDetectTopologyRejectsMalformedData(t *testing.T) {
	for _, test := range []struct {
		name string
		data map[leaf]Registers
		max  uint32
	}{
		{"absent", map[leaf]Registers{}, 0x0a},
		{"zero count", map[leaf]Registers{{0x0b, 0}: {EAX: 1, ECX: 1 << 8}}, 0x0b},
		{"non-increasing shifts", map[leaf]Registers{
			{0x0b, 0}: {EAX: 2, EBX: 2, ECX: 1 << 8},
			{0x0b, 1}: {EAX: 2, EBX: 4, ECX: 2 << 8},
		}, 0x0b},
		{"inconsistent divisibility", map[leaf]Registers{
			{0x0b, 0}: {EAX: 2, EBX: 3, ECX: 1 << 8},
			{0x0b, 1}: {EAX: 3, EBX: 8, ECX: 2 << 8},
		}, 0x0b},
		{"changing x2apic", map[leaf]Registers{
			{0x0b, 0}: {EAX: 1, EBX: 2, ECX: 1 << 8, EDX: 1},
			{0x0b, 1}: {EAX: 3, EBX: 8, ECX: 2 << 8, EDX: 2},
		}, 0x0b},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got, ok := DetectTopology(source(test.data), test.max); ok || got.Valid {
				t.Fatalf("accepted %+v", got)
			}
		})
	}
}
