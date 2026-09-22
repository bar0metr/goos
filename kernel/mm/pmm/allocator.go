package pmm

import (
	"goos/kernel/boot"
	"math/bits"
)

const (
	PageSize       = boot.PageSize
	managedBytes   = uint64(1 << 30)
	managedPages   = managedBytes / PageSize
	bitmapWordBits = uint64(64)
	lowMemoryPages = uint64(256)
	invalidPage    = ^uint64(0)
)

type Stats struct {
	ManagedPages uint64
	FreePages    uint64
	UsedPages    uint64
}

// Allocator tracks the first GiB of physical memory with one bit per page.
// The fixed-size metadata avoids allocation while the kernel allocator is
// itself being established. Higher memory will be added with sparse bitmaps.
type Allocator struct {
	used        [managedPages / bitmapWordBits]uint64
	freePages   uint64
	managedPage uint64
	next        uint64
}

func (allocator *Allocator) Init(regions []boot.MemoryRegion) {
	for index := range allocator.used {
		allocator.used[index] = ^uint64(0)
	}
	allocator.freePages = 0
	allocator.managedPage = 0
	allocator.next = lowMemoryPages

	for _, region := range regions {
		if region.Kind != boot.MemoryUsable {
			continue
		}
		start := region.Start / PageSize
		end := start + region.Pages
		if end < start {
			continue
		}
		if end > managedPages {
			end = managedPages
		}
		if end > allocator.managedPage {
			allocator.managedPage = end
		}
		if start < lowMemoryPages {
			start = lowMemoryPages
		}
		for page := start; page < end; page++ {
			allocator.markFree(page)
		}
	}
}

func (allocator *Allocator) Allocate() (uint64, bool) {
	if allocator.freePages == 0 || allocator.managedPage == 0 {
		return 0, false
	}
	page, found := allocator.firstFree(allocator.next, allocator.managedPage)
	if !found {
		page, found = allocator.firstFree(0, allocator.next)
	}
	if !found {
		return 0, false
	}
	allocator.markUsed(page)
	allocator.next = page + 1
	return page * PageSize, true
}

func (allocator *Allocator) AllocateContiguous(count uint64) (uint64, bool) {
	if count == 0 || count > allocator.freePages || count > allocator.managedPage {
		return 0, false
	}
	for start := lowMemoryPages; start+count <= allocator.managedPage; {
		first, found := allocator.firstFree(start, allocator.managedPage)
		if !found || first+count > allocator.managedPage {
			return 0, false
		}
		end := first
		for end < first+count && !allocator.isUsed(end) {
			end++
		}
		if end < first+count {
			start = end + 1
			continue
		}
		for page := first; page < end; page++ {
			allocator.markUsed(page)
		}
		allocator.next = end
		return first * PageSize, true
	}
	return 0, false
}

// firstFree returns the lowest free page in [start, end), skipping bitmap
// words with no free page instead of testing each bit.
func (allocator *Allocator) firstFree(start, end uint64) (uint64, bool) {
	for page := start; page < end; {
		free := ^allocator.used[page/bitmapWordBits] >> (page % bitmapWordBits)
		if free == 0 {
			page = (page/bitmapWordBits + 1) * bitmapWordBits
			continue
		}
		page += uint64(bits.TrailingZeros64(free))
		return page, page < end
	}
	return 0, false
}

func (allocator *Allocator) Free(address uint64) bool {
	if address%PageSize != 0 {
		return false
	}
	page := address / PageSize
	if page < lowMemoryPages || page >= allocator.managedPage || !allocator.isUsed(page) {
		return false
	}
	allocator.markFree(page)
	if page < allocator.next {
		allocator.next = page
	}
	return true
}

func (allocator *Allocator) Statistics() Stats {
	return Stats{
		ManagedPages: allocator.managedPage,
		FreePages:    allocator.freePages,
		UsedPages:    allocator.managedPage - allocator.freePages,
	}
}

// IsAllocated reports whether the page containing address is marked in use.
func (allocator *Allocator) IsAllocated(address uint64) bool {
	page := address / PageSize
	return page < managedPages && allocator.isUsed(page)
}

func (allocator *Allocator) isUsed(page uint64) bool {
	return allocator.used[page/bitmapWordBits]&(uint64(1)<<(page%bitmapWordBits)) != 0
}

func (allocator *Allocator) markFree(page uint64) {
	word := &allocator.used[page/bitmapWordBits]
	mask := uint64(1) << (page % bitmapWordBits)
	if *word&mask != 0 {
		*word &^= mask
		allocator.freePages++
	}
}

func (allocator *Allocator) markUsed(page uint64) {
	word := &allocator.used[page/bitmapWordBits]
	mask := uint64(1) << (page % bitmapWordBits)
	if *word&mask == 0 {
		*word |= mask
		allocator.freePages--
	}
}
