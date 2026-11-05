package network

// Reassembly keeps out-of-order bytes in the receive ring itself, at the
// offset their sequence number gives past the contiguous bytes. tcpReorder
// records which ranges hold bytes. The window bounds the ranges, so the
// receive buffer, not a slot count, bounds what a connection holds out of
// order. Each span costs eight bytes; tcpReorderSpans caps how many one
// ring may track, so a peer that sends one byte per gap cannot grow the
// list without bound.
type tcpReorder struct {
	// spans are sorted, disjoint and not adjacent, all past rcvNxt.
	spans []tcpSpan
}

// tcpSpan is a held range of sequence numbers, start included.
type tcpSpan struct{ start, end uint32 }

// tcpReorderSpans is the most spans a ring of size bytes tracks: one per
// 512 bytes, at least 16.
func tcpReorderSpans(size int) int { return max(16, size/512) }

// heldExtent is how far past rcvNxt the held bytes reach.
func (entry *tcpEndpoint) heldExtent() int {
	if entry.reorder == nil || len(entry.reorder.spans) == 0 {
		return 0
	}
	return int(entry.reorder.spans[len(entry.reorder.spans)-1].end - entry.rcvNxt)
}

// heldBytes counts the bytes the spans hold.
func (entry *tcpEndpoint) heldBytes() int {
	held := 0
	if entry.reorder != nil {
		for _, span := range entry.reorder.spans {
			held += int(span.end - span.start)
		}
	}
	return held
}

// storeTCP puts payload, which starts at sequence, at or after rcvNxt and
// inside the window, into the receive ring. Bytes at rcvNxt extend the
// stream and pull in every held span they reach; later bytes join the
// spans. It reports false for bytes it could not track.
func (stack *Stack) storeTCP(entry *tcpEndpoint, sequence uint32, payload []byte) bool {
	if len(payload) == 0 {
		return true
	}
	offset := int(sequence - entry.rcvNxt)
	if offset+len(payload) > len(entry.recvRing)-int(entry.recvLen) {
		return false
	}
	if offset != 0 && !entry.holdSpan(sequence, sequence+uint32(len(payload))) {
		return false
	}
	ringStore(entry.recvRing, int(entry.recvHead), int(entry.recvLen)+offset, payload)
	if offset == 0 {
		entry.advanceReceive(uint32(len(payload)))
	}
	return true
}

// advanceReceive moves rcvNxt over count new contiguous bytes and over the
// spans that now touch it.
func (entry *tcpEndpoint) advanceReceive(count uint32) {
	entry.recvLen += count
	entry.rcvNxt += count
	if entry.reorder == nil {
		return
	}
	spans := entry.reorder.spans
	taken := 0
	for taken < len(spans) && seqLE(spans[taken].start, entry.rcvNxt) {
		if seqGT(spans[taken].end, entry.rcvNxt) {
			grown := spans[taken].end - entry.rcvNxt
			entry.recvLen += grown
			entry.rcvNxt += grown
		}
		taken++
	}
	entry.reorder.spans = append(spans[:0], spans[taken:]...)
}

// holdSpan merges [start, end) into the spans. It refuses a span that
// would exceed tcpReorderSpans without touching one already held.
func (entry *tcpEndpoint) holdSpan(start, end uint32) bool {
	if entry.reorder == nil {
		entry.reorder = &tcpReorder{}
	}
	spans := entry.reorder.spans
	base := entry.rcvNxt
	low, high := start-base, end-base
	first := 0
	for first < len(spans) && spans[first].end-base < low {
		first++
	}
	last := first
	for last < len(spans) && spans[last].start-base <= high {
		low, high = min(low, spans[last].start-base), max(high, spans[last].end-base)
		last++
	}
	merged := tcpSpan{start: base + low, end: base + high}
	if first == last {
		if len(spans) >= tcpReorderSpans(len(entry.recvRing)) {
			return false
		}
		spans = append(spans, tcpSpan{})
		copy(spans[first+1:], spans[first:])
		spans[first] = merged
	} else {
		spans[first] = merged
		spans = append(spans[:first+1], spans[last:]...)
	}
	entry.reorder.spans = spans
	return true
}

// clipHeld drops held bytes at or after at, the peer's FIN.
func (entry *tcpEndpoint) clipHeld(at uint32) {
	if entry.reorder == nil {
		return
	}
	spans := entry.reorder.spans
	kept := 0
	for _, span := range spans {
		if seqGE(span.start, at) {
			continue
		}
		if seqGT(span.end, at) {
			span.end = at
		}
		spans[kept] = span
		kept++
	}
	entry.reorder.spans = spans[:kept]
}
