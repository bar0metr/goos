package x86

import (
	"goos/internal/cpuinfo"
	"testing"
)

func cacheRegisters(cacheType cpuinfo.CacheType, level, sharing, line, partitions, ways, sets uint32) Registers {
	return Registers{
		EAX: uint32(cacheType) | level<<5 | (sharing-1)<<14,
		EBX: line - 1 | (partitions-1)<<12 | (ways-1)<<22,
		ECX: sets - 1,
	}
}

func TestDetectCaches(t *testing.T) {
	data := map[leaf]Registers{
		{4, 0}: cacheRegisters(CacheData, 1, 2, 64, 1, 8, 64),
		{4, 1}: cacheRegisters(CacheInstruction, 1, 2, 64, 1, 8, 64),
		{4, 2}: cacheRegisters(CacheUnified, 2, 2, 64, 1, 8, 1024),
		{4, 3}: cacheRegisters(CacheUnified, 3, 8, 64, 1, 16, 8192),
		{4, 4}: {},
	}
	caches, count := DetectCaches(source(data), "GenuineIntel", 4, 0)
	if count != 4 {
		t.Fatalf("count %d", count)
	}
	checks := []struct {
		kind  cpuinfo.CacheType
		level uint8
		size  uint64
	}{
		{CacheData, 1, 32 << 10},
		{CacheInstruction, 1, 32 << 10},
		{CacheUnified, 2, 512 << 10},
		{CacheUnified, 3, 8 << 20},
	}
	for index, want := range checks {
		got := caches[index]
		if got.Type != want.kind || got.Level != want.level || got.Size != want.size || got.LineSize != 64 {
			t.Errorf("cache %d: %+v", index, got)
		}
	}
}

func TestDetectCachesUsesAMDExtendedLeaf(t *testing.T) {
	data := map[leaf]Registers{
		{4, 0}:          cacheRegisters(CacheData, 1, 1, 32, 1, 1, 1),
		{0x8000001d, 0}: cacheRegisters(CacheUnified, 3, 4, 64, 1, 16, 4096),
		{0x8000001d, 1}: {},
	}
	caches, count := DetectCaches(source(data), "AuthenticAMD", 4, 0x8000001d)
	if count != 1 || caches[0].Type != CacheUnified || caches[0].Size != 4<<20 {
		t.Fatalf("caches: %+v count=%d", caches[0], count)
	}
}

func TestDecodeCacheRejectsInvalidRecords(t *testing.T) {
	for _, registers := range []Registers{
		{EAX: 4 | 1<<5},
		{EAX: uint32(CacheData)},
	} {
		if cache, ok := decodeCache(registers); ok {
			t.Fatalf("accepted %+v", cache)
		}
	}
}

func TestCacheSizeMultiplicationRejectsOverflow(t *testing.T) {
	if _, ok := multiply64(^uint64(0), 2); ok {
		t.Fatal("accepted overflowing cache size")
	}
}
