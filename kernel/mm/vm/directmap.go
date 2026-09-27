package vm

import "goos/kernel/mm/pmm"

const (
	// DirectMapBase is where the kernel maps every managed physical page:
	// physical address p is reachable at DirectMapBase+p. It is the start of
	// PML4 slot 256, the first canonical upper-half address, and the slot is
	// shared by every address space (ADR 0103).
	DirectMapBase = uint64(0xffff800000000000)
	// KernelLowBase aliases the bootstrap low GiB in the higher half. Kernel
	// entry code and kernel stacks use this alias while process roots leave the
	// lower half to userspace.
	KernelLowBase = uint64(0xffffc00000000000)
	// LargePageSize is the size of the direct map's leaves.
	LargePageSize = uint64(2 << 20)

	directMapSlot    = (DirectMapBase >> 39) & 0x1ff
	kernelLowSlot    = (KernelLowBase >> 39) & 0x1ff
	identityMapStart = uint64(1 << 20)
	entryLarge       = uint64(1 << 7)
)

// KernelAlias returns the higher-half alias of one bootstrap identity address.
func KernelAlias(address uint64) (uint64, bool) {
	if address < identityMapStart || address >= IdentityMapLimit {
		return 0, false
	}
	return KernelLowBase + address, true
}

// KernelPhysical returns the bootstrap physical address behind a higher-half
// kernel alias.
func KernelPhysical(address uint64) (uint64, bool) {
	if address < KernelLowBase+identityMapStart || address >= KernelLowBase+IdentityMapLimit {
		return 0, false
	}
	return address - KernelLowBase, true
}

// physicalRange is a half-open range of physical addresses.
type physicalRange struct {
	start uint64
	end   uint64
}

// directMap records the physical ranges the direct map covers: the page
// allocator's segments. The leaves are two MiB and may reach past a range's
// ends, but the kernel only uses the recorded ranges.
type directMap struct {
	ranges [pmm.MaxSegments]physicalRange
	count  int
}

func (mapped *directMap) add(start, end uint64) bool {
	if mapped.count == len(mapped.ranges) || start >= end || start%PageSize != 0 || end%PageSize != 0 || end > pmm.MaxPhysical {
		return false
	}
	mapped.ranges[mapped.count] = physicalRange{start: start, end: end}
	mapped.count++
	return true
}

// kernelAddress returns the kernel virtual address of the length bytes of
// physical memory at physical. Memory in the identity-mapped range
// [1 MiB, IdentityMapLimit) is available at its physical address: page tables,
// stack backing, and the boot archive live there and are not all managed
// pages. Memory above IdentityMapLimit is reached through the direct map, and
// only inside a range the direct map records. The range must not wrap or
// straddle the two.
func (mapped *directMap) kernelAddress(physical, length uint64) (uint64, bool) {
	end := physical + length
	if length == 0 || end < physical {
		return 0, false
	}
	if end <= IdentityMapLimit {
		return physical, physical >= identityMapStart
	}
	if physical < IdentityMapLimit {
		return 0, false
	}
	for index := 0; index < mapped.count; index++ {
		covered := mapped.ranges[index]
		if physical >= covered.start && end <= covered.end {
			return DirectMapBase + physical, true
		}
	}
	return 0, false
}

// largePageEntry returns the direct-map leaf for the two MiB page at
// physical: present, writable, supervisor-only, and never executable.
func largePageEntry(physical uint64) uint64 {
	return physical&addressMask&^(LargePageSize-1) | entryPresent | entryWritable | entryLarge | entryNX
}
