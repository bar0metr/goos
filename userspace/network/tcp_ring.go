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
// pays for. A connection also pays for a send floor from its handshake on,
// before its send ring exists, so the minimum it needs to make progress is
// never left to the budgets after admission.

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
	total := stack.tcpMemoryBudget() - stack.tcpBytes
	mine := stack.tcpOwnerMemoryBudget() - stack.tcpOwnerBytes[owner]
	return max(min(total, mine), 0)
}

// tcpMemoryBudget and tcpOwnerMemoryBudget are the budgets in force:
// SetTCPMemoryBudget's override, else the tuning, else the default.
func (stack *Stack) tcpMemoryBudget() int {
	return budgetOr(stack.tcpBudget.memory, budgetOr(int(stack.tuning.TCP.MemoryBudget), DefaultTCPMemoryBudget))
}

func (stack *Stack) tcpOwnerMemoryBudget() int {
	return budgetOr(stack.tcpBudget.ownerMemory, budgetOr(int(stack.tuning.TCP.OwnerMemoryBudget), DefaultTCPOwnerMemoryBudget))
}

func (stack *Stack) charge(entry *tcpEndpoint, bytes int) {
	stack.tcpBytes += bytes
	stack.tcpOwnerBytes[entry.owner] += bytes
	entry.charged = uint32(int(entry.charged) + bytes)
}

func (stack *Stack) tcpSendLimit() int { return max(int(stack.tuning.TCP.SendBuffer), 1) }

// tcpSendFloor is the send ring every admitted connection holds a charge
// for, whether it has written or not.
func (stack *Stack) tcpSendFloor() int { return min(tcpBufferFloor, stack.tcpSendLimit()) }

// sendCharge is what the send side costs the budgets: the ring, or the
// reservation while the ring is smaller.
func (entry *tcpEndpoint) sendCharge() int { return max(len(entry.sendRing), int(entry.sendReserve)) }

// tcpSendRoom is how many bytes a write takes now: room left in the ring
// and what the ring may still grow by under the tuning and the budgets. The
// reservation is paid for, so a connection whose ring is empty always has
// room and its writability never waits for another connection's memory.
func (stack *Stack) tcpSendRoom(entry *tcpEndpoint) int {
	limit := stack.tcpSendLimit()
	capacity := len(entry.sendRing)
	if capacity < limit {
		capacity = min(limit, entry.sendCharge()+stack.tcpMemoryLeft(entry.owner))
	}
	return max(min(capacity, limit)-int(entry.sendLen), 0)
}

// growSend makes the send ring hold need bytes. The caller checked
// tcpSendRoom, so the budgets allow it. Growth the budgets cut below the
// doubling it wanted counts in MemoryCapped.
func (stack *Stack) growSend(entry *tcpEndpoint, need int) {
	old := len(entry.sendRing)
	if need <= old {
		return
	}
	limit := stack.tcpSendLimit()
	paid := entry.sendCharge()
	wanted := min(max(need, 2*old, min(tcpBufferFloor, limit)), limit)
	size := max(min(wanted, paid+stack.tcpMemoryLeft(entry.owner)), need)
	if size < wanted {
		increment(&stack.tcpStats.MemoryCapped)
	}
	entry.sendRing = growRing(entry.sendRing, int(entry.sendHead), size)
	entry.sendHead = 0
	stack.charge(entry, entry.sendCharge()-paid)
}

// trimSend gives back a drained send ring larger than the reservation once
// the budgets could not pay for another full one. The next write grows a
// ring again from the reservation.
func (stack *Stack) trimSend(entry *tcpEndpoint) {
	if entry.sendLen != 0 || len(entry.sendRing) <= int(entry.sendReserve) ||
		stack.tcpMemoryLeft(entry.owner) >= stack.tcpSendLimit() {
		return
	}
	paid := entry.sendCharge()
	entry.sendRing, entry.sendHead, entry.sendSent = nil, 0, 0
	stack.charge(entry, entry.sendCharge()-paid)
}

// freeSend drops the send ring, its reservation and their bytes once
// nothing can be sent.
func (stack *Stack) freeSend(entry *tcpEndpoint) {
	stack.charge(entry, -entry.sendCharge())
	entry.sendRing, entry.sendReserve = nil, 0
	entry.sendHead, entry.sendLen, entry.sendSent = 0, 0, 0
}

func (stack *Stack) freeBuffers(entry *tcpEndpoint) {
	stack.freeSend(entry)
	stack.charge(entry, -len(entry.recvRing))
	entry.recvRing, entry.reorder = nil, nil
	entry.recvHead, entry.recvLen = 0, 0
}

// allocateReceive admits a connection to the budgets: it gives it its
// first receive ring and reserves its send floor, both charged now so later
// growth elsewhere can never take them. The receive ring is tcpBufferFloor
// bytes, or TCPLoopbackMSS to a local peer so a loopback segment fits whole
// from the start. A local peer gets the floor too once the larger ring
// would leave less than half of the smaller budget, so a crowd of loopback
// connections keeps room for later handshakes and for growth.
// False means the budgets cannot pay even the minimum; DropMemory counts it
// and the handshake does not start.
func (stack *Stack) allocateReceive(entry *tcpEndpoint, local bool) bool {
	if entry.recvRing != nil {
		return true
	}
	limit := max(int(stack.tuning.TCP.ReceiveBuffer), 1)
	floor := min(tcpBufferFloor, limit)
	reserve := stack.tcpSendFloor()
	left := stack.tcpMemoryLeft(entry.owner)
	size := floor
	if local {
		half := min(stack.tcpMemoryBudget(), stack.tcpOwnerMemoryBudget()) / 2
		if larger := min(TCPLoopbackMSS, limit); left-larger-reserve >= half {
			size = larger
		}
	}
	if left < size+reserve {
		increment(&stack.tcpStats.DropMemory)
		return false
	}
	entry.recvRing = make([]byte, size)
	entry.sendReserve = uint16(reserve)
	stack.charge(entry, size+reserve)
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
	if grown < min(2*size, limit) {
		increment(&stack.tcpStats.MemoryCapped)
	}
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
