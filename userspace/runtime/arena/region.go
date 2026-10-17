//go:build goos_userspace

package arena

import (
	"goos/kernel/kcall"
	"goos/userspace/runtime/sys"
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
type Region struct {
	state regionPosition
	link  *Region
}

// Scope is one entry into a region, left with Leave.
type Scope struct{ state regionScope }

var (
	scopedRegions regionStack
	// regionCursor is the allocation position of the innermost entered
	// region.
	regionCursor allocatorPosition
	// regions lists every region, for the collector.
	regions *Region
)

// regionTrim is the used size above which Clear gives a region's pages back.
const regionTrim = 64 << 10

// NewRegion reserves a region of size bytes, between one page and 4 MiB.
func NewRegion(size uint32) *Region {
	if size < 4096 || size > 4<<20 {
		panic("arena: region size out of bounds")
	}
	if scopedRegions.depth != 0 {
		panic("arena: region created inside a region")
	}
	length := (uintptr(size) + pageBytes - 1) &^ (pageBytes - 1)
	base, status := sys.VMReserve(uint64(length), kcall.VMRead|kcall.VMWrite)
	if status != kcall.StatusOK {
		fail()
	}
	first := uintptr(base)
	region := &Region{state: regionPosition{position: allocatorPosition{start: first, next: first, end: first + length, high: first}}, link: regions}
	regions = region
	return region
}

// Enter makes the region the target of every allocation until Leave.
func (r *Region) Enter() Scope {
	current, scope := enterRegion(regionCursor, &r.state, &scopedRegions)
	regionCursor = current
	return Scope{state: scope}
}

// Leave returns allocation to whatever was entered before.
func (s Scope) Leave() { regionCursor = leaveRegion(regionCursor, s.state, &scopedRegions) }

// Clear drops every allocation in the region.
func (r *Region) Clear() {
	if r.state.active {
		panic("arena: cannot clear active region")
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

// Used is the bytes allocated in the region since it was last cleared.
func (r *Region) Used() uint64 {
	if r.state.active {
		return uint64(regionCursor.next - regionCursor.start)
	}
	return uint64(r.state.position.next - r.state.position.start)
}

// regionAlloc is a zeroed bump allocation in the innermost entered region.
func regionAlloc(size uintptr) unsafe.Pointer {
	first := regionCursor.next
	rounded := (size + 15) &^ 15
	last := first + rounded
	if rounded < size || last < first || last > regionCursor.end {
		fail()
	}
	regionCursor.next = last
	// Memory below high may hold a previous generation's bytes.
	if first < regionCursor.high {
		clearMemory(first, min(last, regionCursor.high)-first)
	}
	if last > regionCursor.high {
		regionCursor.high = last
	}
	return pointerAt(first)
}

// inRegion reports whether an address lies in any region's reservation.
func inRegion(address uintptr) bool {
	for region := regions; region != nil; region = region.link {
		position := region.state.position
		if address >= position.start && address < position.end {
			return true
		}
	}
	return false
}

// markRegions scans the used part of every region. No region is entered
// while a collection runs, so every region's position is its own.
func markRegions() {
	for region := regions; region != nil; region = region.link {
		position := region.state.position
		native.markRange(position.start, position.next)
	}
}
