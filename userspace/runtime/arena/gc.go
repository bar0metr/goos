package arena

import "math/bits"

// The collector is a non-moving, conservative mark and sweep over one heap.
//
// Marking treats every aligned word of the roots and of every marked
// allocation as a possible pointer. A word that falls inside a live
// allocation, at its start or anywhere inside it, marks that allocation. A
// word that only looks like a pointer keeps garbage alive; nothing reachable
// is ever swept. Words outside the heap are ignored, so the scan never follows
// a value into memory the heap does not own.
//
// The mark stack is bounded. An allocation marked while it is full is left
// marked but unscanned and overflow is set; finishMark then scans every marked
// allocation again until a pass completes without overflow. Deep graphs need
// no recursion and no memory beyond the stack the heap was given.

// beginMark starts a collection. Every mark bit is clear: sweep clears them.
func (h *heap) beginMark() {
	h.workTop = 0
	h.overflow = false
}

// markRange marks what the words in [start, end) point to and everything
// reachable from it.
func (h *heap) markRange(start, end uintptr) {
	h.scanWords(start, end)
	h.drain()
}

func (h *heap) scanWords(start, end uintptr) {
	start = (start + wordBytes - 1) &^ (wordBytes - 1)
	for address := start; address+wordBytes <= end && address >= start; address += wordBytes {
		// SAFETY: [start, end) is a root range the caller owns or an
		// allocation the heap handed out; both are mapped and readable.
		value := *(*uintptr)(pointerAt(address))
		if h.contains(value) {
			h.scanning = address
			h.markValue(value)
		}
	}
}

func (h *heap) markValue(value uintptr) {
	page, slot, start, ok := h.find(value)
	if !ok {
		return
	}
	m := &h.meta[page]
	pointerFree := false
	if slot == noPage {
		if m.marked {
			return
		}
		m.marked, pointerFree = true, m.pointerFree
	} else {
		bit := uint64(1) << (slot % 64)
		if m.mark[slot/64]&bit != 0 {
			return
		}
		m.mark[slot/64] |= bit
		pointerFree = m.noscan[slot/64]&bit != 0
	}
	if start == h.watch && h.watchAt == 0 {
		h.watchAt, h.watchPhase = h.scanning, h.phase
	}
	if pointerFree {
		// Nothing in it can keep anything alive, and reading it would
		// make the kernel supply pages the program never touched.
		return
	}
	if h.workTop == len(h.work) {
		h.overflow = true
		return
	}
	h.work[h.workTop] = start
	h.workTop++
	if h.workTop > h.workHigh {
		h.workHigh = h.workTop
	}
}

func (h *heap) drain() {
	for h.workTop > 0 {
		h.workTop--
		start := h.work[h.workTop]
		page, slot, found, ok := h.find(start)
		if !ok || found != start {
			panic("arena: mark stack holds a non-allocation")
		}
		h.scanWords(start, start+h.extent(page, slot))
	}
}

// finishMark completes marking after every root range was marked.
func (h *heap) finishMark() {
	for h.overflow {
		h.overflow = false
		for page := uint32(0); page < h.frontier; {
			m := &h.meta[page]
			switch m.kind {
			case pageRun:
				if m.marked && !m.pointerFree {
					start := h.pageAddress(page)
					h.scanWords(start, start+uintptr(m.count)<<pageShift)
					h.drain()
				}
				page += m.count
				continue
			case pageSlab:
				size := classSizes[m.class]
				for word := 0; word < slotWords; word++ {
					for marked := m.mark[word] &^ m.noscan[word]; marked != 0; marked &= marked - 1 {
						slot := uintptr(word*64 + bits.TrailingZeros64(marked))
						start := h.pageAddress(page) + slot*size
						h.scanWords(start, start+size)
						h.drain()
					}
				}
			}
			page++
		}
	}
}

// sweep frees every allocation left unmarked, clears the marks, and sets the
// growth that starts the next collection.
func (h *heap) sweep() {
	for page := uint32(0); page < h.frontier; {
		m := &h.meta[page]
		switch m.kind {
		case pageRun:
			if m.marked {
				m.marked = false
				page += m.count
			} else {
				if poisonFreed {
					h.poison(h.pageAddress(page), pageBytes)
				}
				page = h.release(page, noPage)
			}
			continue
		case pageFree:
			if m.count == 0 || m.head != page {
				panic("arena: sweep found a free page outside a run")
			}
			page += m.count
			continue
		case pageSlab:
			if end := h.sweepSlab(page); end != 0 {
				page = end
				continue
			}
		case pageRunTail:
			panic("arena: sweep found a run tail without its head")
		}
		page++
	}
	h.collections++
	h.collectedMallocs = h.mallocs
	h.nextCollection = h.liveBytes * 2
	if h.nextCollection < h.liveBytes+collectionFloor {
		h.nextCollection = h.liveBytes + collectionFloor
	}
}

// sweepSlab frees the unmarked slots of a slab. When that empties it, it
// returns the page after the free run the slab joined, and zero otherwise.
func (h *heap) sweepSlab(page uint32) uint32 {
	m := &h.meta[page]
	slots := slotsPerPage(m.class)
	live := 0
	for word := 0; word < slotWords; word++ {
		if m.mark[word]&^m.alloc[word] != 0 {
			panic("arena: mark on a free slot")
		}
		live += bits.OnesCount64(m.alloc[word])
	}
	if live != int(m.used) {
		println("arena: slab mismatch page", page, "class", m.class, "owner", m.owner,
			"bitmap", live, "used", m.used, "gc", h.collections)
		panic("arena: slab count does not match its bitmap")
	}
	dead := 0
	for word := 0; word < slotWords; word++ {
		dead += bits.OnesCount64(m.alloc[word] &^ m.mark[word])
	}
	if dead == 0 && m.used != 0 {
		m.mark = [slotWords]uint64{}
		return 0
	}
	size := uint64(classSizes[m.class])
	if poisonFreed {
		h.poisonSlots(page, uintptr(size))
	}
	h.liveBytes -= uint64(dead) * size
	h.liveObjects -= uint64(dead)
	h.frees += uint64(dead)
	wasFull := uint32(m.used) == slots
	m.head = 0
	for word := 0; word < slotWords; word++ {
		m.alloc[word] &= m.mark[word]
	}
	m.mark = [slotWords]uint64{}
	m.used -= uint16(dead)
	switch {
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
