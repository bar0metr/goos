package goroutine

// sleepEntry is a goroutine waiting for a time: a sleep, or the deadline of
// a handle wait. seq is the goroutine's sleep or wait count when it was
// added; an entry whose goroutine moved on is stale and skipped.
type sleepEntry struct {
	when  int64
	index uint32
	gen   uint32
	seq   uint32
	kind  uint8
}

const (
	sleepWake uint8 = iota
	sleepWaitTimeout
)

// sleepHeap orders entries by when. Each goroutine has at most one entry of
// each kind live, so the table's size bounds it.
type sleepHeap struct {
	entries [2 * MaxGoroutines]sleepEntry
	count   int
}

func (h *sleepHeap) push(entry sleepEntry) bool {
	if h.count == len(h.entries) {
		return false
	}
	position := h.count
	h.count++
	for position > 0 {
		parent := (position - 1) / 2
		if h.entries[parent].when <= entry.when {
			break
		}
		h.entries[position] = h.entries[parent]
		position = parent
	}
	h.entries[position] = entry
	return true
}

// remove deletes the matching wait deadline. Reinsert the tail in place so
// cancellation releases its slot before this goroutine starts another wait.
func (h *sleepHeap) remove(index, gen, seq uint32) bool {
	for i := 0; i < h.count; i++ {
		e := h.entries[i]
		if e.kind != sleepWaitTimeout || e.index != index || e.gen != gen || e.seq != seq {
			continue
		}
		h.count--
		copy(h.entries[i:h.count], h.entries[i+1:h.count+1])
		// Rebuild from the retained entries without a second array.
		n := h.count
		h.count = 0
		for j := 0; j < n; j++ {
			h.push(h.entries[j])
		}
		return true
	}
	return false
}

// first returns the earliest entry's time.
func (h *sleepHeap) first() (int64, bool) {
	if h.count == 0 {
		return 0, false
	}
	return h.entries[0].when, true
}

// pop removes the earliest entry.
func (h *sleepHeap) pop() sleepEntry {
	top := h.entries[0]
	h.count--
	last := h.entries[h.count]
	position := 0
	for {
		child := 2*position + 1
		if child >= h.count {
			break
		}
		if child+1 < h.count && h.entries[child+1].when < h.entries[child].when {
			child++
		}
		if last.when <= h.entries[child].when {
			break
		}
		h.entries[position] = h.entries[child]
		position = child
	}
	if h.count > 0 {
		h.entries[position] = last
	}
	return top
}
