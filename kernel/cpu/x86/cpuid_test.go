package x86

import "testing"

type leaf struct {
	leaf    uint32
	subleaf uint32
}

type fakeSource struct {
	data    map[leaf]Registers
	queries map[leaf]int
}

func (source *fakeSource) Query(leafValue, subleaf uint32) Registers {
	key := leaf{leafValue, subleaf}
	if source.queries == nil {
		source.queries = make(map[leaf]int)
	}
	source.queries[key]++
	return source.data[key]
}

func source(data map[leaf]Registers) *fakeSource { return &fakeSource{data: data} }

func words(value string) [12]uint32 {
	var bytes [48]byte
	copy(bytes[:], value)
	var result [12]uint32
	for index := range result {
		offset := index * 4
		result[index] = uint32(bytes[offset]) | uint32(bytes[offset+1])<<8 |
			uint32(bytes[offset+2])<<16 | uint32(bytes[offset+3])<<24
	}
	return result
}

func vendorRegisters(maximum uint32, vendor string) Registers {
	value := words(vendor)
	return Registers{EAX: maximum, EBX: value[0], EDX: value[1], ECX: value[2]}
}

func TestProbeIdentity(t *testing.T) {
	brand := words("  Example CPU 9000                         ")
	data := map[leaf]Registers{
		{0, 0}:          vendorRegisters(0x16, "GenuineIntel"),
		{1, 0}:          {EAX: 7 | 5<<4 | 6<<8 | 5<<16, EBX: 9 << 24, ECX: 1 << 31},
		{0x40000000, 0}: {EBX: words("KVMKVMKVM")[0], ECX: words("KVMKVMKVM")[1], EDX: words("KVMKVMKVM")[2]},
		{0x80000000, 0}: {EAX: 0x80000008},
		{0x80000001, 0}: {ECX: 1 << 2},
		{0x80000002, 0}: {EAX: brand[0], EBX: brand[1], ECX: brand[2], EDX: brand[3]},
		{0x80000003, 0}: {EAX: brand[4], EBX: brand[5], ECX: brand[6], EDX: brand[7]},
		{0x80000004, 0}: {EAX: brand[8], EBX: brand[9], ECX: brand[10], EDX: brand[11]},
		{0x80000008, 0}: {EAX: 46 | 48<<8},
		{0x16, 0}:       {EAX: 2400, EBX: 3600, ECX: 100},
	}
	got := Probe(source(data)).Identity
	if got.Vendor.String() != "GenuineIntel" || got.Brand.String() != "Example CPU 9000" {
		t.Fatalf("identity strings: vendor=%q brand=%q", got.Vendor.String(), got.Brand.String())
	}
	if !got.SignatureValid || got.Family != 6 || got.Model != 85 || got.Stepping != 7 || got.BootstrapAPICID != 9 {
		t.Fatalf("signature: %+v", got)
	}
	if !got.AddressWidthsValid || got.PhysicalAddressBits != 46 || got.LinearAddressBits != 48 {
		t.Fatalf("address widths: %+v", got)
	}
	if !got.FrequencyValid || got.BaseMHz != 2400 || got.MaximumMHz != 3600 || got.BusMHz != 100 {
		t.Fatalf("frequency: %+v", got)
	}
	if !got.HypervisorPresent || got.HypervisorVendor.String() != "KVMKVMKVM" || !got.SVM {
		t.Fatalf("virtualization: %+v", got)
	}
}

func TestProbePreservesVendorsAndDecodesExtendedSignature(t *testing.T) {
	for _, test := range []struct {
		name      string
		vendor    string
		signature uint32
		family    uint32
		model     uint32
		stepping  uint32
	}{
		{"amd", "AuthenticAMD", 1 | 3<<4 | 0xf<<8 | 2<<16 | 8<<20, 0x17, 0x23, 1},
		{"unknown", "OddVendor123", 2 | 4<<4 | 5<<8 | 9<<16 | 7<<20, 5, 4, 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := Probe(source(map[leaf]Registers{
				{0, 0}:          vendorRegisters(1, test.vendor),
				{1, 0}:          {EAX: test.signature},
				{0x80000000, 0}: {},
			})).Identity
			if got.Vendor.String() != test.vendor || got.Family != test.family || got.Model != test.model || got.Stepping != test.stepping {
				t.Fatalf("got vendor=%q family=%#x model=%#x stepping=%d", got.Vendor.String(), got.Family, got.Model, got.Stepping)
			}
		})
	}
}

func TestProbeOmitsUnavailableBrandAndHypervisor(t *testing.T) {
	input := source(map[leaf]Registers{
		{0, 0}:          vendorRegisters(1, "GenuineIntel"),
		{1, 0}:          {},
		{0x80000000, 0}: {EAX: 0x80000001},
	})
	got := Probe(input).Identity
	if got.Brand.Length != 0 || got.HypervisorPresent || got.HypervisorVendor.Length != 0 {
		t.Fatalf("reported unavailable data: %+v", got)
	}
	if input.queries[leaf{0x40000000, 0}] != 0 {
		t.Fatal("queried hypervisor leaf without the hypervisor bit")
	}
	if input.queries[leaf{0x80000002, 0}] != 0 {
		t.Fatal("queried unavailable brand leaf")
	}
}
