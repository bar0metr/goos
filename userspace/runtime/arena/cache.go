package arena

import (
	"math/bits"
	"sync/atomic"
	"unsafe"
)

// Thread-owned slabs (ADR 0203, ADR 0205). Once a process runs several
// threads, each thread owns up to maxCachedSlabs slabs per class. Only the
// owner allocates from them and frees into them, without the heap lock; a
// slab's page record names its owner. A thread that frees a slot of a slab
// another thread owns sets the slot's bit in the slab's mark bitmap, and the
// owner folds those bits into its allocation bitmap when its slabs of that
// class run out. A collection folds them too, while it holds every thread,
// before it marks: from then until its sweep ends the bitmap holds marks. A
// second free of one slot finds its bit already set.
//
// Ownership changes only under the heap lock. A thread that frees remotely
// reads the owner without it, so its bit can land after the owner gave the
// slab up: the slab then keeps the bit until the next owner folds it or a
// collection does. A lost or late bit is never unsafe: the slot stays
// allocated, and a collection frees it once nothing points to it.
//
// The functions here run without the heap lock only where they say so.

// maxCachedSlabs bounds the slabs a thread owns per class. A refill aims to
// give a thread targetCachedSlots free slots: one slab of a small class
// holds more, and a class with few slots per page takes several slabs, up
// to the bound (cacheTarget).
const (
	maxCachedSlabs    = 4
	targetCachedSlots = 16
)

// classCache is the slabs a thread owns of one class. pages[:count] hold
// page plus one and the rest zero; hint is the slab the fast path tries
// first.
type classCache struct {
	pages [maxCachedSlabs]uint32
	count uint8
	hint  uint8
}

// threadCache is the slabs a thread owns, by class.
type threadCache struct {
	classes [classCount]classCache
}

// threadCounts is what a thread counted without the heap lock. foldCounts
// adds it to the heap's counters.
type threadCounts struct {
	allocs, allocBytes, frees, freeBytes, cleared uint64
	localFrees, remoteFrees, refills, drained     uint64
	// misses counts fast-path allocations that found every owned slab of
	// their class full; batches counts refills that took new slabs and
	// claimed the slabs they took.
	misses, batches, claimed uint64
	sizeCounts, sizeBytes    [threadSizeBuckets]uint64
}

// cacheTarget is how many slabs of class a refill gives a thread.
func cacheTarget(class uint8) uint32 {
	slots := slotsPerPage(class)
	return min(max((targetCachedSlots+slots-1)/slots, 1), maxCachedSlabs)
}

// cacheBound is the most slab pages one thread's cache can own.
func cacheBound() uint32 {
	pages := uint32(0)
	for class := range classSizes {
		pages += cacheTarget(uint8(class))
	}
	return pages
}

// cacheAlloc takes a slot from a slab c owns for class, the hinted one
// first, or returns zero when c owns none with a free slot. Only c's thread
// calls it, and it needs no lock: no other thread allocates from those
// slabs or changes their allocation bitmaps.
func (h *heap) cacheAlloc(c *threadCache, n *threadCounts, class uint8, pointerFree bool) uintptr {
	cc := &c.classes[class]
	slots := h.classSlots[class]
	index := cc.hint
	for tries := uint8(0); tries < cc.count; tries++ {
		page := cc.pages[index] - 1
		if h.meta[page].used < slots {
			cc.hint = index
			address, cleared := h.takeSlot(page, pointerFree)
			size := uint64(classSizes[class])
			n.allocs++
			n.allocBytes += size
			n.cleared += uint64(cleared)
			return address
		}
		if index++; index == cc.count {
			index = 0
		}
	}
	n.misses++
	return 0
}

// ownedSlot resolves address to its slot in a slab some thread owns. It
// returns that thread's owner value, zero when the page is not an owned
// slab, and ok false when address is not the start of a slot.
func (h *heap) ownedSlot(address uintptr) (page, slot, owner uint32, ok bool) {
	page = uint32((address - h.base) >> pageShift)
	m := &h.meta[page]
	owner = atomic.LoadUint32(&m.owner)
	if owner == 0 || m.kind != pageSlab {
		return page, 0, 0, true
	}
	size := uint32(classSizes[m.class])
	offset := uint32(address & (pageBytes - 1))
	slot = offset / size
	if offset != slot*size || offset+size > uint32(pageBytes) {
		return page, slot, owner, false
	}
	return page, slot, owner, true
}

// cacheFree frees a slot of a slab the calling thread owns, without the heap
// lock. It reports false for a slot that is not allocated.
func (h *heap) cacheFree(n *threadCounts, page, slot uint32) bool {
	m := &h.meta[page]
	bit := uint64(1) << (slot % 64)
	if m.alloc[slot/64]&bit == 0 || atomic.LoadUint64(&m.mark[slot/64])&bit != 0 {
		return false
	}
	m.alloc[slot/64] &^= bit
	m.head = min(m.head, slot/64)
	m.used--
	n.frees++
	n.freeBytes += uint64(classSizes[m.class])
	n.localFrees++
	return true
}

// remoteFree records the free of a slot in a slab another thread owns, with
// no lock. It reports false when the slot was already freed this way.
func (h *heap) remoteFree(n *threadCounts, page, slot uint32) bool {
	word := &h.meta[page].mark[slot/64]
	bit := uint64(1) << (slot % 64)
	for {
		old := atomic.LoadUint64(word)
		if old&bit != 0 {
			return false
		}
		if atomic.CompareAndSwapUint64(word, old, old|bit) {
			n.remoteFrees++
			return true
		}
	}
}

// drainRemote folds the remote frees of a slab into its allocation bitmap.
// The slab's owner calls it, or a thread holding the heap lock while the
// slab has no owner or its owner is held.
func (h *heap) drainRemote(n *threadCounts, page uint32) {
	m := &h.meta[page]
	drained := uint64(0)
	for word := uint32(0); word < slotWords; word++ {
		if atomic.LoadUint64(&m.mark[word]) == 0 {
			continue
		}
		freed := atomic.SwapUint64(&m.mark[word], 0)
		if freed&^m.alloc[word] != 0 {
			panic("arena: remote free of a free slot")
		}
		m.alloc[word] &^= freed
		m.head = min(m.head, word)
		drained += uint64(bits.OnesCount64(freed))
	}
	if drained == 0 {
		return
	}
	m.used -= uint16(drained)
	n.frees += drained
	n.freeBytes += drained * uint64(classSizes[m.class])
	n.drained += drained
}

// refill makes c own a slab of class with a free slot. It folds the remote
// frees of the slabs c owns of class first; when every one is still full,
// it gives them up and takes up to want slabs, listed ones first, then new
// ones. owner is the thread's slot plus one. It reports false when c owns
// no slab with a free slot afterwards: the heap had no page. The caller
// holds the heap lock.
func (h *heap) refill(c *threadCache, n *threadCounts, class uint8, owner, want uint32) bool {
	n.refills++
	cc := &c.classes[class]
	slots := uint32(h.classSlots[class])
	room := false
	for index := uint8(0); index < cc.count; index++ {
		page := cc.pages[index] - 1
		h.drainRemote(n, page)
		if !room && uint32(h.meta[page].used) < slots {
			cc.hint, room = index, true
		}
	}
	if room {
		return true
	}
	// Every slab is full. A full slab given up stays off the lists until a
	// free or a sweep gives it a free slot.
	for index := uint8(0); index < cc.count; index++ {
		h.disown(n, cc.pages[index]-1)
		cc.pages[index] = 0
		h.diag.disownedFull++
	}
	cc.count, cc.hint = 0, 0
	want = min(max(want, 1), maxCachedSlabs)
	for uint32(cc.count) < want {
		page := h.partial[class]
		if page != noPage {
			h.removePartial(page)
			// A free that read a stale owner may have left bits.
			h.drainRemote(n, page)
		} else if page = h.newSlab(class); page == noPage {
			break
		}
		atomic.StoreUint32(&h.meta[page].owner, owner)
		cc.pages[cc.count] = page + 1
		cc.count++
		h.cachedPages++
		n.claimed++
	}
	if cc.count == 0 {
		return false
	}
	n.batches++
	h.diag.peakCached = max(h.diag.peakCached, h.cachedPages)
	return true
}

// disown gives up a thread's slab: it folds the remote frees, clears the
// owner, and lists the slab, or gives its page back when it is empty. The
// caller holds the heap lock, and the owner is the caller or is held.
func (h *heap) disown(n *threadCounts, page uint32) {
	m := &h.meta[page]
	atomic.StoreUint32(&m.owner, 0)
	h.cachedPages--
	h.drainRemote(n, page)
	switch {
	case m.used == 0:
		h.givePages(page, 1)
	case uint32(m.used) < uint32(h.classSlots[m.class]):
		h.pushPartial(page)
	}
}

// returnCache gives every slab c owns back to the heap and returns how many
// it gave. The caller holds the heap lock, and c's thread is the caller, is
// held, or has ended.
func (h *heap) returnCache(c *threadCache, n *threadCounts) uint64 {
	returned := uint64(0)
	for class := range c.classes {
		cc := &c.classes[class]
		for index := uint8(0); index < cc.count; index++ {
			h.disown(n, cc.pages[index]-1)
			cc.pages[index] = 0
			returned++
		}
		cc.count, cc.hint = 0, 0
	}
	h.diag.returnedSlabs += returned
	return returned
}

// drainCache folds the remote frees of every slab c owns and keeps them
// owned. A collection that keeps the threads' slabs calls it for every
// thread before it marks, so the mark bitmaps hold no remote free when
// marking starts. It returns how many slabs c owns. The caller holds the
// heap lock and c's thread is the caller or is held.
func (h *heap) drainCache(c *threadCache, n *threadCounts) uint64 {
	kept := uint64(0)
	for class := range c.classes {
		cc := &c.classes[class]
		for index := uint8(0); index < cc.count; index++ {
			h.drainRemote(n, cc.pages[index]-1)
		}
		kept += uint64(cc.count)
	}
	return kept
}

// checkCache stops the process when a slab c names is no longer a slab of
// its class owned by owner: a sweep that kept the threads' slabs must leave
// every one where it was. The caller holds the heap lock.
func (h *heap) checkCache(c *threadCache, owner uint32) {
	for class := range c.classes {
		cc := &c.classes[class]
		if uint32(cc.count) > cacheTarget(uint8(class)) || cc.count != 0 && cc.hint >= cc.count {
			panic("arena: thread cache out of bounds")
		}
		for index := uint8(0); index < cc.count; index++ {
			m := &h.meta[cc.pages[index]-1]
			if m.kind != pageSlab || int(m.class) != class || m.owner != owner || m.flags&pageListed != 0 {
				panic("arena: thread cache names a slab it does not own")
			}
		}
	}
}

// foldCounts adds a thread's counts to the heap's and clears them. The
// caller holds the heap lock and is the counting thread, or that thread is
// held or has ended.
func (h *heap) foldCounts(n *threadCounts) {
	// Another thread may have freed what this one allocated before this
	// one folds its allocations; the sums are exact once every thread
	// folded, and wrap in between.
	h.liveBytes += n.allocBytes - n.freeBytes
	h.liveObjects += n.allocs - n.frees
	h.totalAlloc += n.allocBytes
	h.mallocs += n.allocs
	h.frees += n.frees
	if int64(h.liveBytes) >= 0 && h.liveBytes > h.peakLive {
		h.peakLive = h.liveBytes
	}
	c := &h.diag
	c.clearedBytes += n.cleared
	c.localAllocs += n.allocs
	c.localFrees += n.localFrees
	c.remoteFrees += n.remoteFrees
	c.refills += n.refills
	c.drained += n.drained
	c.misses += n.misses
	c.batches += n.batches
	c.claimed += n.claimed
	for bucket := range n.sizeCounts {
		c.sizeCounts[bucket] += n.sizeCounts[bucket]
		c.sizeBytes[bucket] += n.sizeBytes[bucket]
	}
	// Cleared in place: TinyGo puts a composite literal this large on the
	// heap.
	clearMemory(uintptr(unsafe.Pointer(n)), unsafe.Sizeof(*n))
}

// foldStrayFrees folds the remote frees left in slabs no thread owns. A
// collection calls it after it took the threads' slabs back or folded their
// remote frees, before it marks: from then on the mark bitmaps hold marks.
func (h *heap) foldStrayFrees() {
	// The counts are the heap's own: TinyGo would put a local this large,
	// whose address is passed on, on the heap.
	n := &h.stray
	for page := uint32(0); page < h.frontier; page++ {
		m := &h.meta[page]
		if m.kind != pageSlab || m.mark == [slotWords]uint64{} {
			continue
		}
		if m.owner != 0 {
			// A kept slab: drainCache folded it, and an owned slab is
			// never listed.
			h.drainRemote(n, page)
			continue
		}
		slots := uint32(h.classSlots[m.class])
		wasFull := uint32(m.used) == slots
		h.drainRemote(n, page)
		switch {
		case m.used == 0:
			if !wasFull {
				h.removePartial(page)
			}
			h.givePages(page, 1)
		case wasFull:
			h.pushPartial(page)
			h.diag.fullToPartial++
		}
	}
	h.foldCounts(n)
}

// clearBit clears bit in word, which other threads may set at the same time.
func clearBit(word *uint64, bit uint64) {
	for {
		old := atomic.LoadUint64(word)
		if old&bit == 0 || atomic.CompareAndSwapUint64(word, old, old&^bit) {
			return
		}
	}
}
