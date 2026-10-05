package pmm

import (
	"goos/kernel/boot"
	"goos/kernel/lock"
	"math/bits"
)

const (
	PageSize = boot.PageSize
	// LowLimit bounds the low allocation class. The kernel identity-maps the
	// physical memory below it, so a low page's physical address is also a
	// kernel pointer to it (ADR 0103).
	LowLimit = uint64(1 << 30)
	// MaxPhysical is the exclusive upper bound of managed physical memory:
	// 512 GiB, the span of the one PML4 slot that holds the kernel's direct
	// map. Usable memory above it is left unmanaged and counted as such.
	MaxPhysical = uint64(512 << 30)
	// MaxSegments bounds the discontiguous physical ranges the allocator
	// manages. Usable memory in further ranges is left unmanaged.
	MaxSegments = 64

	firstManagedPage = uint64(256)
	lowPages         = LowLimit / PageSize
	maxPages         = MaxPhysical / PageSize
	bitmapWordBits   = uint64(64)
	wordsPerPage     = PageSize / 8
)

type Stats struct {
	// ManagedPages counts the usable pages the allocator owns, including the
	// pages that hold its own bitmap.
	ManagedPages uint64
	FreePages    uint64
	UsedPages    uint64
	// HighPages counts the managed pages at or above LowLimit.
	HighPages uint64
	// UnmanagedPages counts usable pages above the first MiB that the
	// allocator does not own: above MaxPhysical, in ranges beyond
	// MaxSegments, or in ranges dropped because their bitmap did not fit.
	UnmanagedPages uint64
}

// MetadataView returns a kernel-addressable view of words 64-bit words of
// physical memory starting at physical, or nil when it cannot. Init stores the
// allocator's bitmap through it in pages it carves out of low usable memory.
type MetadataView func(physical uint64, words uint64) []uint64

// segment is one contiguous run of usable pages, [start, end) in page
// numbers, with one bit per page in used. Every page in it is usable RAM, so a
// set bit always means allocated and a clear bit always means free.
type segment struct {
	start uint64
	end   uint64
	used  []uint64
	free  uint64
	next  uint64
}

// Allocator tracks usable physical memory below MaxPhysical as a bounded,
// sorted array of segments, each with its own bitmap (ADR 0103). Segments never
// straddle LowLimit: the first lowCount lie below it and the rest above it.
// The bitmaps live in pages Init carves out of low memory and keeps marked
// used, so the allocator needs no memory of its own beyond this structure.
//
// Every processor allocates and frees, so each operation after Init holds
// the allocator's lock. The caller has disabled local interrupts.
type Allocator struct {
	lock           lock.Spin
	segments       [MaxSegments]segment
	count          int
	lowCount       int
	managedPages   uint64
	highPages      uint64
	freePages      uint64
	unmanagedPages uint64
	metadataStart  uint64
	metadataEnd    uint64
}

// Init builds the segments from the usable regions of the memory map and
// carves their bitmap out of the highest low segment that can hold it. Only
// usable memory at or above the first MiB is managed; every other kind of
// region, and any hole between regions, stays outside every segment. Init
// reports false when no usable memory can be managed.
func (allocator *Allocator) Init(regions []boot.MemoryRegion, view MetadataView) bool {
	*allocator = Allocator{}
	allocator.lock.SetLevel(lock.LevelMemory)
	allocator.planSegments(regions)
	words, metadata := uint64(0), -1
	for allocator.count != 0 {
		words = allocator.metadataWords()
		metadata = allocator.metadataSegment((words + wordsPerPage - 1) / wordsPerPage)
		if metadata >= 0 {
			break
		}
		allocator.dropLastSegment()
	}
	if allocator.count == 0 || view == nil {
		*allocator = Allocator{}
		return false
	}
	metadataPages := (words + wordsPerPage - 1) / wordsPerPage
	allocator.metadataEnd = allocator.segments[metadata].end
	allocator.metadataStart = allocator.metadataEnd - metadataPages
	storage := view(allocator.metadataStart*PageSize, words)
	if uint64(len(storage)) != words {
		*allocator = Allocator{}
		return false
	}
	offset := uint64(0)
	for index := 0; index < allocator.count; index++ {
		current := &allocator.segments[index]
		pages := current.end - current.start
		length := (pages + bitmapWordBits - 1) / bitmapWordBits
		current.used = storage[offset : offset+length : offset+length]
		offset += length
		for word := range current.used {
			current.used[word] = 0
		}
		// Bits past the segment's last page are marked used, so no scan can
		// mistake them for free pages.
		if tail := pages % bitmapWordBits; tail != 0 {
			current.used[length-1] = ^uint64(0) << tail
		}
		current.free = pages
		current.next = current.start
		allocator.managedPages += pages
		allocator.freePages += pages
		if current.start >= lowPages {
			allocator.highPages += pages
		}
	}
	owner := &allocator.segments[metadata]
	for page := allocator.metadataStart; page < allocator.metadataEnd; page++ {
		owner.markUsed(page)
		allocator.freePages--
	}
	return true
}

// planSegments walks the usable regions in address order, whatever order the
// firmware listed them in, and records them as segments. Adjacent or
// overlapping usable regions become one segment unless LowLimit separates
// them.
func (allocator *Allocator) planSegments(regions []boot.MemoryRegion) {
	cursor := firstManagedPage
	for {
		start, end, found := nextUsable(regions, cursor)
		if !found {
			return
		}
		if start < lowPages && end > lowPages {
			end = lowPages
		}
		cursor = end
		if end > maxPages {
			clipped := maxPages
			if start > clipped {
				clipped = start
			}
			allocator.unmanagedPages += end - clipped
			end = clipped
		}
		if start < end {
			allocator.addRange(start, end)
		}
	}
}

// nextUsable returns the lowest run of usable pages at or above cursor.
func nextUsable(regions []boot.MemoryRegion, cursor uint64) (uint64, uint64, bool) {
	start, end, found := uint64(0), uint64(0), false
	for _, region := range regions {
		first := region.Start / PageSize
		last := first + region.Pages
		if region.Kind != boot.MemoryUsable || region.Start%PageSize != 0 || last < first || last <= cursor {
			continue
		}
		if first < cursor {
			first = cursor
		}
		if !found || first < start {
			start, end, found = first, last, true
		}
	}
	return start, end, found
}

func (allocator *Allocator) addRange(start, end uint64) {
	if allocator.count != 0 {
		last := &allocator.segments[allocator.count-1]
		if last.end == start && start != lowPages {
			last.end = end
			return
		}
	}
	if allocator.count == MaxSegments {
		allocator.unmanagedPages += end - start
		return
	}
	allocator.segments[allocator.count] = segment{start: start, end: end}
	allocator.count++
	if start < lowPages {
		allocator.lowCount++
	}
}

func (allocator *Allocator) metadataWords() uint64 {
	words := uint64(0)
	for index := 0; index < allocator.count; index++ {
		current := allocator.segments[index]
		words += (current.end - current.start + bitmapWordBits - 1) / bitmapWordBits
	}
	return words
}

// metadataSegment returns the highest low segment with room for pages bitmap
// pages at its top, or -1.
func (allocator *Allocator) metadataSegment(pages uint64) int {
	for index := allocator.lowCount - 1; index >= 0; index-- {
		current := allocator.segments[index]
		if current.end-current.start >= pages {
			return index
		}
	}
	return -1
}

func (allocator *Allocator) dropLastSegment() {
	allocator.count--
	dropped := allocator.segments[allocator.count]
	allocator.unmanagedPages += dropped.end - dropped.start
	if dropped.start < lowPages {
		allocator.lowCount--
	}
	allocator.segments[allocator.count] = segment{}
}

// Allocate returns a free page from any segment. It takes from the highest
// segment that has one, so memory below LowLimit, which is also a kernel
// pointer, and memory below 4 GiB, which 32-bit devices can reach, are used
// last. The page may lie above LowLimit: the kernel reaches it only through
// the direct map, and it must never be used as a pointer, a page table, or a
// kernel stack. Those use AllocateLow or AllocateContiguous.
func (allocator *Allocator) Allocate() (uint64, bool) {
	allocator.lock.Lock()
	defer allocator.lock.Unlock()
	for index := allocator.count - 1; index >= allocator.lowCount; index-- {
		if page, ok := allocator.segments[index].take(); ok {
			allocator.freePages--
			return page * PageSize, true
		}
	}
	return allocator.allocateLow()
}

// AllocateLow returns a free page below LowLimit, from the lowest segment
// that has one.
func (allocator *Allocator) AllocateLow() (uint64, bool) {
	allocator.lock.Lock()
	defer allocator.lock.Unlock()
	return allocator.allocateLow()
}

func (allocator *Allocator) allocateLow() (uint64, bool) {
	for index := 0; index < allocator.lowCount; index++ {
		if page, ok := allocator.segments[index].take(); ok {
			allocator.freePages--
			return page * PageSize, true
		}
	}
	return 0, false
}

// AllocateContiguous returns count physically contiguous pages below
// LowLimit. A run never crosses a segment boundary, so it never spans a
// reserved hole.
func (allocator *Allocator) AllocateContiguous(count uint64) (uint64, bool) {
	if count == 0 {
		return 0, false
	}
	allocator.lock.Lock()
	defer allocator.lock.Unlock()
	for index := 0; index < allocator.lowCount; index++ {
		if page, ok := allocator.segments[index].takeRun(count); ok {
			allocator.freePages -= count
			return page * PageSize, true
		}
	}
	return 0, false
}

// Free returns an allocated page. Unaligned addresses, pages outside every
// segment, the allocator's own bitmap pages, and pages that are already free
// are rejected.
func (allocator *Allocator) Free(address uint64) bool {
	if address%PageSize != 0 {
		return false
	}
	page := address / PageSize
	if page >= allocator.metadataStart && page < allocator.metadataEnd {
		return false
	}
	allocator.lock.Lock()
	defer allocator.lock.Unlock()
	current := allocator.segmentOf(page)
	if current == nil || !current.isUsed(page) {
		return false
	}
	current.markFree(page)
	allocator.freePages++
	if page < current.next {
		current.next = page
	}
	return true
}

func (allocator *Allocator) Statistics() Stats {
	allocator.lock.Lock()
	defer allocator.lock.Unlock()
	return Stats{
		ManagedPages:   allocator.managedPages,
		FreePages:      allocator.freePages,
		UsedPages:      allocator.managedPages - allocator.freePages,
		HighPages:      allocator.highPages,
		UnmanagedPages: allocator.unmanagedPages,
	}
}

// IsAllocated reports whether the page containing address is managed and in
// use. Memory the allocator does not manage is never reported as allocated.
func (allocator *Allocator) IsAllocated(address uint64) bool {
	allocator.lock.Lock()
	defer allocator.lock.Unlock()
	page := address / PageSize
	current := allocator.segmentOf(page)
	return current != nil && current.isUsed(page)
}

// Segment returns the physical address range [start, end) of the index-th
// managed segment, in ascending address order.
func (allocator *Allocator) Segment(index int) (uint64, uint64, bool) {
	if index < 0 || index >= allocator.count {
		return 0, 0, false
	}
	current := allocator.segments[index]
	return current.start * PageSize, current.end * PageSize, true
}

// segmentOf finds the segment containing page by binary search over the
// sorted, non-overlapping segments.
func (allocator *Allocator) segmentOf(page uint64) *segment {
	low, high := 0, allocator.count
	for low < high {
		middle := (low + high) / 2
		current := &allocator.segments[middle]
		switch {
		case page < current.start:
			high = middle
		case page >= current.end:
			low = middle + 1
		default:
			return current
		}
	}
	return nil
}

// take allocates one page, searching from after the last allocation and then
// from the segment start, so that a freed page is found again.
func (current *segment) take() (uint64, bool) {
	if current.free == 0 {
		return 0, false
	}
	page, found := current.firstFree(current.next, current.end)
	if !found {
		page, found = current.firstFree(current.start, current.next)
	}
	if !found {
		return 0, false
	}
	current.markUsed(page)
	current.next = page + 1
	return page, true
}

// takeRun allocates the lowest run of count free pages in the segment.
func (current *segment) takeRun(count uint64) (uint64, bool) {
	if count > current.free {
		return 0, false
	}
	for start := current.start; start+count <= current.end; {
		first, found := current.firstFree(start, current.end)
		if !found || first+count > current.end {
			return 0, false
		}
		end := first
		for end < first+count && !current.isUsed(end) {
			end++
		}
		if end < first+count {
			start = end + 1
			continue
		}
		for page := first; page < end; page++ {
			current.markUsed(page)
		}
		current.next = end
		return first, true
	}
	return 0, false
}

// firstFree returns the lowest free page in [from, to), skipping bitmap words
// with no free page instead of testing each bit (ADR 0057).
func (current *segment) firstFree(from, to uint64) (uint64, bool) {
	for page := from; page < to; {
		offset := page - current.start
		free := ^current.used[offset/bitmapWordBits] >> (offset % bitmapWordBits)
		if free == 0 {
			page += bitmapWordBits - offset%bitmapWordBits
			continue
		}
		page += uint64(bits.TrailingZeros64(free))
		return page, page < to
	}
	return 0, false
}

func (current *segment) isUsed(page uint64) bool {
	offset := page - current.start
	return current.used[offset/bitmapWordBits]&(uint64(1)<<(offset%bitmapWordBits)) != 0
}

func (current *segment) markFree(page uint64) {
	offset := page - current.start
	current.used[offset/bitmapWordBits] &^= uint64(1) << (offset % bitmapWordBits)
	current.free++
}

func (current *segment) markUsed(page uint64) {
	offset := page - current.start
	current.used[offset/bitmapWordBits] |= uint64(1) << (offset % bitmapWordBits)
	current.free--
}
