//go:build goos_userspace

package arena

import (
	"sync/atomic"

	"goos/internal/validation"
	"goos/kernel/kcall"
	"goos/userspace/runtime/sys"
	"goos/userspace/runtime/tls"
	"unsafe"
)

// Region is scratch memory for allocations with one obvious lifetime: a
// packet, a request, a key exchange. While a region is entered, every
// allocation the program makes is a bump allocation in it, and none reaches
// the collected heap. Clear drops everything in it at once.
//
// A region's memory is its own reservation, not part of the heap. The
// collector scans the used part of every region as roots, so heap objects a
// region value points to stay alive until the region is cleared; bytes past
// the used part are never scanned. No collection runs while a region is
// entered.
//
// Clear is valid only after the caller has dropped every pointer into the
// region. Enter may nest other regions, but never the same region twice.
// Leave is LIFO. A region that runs out of room stops the process through
// OnExhausted: it never spills into the heap.
//
// A region lives until Release. Create one per lifetime that repeats, such as
// one per connection or one for packet scratch, and Clear it between uses;
// creating a region per request without releasing it keeps every reservation
// and every region record for the life of the process.
//
// A region belongs to the thread that enters it until it leaves (ADR 0203):
// entering a region another thread has entered, or clearing or releasing an
// entered region, stops the process. Each thread has its own entered
// regions; another thread collecting scans this thread's regions up to where
// it allocates.
type Region struct {
	state    regionPosition
	link     *Region
	released bool
	// enteredBy is the slot plus one of the thread that has the region
	// entered, or zero.
	enteredBy atomic.Int32
}

// Scope is one entry into a region, left with Leave.
type Scope struct {
	state regionScope
	// outer is the region entered before, or nil.
	outer *Region
}

// regions lists every region, for the collector. The heap lock guards it
// once threads exist.
var regions *Region

// regionTrim is the used size above which Clear gives a region's pages back.
const regionTrim = 64 << 10

// NewRegion reserves a region of size bytes, between one page and 4 MiB.
func NewRegion(size uint32) *Region {
	if size < 4096 || size > 4<<20 {
		panic("arena: region size out of bounds")
	}
	if self().regions.depth != 0 {
		panic("arena: region created inside a region")
	}
	length := (uintptr(size) + pageBytes - 1) &^ (pageBytes - 1)
	base, status := sys.VMReserve(uint64(length), kcall.VMRead|kcall.VMWrite)
	if status != kcall.StatusOK {
		fail()
	}
	first := uintptr(base)
	region := &Region{state: regionPosition{position: allocatorPosition{start: first, next: first, end: first + length, high: first}}}
	lockHeap()
	region.link = regions
	regions = region
	unlockHeap()
	return region
}

// Enter makes the region the target of every allocation until Leave.
func (r *Region) Enter() Scope {
	tls.Pin()
	defer tls.Unpin()
	t := self()
	t.beginRegionChange()
	defer t.endRegionChange()
	if !r.enteredBy.CompareAndSwap(0, int32(selfSlot()+1)) {
		panic("arena: region entered by another thread")
	}
	if t.innermost != nil {
		// The region entered before keeps its position in its record
		// while this one takes the allocations, so a collector reads it.
		t.innermost.state.position = t.cursor
	}
	current, scope := enterRegion(t.cursor, &r.state, &t.regions)
	t.cursor = current
	if validation.Enabled {
		holdTransition(t)
	}
	outer := t.innermost
	t.innermost = r
	return Scope{state: scope, outer: outer}
}

// Leave returns allocation to whatever was entered before.
func (s Scope) Leave() {
	tls.Pin()
	defer tls.Unpin()
	t := self()
	t.beginRegionChange()
	defer t.endRegionChange()
	region := t.innermost
	t.cursor = leaveRegion(t.cursor, s.state, &t.regions)
	if validation.Enabled {
		holdTransition(t)
	}
	t.innermost = s.outer
	if region != nil {
		region.enteredBy.Store(0)
	}
}

// Clear drops every allocation in the region.
func (r *Region) Clear() {
	if r.state.active || r.enteredBy.Load() != 0 {
		panic("arena: cannot clear active region")
	}
	if r.released {
		panic("arena: region used after release")
	}
	position := &r.state.position
	if position.high-position.start > regionTrim {
		end := (position.high + pageBytes - 1) &^ (pageBytes - 1)
		if discard(position.start, end-position.start) {
			position.high = position.start
		}
	}
	position.next = position.start
}

// Release gives the region's memory back for good: it unmaps the reservation
// and takes the region out of the collector's roots. Nothing may point into the
// region afterwards, and the region must not be entered, cleared, or released
// again.
func (r *Region) Release() {
	if r.state.active || r.enteredBy.Load() != 0 {
		panic("arena: cannot release active region")
	}
	if r.released {
		panic("arena: region released twice")
	}
	lockHeap()
	link := &regions
	for *link != nil && *link != r {
		link = &(*link).link
	}
	if *link == nil {
		unlockHeap()
		panic("arena: released region is not registered")
	}
	*link = r.link
	r.link = nil
	unlockHeap()
	position := r.state.position
	if sys.VMUnmap(uint64(position.start), uint64(position.end-position.start)) != kcall.StatusOK {
		panic("arena: region unmap failed")
	}
	// A zero position makes enterRegion refuse the region.
	r.state.position = allocatorPosition{}
	r.released = true
}

// Used is the bytes allocated in the region since it was last cleared.
func (r *Region) Used() uint64 {
	position := r.position()
	return uint64(position.next - position.start)
}

// position is where the region allocates now: the cursor of the thread
// that has it as its innermost entered region, or its record.
func (r *Region) position() allocatorPosition {
	if slot := int(r.enteredBy.Load()) - 1; slot >= 0 && threads[slot].innermost == r {
		return threads[slot].cursor
	}
	return r.state.position
}

// regionAlloc is a zeroed bump allocation in the thread's innermost entered
// region.
func regionAlloc(t *threadState, size uintptr) unsafe.Pointer {
	cursor := &t.cursor
	first := cursor.next
	rounded := (size + 15) &^ 15
	last := first + rounded
	if rounded < size || last < first || last > cursor.end {
		fail()
	}
	cursor.next = last
	// Memory below high may hold a previous generation's bytes.
	if first < cursor.high {
		clearMemory(first, min(last, cursor.high)-first)
	}
	if last > cursor.high {
		cursor.high = last
	}
	return pointerAt(first)
}

// inRegion reports whether an address lies in any region's reservation. The
// caller holds the heap lock once threads exist.
func inRegion(address uintptr) bool {
	for region := regions; region != nil; region = region.link {
		position := region.state.position
		if address >= position.start && address < position.end {
			return true
		}
	}
	return false
}

// markRegions scans the used part of every region. The collecting thread
// has none entered; another thread's entered regions are read where it
// stopped.
func markRegions() {
	for region := regions; region != nil; region = region.link {
		position := region.position()
		native.markRange(position.start, position.next)
	}
}
