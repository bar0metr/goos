// Package region is the native virtual-memory region model of a process
// (ADR 0104): which ranges of its address space are in use, with what
// protection, and what backs them. The page tables say which pages are
// present; a region says what a page may become. The kernel consults the map
// to resolve a demand-zero fault, to refuse a mapping that would overlap
// another, and to know what an unmap or a protection change covers.
//
// The package is pure bookkeeping over a bounded, sorted table. It never
// touches page tables or physical memory, so it is tested on the host.
package region

import "goos/kernel/lock"

const (
	PageSize = uint64(4096)
	// MaxRegions bounds the regions of one process. Every mapping a process
	// makes is one region, and splitting one by a partial unmap or
	// protection change takes another; a process that needs more is refused
	// with resource exhausted. It is an intentional system limit: the table
	// lives in the process and costs a fixed amount per process.
	MaxRegions = 128
)

// Protection is what the pages of a region may be accessed for.
type Protection uint8

const (
	Read Protection = 1 << iota
	Write
	Execute
)

// Valid reports whether the protection is readable when anything at all is
// allowed, and never writable and executable at once (W^X).
func (protection Protection) Valid() bool {
	if protection&^(Read|Write|Execute) != 0 {
		return false
	}
	if protection == 0 {
		return true
	}
	return protection&Read != 0 && protection&(Write|Execute) != Write|Execute
}

// Backing is what supplies a region's pages.
type Backing uint8

const (
	// Anonymous memory is demand-zero: a page is absent until first touched,
	// then a zeroed page is allocated and charged to the process.
	Anonymous Backing = iota + 1
	// Image is an executable's segment. Pages holding file bytes are mapped
	// when the image is loaded; the zero-filled tail past them is supplied on
	// demand like anonymous memory.
	Image
	// Shared is a shared-memory object mapped whole.
	Shared
	// Device is a DMA window or another kernel-provided mapping whose pages
	// are all mapped when it is created.
	Device
	// Guard is address space that must stay unmapped: a touch is a guard
	// violation, never a demand fault.
	Guard
)

func (backing Backing) valid() bool { return backing >= Anonymous && backing <= Guard }

// DemandZero reports whether an absent page of this backing may be supplied as
// a zeroed page on first touch.
func (backing Backing) DemandZero() bool { return backing == Anonymous || backing == Image }

// Flags qualify a region.
type Flags uint8

const (
	// Pinned regions were created by the kernel for the process's own
	// structure — its image, stack guard, startup page — and cannot be
	// unmapped or reprotected through the native VM calls.
	Pinned Flags = 1 << iota
	// FromObject says an image region's source is an executable object
	// rather than an extent of the system image (ADR 0106).
	FromObject
	// StackBatch reserves repeated fixed stacks and their lower guards in
	// one region (ADR 0204). Only whole-region removal is allowed.
	StackBatch
)

const (
	StackBytes      = uint64(256 << 10)
	StackGuardBytes = uint64(64 << 10)
	StackStride     = StackBytes + StackGuardBytes
)

// GuardedAt reports whether address lies in a guard of this region.
func (r Region) GuardedAt(address uint64) bool {
	return r.Contains(address) && (r.Backing == Guard ||
		r.Flags&StackBatch != 0 && (address-r.Base)%StackStride < StackGuardBytes)
}

// Region is one contiguous range of a process's address space.
type Region struct {
	Base       uint64
	Length     uint64
	Protection Protection
	Backing    Backing
	Flags      Flags
	// Object names what backs the region — a shared-memory object's
	// identity, an image's key — and Offset where in it the region starts.
	Object uint64
	Offset uint64
	// Data is how many bytes at the start of an image region come from its
	// source; the rest of the region is zero-filled (ADR 0106).
	Data uint64
}

// End is the first address past the region.
func (region Region) End() uint64 { return region.Base + region.Length }

// Contains reports whether address lies in the region.
func (region Region) Contains(address uint64) bool {
	return address >= region.Base && address-region.Base < region.Length
}

// Status is the outcome of a map operation.
type Status uint8

const (
	OK Status = iota
	InvalidArgument
	Overlap
	NotMapped
	PermissionDenied
	Exhausted
)

// Map is the region table of one address space, bounded to [lower, upper).
// Regions are kept sorted by base and never overlap.
type Map struct {
	lower, upper uint64
	regions      [MaxRegions]Region
	count        int
	// space is the lock of the address space the map describes, which a
	// validation build checks the caller holds.
	space *lock.RW
}

// Guard names the lock of the address space the map describes.
func (m *Map) Guard(space *lock.RW) { m.space = space }

func (m *Map) reading() {
	if m.space != nil {
		m.space.AssertHeld()
	}
}

func (m *Map) writing() {
	if m.space != nil {
		m.space.AssertWriter()
	}
}

// Init empties the map and fixes the range regions may occupy. Both bounds
// must be page aligned.
func (m *Map) Init(lower, upper uint64) bool {
	m.count = 0
	m.regions = [MaxRegions]Region{}
	if lower%PageSize != 0 || upper%PageSize != 0 || lower >= upper {
		m.lower, m.upper = 0, 0
		return false
	}
	m.lower, m.upper = lower, upper
	return true
}

// CopyFrom makes m a copy of source in place. Assigning a whole Map builds a
// temporary, which the kernel's leaking heap never frees.
func (m *Map) CopyFrom(source *Map) {
	m.writing()
	m.lower, m.upper, m.count = source.lower, source.upper, source.count
	copy(m.regions[:], source.regions[:])
}

// Count is the number of regions.
func (m *Map) Count() int {
	m.reading()
	return m.count
}

// At returns the index-th region in address order.
func (m *Map) At(index int) (Region, bool) {
	m.reading()
	if index < 0 || index >= m.count {
		return Region{}, false
	}
	return m.regions[index], true
}

// Bounds are the range regions may occupy.
func (m *Map) Bounds() (uint64, uint64) {
	m.reading()
	return m.lower, m.upper
}

// validRange accepts a page-aligned, non-empty range inside the map's bounds
// whose end does not wrap.
func (m *Map) validRange(base, length uint64) bool {
	return base%PageSize == 0 && length != 0 && length%PageSize == 0 &&
		base >= m.lower && base < m.upper && length <= m.upper-base
}

// Find returns the region containing address.
func (m *Map) Find(address uint64) (Region, bool) {
	m.reading()
	index, found := m.find(address)
	if !found {
		return Region{}, false
	}
	return m.regions[index], true
}

// find returns the index of the region containing address, or of the first
// region above it with found false.
func (m *Map) find(address uint64) (int, bool) {
	low, high := 0, m.count
	for low < high {
		middle := (low + high) / 2
		if m.regions[middle].End() <= address {
			low = middle + 1
		} else {
			high = middle
		}
	}
	return low, low < m.count && m.regions[low].Contains(address)
}

// Free reports whether [base, base+length) is inside the bounds and touches
// no region.
func (m *Map) Free(base, length uint64) bool {
	m.writing()
	if !m.validRange(base, length) {
		return false
	}
	index, _ := m.find(base)
	return index == m.count || m.regions[index].Base >= base+length
}

// Insert adds a region over free address space.
func (m *Map) Insert(region Region) Status {
	m.writing()
	if !m.validRange(region.Base, region.Length) || !region.Protection.Valid() || !region.Backing.valid() ||
		region.Backing == Guard && region.Protection != 0 {
		return InvalidArgument
	}
	if region.Flags&StackBatch != 0 && (region.Backing != Anonymous ||
		region.Protection != Read|Write || region.Length%StackStride != 0) {
		return InvalidArgument
	}
	if !m.Free(region.Base, region.Length) {
		return Overlap
	}
	if m.count == MaxRegions {
		return Exhausted
	}
	index, _ := m.find(region.Base)
	copy(m.regions[index+1:m.count+1], m.regions[index:m.count])
	m.regions[index] = region
	m.count++
	return OK
}

// FindFree returns the lowest page-aligned base at or above from where length
// bytes of free address space fit below limit. A deterministic first fit keeps
// the layout reproducible; a randomized base can replace from later without
// changing the callers (ASLR readiness).
func (m *Map) FindFree(length, from, limit uint64) (uint64, bool) {
	m.reading()
	if length == 0 || length%PageSize != 0 || from%PageSize != 0 {
		return 0, false
	}
	if from < m.lower {
		from = m.lower
	}
	if limit > m.upper {
		limit = m.upper
	}
	candidate := from
	index, _ := m.find(candidate)
	for {
		if candidate >= limit || length > limit-candidate {
			return 0, false
		}
		if index == m.count || m.regions[index].Base >= candidate+length {
			return candidate, true
		}
		if end := m.regions[index].End(); end > candidate {
			candidate = end
		}
		index++
	}
}

// Piece is a part of a region a range covers: the covered range and the
// region it lies in.
type Piece struct {
	Base   uint64
	Length uint64
	Region Region
}

// Pieces receives the parts of regions a range covers. A range can cover
// every region, so it holds as many pieces as a map holds regions. The kernel
// keeps one and passes it by pointer: nothing here allocates.
type Pieces [MaxRegions]Piece

// Overlapping fills pieces with the part of each region [base, base+length)
// covers, in address order, and returns how many there are. It changes
// nothing; the caller inspects the pieces — to check a policy, to unmap their
// pages — before it calls Remove or Protect.
func (m *Map) Overlapping(base, length uint64, pieces *Pieces) int {
	m.reading()
	if !m.validRange(base, length) || pieces == nil {
		return 0
	}
	end := base + length
	count := 0
	for index, _ := m.find(base); index < m.count && m.regions[index].Base < end; index++ {
		region := m.regions[index]
		start, stop := maxOf(region.Base, base), minOf(region.End(), end)
		pieces[count] = Piece{Base: start, Length: stop - start, Region: region}
		count++
	}
	return count
}

// Covered reports whether every page of [base, base+length) lies in some
// region, and none of them is pinned unless allowPinned.
func (m *Map) Covered(base, length uint64, allowPinned bool) bool {
	m.reading()
	if !m.validRange(base, length) {
		return false
	}
	address, end := base, base+length
	index, found := m.find(address)
	if !found {
		return false
	}
	for address < end {
		if index == m.count || !m.regions[index].Contains(address) {
			return false
		}
		if !allowPinned && m.regions[index].Flags&Pinned != 0 {
			return false
		}
		address = m.regions[index].End()
		index++
	}
	return true
}

// Remove takes [base, base+length) out of every region it overlaps, splitting
// a region it cuts through the middle of. Holes in the range are ignored.
// Pinned regions are refused unless allowPinned. Nothing changes when the
// result is not OK: the slots a split needs are counted first.
func (m *Map) Remove(base, length uint64, allowPinned bool) Status {
	m.writing()
	if !m.validRange(base, length) {
		return InvalidArgument
	}
	end := base + length
	first, _ := m.find(base)
	splits := 0
	for index := first; index < m.count && m.regions[index].Base < end; index++ {
		region := m.regions[index]
		if !allowPinned && region.Flags&Pinned != 0 {
			return PermissionDenied
		}
		if region.Flags&StackBatch != 0 && (base > region.Base || end < region.End()) {
			return PermissionDenied
		}
		if region.Base < base && region.End() > end {
			splits++
		}
	}
	if m.count+splits > MaxRegions {
		return Exhausted
	}
	for index := first; index < m.count && m.regions[index].Base < end; {
		region := m.regions[index]
		cutStart, cutEnd := maxOf(region.Base, base), minOf(region.End(), end)
		switch {
		case region.Base < cutStart && region.End() > cutEnd:
			m.insertAt(index+1, tail(region, cutEnd))
			m.regions[index] = head(region, cutStart)
			index += 2
		case region.Base < cutStart:
			m.regions[index] = head(region, cutStart)
			index++
		case region.End() > cutEnd:
			m.regions[index] = tail(region, cutEnd)
			index++
		default:
			m.removeAt(index)
		}
	}
	return OK
}

// Protect changes the protection of [base, base+length), which must be wholly
// covered by unpinned regions, splitting regions at the range's edges. The
// caller decides beforehand, with Overlapping, whether the new protection is
// allowed for each backing it covers.
func (m *Map) Protect(base, length uint64, protection Protection) Status {
	m.writing()
	return m.protect(base, length, protection, false)
}

// Restrict is Protect for a request that only takes rights away: every
// region the range covers must already allow everything protection allows.
// It may cover pinned regions, because removing a right never lets the
// process reach more than the kernel gave it. A Linux program uses it to
// make the relocated part of its image read-only.
func (m *Map) Restrict(base, length uint64, protection Protection) Status {
	m.writing()
	return m.protect(base, length, protection, true)
}

func (m *Map) protect(base, length uint64, protection Protection, restrict bool) Status {
	if !m.validRange(base, length) || !protection.Valid() {
		return InvalidArgument
	}
	if !m.Covered(base, length, restrict) {
		if m.Covered(base, length, true) {
			return PermissionDenied
		}
		return NotMapped
	}
	end := base + length
	first, _ := m.find(base)
	splits := 0
	for index := first; index < m.count && m.regions[index].Base < end; index++ {
		region := m.regions[index]
		if region.Flags&StackBatch != 0 || region.Backing == Guard && protection != 0 {
			return PermissionDenied
		}
		if restrict && protection&^region.Protection != 0 {
			return PermissionDenied
		}
		if region.Protection == protection {
			continue
		}
		if region.Base < base {
			splits++
		}
		if region.End() > end {
			splits++
		}
	}
	if m.count+splits > MaxRegions {
		return Exhausted
	}
	for index := first; index < m.count && m.regions[index].Base < end; index++ {
		region := m.regions[index]
		if region.Protection == protection {
			continue
		}
		cutStart, cutEnd := maxOf(region.Base, base), minOf(region.End(), end)
		if region.Base < cutStart {
			m.insertAt(index, head(region, cutStart))
			index++
		}
		middle := tail(head(region, cutEnd), cutStart)
		middle.Protection = protection
		m.regions[index] = middle
		if region.End() > cutEnd {
			m.insertAt(index+1, tail(region, cutEnd))
			index++
		}
	}
	return OK
}

func (m *Map) insertAt(index int, region Region) {
	copy(m.regions[index+1:m.count+1], m.regions[index:m.count])
	m.regions[index] = region
	m.count++
}

func (m *Map) removeAt(index int) {
	copy(m.regions[index:m.count-1], m.regions[index+1:m.count])
	m.count--
	m.regions[m.count] = Region{}
}

// head is the part of region below at.
func head(region Region, at uint64) Region {
	region.Length = at - region.Base
	if region.Data > region.Length {
		region.Data = region.Length
	}
	return region
}

// tail is the part of region from at on, its offset and source bytes
// advanced to match.
func tail(region Region, at uint64) Region {
	delta := at - region.Base
	region.Offset += delta
	if region.Data > delta {
		region.Data -= delta
	} else {
		region.Data = 0
	}
	region.Length = region.End() - at
	region.Base = at
	return region
}

func maxOf(a, b uint64) uint64 {
	if a > b {
		return a
	}
	return b
}

func minOf(a, b uint64) uint64 {
	if a < b {
		return a
	}
	return b
}
