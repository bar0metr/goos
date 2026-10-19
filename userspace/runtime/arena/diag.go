package arena

import "math/bits"

// sizeBuckets is the number of 16-byte steps up to maxSmallSize. Every class
// size is a multiple of 16, so the histogram resolves exactly which class a
// request takes under any table of such sizes.
const sizeBuckets = maxSmallSize / 16

// counters is what the allocator counts as it runs. It lives in the heap
// state, which the collector skips, so no count is ever taken for a pointer.
// Every field is a fixed-size number: counting allocates nothing.
type counters struct {
	sizeCounts, sizeBytes                    [sizeBuckets]uint64
	largeAllocs, largeRequested, largePages  uint64
	newSlabs, pagesFromRuns, pagesFromFresh  uint64
	pagesRecommitted, clearedBytes           uint64
	partialToFull, fullToPartial             uint64
	runSearches, runCandidates               uint64
	discardCalls, discardFailures, discarded uint64
	trims                                    uint64
	// What threads counted (cache.go): allocations from owned slabs, frees
	// into them by the owner and by other threads, refills, remote frees
	// folded, and slabs given back.
	localAllocs, localFrees, remoteFrees uint64
	refills, drained, returnedSlabs      uint64
	// slabChunks marks every page of slab bitmaps a slab would have touched
	// so far had the bitmaps their own array indexed by page.
	slabChunks [slabChunkWords]uint64
}

// slabsPerChunk is how many slabs' bitmaps fill a page, and slabChunkWords
// the bitmap of such pages for the largest heap.
const (
	slabsPerChunk  = uint32(pageBytes / slabBitmapBytes)
	slabChunkWords = (heapPagesMax/slabsPerChunk + 64) / 64
	heapPagesMax   = uint32(1 << 16)
)

// noteSlab records the bitmap page the slab at page would touch.
func (c *counters) noteSlab(page uint32) {
	if chunk := page / slabsPerChunk; chunk/64 < uint32(len(c.slabChunks)) {
		c.slabChunks[chunk/64] |= 1 << (chunk % 64)
	}
}

// Diagnostics is what the allocator counted since the heap was set up and a
// snapshot of what its metadata holds now. The counters are cumulative; the
// snapshot fields describe one moment.
type Diagnostics struct {
	// SizeCounts and SizeBytes histogram small requests: entry i counts
	// requests of 16*i+1 to 16*(i+1) bytes and the bytes they asked for.
	// Only validation images and host builds keep them (sizeProfile).
	SizeCounts, SizeBytes [sizeBuckets]uint64
	// LargeAllocs counts page-run allocations, LargeRequested the bytes they
	// asked for, and LargePages the pages they were given.
	LargeAllocs, LargeRequested, LargePages uint64
	// NewSlabs counts slabs set up for a class. Every other small
	// allocation took a free slot in a slab that already existed.
	NewSlabs uint64
	// PagesFromRuns and PagesFromFresh count pages taken for slabs and runs
	// from free runs and from the never-used part of the reservation.
	PagesFromRuns, PagesFromFresh uint64
	// PagesRecommitted counts pages taken from free runs after a discard
	// gave them back: the kernel supplies and charges each one again when it
	// is touched.
	PagesRecommitted uint64
	// ClearedBytes is what allocation zeroed because the memory was reused.
	ClearedBytes uint64
	// PartialToFull and FullToPartial count slabs leaving and joining their
	// class's list of slabs with free slots.
	PartialToFull, FullToPartial uint64
	// RunSearches counts searches of the free-run lists and RunCandidates
	// the runs they looked at.
	RunSearches, RunCandidates uint64
	// DiscardCalls counts discards, DiscardFailures the ones the kernel
	// refused, and DiscardedPages the pages given back. They are cumulative:
	// a discarded page may have been taken again since.
	DiscardCalls, DiscardFailures, DiscardedPages uint64
	// Trims counts idle and pressure trims that gave back retained pages.
	Trims uint64
	// Retained is how many free pages a collection or an explicit free
	// leaves undiscarded.
	Retained uint32

	// Frontier is the first page never handed out.
	Frontier uint32
	// MetadataPages is the pages of metadata up to the frontier: touched
	// once, they stay charged to the process.
	MetadataPages uint64
	// PartialSlabs, FullSlabs, and FreeSlots are, per class, the slabs with
	// a free slot, the full ones, and the free slots of the partial ones.
	PartialSlabs, FullSlabs, FreeSlots [classCount]uint32
	// FreeRuns is the number of free runs, FreeRunPages the pages in them,
	// LongestFreeRun the longest, and DirtyFreePages the free pages not
	// given back to the kernel.
	FreeRuns, FreeRunPages, LongestFreeRun, DirtyFreePages uint32
	// RunPages is the pages held by run allocations.
	RunPages uint32
	// SlabMetadataChunks is how many pages of 96-byte slab bitmaps the
	// slabs alive now would touch if bitmaps were kept in their own array
	// indexed by page, and SlabChunksTouched how many every slab so far
	// would have touched: such pages, once touched, stay charged. They size
	// that alternative layout.
	SlabMetadataChunks, SlabChunksTouched uint32

	// The rest is what the runtime counted for threads (ADR 0203).
	// ThreadsLive is the threads alive now and ThreadsPeak the most at
	// once.
	ThreadsLive, ThreadsPeak uint32
	// LocalAllocs counts allocations from a thread's own slabs, LocalFrees
	// frees by the owner into them, and RemoteFrees frees by another
	// thread. Refills counts slow paths that gave a thread a slab, Drained
	// the remote frees folded in, and ReturnedSlabs the slabs threads gave
	// back at a collection or their exit.
	LocalAllocs, LocalFrees, RemoteFrees, Refills, Drained, ReturnedSlabs uint64
	// HeapLocks and GCLocks count acquisitions of the heap and collection
	// locks, and HeapContended and GCContended the ones that had to wait.
	HeapLocks, HeapContended, GCLocks, GCContended uint64
	// Suspensions counts the times every other thread was held for a
	// collection and SuspendFailures the collections given up because they
	// could not be.
	Suspensions, SuspendFailures uint64
	// RendezvousLast, RendezvousMax, and RendezvousTotal are how long
	// holding the other threads took, and PauseLast, PauseMax, and
	// PauseTotal how long collections took, in nanoseconds.
	RendezvousLast, RendezvousMax, RendezvousTotal uint64
	PauseLast, PauseMax, PauseTotal                uint64
}

// ProfileVersion is the version of the ALLOC_PROFILE format AppendProfile
// writes. Version 2 added the version field and the threads line; a report
// without the field is version 1.
const ProfileVersion = 2

// slabBitmapBytes is the size of the three slot bitmaps of a slab.
const slabBitmapBytes = 3 * slotWords * 8

// diagnostics fills d from the counters and a walk of the metadata.
func (h *heap) diagnostics(d *Diagnostics) {
	c := &h.diag
	// Arrays are copied with copy: TinyGo puts a temporary this large on
	// the heap, and diagnostics must not allocate.
	copy(d.SizeCounts[:], c.sizeCounts[:])
	copy(d.SizeBytes[:], c.sizeBytes[:])
	d.LargeAllocs, d.LargeRequested, d.LargePages = c.largeAllocs, c.largeRequested, c.largePages
	d.NewSlabs, d.PagesFromRuns, d.PagesFromFresh = c.newSlabs, c.pagesFromRuns, c.pagesFromFresh
	d.PagesRecommitted, d.ClearedBytes = c.pagesRecommitted, c.clearedBytes
	d.PartialToFull, d.FullToPartial = c.partialToFull, c.fullToPartial
	d.RunSearches, d.RunCandidates = c.runSearches, c.runCandidates
	d.DiscardCalls, d.DiscardFailures, d.DiscardedPages = c.discardCalls, c.discardFailures, c.discarded
	d.Trims, d.Retained = c.trims, h.retain
	d.Frontier = h.frontier
	d.MetadataPages = uint64((metaBytes(h.frontier) + pageBytes - 1) >> pageShift)
	d.PartialSlabs, d.FullSlabs, d.FreeSlots = [classCount]uint32{}, [classCount]uint32{}, [classCount]uint32{}
	d.FreeRuns, d.FreeRunPages, d.LongestFreeRun, d.DirtyFreePages, d.RunPages = 0, 0, 0, 0, 0
	d.SlabMetadataChunks, d.SlabChunksTouched = 0, 0
	d.LocalAllocs, d.LocalFrees, d.RemoteFrees = c.localAllocs, c.localFrees, c.remoteFrees
	d.Refills, d.Drained, d.ReturnedSlabs = c.refills, c.drained, c.returnedSlabs
	for _, word := range c.slabChunks {
		d.SlabChunksTouched += uint32(bits.OnesCount64(word))
	}
	perChunk := slabsPerChunk
	lastChunk := noPage
	for page := uint32(0); page < h.frontier; {
		m := &h.meta[page]
		switch m.kind {
		case pageFree:
			d.FreeRuns++
			d.FreeRunPages += m.count
			d.LongestFreeRun = max(d.LongestFreeRun, m.count)
			for index := page; index < page+m.count; index++ {
				if h.meta[index].flags&pageDirty != 0 {
					d.DirtyFreePages++
				}
			}
			page += m.count
			continue
		case pageRun:
			d.RunPages += m.count
			page += m.count
			continue
		case pageSlab:
			if free := slotsPerPage(m.class) - uint32(m.used); free != 0 {
				d.PartialSlabs[m.class]++
				d.FreeSlots[m.class] += free
			} else {
				d.FullSlabs[m.class]++
			}
			if chunk := page / perChunk; chunk != lastChunk {
				d.SlabMetadataChunks++
				lastChunk = chunk
			}
		}
		page++
	}
}

// AppendProfile writes d as lines of the form
//
//	ALLOC_PROFILE name version=2 key=value ...
//
// and ends with "ALLOC_PROFILE name end lines=N", N the number of lines
// before it.
//
// one at a time into line, each ending in a newline, and hands each to emit.
// A line never exceeds len(line), so a console that relays whole writes keeps
// every line intact.
// Small requests are listed as bucket:count:bytes for each non-empty 16-byte
// bucket. Nothing is allocated; emit must not keep line.
func (d *Diagnostics) AppendProfile(name string, line []byte, emit func([]byte)) {
	w := profileWriter{line: line, name: name, emit: emit}
	w.start()
	w.pair("version", ProfileVersion)
	w.pair("large", d.LargeAllocs)
	w.pair("large_bytes", d.LargeRequested)
	w.pair("large_pages", d.LargePages)
	w.pair("new_slabs", d.NewSlabs)
	w.pair("from_runs", d.PagesFromRuns)
	w.pair("from_fresh", d.PagesFromFresh)
	w.pair("recommitted", d.PagesRecommitted)
	w.pair("cleared", d.ClearedBytes)
	w.pair("to_full", d.PartialToFull)
	w.pair("to_partial", d.FullToPartial)
	w.pair("searches", d.RunSearches)
	w.pair("candidates", d.RunCandidates)
	w.pair("discards", d.DiscardCalls)
	w.pair("discard_failures", d.DiscardFailures)
	w.pair("discarded", d.DiscardedPages)
	w.pair("trims", d.Trims)
	w.pair("retained", uint64(d.Retained))
	w.pair("frontier", uint64(d.Frontier))
	w.pair("meta_pages", d.MetadataPages)
	w.pair("slab_chunks", uint64(d.SlabMetadataChunks))
	w.pair("slab_chunks_touched", uint64(d.SlabChunksTouched))
	w.pair("free_runs", uint64(d.FreeRuns))
	w.pair("free_pages", uint64(d.FreeRunPages))
	w.pair("longest_free", uint64(d.LongestFreeRun))
	w.pair("dirty_free", uint64(d.DirtyFreePages))
	w.pair("run_pages", uint64(d.RunPages))
	w.flush()
	w.start()
	w.text("slabs")
	for class := range classSizes {
		if d.PartialSlabs[class]|d.FullSlabs[class] == 0 {
			continue
		}
		w.fields(4, uint64(classSizes[class]), uint64(d.PartialSlabs[class]), uint64(d.FullSlabs[class]), uint64(d.FreeSlots[class]))
	}
	w.flush()
	w.start()
	w.text("sizes")
	for bucket := range d.SizeCounts {
		if d.SizeCounts[bucket] != 0 {
			w.fields(3, uint64(bucket), d.SizeCounts[bucket], d.SizeBytes[bucket], 0)
		}
	}
	w.flush()
	w.start()
	w.text("threads")
	w.pair("live", uint64(d.ThreadsLive))
	w.pair("peak", uint64(d.ThreadsPeak))
	w.pair("local_allocs", d.LocalAllocs)
	w.pair("local_frees", d.LocalFrees)
	w.pair("remote_frees", d.RemoteFrees)
	w.pair("refills", d.Refills)
	w.pair("drained", d.Drained)
	w.pair("returned", d.ReturnedSlabs)
	w.pair("heap_locks", d.HeapLocks)
	w.pair("heap_contended", d.HeapContended)
	w.pair("gc_locks", d.GCLocks)
	w.pair("gc_contended", d.GCContended)
	w.pair("suspensions", d.Suspensions)
	w.pair("suspend_failures", d.SuspendFailures)
	w.pair("rendezvous_last", d.RendezvousLast)
	w.pair("rendezvous_max", d.RendezvousMax)
	w.pair("rendezvous_total", d.RendezvousTotal)
	w.pair("pause_last", d.PauseLast)
	w.pair("pause_max", d.PauseMax)
	w.pair("pause_total", d.PauseTotal)
	w.flush()
	// The last line counts the ones before it, so a reader can tell a
	// profile whose lines all arrived from one that lost some.
	lines := uint64(w.lines)
	w.start()
	w.text("end")
	w.pair("lines", lines)
	w.flush()
}

// profileWriter assembles profile lines in a caller's buffer.
type profileWriter struct {
	line []byte
	size int
	// head is where the fields start, so a full line can be sent and the
	// next one started with the same prefix.
	head, mark int
	name       string
	emit       func([]byte)
	label      string
	// lines counts the lines sent.
	lines int
}

func (w *profileWriter) start() {
	w.size, w.label = 0, ""
	w.put("ALLOC_PROFILE ")
	w.put(w.name)
	w.head = w.size
}

// put and putUint leave the last byte of the line for its newline.
func (w *profileWriter) put(text string) {
	w.size += copy(w.line[w.size:len(w.line)-1], text)
}

func (w *profileWriter) putUint(value uint64) {
	var digits [20]byte
	index := len(digits)
	for {
		index--
		digits[index] = byte('0' + value%10)
		value /= 10
		if value == 0 {
			break
		}
	}
	w.size += copy(w.line[w.size:len(w.line)-1], digits[index:])
}

// room makes sure need more bytes fit, sending the line so far when they do
// not and starting the next with the same prefix and label.
func (w *profileWriter) room(need int) {
	if w.size+need < len(w.line) || w.size == w.head {
		return
	}
	w.flush()
	label := w.label
	w.start()
	if label != "" {
		w.text(label)
	}
}

func (w *profileWriter) text(label string) {
	w.label = label
	w.put(" ")
	w.put(label)
	w.head = w.size
}

func (w *profileWriter) pair(key string, value uint64) {
	w.room(len(key) + 22)
	w.put(" ")
	w.put(key)
	w.put("=")
	w.putUint(value)
}

// fields writes the first count of a, b, c, d joined by colons. It takes
// them one by one: a variadic slice could be a heap allocation.
func (w *profileWriter) fields(count int, a, b, c, d uint64) {
	w.room(count * 21)
	w.put(" ")
	w.putUint(a)
	for index, value := range [...]uint64{b, c, d} {
		if index+1 < count {
			w.put(":")
			w.putUint(value)
		}
	}
}

func (w *profileWriter) flush() {
	if w.size > w.head {
		w.line[w.size] = '\n'
		w.emit(w.line[:w.size+1])
		w.lines++
	}
	w.size = w.head
}
