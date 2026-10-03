package cpuinfo

import "testing"

func TestSummaryWireRoundTripAndReservedBytes(t *testing.T) {
	var summary Summary
	summary.Validity = ValidIdentity | ValidTopology
	summary.PresentLogicalCPUs, summary.OnlineCPUs = 8, 1
	summary.Family, summary.Model, summary.Stepping = 6, 85, 7
	summary.ThreadsPerCore, summary.CoresPerSocket, summary.Sockets = 2, 4, 1
	copy(summary.Vendor[:], "GenuineIntel")
	summary.UsableFeatures.Add(FeatureSSE2)
	var wire [SummarySize]byte
	for index := range wire {
		wire[index] = 0xff
	}
	if !summary.Encode(wire[:]) {
		t.Fatal("encode failed")
	}
	for index, value := range wire[288:] {
		if value != 0 {
			t.Fatalf("reserved byte %d is %#x", index, value)
		}
	}
	got, ok := DecodeSummary(wire[:])
	if !ok || got.PresentLogicalCPUs != 8 || got.Model != 85 || !got.UsableFeatures.Has(FeatureSSE2) || string(got.Vendor[:12]) != "GenuineIntel" {
		t.Fatalf("round trip: %+v %v", got, ok)
	}
}

func TestEntryAndCacheWireRoundTrip(t *testing.T) {
	entry := CPUEntry{LogicalID: 3, APICID: 7, ACPIUID: 9, Flags: CPUEntryPresent, PackageID: 1, CoreID: 2, ThreadID: 1}
	var entryWire [CPUEntrySize]byte
	entry.Encode(entryWire[:])
	if got, ok := DecodeCPUEntry(entryWire[:]); !ok || got != entry {
		t.Fatalf("entry: %+v %v", got, ok)
	}
	cache := CacheInfo{Validity: 1, Level: 3, Type: CacheUnified, LineSize: 64, Partitions: 1, Ways: 16, Sets: 8192, SharingCount: 8, Size: 8 << 20}
	var cacheWire [CacheInfoSize]byte
	cache.Encode(cacheWire[:])
	if got, ok := DecodeCacheInfo(cacheWire[:]); !ok || got != cache {
		t.Fatalf("cache: %+v %v", got, ok)
	}
}

func TestWireRejectsWrongSizesAndVersions(t *testing.T) {
	if _, ok := DecodeSummary(make([]byte, SummarySize-1)); ok {
		t.Fatal("accepted short summary")
	}
	bad := make([]byte, SummarySize)
	put16(bad[0:2], Version+1)
	put16(bad[2:4], SummarySize)
	if _, ok := DecodeSummary(bad); ok {
		t.Fatal("accepted unknown version")
	}
}
