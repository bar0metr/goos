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
	got, ok := DetectTopology(source(data), "GenuineIntel", 0x1f, 0)
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
	got, ok := DetectTopology(source(topologyFixture(0x0b)), "GenuineIntel", 0x0b, 0)
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
			if got, ok := DetectTopology(source(test.data), "unknown", test.max, 0); ok || got.Valid {
				t.Fatalf("accepted %+v", got)
			}
		})
	}
}

func TestDetectTopologyUsesConservativeVendorFallback(t *testing.T) {
	intel := source(map[leaf]Registers{
		{1, 0}: {EBX: 8 << 16},
		{4, 0}: {EAX: 1 | 3<<26},
	})
	got, ok := DetectTopology(intel, "GenuineIntel", 4, 0)
	if !ok || got.ThreadsPerCore != 2 || got.CoresPerPackage != 4 || got.PackageShift != 3 {
		t.Fatalf("Intel fallback: %+v %v", got, ok)
	}

	amd := source(map[leaf]Registers{
		{1, 0}:          {EBX: 8 << 16},
		{0x80000008, 0}: {ECX: 3 | 2<<12},
	})
	got, ok = DetectTopology(amd, "AuthenticAMD", 1, 0x80000008)
	if !ok || got.ThreadsPerCore != 2 || got.CoresPerPackage != 4 || got.PackageShift != 3 {
		t.Fatalf("AMD fallback: %+v %v", got, ok)
	}
}

func TestDetectTopologyRejectsAmbiguousVendorFallback(t *testing.T) {
	data := source(map[leaf]Registers{
		{1, 0}: {EBX: 6 << 16},
		{4, 0}: {EAX: 1 | 2<<26},
	})
	if got, ok := DetectTopology(data, "GenuineIntel", 4, 0); ok || got.Valid {
		t.Fatalf("accepted ambiguous fallback: %+v", got)
	}
}
