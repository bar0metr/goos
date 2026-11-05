package network

// The send and receive streams live in rings the stack allocates per
// connection: the receive ring when the handshake starts, the send ring on
// the first write. sendHead indexes the byte at sendSeq and recvHead the next
// byte a read returns. An ACK or a read moves the head; stored bytes stay
// where TCPWrite or the receive path put them. A head returns to zero when
// its ring empties, so a stream that drains between writes keeps its
// segments contiguous.
//
// A ring starts at tcpBufferFloor bytes, or the tuned size when that is
// smaller, and doubles while its stream fills more than half of it, up to
// the tuned size. Growth copies the ring once and never shrinks the window
// already advertised. The budgets charge each ring's full length to the
// stack and the owner, so the memory a connection holds is the memory it
// pays for.

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
func ringAdvance(head, count, size int) uint32 {
	head += count
	if head >= size {
		head -= size
	}
	return uint32(head)
}

// growRing returns a ring of size bytes holding ring's bytes rotated so the
// byte at head comes first. Offsets from the head stay where they were.
func growRing(ring []byte, head, size int) []byte {
	grown := make([]byte, size)
	copied := copy(grown, ring[head:])
	copy(grown[copied:], ring[:head])
	return grown
}

// tcpMemoryLeft is the ring bytes owner may still charge.
func (stack *Stack) tcpMemoryLeft(owner uint8) int {
	total := budgetOr(stack.tcpBudget.memory, DefaultTCPMemoryBudget) - stack.tcpBytes
	mine := budgetOr(stack.tcpBudget.ownerMemory, DefaultTCPOwnerMemoryBudget) - stack.tcpOwnerBytes[owner]
	return max(min(total, mine), 0)
}

func (stack *Stack) charge(entry *tcpEndpoint, bytes int) {
	stack.tcpBytes += bytes
	stack.tcpOwnerBytes[entry.owner] += bytes
	entry.charged = uint32(int(entry.charged) + bytes)
}

func (stack *Stack) tcpSendLimit() int { return max(int(stack.tuning.TCP.SendBuffer), 1) }

// tcpSendRoom is how many bytes a write takes now: room left in the ring
// and what the ring may still grow by under the tuning and the budgets.
func (stack *Stack) tcpSendRoom(entry *tcpEndpoint) int {
	limit := stack.tcpSendLimit()
	capacity := len(entry.sendRing)
	if capacity < limit {
		capacity = min(limit, capacity+stack.tcpMemoryLeft(entry.owner))
	}
	return max(min(capacity, limit)-int(entry.sendLen), 0)
}

// growSend makes the send ring hold need bytes. The caller checked
// tcpSendRoom, so the budgets allow it.
func (stack *Stack) growSend(entry *tcpEndpoint, need int) {
	old := len(entry.sendRing)
	if need <= old {
		return
	}
	limit := stack.tcpSendLimit()
	size := min(max(need, 2*old, min(tcpBufferFloor, limit)), limit, old+stack.tcpMemoryLeft(entry.owner))
	size = max(size, need)
	entry.sendRing = growRing(entry.sendRing, int(entry.sendHead), size)
	entry.sendHead = 0
	stack.charge(entry, size-old)
}

// freeSend drops the send ring and its bytes once nothing can be sent.
func (stack *Stack) freeSend(entry *tcpEndpoint) {
	stack.charge(entry, -len(entry.sendRing))
	entry.sendRing = nil
	entry.sendHead, entry.sendLen, entry.sendSent = 0, 0, 0
}

func (stack *Stack) freeBuffers(entry *tcpEndpoint) {
	stack.freeSend(entry)
	stack.charge(entry, -len(entry.recvRing))
	entry.recvRing, entry.reorder = nil, nil
	entry.recvHead, entry.recvLen = 0, 0
}

// allocateReceive gives a connection its first receive ring:
// tcpBufferFloor bytes, or TCPLoopbackMSS to a local peer so a loopback
// segment fits whole from the start. False means the budgets have no room;
// the handshake does not start.
func (stack *Stack) allocateReceive(entry *tcpEndpoint, local bool) bool {
	if entry.recvRing != nil {
		return true
	}
	size := tcpBufferFloor
	if local {
		size = TCPLoopbackMSS
	}
	size = min(size, max(int(stack.tuning.TCP.ReceiveBuffer), 1))
	if stack.tcpMemoryLeft(entry.owner) < size {
		increment(&stack.tcpStats.DropMemory)
		return false
	}
	entry.recvRing = make([]byte, size)
	stack.charge(entry, size)
	return true
}

// growReceive doubles the receive ring while the stream fills more than
// half of it. The window only widens: bytes already promised keep their
// place relative to the head.
func (stack *Stack) growReceive(entry *tcpEndpoint) {
	size, limit := len(entry.recvRing), int(stack.tuning.TCP.ReceiveBuffer)
	if size == 0 || size >= limit || 2*(int(entry.recvLen)+entry.heldExtent()) < size {
		return
	}
	grown := min(2*size, limit, size+stack.tcpMemoryLeft(entry.owner))
	if grown <= size {
		return
	}
	entry.recvRing = growRing(entry.recvRing, int(entry.recvHead), grown)
	entry.recvHead = 0
	stack.charge(entry, grown-size)
}

// tcpReceiveLimit is the receive buffer in force: the ring, or less when
// the tuning was lowered after the ring grew.
func (stack *Stack) tcpReceiveLimit(entry *tcpEndpoint) int {
	return min(int(stack.tuning.TCP.ReceiveBuffer), len(entry.recvRing))
}

// sendSpan returns the send-ring bytes from offset bytes after sendSeq.
func (entry *tcpEndpoint) sendSpan(offset, length int) ([]byte, []byte) {
	return ringSpan(entry.sendRing, int(entry.sendHead), offset, length)
}

// sendTCPData sends the send-ring bytes from offset bytes after sendSeq. A
// segment that wraps past the end of the ring leaves in two parts, and putTCP
// joins them in the outgoing packet buffer. A local peer takes the segment
// without a packet, so its senders stop each segment at the end of the ring
// (sendContiguous) and a wrapped one never reaches it.
func (stack *Stack) sendTCPData(entry *tcpEndpoint, flags uint8, sequence uint32, offset, length int, now uint64) bool {
	if offset+length > len(entry.sendRing) {
		return false
	}
	first, second := entry.sendSpan(offset, length)
	if len(second) != 0 {
		increment(&stack.tcpStats.SplitSegments)
	}
	return stack.sendTCPParts(entry, flags, sequence, entry.rcvNxt, first, second, now)
}

// sendContiguous is how many stored bytes from offset bytes after sendSeq
// lie before the end of the send ring.
func (entry *tcpEndpoint) sendContiguous(offset int) int {
	start := int(entry.sendHead) + offset
	if start >= len(entry.sendRing) {
		start -= len(entry.sendRing)
	}
	return len(entry.sendRing) - start
}

// tcpLocal reports whether an endpoint's peer is in this stack.
func (stack *Stack) tcpLocal(entry *tcpEndpoint) bool {
	route, result := stack.udpRoute(entry.local, entry.remote)
	return result == ResultOK && route.path == udpLocal
}
