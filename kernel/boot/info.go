package boot

const PageSize = uint64(4096)

// MaxInitramfs bounds the boot archive the firmware loader reads. The build
// refuses a larger archive and the kernel refuses to boot from one, so an
// archive can never be used cut short. The kernel and the archive share the
// 8 MiB EFI system partition, which caps it at 6 MiB (ADR 0087).
const MaxInitramfs = uint64(6 << 20)

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

// MaxRegions bounds the memory map the kernel keeps. Adjacent regions of
// one kind are merged as they are added, so it bounds the changes of kind
// along physical memory, not the firmware's descriptor count. OVMF with four
// vCPUs under Proxmox gives 138 descriptors.
const MaxRegions = 256

// Info is the immutable handoff from firmware-specific boot code to the
// architecture-independent kernel.
type Info struct {
	Regions          [MaxRegions]MemoryRegion
	RegionCount      int
	UsableBytes      uint64
	FirmwareName     string
	InitramfsAddress uint64
	InitramfsSize    uint64
	Framebuffer      FramebufferInfo
	// ACPIRoot is the physical address of the firmware's RSDP, or zero.
	ACPIRoot uint64
}

// AddRegion appends a region, or extends the last one when the new region
// continues it with the same kind. Firmware splits memory of one kind into
// many descriptors (boot services data, per CPU), and the kernel needs only
// the kind.
func (info *Info) AddRegion(region MemoryRegion) bool {
	if region.Pages == 0 || region.Start%PageSize != 0 || region.Pages > (^uint64(0)-region.Start)/PageSize {
		return false
	}
	if info.RegionCount > 0 {
		last := &info.Regions[info.RegionCount-1]
		if last.Kind == region.Kind && last.End() == region.Start && last.Pages+region.Pages <= (^uint64(0)-last.Start)/PageSize {
			last.Pages += region.Pages
			if region.Kind == MemoryUsable {
				info.UsableBytes += region.Pages * PageSize
			}
			return true
		}
	}
	if info.RegionCount == len(info.Regions) {
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
