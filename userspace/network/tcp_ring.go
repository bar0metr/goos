package network

// The send and receive streams live in fixed rings. sendHead indexes the byte
// at sendSeq and recvHead the next byte a read returns. An ACK or a read
// moves the head; stored bytes stay where TCPWrite or the receive path put
// them. A head returns to zero when its ring empties, so a stream that drains
// between writes keeps its segments contiguous.

// ringSpan returns length stored bytes that start offset bytes after head,
// as a part up to the end of ring and the part that wraps to its start.
// head is below len(ring) and offset+length is at most len(ring).
func ringSpan(ring []byte, head, offset, length int) ([]byte, []byte) {
	start := head + offset
	if start >= len(ring) {
		start -= len(ring)
	}
	if end := start + length; end <= len(ring) {
		return ring[start:end], nil
	}
	return ring[start:], ring[:start+length-len(ring)]
}

// ringStore copies data into ring offset bytes after head.
func ringStore(ring []byte, head, offset int, data []byte) {
	first, second := ringSpan(ring, head, offset, len(data))
	copied := copy(first, data)
	copy(second, data[copied:])
}

// ringLoad copies length bytes from head into dst and returns the count.
func ringLoad(dst, ring []byte, head, length int) int {
	first, second := ringSpan(ring, head, 0, length)
	copied := copy(dst, first)
	return copied + copy(dst[copied:], second)
}

// ringAdvance moves head forward by count within a ring of size bytes.
func ringAdvance(head, count, size int) uint16 {
	head += count
	if head >= size {
		head -= size
	}
	return uint16(head)
}

// sendSpan returns the send-ring bytes from offset bytes after sendSeq.
func (entry *tcpEndpoint) sendSpan(offset, length int) ([]byte, []byte) {
	return ringSpan(entry.send[:], int(entry.sendHead), offset, length)
}

// sendTCPData sends the send-ring bytes from offset bytes after sendSeq. A
// segment that wraps past the end of the ring leaves in two parts, and putTCP
// joins them in the outgoing packet buffer.
func (stack *Stack) sendTCPData(entry *tcpEndpoint, flags uint8, sequence uint32, offset, length int, now uint64) bool {
	first, second := entry.sendSpan(offset, length)
	if len(second) != 0 {
		increment(&stack.tcpStats.SplitSegments)
	}
	return stack.sendTCPParts(entry, flags, sequence, entry.rcvNxt, first, second, now)
}
