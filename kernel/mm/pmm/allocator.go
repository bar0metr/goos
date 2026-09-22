package pmm

import "goos/kernel/boot"

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
	for checked := uint64(0); checked < allocator.managedPage; checked++ {
		page := (allocator.next + checked) % allocator.managedPage
		if !allocator.isUsed(page) {
			allocator.markUsed(page)
			allocator.next = page + 1
			return page * PageSize, true
		}
	}
	return 0, false
}

func (allocator *Allocator) AllocateContiguous(count uint64) (uint64, bool) {
	if count == 0 || count > allocator.freePages || count > allocator.managedPage {
		return 0, false
	}
	for start := lowMemoryPages; start+count <= allocator.managedPage; start++ {
		available := true
		for page := start; page < start+count; page++ {
			if allocator.isUsed(page) {
				available = false
				start = page
				break
			}
		}
		if !available {
			continue
		}
		for page := start; page < start+count; page++ {
			allocator.markUsed(page)
		}
		allocator.next = start + count
		return start * PageSize, true
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
