package network

import "math/bits"

// The registry maps slots to control blocks, and the indexes below find an
// endpoint without walking the registry:
//
//   - tcpTuples: connections, SYN-SENT on, by four-tuple;
//   - tcpPorts: endpoints from BOUND on, by local port, for the bind rules;
//   - tcpListeners: listeners by local port;
//   - tcpOwnerLists: every endpoint by owner, for quotas and session end;
//   - a listener's children: the children not yet accepted, oldest first.
//
// Each index is a list threaded through the control blocks, so linking and
// unlinking cost nothing beyond the hash.
const (
	tcpLinkTuple = iota
	tcpLinkPort
	tcpLinkListen
	tcpLinkOwner
	tcpLinkChild
	tcpLinkKinds
	// tcpHashedIndexes are the kinds filed under a hash.
	tcpHashedIndexes = tcpLinkListen + 1
	// tcpMaxBuckets bounds an index's bucket table.
	tcpMaxBuckets = 1 << 16
)

type tcpLink struct{ prev, next *tcpEndpoint }

// tcpList is a doubly linked list threaded through the links of one kind.
type tcpList struct {
	head, tail *tcpEndpoint
	count      int
}

func (list *tcpList) pushBack(entry *tcpEndpoint, kind int) {
	link := &entry.links[kind]
	link.prev, link.next = list.tail, nil
	if list.tail != nil {
		list.tail.links[kind].next = entry
	} else {
		list.head = entry
	}
	list.tail = entry
	list.count++
	entry.members |= 1 << kind
}

func (list *tcpList) remove(entry *tcpEndpoint, kind int) {
	if entry.members&(1<<kind) == 0 {
		return
	}
	link := &entry.links[kind]
	if link.prev != nil {
		link.prev.links[kind].next = link.next
	} else {
		list.head = link.next
	}
	if link.next != nil {
		link.next.links[kind].prev = link.prev
	} else {
		list.tail = link.prev
	}
	*link = tcpLink{}
	list.count--
	entry.members &^= 1 << kind
}

// tcpIndex is a chained hash table of endpoints. The table doubles once it
// holds as many entries as buckets.
type tcpIndex struct {
	buckets []tcpList
	count   int
	kind    int
}

func (index *tcpIndex) insert(entry *tcpEndpoint, hash uint32) {
	if entry.members&(1<<index.kind) != 0 {
		return
	}
	if len(index.buckets) == 0 {
		index.buckets = make([]tcpList, 16)
	} else if index.count >= len(index.buckets) && len(index.buckets) < tcpMaxBuckets {
		index.grow()
	}
	entry.hashes[index.kind] = hash
	index.buckets[hash&uint32(len(index.buckets)-1)].pushBack(entry, index.kind)
	index.count++
}

func (index *tcpIndex) remove(entry *tcpEndpoint) {
	if entry.members&(1<<index.kind) == 0 {
		return
	}
	index.buckets[entry.hashes[index.kind]&uint32(len(index.buckets)-1)].remove(entry, index.kind)
	index.count--
}

// bucket is the first entry filed under hash's bucket.
func (index *tcpIndex) bucket(hash uint32) *tcpEndpoint {
	if len(index.buckets) == 0 {
		return nil
	}
	return index.buckets[hash&uint32(len(index.buckets)-1)].head
}

func (index *tcpIndex) grow() {
	old := index.buckets
	index.buckets = make([]tcpList, 2*len(old))
	mask := uint32(len(index.buckets) - 1)
	for bucket := range old {
		for entry := old[bucket].head; entry != nil; {
			next := entry.links[index.kind].next
			entry.members &^= 1 << index.kind
			index.buckets[entry.hashes[index.kind]&mask].pushBack(entry, index.kind)
			entry = next
		}
	}
}

// mix64 is the splitmix64 finalizer.
func mix64(value uint64) uint64 {
	value = (value ^ value>>30) * 0xbf58476d1ce4e5b9
	value = (value ^ value>>27) * 0x94d049bb133111eb
	return value ^ value>>31
}

func ipv4Word(address IPv4) uint64 {
	return uint64(address[0])<<24 | uint64(address[1])<<16 | uint64(address[2])<<8 | uint64(address[3])
}

// tupleHash and portHash mix in a per-stack seed, so a peer that picks its
// addresses and ports cannot aim them at one bucket.
func (stack *Stack) tupleHash(local IPv4, localPort uint16, remote IPv4, remotePort uint16) uint32 {
	hash := mix64(stack.tcpSeed ^ ipv4Word(local)<<32 ^ ipv4Word(remote))
	return uint32(mix64(hash ^ uint64(localPort)<<16 ^ uint64(remotePort)))
}

func (stack *Stack) portHash(port uint16) uint32 {
	return uint32(mix64(stack.tcpSeed ^ uint64(port)<<40))
}

// allocateTCP files a new endpoint in the lowest free slot, or returns nil
// when the registry is at MaxTCPEndpoints.
func (stack *Stack) allocateTCP(owner int) *tcpEndpoint {
	slot := -1
	for word, marks := range stack.tcpUsed {
		if ^marks != 0 {
			if index := word*64 + bits.TrailingZeros64(^marks); index < len(stack.tcp) {
				slot = index
			}
			break
		}
	}
	if slot < 0 {
		if len(stack.tcp) >= MaxTCPEndpoints {
			return nil
		}
		slot = len(stack.tcp)
		stack.tcp = append(stack.tcp, nil)
		if slot/64 >= len(stack.tcpUsed) {
			stack.tcpUsed = append(stack.tcpUsed, 0)
		}
	}
	stack.tcpGeneration++
	if stack.tcpGeneration == tcpGenerations {
		stack.tcpGeneration = 1
	}
	entry := &tcpEndpoint{state: TCPOpen, owner: uint8(owner), generation: stack.tcpGeneration, slot: int32(slot + 1),
		peerMSS: tcpDefaultMSS, ssthresh: tcpInitialSSThresh, rto: stack.tuning.TCP.RTOMin}
	stack.tcp[slot] = entry
	stack.tcpUsed[slot/64] |= 1 << (slot % 64)
	stack.tcpLive++
	stack.tcpOwnerLists[owner].pushBack(entry, tcpLinkOwner)
	return entry
}

// releaseTCP unlinks an endpoint from the registry, every index and the
// timers and frees its rings. The control block stays zeroed, so code up
// the call stack that still holds it sees TCPFree and can arm nothing.
func (stack *Stack) releaseTCP(entry *tcpEndpoint) {
	if entry.slot == 0 {
		return
	}
	for child := entry.children.head; child != nil; child = entry.children.head {
		stack.detachChild(child)
	}
	if entry.parent != nil {
		stack.detachChild(entry)
	}
	stack.tcpTuples.remove(entry)
	stack.tcpPorts.remove(entry)
	stack.tcpListeners.remove(entry)
	stack.tcpOwnerLists[entry.owner].remove(entry, tcpLinkOwner)
	stack.tcpTimers.set(entry.slot, 0)
	stack.freeBuffers(entry)
	slot := int(entry.slot - 1)
	stack.tcp[slot] = nil
	stack.tcpUsed[slot/64] &^= 1 << (slot % 64)
	stack.tcpLive--
	*entry = tcpEndpoint{generation: entry.generation}
}

// setOwner moves an endpoint to another owner's list and budget.
func (stack *Stack) setOwner(entry *tcpEndpoint, owner uint8) {
	stack.tcpOwnerLists[entry.owner].remove(entry, tcpLinkOwner)
	stack.tcpOwnerBytes[entry.owner] -= int(entry.charged)
	entry.owner = owner
	stack.tcpOwnerBytes[owner] += int(entry.charged)
	stack.tcpOwnerLists[owner].pushBack(entry, tcpLinkOwner)
}

// indexConnection files an endpoint whose four-tuple is now fixed.
func (stack *Stack) indexConnection(entry *tcpEndpoint) {
	stack.tcpTuples.insert(entry, stack.tupleHash(entry.local, entry.port, entry.remote, entry.remotePort))
	stack.tcpPorts.insert(entry, stack.portHash(entry.port))
}

func (stack *Stack) attachChild(listener, child *tcpEndpoint) {
	child.parent = listener
	listener.children.pushBack(child, tcpLinkChild)
}

func (stack *Stack) detachChild(child *tcpEndpoint) {
	if child.parent != nil {
		child.parent.children.remove(child, tcpLinkChild)
		child.parent = nil
	}
}

// lookupTuple finds the connection with a four-tuple, adding the entries it
// looked at to probes.
func (stack *Stack) lookupTuple(local IPv4, localPort uint16, remote IPv4, remotePort uint16, probes *uint64) *tcpEndpoint {
	for entry := stack.tcpTuples.bucket(stack.tupleHash(local, localPort, remote, remotePort)); entry != nil; entry = entry.links[tcpLinkTuple].next {
		*probes++
		if entry.local == local && entry.port == localPort && entry.remote == remote && entry.remotePort == remotePort {
			return entry
		}
	}
	return nil
}

// nextTCP returns the first slot at or after from that is in use, or -1.
// The paths that touch every endpoint, statistics, reconfiguration and
// neighbour resolution, walk them as
//
//	for index := stack.nextTCP(0); index >= 0; index = stack.nextTCP(index + 1)
//
// which reads a mark per 64 slots instead of every slot. No segment or
// timer path walks the registry.
func (stack *Stack) nextTCP(from int) int {
	if from < 0 {
		from = 0
	}
	for word := from / 64; word < len(stack.tcpUsed); word++ {
		marks := stack.tcpUsed[word]
		if word == from/64 {
			marks &^= 1<<(from%64) - 1
		}
		if marks != 0 {
			if index := word*64 + bits.TrailingZeros64(marks); index < len(stack.tcp) {
				return index
			}
			return -1
		}
	}
	return -1
}
