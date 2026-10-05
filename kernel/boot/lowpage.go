package boot

// Processor start-up code runs from a page below 1 MiB: a started processor
// begins in real mode at an address its start-up signal names in 4 KiB
// units. LowPageFloor skips the real-mode interrupt table and BIOS data,
// LowPageCeiling the extended BIOS data area.
const (
	LowPageFloor   = uint64(0x10000)
	LowPageCeiling = uint64(0x9f000)
)

// LowPage returns the highest usable page in [LowPageFloor, LowPageCeiling).
// The page allocator manages nothing below 1 MiB, so the page stays the
// caller's for good.
func LowPage(regions []MemoryRegion) (uint64, bool) {
	best, found := uint64(0), false
	for _, region := range regions {
		if region.Kind != MemoryUsable || region.Start%PageSize != 0 || region.Pages == 0 {
			continue
		}
		start, end := region.Start, region.End()
		if end < start {
			continue
		}
		if start < LowPageFloor {
			start = LowPageFloor
		}
		if end > LowPageCeiling {
			end = LowPageCeiling
		}
		if end < start+PageSize {
			continue
		}
		if page := end - PageSize; !found || page > best {
			best, found = page, true
		}
	}
	return best, found
}
