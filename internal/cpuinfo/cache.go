package cpuinfo

const MaxCaches = 16

const CacheGeometryValid uint32 = 1

// CacheType is a stable cache kind in the CPU cache record.
type CacheType uint8

const (
	CacheData CacheType = iota + 1
	CacheInstruction
	CacheUnified
)
