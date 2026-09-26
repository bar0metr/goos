package vfsservice

import (
	"goos/kernel/ramfs"
	"goos/userspace/fsproto"
)

// scratchSlot is the file-cache slot a write to a page that is not cached
// takes its bytes from. It never holds a cached page, so such a write needs
// no read of the page first.
const scratchSlot = fsproto.CachePages - 1

// cachedPage is one page of a file held in a slot of the file cache. length
// is how many of its bytes are file data; a page shorter than PageSize is the
// last page of its file.
type cachedPage struct {
	node   ramfs.NodeID
	page   uint64
	length uint64
	stamp  uint64
	used   bool
}

// PageCache is the VFS's index of the file cache, the memory it shares with a
// filesystem service (ADR 0098). The service fills a slot when the VFS asks it
// to read a page, and the VFS serves reads of that page from the slot until
// the page is dropped. The index lives only in the VFS: the service never
// learns which slot holds what, and nothing it writes into the memory can make
// the VFS serve one file's bytes as another's.
//
// It is coherent for the same reason the attribute cache is: the VFS is the
// service's only client and runs one request at a time, so every change to a
// file passes through the backend that owns this index.
type PageCache struct {
	memory []byte
	pages  [scratchSlot]cachedPage
	clock  uint64
}

// Attach gives the cache the shared mapping, which must be the whole file
// cache, and starts it empty.
func (cache *PageCache) Attach(memory []byte) bool {
	if len(memory) != fsproto.CacheSize {
		return false
	}
	cache.memory = memory
	cache.ForgetAll()
	return true
}

// Ready reports whether a mapping is attached.
func (cache *PageCache) Ready() bool { return cache.memory != nil }

// Slot returns the bytes of one slot.
func (cache *PageCache) Slot(slot int) []byte {
	start := slot * fsproto.PageSize
	return cache.memory[start : start+fsproto.PageSize]
}

// Run returns the bytes of count consecutive slots from start.
func (cache *PageCache) Run(start, count int) []byte {
	return cache.memory[start*fsproto.PageSize : (start+count)*fsproto.PageSize]
}

// Lookup returns the slot holding page of node and how much of it is data.
func (cache *PageCache) Lookup(node ramfs.NodeID, page uint64) (int, uint64, bool) {
	for slot := range cache.pages {
		if entry := &cache.pages[slot]; entry.used && entry.node == node && entry.page == page {
			cache.clock++
			entry.stamp = cache.clock
			return slot, entry.length, true
		}
	}
	return 0, 0, false
}

// Victim returns the slot to fill next, empty if there is one and otherwise
// the one used longest ago, and drops what it held.
func (cache *PageCache) Victim() int {
	victim := 0
	for slot := range cache.pages {
		if !cache.pages[slot].used {
			return slot
		}
		if cache.pages[slot].stamp < cache.pages[victim].stamp {
			victim = slot
		}
	}
	cache.pages[victim] = cachedPage{}
	return victim
}

// Fill records that slot now holds length bytes of page of node.
func (cache *PageCache) Fill(slot int, node ramfs.NodeID, page uint64, length uint64) {
	cache.clock++
	cache.pages[slot] = cachedPage{node: node, page: page, length: length, stamp: cache.clock, used: true}
}

// Written records a write of count bytes at within into slot, which held
// length bytes of its page. A write that begins past the page's data would
// leave bytes the cache never saw, so the page is dropped instead.
func (cache *PageCache) Written(slot int, length, within, count uint64) {
	entry := &cache.pages[slot]
	switch {
	case within > length:
		*entry = cachedPage{}
	case within+count > length:
		entry.length = within + count
	}
}

// ClaimRun empties count consecutive slots — the run whose newest page was
// used longest ago — and returns the first. A read or write that spans pages
// moves them in one exchange through such a run (ADR 0107).
func (cache *PageCache) ClaimRun(count int) int {
	best, bestAge := 0, ^uint64(0)
	for start := 0; start+count <= scratchSlot; start++ {
		age := uint64(0)
		for slot := start; slot < start+count; slot++ {
			if entry := &cache.pages[slot]; entry.used && entry.stamp > age {
				age = entry.stamp
			}
		}
		if age < bestAge {
			best, bestAge = start, age
		}
	}
	for slot := best; slot < best+count; slot++ {
		cache.pages[slot] = cachedPage{}
	}
	return best
}

// ForgetRange drops pages first through last of node.
func (cache *PageCache) ForgetRange(node ramfs.NodeID, first, last uint64) {
	for slot := range cache.pages {
		if entry := &cache.pages[slot]; entry.used && entry.node == node && entry.page >= first && entry.page <= last {
			*entry = cachedPage{}
		}
	}
}

// Drop forgets one slot.
func (cache *PageCache) Drop(slot int) { cache.pages[slot] = cachedPage{} }

// DropPartial forgets the pages of node shorter than a page, except keep. A
// write elsewhere in the file may have extended it past them, so the length
// they record could end the file too early.
func (cache *PageCache) DropPartial(node ramfs.NodeID, keep int) {
	for slot := range cache.pages {
		if entry := &cache.pages[slot]; entry.used && entry.node == node && entry.length < fsproto.PageSize && slot != keep {
			*entry = cachedPage{}
		}
	}
}

// Forget drops every page of node.
func (cache *PageCache) Forget(node ramfs.NodeID) {
	for slot := range cache.pages {
		if cache.pages[slot].node == node {
			cache.pages[slot] = cachedPage{}
		}
	}
}

// ForgetAll drops every page: after a rename or unlink, which can move node
// IDs, after any transport failure, and when a service is attached.
func (cache *PageCache) ForgetAll() {
	cache.pages = [scratchSlot]cachedPage{}
}

// Resident counts the cached pages, for tests and diagnostics.
func (cache *PageCache) Resident() int {
	count := 0
	for _, entry := range cache.pages {
		if entry.used {
			count++
		}
	}
	return count
}
