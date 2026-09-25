package boot

const PageSize = uint64(4096)

// MaxInitramfs bounds the boot archive the firmware loader reads. The build
// refuses a larger archive and the kernel refuses to boot from one, so an
// archive can never be used cut short.
const MaxInitramfs = uint64(4 << 20)

// MemoryKind is architecture-neutral. Firmware-specific numeric values never
// cross the boot package boundary.
type MemoryKind uint8

const (
	MemoryReserved MemoryKind = iota
	MemoryUsable
	MemoryReclaimable
	MemoryRuntime
	// MemoryLoader is memory the firmware allocated for the loaded kernel:
	// its image, the TinyGo heap and stack, and the boot archive. It stays
	// reserved, and the kernel keeps reaching it at its physical address.
	MemoryLoader
)

// MemoryRegion describes whole physical pages using a half-open interval.
type MemoryRegion struct {
	Start uint64
	Pages uint64
	Kind  MemoryKind
}

func (region MemoryRegion) End() uint64 {
	return region.Start + region.Pages*PageSize
}

// Info is the immutable handoff from firmware-specific boot code to the
// architecture-independent kernel.
type Info struct {
	Regions          [128]MemoryRegion
	RegionCount      int
	UsableBytes      uint64
	FirmwareName     string
	InitramfsAddress uint64
	InitramfsSize    uint64
	// ACPIRoot is the physical address of the firmware's RSDP, or zero.
	ACPIRoot uint64
}

func (info *Info) AddRegion(region MemoryRegion) bool {
	if region.Pages == 0 || region.Start%PageSize != 0 || info.RegionCount == len(info.Regions) {
		return false
	}
	if region.End() < region.Start {
		return false
	}
	info.Regions[info.RegionCount] = region
	info.RegionCount++
	if region.Kind == MemoryUsable {
		info.UsableBytes += region.Pages * PageSize
	}
	return true
}

func (info *Info) MemoryRegions() []MemoryRegion {
	return info.Regions[:info.RegionCount]
}
