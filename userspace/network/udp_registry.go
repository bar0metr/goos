package network

import (
	"goos/internal/validation"
	"math/bits"
)

// udpCrossCheck makes a validation build compare each delivery lookup with
// a walk of the registry. A disagreement panics.
const udpCrossCheck = validation.Enabled

// The UDP registry maps slots to endpoints the way the TCP registry does
// (tcp_registry.go): it grows to the highest slot in use, up to
// MaxUDPEndpoints, a bitmap marks the slots in use, and a slot's endpoint
// stays allocated after Close so reopening it allocates nothing. Endpoints
// from BOUND on are filed in udpPorts by local port, so a received
// datagram, a bind check and an ephemeral draw read one bucket instead of
// every endpoint.

// udpPortIndex is a chained hash table of bound endpoints keyed by port.
// The table doubles once it holds as many endpoints as buckets.
type udpPortIndex struct {
	buckets []*udpEndpoint
	count   int
}

func (index *udpPortIndex) insert(entry *udpEndpoint, hash uint32) {
	if entry.indexed {
		return
	}
	if len(index.buckets) == 0 {
		index.buckets = make([]*udpEndpoint, 16)
	} else if index.count >= len(index.buckets) && len(index.buckets) < MaxUDPEndpoints {
		index.grow()
	}
	entry.hash, entry.indexed = hash, true
	index.push(entry)
	index.count++
}

func (index *udpPortIndex) push(entry *udpEndpoint) {
	bucket := &index.buckets[entry.hash&uint32(len(index.buckets)-1)]
	entry.portPrev, entry.portNext = nil, *bucket
	if *bucket != nil {
		(*bucket).portPrev = entry
	}
	*bucket = entry
}

func (index *udpPortIndex) remove(entry *udpEndpoint) {
	if !entry.indexed {
		return
	}
	if entry.portPrev != nil {
		entry.portPrev.portNext = entry.portNext
	} else {
		index.buckets[entry.hash&uint32(len(index.buckets)-1)] = entry.portNext
	}
	if entry.portNext != nil {
		entry.portNext.portPrev = entry.portPrev
	}
	entry.portPrev, entry.portNext, entry.indexed = nil, nil, false
	index.count--
}

// bucket is the first endpoint filed under hash's bucket.
func (index *udpPortIndex) bucket(hash uint32) *udpEndpoint {
	if len(index.buckets) == 0 {
		return nil
	}
	return index.buckets[hash&uint32(len(index.buckets)-1)]
}

func (index *udpPortIndex) grow() {
	old := index.buckets
	index.buckets = make([]*udpEndpoint, 2*len(old))
	for _, head := range old {
		for entry := head; entry != nil; {
			next := entry.portNext
			index.push(entry)
			entry = next
		}
	}
}

// udpPortHash mixes in a per-stack seed, so a peer cannot aim the ports it
// sends to at one bucket.
func (stack *Stack) udpPortHash(port uint16) uint32 {
	return uint32(mix64(stack.udpSeed ^ uint64(port)<<40))
}

// allocateUDP files a new endpoint in the lowest free slot, or returns -1
// when the registry is at MaxUDPEndpoints.
func (stack *Stack) allocateUDP() int {
	for word, marks := range stack.udpUsed {
		if ^marks != 0 {
			if index := word*64 + bits.TrailingZeros64(^marks); index < len(stack.udp) {
				return index
			}
			break
		}
	}
	if len(stack.udp) >= MaxUDPEndpoints {
		return -1
	}
	slot := len(stack.udp)
	stack.udp = append(stack.udp, &udpEndpoint{})
	if slot/64 >= len(stack.udpUsed) {
		stack.udpUsed = append(stack.udpUsed, 0)
	}
	return slot
}

// nextUDP returns the first slot at or after from that is in use, or -1.
func (stack *Stack) nextUDP(from int) int {
	for word := from / 64; word < len(stack.udpUsed); word++ {
		marks := stack.udpUsed[word]
		if word == from/64 {
			marks &^= 1<<(from%64) - 1
		}
		if marks != 0 {
			if index := word*64 + bits.TrailingZeros64(marks); index < len(stack.udp) {
				return index
			}
			return -1
		}
	}
	return -1
}

// udpBucket is the first endpoint that may be bound to port.
func (stack *Stack) udpBucket(port uint16) *udpEndpoint {
	return stack.udpPorts.bucket(stack.udpPortHash(port))
}

// takeBuffer returns a free receive buffer, allocating one while the pool
// is below UDPReceiveBuffers, or -1.
func (stack *Stack) takeBuffer() int {
	if count := len(stack.udpFree); count != 0 {
		index := stack.udpFree[count-1]
		stack.udpFree = stack.udpFree[:count-1]
		return int(index)
	}
	if len(stack.udpBuffers) >= UDPReceiveBuffers {
		return -1
	}
	stack.udpBuffers = append(stack.udpBuffers, &udpBuffer{})
	return len(stack.udpBuffers) - 1
}

// releaseBuffer returns a buffer to the pool and the owner's share.
func (stack *Stack) releaseBuffer(owner uint8, index uint16) {
	stack.udpBuffers[index].used = false
	stack.udpFree = append(stack.udpFree, index)
	stack.udpOwnerBuffers[owner]--
}
