package network

// A tombstone keeps what a TIME_WAIT connection protects once its endpoint
// is gone: the four-tuple, which no new connection may use until deadline,
// and the sequence numbers that acknowledge a retransmitted FIN, as TIME_WAIT
// does. A connection its owner has closed turns into one as it enters
// TIME_WAIT, so TIME_WAIT costs a tombstone, not an endpoint of the quota.
//
// Tombstones have their own budget, global and per owner. With no room, the
// connection stays an endpoint in TIME_WAIT and keeps counting against its
// owner's quota, so the owner's next open fails with Busy; the stack never
// drops a four-tuple's protection to make room.
type tcpTombstone struct {
	used       bool
	owner      uint8
	reuse      bool
	local      IPv4
	port       uint16
	remote     IPv4
	remotePort uint16
	sndNxt     uint32
	rcvNxt     uint32
	deadline   uint64
	// links chain the tombstone into the tuple and port tables, as
	// indexes plus one; hashes are the hashes it was filed under.
	links  [2]tcpTombLink
	hashes [2]uint32
}

type tcpTombLink struct{ prev, next int32 }

const (
	tombTuple = iota
	tombPort
)

// tcpTombstones holds the tombstones in a table that grows with the
// TIME_WAIT load, a free list, two hash tables and the expiry heap.
type tcpTombstones struct {
	slots   []tcpTombstone
	free    []int32
	buckets [2][]int32
	count   int
	owned   [tcpOwners]int
	timers  tcpTimers
}

// tcpRetiredOwner holds the TIME_WAIT four-tuples of owners whose session
// ended. No session has its number, so none is charged for them.
const tcpRetiredOwner = 255

func (tombs *tcpTombstones) bucketOf(kind int, hash uint32) *int32 {
	table := tombs.buckets[kind]
	return &table[hash&uint32(len(table)-1)]
}

func (tombs *tcpTombstones) link(index int32, kind int, hash uint32) {
	tomb := &tombs.slots[index]
	head := tombs.bucketOf(kind, hash)
	tomb.hashes[kind] = hash
	tomb.links[kind] = tcpTombLink{next: *head}
	if *head != 0 {
		tombs.slots[*head-1].links[kind].prev = index + 1
	}
	*head = index + 1
}

func (tombs *tcpTombstones) unlink(index int32, kind int) {
	tomb := &tombs.slots[index]
	link := tomb.links[kind]
	if link.prev != 0 {
		tombs.slots[link.prev-1].links[kind].next = link.next
	} else {
		*tombs.bucketOf(kind, tomb.hashes[kind]) = link.next
	}
	if link.next != 0 {
		tombs.slots[link.next-1].links[kind].prev = link.prev
	}
	tomb.links[kind] = tcpTombLink{}
}

// grow doubles both hash tables before one more tombstone would put more
// than two in a bucket.
func (tombs *tcpTombstones) grow() {
	size := 16
	if len(tombs.buckets[0]) != 0 {
		if tombs.count+1 <= 2*len(tombs.buckets[0]) || len(tombs.buckets[0]) >= tcpMaxBuckets {
			return
		}
		size = 2 * len(tombs.buckets[0])
	}
	tombs.buckets[tombTuple], tombs.buckets[tombPort] = make([]int32, size), make([]int32, size)
	for index := range tombs.slots {
		if tombs.slots[index].used {
			tombs.link(int32(index), tombTuple, tombs.slots[index].hashes[tombTuple])
			tombs.link(int32(index), tombPort, tombs.slots[index].hashes[tombPort])
		}
	}
}

func (tombs *tcpTombstones) remove(index int32) {
	tomb := &tombs.slots[index]
	tombs.unlink(index, tombTuple)
	tombs.unlink(index, tombPort)
	tombs.owned[tomb.owner]--
	tombs.count--
	tombs.timers.set(index, 0)
	*tomb = tcpTombstone{}
	tombs.free = append(tombs.free, index)
}

// portConflict applies the bind rule to the tombstones on port.
func (tombs *tcpTombstones) portConflict(address IPv4, port uint16, reuse bool, hash uint32) bool {
	if tombs.count == 0 {
		return false
	}
	for index := *tombs.bucketOf(tombPort, hash); index != 0; index = tombs.slots[index-1].links[tombPort].next {
		tomb := &tombs.slots[index-1]
		if tomb.port == port && addressesOverlap(address, tomb.local) && (!reuse || !tomb.reuse) {
			return true
		}
	}
	return false
}

func (stack *Stack) tcpTimeWaitRoom(owner uint8) bool {
	tombs := &stack.tcpTombstones
	if tombs.count >= budgetOr(stack.tcpBudget.timeWait, DefaultTCPTimeWaitBudget) {
		return false
	}
	return owner == tcpRetiredOwner || tombs.owned[owner] < budgetOr(stack.tcpBudget.ownerTimeWait, DefaultTCPOwnerTimeWaitBudget)
}

// tombstoneTCP moves a TIME_WAIT endpoint's four-tuple into a tombstone
// charged to owner and releases the endpoint. False means the budget had no
// room and the endpoint stays.
func (stack *Stack) tombstoneTCP(entry *tcpEndpoint, owner uint8) bool {
	if entry.slot == 0 || entry.state != TCPTimeWait || !stack.tcpTimeWaitRoom(owner) {
		return false
	}
	tombs := &stack.tcpTombstones
	tombs.grow()
	var index int32
	if count := len(tombs.free); count != 0 {
		index = tombs.free[count-1]
		tombs.free = tombs.free[:count-1]
	} else {
		index = int32(len(tombs.slots))
		tombs.slots = append(tombs.slots, tcpTombstone{})
	}
	tombs.slots[index] = tcpTombstone{used: true, owner: owner, reuse: entry.reuse, local: entry.local, port: entry.port,
		remote: entry.remote, remotePort: entry.remotePort, sndNxt: entry.sndNxt, rcvNxt: entry.rcvNxt, deadline: entry.deadline}
	tombs.count++
	tombs.owned[owner]++
	tombs.link(index, tombTuple, stack.tupleHash(entry.local, entry.port, entry.remote, entry.remotePort))
	tombs.link(index, tombPort, stack.portHash(entry.port))
	tombs.timers.set(index, max(entry.deadline, 1))
	stack.releaseTCP(entry)
	return true
}

// settleTCP turns a closed connection that reached TIME_WAIT into a
// tombstone. The segment paths call it once a segment is done with the
// endpoint, since the handlers still send from it.
func (stack *Stack) settleTCP(entry *tcpEndpoint) {
	if entry.slot == 0 || entry.state != TCPTimeWait || !entry.orphan {
		return
	}
	if !stack.tombstoneTCP(entry, entry.owner) && !entry.timeWaitHeld {
		entry.timeWaitHeld = true
		increment(&stack.tcpStats.TimeWaitHeld)
	}
}

// recycleTimeWait gives an owner at its quota the slot of its closed
// TIME_WAIT connection that expires first, when the TIME_WAIT budget has
// room again for its four-tuple. It walks only the owner's endpoints.
func (stack *Stack) recycleTimeWait(owner int) bool {
	var oldest *tcpEndpoint
	for entry := stack.tcpOwnerLists[owner].head; entry != nil; entry = entry.links[tcpLinkOwner].next {
		if entry.state == TCPTimeWait && entry.orphan && (oldest == nil || entry.deadline < oldest.deadline) {
			oldest = entry
		}
	}
	return oldest != nil && stack.tombstoneTCP(oldest, uint8(owner))
}

// tcpTombstone is the index of the tombstone protecting a four-tuple, or -1.
func (stack *Stack) tcpTombstone(local IPv4, localPort uint16, remote IPv4, remotePort uint16) int32 {
	var probes uint64
	return stack.lookupTombstone(local, localPort, remote, remotePort, &probes)
}

func (stack *Stack) lookupTombstone(local IPv4, localPort uint16, remote IPv4, remotePort uint16, probes *uint64) int32 {
	tombs := &stack.tcpTombstones
	if tombs.count == 0 {
		return -1
	}
	hash := stack.tupleHash(local, localPort, remote, remotePort)
	for index := *tombs.bucketOf(tombTuple, hash); index != 0; index = tombs.slots[index-1].links[tombTuple].next {
		*probes++
		tomb := &tombs.slots[index-1]
		if tomb.local == local && tomb.port == localPort && tomb.remote == remote && tomb.remotePort == remotePort {
			return index - 1
		}
	}
	return -1
}

// receiveTombstone handles a segment for a TIME_WAIT four-tuple without an
// endpoint as TIME_WAIT would: a retransmitted FIN is acknowledged again and
// restarts the interval, and everything else, a new SYN included, is
// dropped.
func (stack *Stack) receiveTombstone(index int32, segment tcpSegment, now uint64) {
	tomb := &stack.tcpTombstones.slots[index]
	if segment.flags&tcpFlagRST != 0 || segment.flags&tcpFlagFIN == 0 ||
		segment.sequence+uint32(len(segment.payload))+1 != tomb.rcvNxt {
		return
	}
	entry := tcpEndpoint{local: tomb.local, port: tomb.port, remote: tomb.remote, remotePort: tomb.remotePort, localMSS: tcpDefaultMSS}
	sndNxt, rcvNxt := tomb.sndNxt, tomb.rcvNxt
	tomb.deadline = addTime(now, stack.tuning.TCP.TimeWait)
	stack.tcpTombstones.timers.set(index, tomb.deadline)
	stack.sendTCP(&entry, tcpFlagACK, sndNxt, rcvNxt, nil, now)
}

func (stack *Stack) advanceTombstones(now uint64) {
	tombs := &stack.tcpTombstones
	for {
		index, at, ok := tombs.timers.earliest()
		if !ok || at > now {
			return
		}
		tombs.remove(index)
	}
}

func (stack *Stack) tombstoneDeadline(earliest *timer) {
	if _, at, ok := stack.tcpTombstones.timers.earliest(); ok {
		earliest.consider(at)
	}
}

// dropTombstones forgets the tombstones of local addresses the interface no
// longer has, as reconfiguration ends their connections.
func (stack *Stack) dropTombstones() {
	tombs := &stack.tcpTombstones
	for index := range tombs.slots {
		if tombs.slots[index].used && !tombs.slots[index].local.IsLoopback() {
			tombs.remove(int32(index))
		}
	}
}

// retireTombstones hands an ended owner's tombstones to tcpRetiredOwner.
func (stack *Stack) retireTombstones(owner int) {
	tombs := &stack.tcpTombstones
	if tombs.owned[owner] == 0 {
		return
	}
	for index := range tombs.slots {
		if tomb := &tombs.slots[index]; tomb.used && int(tomb.owner) == owner {
			tomb.owner = tcpRetiredOwner
			tombs.owned[owner]--
			tombs.owned[tcpRetiredOwner]++
		}
	}
}

// retireTimeWait moves the TIME_WAIT endpoint of an ended owner into a
// retired tombstone; with the budget full it stays an endpoint, under
// tcpRetiredOwner and without its rings, until its interval ends.
func (stack *Stack) retireTimeWait(entry *tcpEndpoint) {
	entry.orphan = true
	if stack.tombstoneTCP(entry, tcpRetiredOwner) {
		return
	}
	stack.freeBuffers(entry)
	stack.setOwner(entry, tcpRetiredOwner)
	if !entry.timeWaitHeld {
		entry.timeWaitHeld = true
		increment(&stack.tcpStats.TimeWaitHeld)
	}
}
