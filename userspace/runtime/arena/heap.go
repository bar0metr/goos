package arena

import (
	"math/bits"
	"unsafe"
)

// The heap is a page-granular reservation split three ways. Allocations of up
// to maxSmallSize bytes share slab pages of one size class; larger ones take a
// run of whole pages. Pages no allocation holds are free runs, coalesced with
// their free neighbours.
//
// Everything the allocator knows about a page is in its pageMeta, kept in a
// separate reservation: the payload pages hold only allocation bytes. A free
// page can therefore be discarded, and a conservative scan can resolve any
// address to its allocation without trusting the bytes stored next to it.
//
// The allocator never allocates. Its only memory is the two reservations it is
// given, so it serves runtime.alloc itself and runs under the collector.

const (
	pageShift    = 12
	pageBytes    = uintptr(1) << pageShift
	maxSmallSize = 2048
	wordBytes    = unsafe.Sizeof(uintptr(0))
	noPage       = ^uint32(0)
	// Free runs are kept in lists by size: list b holds runs of [2^b, 2^(b+1))
	// pages, the last one everything larger.
	runBuckets = 17
	// slotWords is the bitmap width of a slab: 4096/16 slots.
	slotWords = 4
)

// classSizes are the slot sizes of the slab classes, multiples of 16 so every
// slot is 16-byte aligned. 1360 is the largest size three slots of which fit
// a page; without it everything from 1025 to 2048 bytes took half a page, and
// the VFS service makes most of its requests in that range.
var classSizes = [...]uintptr{16, 32, 48, 64, 96, 128, 192, 256, 384, 512, 768, 1024, 1360, 2048}

const classCount = len(classSizes)

type pageKind uint8

const (
	pageFree pageKind = iota
	pageSlab
	pageRun
	pageRunTail
)

const (
	// pageDirty marks a page that may hold non-zero bytes: it has held an
	// allocation since the kernel last supplied it zeroed.
	pageDirty uint8 = 1 << iota
	// pageListed marks a slab on its class's list of slabs with free slots.
	pageListed
)

// pageMeta describes one heap page.
//
// A free run records its length in its first page and its first page in its
// last; a run allocation records its length in its first page and its first
// page in every other. Interior pages of a free run carry only their kind and
// flags. A slab keeps in count the first slot from which no slot has been
// handed out since the page last read as zeroes: those need no clearing. It
// keeps in head the first bitmap word that may hold a free slot; every slot
// in the words before it is in use.
type pageMeta struct {
	kind   pageKind
	class  uint8
	flags  uint8
	marked bool
	used   uint16
	// pointerFree marks a run allocation that holds no pointers: marking
	// keeps it alive without reading it.
	pointerFree bool
	_           uint8
	head        uint32
	count       uint32
	// next and prev link a slab into its class list, or a free run into its
	// size list.
	next, prev uint32
	// owner is the slot plus one of the thread that owns the slab, or zero
	// (cache.go). Threads read it without the heap lock, with atomics.
	owner uint32
	alloc [slotWords]uint64
	// mark holds the collector's marks during a collection. Between
	// collections it is clear, except in a slab a thread owns, where it
	// holds the slots other threads freed (cache.go).
	mark [slotWords]uint64
	// noscan marks the slots that hold no pointers.
	noscan [slotWords]uint64
}

// heap is the allocator and collector state of one reservation.
type heap struct {
	base     uintptr
	capacity uint32
	// frontier is the first page never handed out. Pages from it on are as
	// the kernel supplied them, and their metadata was never touched.
	frontier uint32
	meta     []pageMeta
	partial  [classCount]uint32
	runs     [runBuckets]uint32

	// classOf maps a small size, rounded up to 16 bytes and divided by 16,
	// to its class, and classSlots is each class's slots per slab. They
	// replace a search of classSizes and a division on every allocation.
	classOf    [sizeBuckets + 1]uint8
	classSlots [classCount]uint16

	// work is the mark stack. When it fills, marked objects stop being
	// queued, overflow is set, and finishMark rescans the heap.
	work     []uintptr
	workTop  int
	workHigh int
	overflow bool

	// discard returns a page-aligned range to the kernel, which supplies it
	// zeroed when it is next touched. It reports whether that happened.
	discard func(address, length uintptr) bool
	// retain is how many dirty free pages a collection or an explicit free
	// leaves undiscarded, so the allocations after it reuse them rather than
	// have the kernel supply them again. trim gives every one back.
	// retainSet records that SetRetained chose it; otherwise a collection
	// that several threads' allocations started picks its own (retention).
	retain    uint32
	retainSet bool

	// pageLimit is the process's page quota in bytes, zero when unknown.
	// It caps how far the collection floor grows with allocating threads.
	pageLimit uint64
	// allocators is how many threads allocated since the last collection,
	// at least one. The collector sets it before it sweeps; the sweep sets
	// the next goal and the retention from it.
	allocators uint32
	// cachedPages is how many slabs the threads' caches own now.
	cachedPages uint64

	liveBytes, liveObjects uint64
	totalAlloc, mallocs    uint64
	frees, released        uint64
	inUsePages, dirtyFree  uint64
	peakHeld               uint64
	peakLive               uint64
	collections            uint32
	nextCollection         uint64
	// collectedMallocs is mallocs when the last collection ended.
	collectedMallocs uint64

	// watch is an allocation whose first mark is recorded: the word that
	// marked it in watchAt and the root phase in watchPhase. scanning and
	// phase track where marking is. Validation programs use them to explain
	// why an object outlived a collection; the collector never scans the
	// heap state, so watching keeps nothing alive.
	watch, watchAt, scanning uintptr
	watchPhase, phase        uint8

	diag counters
	// stray counts the remote frees foldStrayFrees folds.
	stray threadCounts
}

// Stats is the state of a heap. Bytes count whole slots and runs, not the
// sizes asked for.
type Stats struct {
	// Reserved is the address space of the heap.
	Reserved uint64
	// Live is the bytes of allocations not freed or swept. Between
	// collections it includes allocations no longer reachable.
	Live uint64
	// Objects is the number of those allocations, and PeakLive the most
	// Live has been.
	Objects, PeakLive uint64
	// TotalAlloc, Mallocs, and Frees are cumulative: bytes and allocations
	// handed out, and allocations freed explicitly or by a sweep.
	TotalAlloc, Mallocs, Frees uint64
	// InUse is the bytes of pages holding at least one allocation.
	InUse uint64
	// Idle is the bytes of the reservation not in use.
	Idle uint64
	// Held is InUse plus free pages not yet given back to the kernel, and
	// PeakHeld the most it has been. It is an upper bound on the heap's
	// resident pages, not a count of them: a page handed to an allocation may
	// never have been touched, and the kernel supplies it only when it is.
	// What a process really costs is the page count the kernel charges it
	// (ProcessInfo).
	Held, PeakHeld uint64
	// Released is the cumulative bytes given back with a discard.
	Released uint64
	// Metadata is the bytes of page metadata in use, which the heap keeps
	// for every page up to the highest it has handed out.
	Metadata uint64
	// Collections is the number of completed collections, and
	// NextCollection the live bytes at which the next one starts.
	Collections    uint32
	NextCollection uint64
}

// collectionFloor is the growth below which no collection starts: a heap
// holding little collects after another MiB of allocation, not after every
// few objects. Each thread that allocated since the last collection adds a
// MiB, up to a quotaShare-th of the process's page quota (goal). With an
// eighth, eight threads collected every 8 MiB and each pause swept that
// much: over 1.5 ms on eight processors with validation poisoning
// (ADR 0205).
const (
	collectionFloor = uint64(1 << 20)
	quotaShare      = 16
)

// collectionReason is why a collection runs. It decides what the
// collection gives back, never what it marks.
type collectionReason uint8

const (
	// collectAutomatic: an allocation crossed the goal. It keeps the
	// threads' slabs and may keep free pages for the allocations after it.
	collectAutomatic collectionReason = iota
	// collectExplicit: runtime.GC or Collect.
	collectExplicit
	// collectIdle: CollectIdle, once every thread waits.
	collectIdle
	// collectPressure: an allocation found no room.
	collectPressure
	collectReasons
)

// metaBytes is how many bytes of metadata a heap of pages pages needs.
func metaBytes(pages uint32) uintptr { return uintptr(pages) * unsafe.Sizeof(pageMeta{}) }

// init sets up a heap over pages pages at base, with metadata and a mark
// stack the caller reserved outside it.
func (h *heap) init(base uintptr, pages uint32, meta []pageMeta, work []uintptr, discard func(address, length uintptr) bool) {
	if base == 0 || base&(pageBytes-1) != 0 || pages == 0 || uint32(len(meta)) < pages || len(work) == 0 {
		panic("arena: invalid heap reservation")
	}
	if base+uintptr(pages)<<pageShift < base {
		panic("arena: heap reservation wraps")
	}
	// Fields are set one by one: TinyGo puts a composite literal this large
	// on the heap, and the native heap does not exist yet.
	h.base, h.capacity, h.frontier, h.meta = base, pages, 0, meta[:pages]
	h.work, h.workTop, h.workHigh, h.overflow, h.discard = work, 0, 0, false, discard
	h.retain, h.retainSet, h.pageLimit, h.allocators, h.cachedPages = defaultRetained, false, 0, 1, 0
	h.liveBytes, h.liveObjects, h.totalAlloc, h.mallocs = 0, 0, 0, 0
	h.frees, h.released, h.inUsePages, h.dirtyFree = 0, 0, 0, 0
	h.peakHeld, h.peakLive, h.collections, h.collectedMallocs = 0, 0, 0, 0
	// The counters are numbers only, cleared in place for the same reason.
	clearMemory(uintptr(unsafe.Pointer(&h.diag)), unsafe.Sizeof(h.diag))
	clearMemory(uintptr(unsafe.Pointer(&h.stray)), unsafe.Sizeof(h.stray))
	for index := range h.partial {
		h.partial[index] = noPage
	}
	class := 0
	for steps := range h.classOf {
		for uintptr(steps)<<4 > classSizes[class] {
			class++
		}
		h.classOf[steps] = uint8(class)
	}
	for class := range h.classSlots {
		h.classSlots[class] = uint16(slotsPerPage(uint8(class)))
	}
	for index := range h.runs {
		h.runs[index] = noPage
	}
	h.nextCollection = collectionFloor
}

func (h *heap) pageAddress(page uint32) uintptr { return h.base + uintptr(page)<<pageShift }

// sizeClass returns the class of a small allocation.
func sizeClass(size uintptr) int {
	for class, slot := range classSizes {
		if size <= slot {
			return class
		}
	}
	panic("arena: size has no class")
}

func slotsPerPage(class uint8) uint32 { return uint32(pageBytes / classSizes[class]) }

// alloc returns size zeroed bytes that the collector scans, or zero when the
// heap has no room. size must not be zero.
func (h *heap) alloc(size uintptr) uintptr { return h.allocObject(size, false) }

// allocObject is alloc for an object that may be pointer-free: the collector
// keeps such an object alive without reading its words.
func (h *heap) allocObject(size uintptr, pointerFree bool) uintptr {
	if size == 0 {
		panic("arena: zero-size heap allocation")
	}
	if size <= maxSmallSize {
		if sizeProfile {
			bucket := (size - 1) >> 4
			h.diag.sizeCounts[bucket]++
			h.diag.sizeBytes[bucket] += uint64(size)
		}
		return h.allocSmall(h.classOf[(size+15)>>4], pointerFree)
	}
	if size > uintptr(h.capacity)<<pageShift {
		return 0
	}
	count := uint32((size + pageBytes - 1) >> pageShift)
	h.diag.largeAllocs++
	h.diag.largeRequested += uint64(size)
	h.diag.largePages += uint64(count)
	return h.allocLarge(count, pointerFree)
}

func (h *heap) allocSmall(class uint8, pointerFree bool) uintptr {
	page := h.partial[class]
	if page == noPage {
		page = h.newSlab(class)
		if page == noPage {
			return 0
		}
		h.pushPartial(page)
	}
	// The search is takeSlot's, written out: calling takeSlot here cost
	// 5% of a small allocation in the host benchmarks.
	m := &h.meta[page]
	slots := uint32(h.classSlots[class])
	slot := noPage
	for word := m.head; word < slotWords && word*64 < slots; word++ {
		if free := ^m.alloc[word]; free != 0 {
			candidate := word*64 + uint32(bits.TrailingZeros64(free))
			if candidate < slots {
				slot = candidate
			}
			if word != m.head {
				m.head = word
			}
			break
		}
	}
	if slot == noPage {
		panic("arena: listed slab has no free slot")
	}
	m.alloc[slot/64] |= 1 << (slot % 64)
	if pointerFree {
		m.noscan[slot/64] |= 1 << (slot % 64)
	} else {
		m.noscan[slot/64] &^= 1 << (slot % 64)
	}
	m.used++
	if uint32(m.used) == slots {
		h.removePartial(page)
		h.diag.partialToFull++
	}
	size := classSizes[class]
	address := h.pageAddress(page) + uintptr(slot)*size
	if slot < m.count {
		clearMemory(address, size)
		h.diag.clearedBytes += uint64(size)
	} else {
		m.count = slot + 1
	}
	m.flags |= pageDirty
	h.counted(size)
	return address
}

// newSlab sets up a page as an empty slab of class, on no list, or returns
// noPage when the heap has no room.
func (h *heap) newSlab(class uint8) uint32 {
	page := h.takePages(1)
	if page == noPage {
		return noPage
	}
	m := &h.meta[page]
	m.kind, m.class, m.used, m.marked = pageSlab, class, 0, false
	m.count, m.head = 0, 0
	if m.flags&pageDirty != 0 {
		m.count = uint32(h.classSlots[class])
	}
	m.alloc, m.mark = [slotWords]uint64{}, [slotWords]uint64{}
	h.diag.newSlabs++
	h.diag.noteSlab(page)
	return page
}

// takeSlot hands out the lowest free slot of the slab at page, which must
// have one, zeroed. It returns the slot's address and the bytes it cleared.
func (h *heap) takeSlot(page uint32, pointerFree bool) (uintptr, uintptr) {
	m := &h.meta[page]
	slots := uint32(h.classSlots[m.class])
	slot := noPage
	for word := m.head; word < slotWords && word*64 < slots; word++ {
		if free := ^m.alloc[word]; free != 0 {
			candidate := word*64 + uint32(bits.TrailingZeros64(free))
			if candidate < slots {
				slot = candidate
			}
			if word != m.head {
				m.head = word
			}
			break
		}
	}
	if slot == noPage {
		panic("arena: listed slab has no free slot")
	}
	m.alloc[slot/64] |= 1 << (slot % 64)
	if pointerFree {
		m.noscan[slot/64] |= 1 << (slot % 64)
	} else {
		m.noscan[slot/64] &^= 1 << (slot % 64)
	}
	m.used++
	size := classSizes[m.class]
	address := h.pageAddress(page) + uintptr(slot)*size
	cleared := uintptr(0)
	if slot < m.count {
		clearMemory(address, size)
		cleared = size
	} else {
		// Slots are taken lowest first, so every slot below this one is
		// in use, and every slot above it is still untouched.
		m.count = slot + 1
	}
	m.flags |= pageDirty
	return address, cleared
}

func (h *heap) allocLarge(count uint32, pointerFree bool) uintptr {
	page := h.takePages(count)
	if page == noPage {
		return 0
	}
	head := &h.meta[page]
	head.kind, head.count, head.marked, head.head = pageRun, count, false, page
	head.pointerFree = pointerFree
	for index := page; index < page+count; index++ {
		m := &h.meta[index]
		if index != page {
			m.kind, m.head, m.count = pageRunTail, page, 0
		}
		if m.flags&pageDirty != 0 {
			clearMemory(h.pageAddress(index), pageBytes)
			h.diag.clearedBytes += uint64(pageBytes)
		}
		m.flags |= pageDirty
	}
	h.counted(uintptr(count) << pageShift)
	return h.pageAddress(page)
}

func (h *heap) counted(size uintptr) {
	h.liveBytes += uint64(size)
	if h.liveBytes > h.peakLive {
		h.peakLive = h.liveBytes
	}
	h.liveObjects++
	h.totalAlloc += uint64(size)
	h.mallocs++
}

// takePages takes count contiguous pages from the free runs, or from the
// frontier when no run is long enough. It returns noPage when neither has
// room.
func (h *heap) takePages(count uint32) uint32 {
	page := noPage
	h.diag.runSearches++
	for bucket := runBucket(count); bucket < runBuckets && page == noPage; bucket++ {
		for candidate := h.runs[bucket]; candidate != noPage; candidate = h.meta[candidate].next {
			h.diag.runCandidates++
			if h.meta[candidate].count >= count {
				page = candidate
				break
			}
		}
	}
	if page != noPage {
		length := h.meta[page].count
		h.unlinkRun(page)
		if length > count {
			h.insertRun(page+count, length-count)
		}
		for index := page; index < page+count; index++ {
			if h.meta[index].flags&pageDirty != 0 {
				h.dirtyFree--
			} else {
				// Below the frontier a clean free page was discarded.
				h.diag.pagesRecommitted++
			}
		}
		h.diag.pagesFromRuns += uint64(count)
	} else {
		if uint64(h.frontier)+uint64(count) > uint64(h.capacity) {
			return noPage
		}
		page = h.frontier
		h.frontier += count
		for index := page; index < page+count; index++ {
			h.meta[index] = pageMeta{next: noPage, prev: noPage}
		}
		h.diag.pagesFromFresh += uint64(count)
	}
	h.inUsePages += uint64(count)
	if held := (h.inUsePages + h.dirtyFree) << pageShift; held > h.peakHeld {
		h.peakHeld = held
	}
	return page
}

// givePages returns pages a slab or run held to the free runs, merged with
// the free runs on either side, and returns the page after the merged run.
func (h *heap) givePages(page, count uint32) uint32 {
	for index := page; index < page+count; index++ {
		m := &h.meta[index]
		m.kind, m.marked, m.used, m.flags = pageFree, false, 0, m.flags&^pageListed
		if m.flags&pageDirty != 0 {
			h.dirtyFree++
		}
	}
	h.inUsePages -= uint64(count)
	if page > 0 && h.meta[page-1].kind == pageFree {
		left := h.meta[page-1].head
		if left >= page || h.meta[left].kind != pageFree || left+h.meta[left].count != page {
			panic("arena: corrupt free run before page")
		}
		count += h.meta[left].count
		h.unlinkRun(left)
		page = left
	}
	if right := page + count; right < h.frontier && h.meta[right].kind == pageFree {
		count += h.meta[right].count
		h.unlinkRun(right)
	}
	h.insertRun(page, count)
	return page + count
}

func runBucket(count uint32) int {
	bucket := bits.Len32(count) - 1
	if bucket >= runBuckets {
		bucket = runBuckets - 1
	}
	return bucket
}

func (h *heap) insertRun(page, count uint32) {
	if count == 0 || uint64(page)+uint64(count) > uint64(h.frontier) {
		panic("arena: free run outside heap")
	}
	first, last := &h.meta[page], &h.meta[page+count-1]
	first.kind, first.count, first.head = pageFree, count, page
	last.kind, last.head = pageFree, page
	bucket := runBucket(count)
	first.prev, first.next = noPage, h.runs[bucket]
	if first.next != noPage {
		h.meta[first.next].prev = page
	}
	h.runs[bucket] = page
}

func (h *heap) unlinkRun(page uint32) {
	m := &h.meta[page]
	if m.kind != pageFree || m.count == 0 || m.head != page {
		panic("arena: corrupt free run")
	}
	if m.prev != noPage {
		h.meta[m.prev].next = m.next
	} else {
		bucket := runBucket(m.count)
		if h.runs[bucket] != page {
			panic("arena: free run missing from its list")
		}
		h.runs[bucket] = m.next
	}
	if m.next != noPage {
		h.meta[m.next].prev = m.prev
	}
	m.next, m.prev = noPage, noPage
}

func (h *heap) pushPartial(page uint32) {
	m := &h.meta[page]
	if m.flags&pageListed != 0 {
		panic("arena: slab listed twice")
	}
	m.flags |= pageListed
	m.prev, m.next = noPage, h.partial[m.class]
	if m.next != noPage {
		h.meta[m.next].prev = page
	}
	h.partial[m.class] = page
}

func (h *heap) removePartial(page uint32) {
	m := &h.meta[page]
	if m.flags&pageListed == 0 {
		panic("arena: slab not listed")
	}
	m.flags &^= pageListed
	if m.prev != noPage {
		h.meta[m.prev].next = m.next
	} else {
		if h.partial[m.class] != page {
			panic("arena: slab missing from its list")
		}
		h.partial[m.class] = m.next
	}
	if m.next != noPage {
		h.meta[m.next].prev = m.prev
	}
	m.next, m.prev = noPage, noPage
}

// contains reports whether address lies in the part of the heap handed out.
func (h *heap) contains(address uintptr) bool {
	return address >= h.base && address-h.base < uintptr(h.frontier)<<pageShift
}

// find resolves an address to the allocation containing it. It returns the
// allocation's first page, its slot (noPage for a run), and its start, or
// ok false when no live allocation contains the address.
func (h *heap) find(address uintptr) (page, slot uint32, start uintptr, ok bool) {
	if !h.contains(address) {
		return 0, 0, 0, false
	}
	page = uint32((address - h.base) >> pageShift)
	m := &h.meta[page]
	switch m.kind {
	case pageSlab:
		// The offset fits 32 bits, and a 32-bit division is the cheaper
		// one on amd64. A slot that ends past the page is the page's
		// unused tail.
		size := uint32(classSizes[m.class])
		slot = uint32(address&(pageBytes-1)) / size
		offset := slot * size
		if offset+size > uint32(pageBytes) || m.alloc[slot/64]&(1<<(slot%64)) == 0 {
			return 0, 0, 0, false
		}
		return page, slot, h.pageAddress(page) + uintptr(offset), true
	case pageRunTail:
		head := m.head
		if head >= page || h.meta[head].kind != pageRun || head+h.meta[head].count <= page {
			panic("arena: corrupt run metadata")
		}
		page = head
		fallthrough
	case pageRun:
		return page, noPage, h.pageAddress(page), true
	}
	return 0, 0, 0, false
}

// extent returns the size of the allocation found at page and slot.
func (h *heap) extent(page, slot uint32) uintptr {
	m := &h.meta[page]
	if slot == noPage {
		return uintptr(m.count) << pageShift
	}
	return classSizes[m.class]
}

// free returns the allocation starting at address. Anything other than the
// start of a live allocation is corruption.
func (h *heap) free(address uintptr) {
	page, slot, start, ok := h.find(address)
	if !ok || start != address {
		if h.contains(address) && h.meta[(address-h.base)>>pageShift].kind == pageSlab {
			panic("arena: double free or free of a non-allocation")
		}
		panic("arena: free of a non-allocation")
	}
	h.release(page, slot)
}

// release drops one allocation. When that frees its pages it returns the page
// after the free run they joined, and zero otherwise.
func (h *heap) release(page, slot uint32) uint32 {
	m := &h.meta[page]
	size := h.extent(page, slot)
	h.liveBytes -= uint64(size)
	h.liveObjects--
	h.frees++
	if slot == noPage {
		return h.givePages(page, m.count)
	}
	slots := slotsPerPage(m.class)
	m.alloc[slot/64] &^= 1 << (slot % 64)
	// A thread that read a stale owner may set a bit of this word at
	// any time (cache.go).
	clearBit(&m.mark[slot/64], 1<<(slot%64))
	m.head = min(m.head, slot/64)
	wasFull := uint32(m.used) == slots
	m.used--
	switch {
	case m.used == 0 && !wasFull && h.partial[m.class] == page && m.next == noPage:
		// The class's only slab stays, so a loop that frees and
		// allocates again does not take and return a page each time.
		// The next sweep gives it back if it is still empty.
	case m.used == 0:
		if !wasFull {
			h.removePartial(page)
		}
		return h.givePages(page, 1)
	case wasFull:
		h.pushPartial(page)
		h.diag.fullToPartial++
	}
	return 0
}

// maxRetained bounds the pages a heap may be told to retain: what an
// explicit free lets wait before it gives pages back. defaultRetained is what
// a heap retains unless told otherwise: nothing. A guest workload ran slower
// the more it retained (ADR 0202): a retained page must be cleared before
// reuse, while a discarded one comes back zeroed and untouched slots on it
// need no clearing.
//
// A collection several threads' allocations started is the exception:
// retainPerAllocator pages per allocating thread, up to maxRetained, unless
// SetRetained chose a count. Eight threads allocating took 20% less time
// keeping 256 pages (ADR 0203), and those pages go back at the next idle
// wait or failed allocation.
const (
	maxRetained        = uint32(collectionFloor >> pageShift)
	defaultRetained    = uint32(0)
	retainPerAllocator = uint32(32)
)

// freeAt is runtime.free's work: it frees the allocation at address and,
// once more than collectionFloor bytes of free pages wait, gives back all but
// the retained ones.
func (h *heap) freeAt(address uintptr) {
	h.free(address)
	if h.dirtyFree<<pageShift > collectionFloor {
		h.releaseDown(uint64(h.retain))
	}
}

// setRetained sets how many free pages trimAfterCollection and freeAt leave.
func (h *heap) setRetained(pages uint32) { h.retain, h.retainSet = min(pages, maxRetained), true }

// retention is how many free pages a collection of reason leaves.
func (h *heap) retention(reason collectionReason) uint32 {
	switch {
	case reason == collectIdle || reason == collectPressure:
		return 0
	case reason == collectAutomatic && !h.retainSet && h.allocators > 1:
		return min(retainPerAllocator*h.allocators, maxRetained)
	}
	return h.retain
}

// trimAfterCollection gives back what an automatic collection freed, except
// the retained pages.
func (h *heap) trimAfterCollection() { h.trimAfter(collectAutomatic) }

// trimAfter gives back what a collection of reason freed, except what
// retention keeps.
func (h *heap) trimAfter(reason collectionReason) {
	keep := h.retention(reason)
	h.diag.retainTarget = keep
	if keep == 0 && reason != collectAutomatic && reason != collectExplicit {
		h.trim()
		return
	}
	h.releaseDown(uint64(keep))
}

// goal sets the live bytes at which the next collection starts: twice what
// the sweep left, and at least floor more. The heap's reservation caps it.
func (h *heap) goal() {
	floor := h.floor()
	next := h.liveBytes + max(h.liveBytes, floor)
	if limit := uint64(h.capacity) << pageShift; next < h.liveBytes || next > limit {
		next = limit
	}
	h.nextCollection = next
	c := &h.diag
	c.lastFloor, c.lastLive, c.lastAllocators = floor, h.liveBytes, h.allocators
	c.maxAllocators = max(c.maxAllocators, h.allocators)
}

// floor is the least growth before the next collection: collectionFloor
// for every thread that allocated since the last one, up to a quotaShare-th
// of the page quota and never below collectionFloor. Without a known quota it
// is collectionFloor.
func (h *heap) floor() uint64 {
	if h.pageLimit == 0 {
		return collectionFloor
	}
	scaled := uint64(max(h.allocators, 1)) * collectionFloor
	return min(scaled, max(collectionFloor, h.pageLimit/quotaShare))
}

// trim gives back every free page: an idle process, or one that ran out of
// room, keeps none.
func (h *heap) trim() {
	if h.dirtyFree != 0 {
		h.diag.trims++
		h.releaseFree()
	}
}

// releaseFree gives every dirty free page back to the kernel.
func (h *heap) releaseFree() {
	if h.dirtyFree == 0 {
		return
	}
	for bucket := range h.runs {
		for page := h.runs[bucket]; page != noPage; page = h.meta[page].next {
			h.releaseRun(page, h.meta[page].count)
		}
	}
}

// releaseDown gives back dirty free pages until at most keep are left. It
// takes them from the longest runs first and from the end of a run, so what
// stays is in the short runs a slab or a small run is taken from.
func (h *heap) releaseDown(keep uint64) {
	if keep == 0 {
		h.releaseFree()
		return
	}
	for bucket := runBuckets - 1; bucket >= 0 && h.dirtyFree > keep; bucket-- {
		for page := h.runs[bucket]; page != noPage && h.dirtyFree > keep; page = h.meta[page].next {
			if !h.releaseTail(page, h.meta[page].count, keep) {
				// The kernel refused; the pages stay dirty and the
				// next trim tries again.
				return
			}
		}
	}
}

// releaseTail discards the dirty pages of a free run from its end until at
// most keep dirty free pages are left. It reports false when a discard was
// refused.
func (h *heap) releaseTail(page, count uint32, keep uint64) bool {
	end := page + count
	for end > page && h.dirtyFree > keep {
		if h.meta[end-1].flags&pageDirty == 0 {
			end--
			continue
		}
		last := end
		for end > page && h.meta[end-1].flags&pageDirty != 0 && uint64(last-end) < h.dirtyFree-keep {
			end--
		}
		if !h.discardPages(end, last) {
			return false
		}
	}
	return true
}

func (h *heap) releaseRun(page, count uint32) {
	for index := page; index < page+count; {
		if h.meta[index].flags&pageDirty == 0 {
			index++
			continue
		}
		first := index
		for index < page+count && h.meta[index].flags&pageDirty != 0 {
			index++
		}
		h.discardPages(first, index)
	}
}

// discardPages gives the dirty free pages [first, last) back to the kernel
// and reports whether it took them. Pages it refused stay dirty, so they are
// cleared before reuse.
func (h *heap) discardPages(first, last uint32) bool {
	h.diag.discardCalls++
	if !h.discard(h.pageAddress(first), uintptr(last-first)<<pageShift) {
		h.diag.discardFailures++
		return false
	}
	for clean := first; clean < last; clean++ {
		h.meta[clean].flags &^= pageDirty
	}
	h.dirtyFree -= uint64(last - first)
	h.released += uint64(last-first) << pageShift
	h.diag.discarded += uint64(last - first)
	return true
}

func (h *heap) stats() Stats {
	reserved := uint64(h.capacity) << pageShift
	inUse := h.inUsePages << pageShift
	return Stats{
		Reserved: reserved, Live: h.liveBytes, Objects: h.liveObjects, PeakLive: h.peakLive,
		TotalAlloc: h.totalAlloc, Mallocs: h.mallocs, Frees: h.frees,
		InUse: inUse, Idle: reserved - inUse, Held: inUse + h.dirtyFree<<pageShift, PeakHeld: h.peakHeld,
		Released: h.released, Metadata: uint64(metaBytes(h.frontier)),
		Collections: h.collections, NextCollection: h.nextCollection,
	}
}

// clearMemory zeroes length bytes at address.
func clearMemory(address, length uintptr) {
	// SAFETY: callers pass a range they own: a slot or the pages of a run
	// that no live allocation holds yet, or the heap's own counters.
	clear(unsafe.Slice((*byte)(pointerAt(address)), length))
}

// pointerAt turns an address the allocator owns into a pointer. It reuses the
// word's bits rather than converting, since an address computed from a
// reservation base is not derived from a Go pointer.
func pointerAt(address uintptr) unsafe.Pointer {
	return *(*unsafe.Pointer)(unsafe.Pointer(&address))
}
