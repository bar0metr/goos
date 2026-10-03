package x86

import "goos/internal/cpuinfo"

const (
	CacheData        = cpuinfo.CacheData
	CacheInstruction = cpuinfo.CacheInstruction
	CacheUnified     = cpuinfo.CacheUnified
)

type Cache struct {
	Level        uint8
	Type         cpuinfo.CacheType
	LineSize     uint32
	Partitions   uint32
	Ways         uint32
	Sets         uint32
	SharingCount uint32
	Size         uint64
}

func DetectCaches(source Source, vendor string, maximumBasicLeaf, maximumExtendedLeaf uint32) ([MaxCaches]Cache, uint8) {
	leaf := uint32(4)
	if vendor == "AuthenticAMD" && maximumExtendedLeaf >= 0x8000001d {
		leaf = 0x8000001d
	} else if maximumBasicLeaf < 4 {
		return [MaxCaches]Cache{}, 0
	}
	var caches [MaxCaches]Cache
	var count uint8
	for subleaf := uint32(0); subleaf < MaxCaches; subleaf++ {
		registers := source.Query(leaf, subleaf)
		if registers.EAX&0x1f == 0 {
			break
		}
		cache, ok := decodeCache(registers)
		if !ok {
			continue
		}
		caches[count] = cache
		count++
	}
	return caches, count
}

func decodeCache(registers Registers) (Cache, bool) {
	cacheType := cpuinfo.CacheType(registers.EAX & 0x1f)
	if cacheType < CacheData || cacheType > CacheUnified {
		return Cache{}, false
	}
	cache := Cache{
		Type:         cacheType,
		Level:        uint8(registers.EAX >> 5 & 0x7),
		SharingCount: registers.EAX>>14&0xfff + 1,
		LineSize:     registers.EBX&0xfff + 1,
		Partitions:   registers.EBX>>12&0x3ff + 1,
		Ways:         registers.EBX>>22&0x3ff + 1,
		Sets:         registers.ECX + 1,
	}
	if cache.Level == 0 || cache.Sets == 0 {
		return Cache{}, false
	}
	size, ok := multiply64(uint64(cache.LineSize), uint64(cache.Partitions))
	if !ok {
		return Cache{}, false
	}
	size, ok = multiply64(size, uint64(cache.Ways))
	if !ok {
		return Cache{}, false
	}
	size, ok = multiply64(size, uint64(cache.Sets))
	if !ok || size == 0 {
		return Cache{}, false
	}
	cache.Size = size
	return cache, true
}

func multiply64(left, right uint64) (uint64, bool) {
	if left != 0 && right > ^uint64(0)/left {
		return 0, false
	}
	return left * right, true
}
