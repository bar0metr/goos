package network

const (
	// tcpTombstonesPerEndpoint is how many TIME_WAIT four-tuples an owner
	// may keep for each endpoint of its quota. A server that a client
	// closes first leaves one per connection for time-wait-ms, so a quota's
	// worth would stop it after a few hundred connections a minute.
	tcpTombstonesPerEndpoint = 4
	// tcpTombstoneSlots holds the TIME_WAIT four-tuples owners have moved
	// out of their endpoint slots. Each owner's share fits at the largest
	// quota, so the table can never fill and no owner can use up another's
	// share.
	tcpTombstoneSlots = MaxTCPSessions * MaxTCPSessionQuota * tcpTombstonesPerEndpoint
)

// tcpTombstoneShare is how many tombstones owner may hold: four per
// endpoint of its quota, and never fewer than for the ordinary quota, so a
// lowered tuning does not shrink the share an ordinary session always had.
func (stack *Stack) tcpTombstoneShare(owner int) int {
	return max(stack.tcpOwnerQuota(owner), MaxTCPEndpointsPerSession) * tcpTombstonesPerEndpoint
}

// tcpTombstone keeps what a TIME_WAIT connection protects after its endpoint
// slot went to a new endpoint: the four-tuple, which no new connection may
// use until deadline, and the sequence numbers that acknowledge a
// retransmitted FIN, as TIME_WAIT does.
type tcpTombstone struct {
	used       bool
	owner      uint8
	local      IPv4
	port       uint16
	remote     IPv4
	remotePort uint16
	reuse      bool
	sndNxt     uint32
	rcvNxt     uint32
	deadline   uint64
}

// recycleTimeWait frees the owner's closed TIME_WAIT endpoint that expires
// first for a new endpoint, moving its four-tuple into a tombstone. It
// returns the freed slot, or -1 when the owner has no such endpoint or its
// tombstone share is full; protection is never dropped to make room.
func (stack *Stack) recycleTimeWait(owner int) int {
	oldest := -1
	for index := range stack.tcp[:] {
		entry := &stack.tcp[index]
		if entry.state != TCPTimeWait || !entry.orphan || int(entry.owner) != owner {
			continue
		}
		if oldest < 0 || entry.deadline < stack.tcp[oldest].deadline {
			oldest = index
		}
	}
	if oldest < 0 || oldest >= len(stack.tcp) {
		return -1
	}
	free, held := -1, 0
	for index := range stack.tcpTombstones[:] {
		tombstone := &stack.tcpTombstones[index]
		if !tombstone.used {
			if free < 0 {
				free = index
			}
		} else if int(tombstone.owner) == owner {
			held++
		}
	}
	if free < 0 || free >= len(stack.tcpTombstones) || held >= stack.tcpTombstoneShare(owner) {
		return -1
	}
	entry := &stack.tcp[oldest]
	stack.tcpTombstones[free] = tcpTombstone{used: true, owner: entry.owner, local: entry.local, port: entry.port,
		remote: entry.remote, remotePort: entry.remotePort, reuse: entry.reuse, sndNxt: entry.sndNxt, rcvNxt: entry.rcvNxt,
		deadline: entry.deadline}
	stack.releaseTCP(entry)
	return oldest
}

func (stack *Stack) tcpTombstone(local IPv4, localPort uint16, remote IPv4, remotePort uint16) *tcpTombstone {
	for index := range stack.tcpTombstones[:] {
		tombstone := &stack.tcpTombstones[index]
		if tombstone.used && tombstone.local == local && tombstone.port == localPort &&
			tombstone.remote == remote && tombstone.remotePort == remotePort {
			return tombstone
		}
	}
	return nil
}

// receiveTombstone handles a segment for a recycled TIME_WAIT four-tuple as
// TIME_WAIT would: a retransmitted FIN is acknowledged again and restarts the
// interval, and everything else, a new SYN included, is dropped.
func (stack *Stack) receiveTombstone(tombstone *tcpTombstone, segment tcpSegment, now uint64) {
	if segment.flags&tcpFlagRST != 0 || segment.flags&tcpFlagFIN == 0 ||
		segment.sequence+uint32(len(segment.payload))+1 != tombstone.rcvNxt {
		return
	}
	entry := tcpEndpoint{local: tombstone.local, port: tombstone.port, remote: tombstone.remote,
		remotePort: tombstone.remotePort, localMSS: tcpDefaultMSS}
	stack.sendTCP(&entry, tcpFlagACK, tombstone.sndNxt, tombstone.rcvNxt, nil, now)
	tombstone.deadline = addTime(now, stack.tuning.TCP.TimeWait)
}

func (stack *Stack) advanceTombstones(now uint64) {
	for index := range stack.tcpTombstones[:] {
		if stack.tcpTombstones[index].used && stack.tcpTombstones[index].deadline <= now {
			stack.tcpTombstones[index] = tcpTombstone{}
		}
	}
}

func (stack *Stack) tombstoneDeadline(earliest *timer) {
	for index := range stack.tcpTombstones[:] {
		if stack.tcpTombstones[index].used {
			earliest.consider(stack.tcpTombstones[index].deadline)
		}
	}
}

// dropTombstones forgets the tombstones of local addresses the interface no
// longer has, as reconfiguration ends their connections.
func (stack *Stack) dropTombstones() {
	for index := range stack.tcpTombstones[:] {
		tombstone := &stack.tcpTombstones[index]
		if tombstone.used && !tombstone.local.IsLoopback() {
			*tombstone = tcpTombstone{}
		}
	}
}

// tcpRetiredOwner holds the TIME_WAIT four-tuples of owners whose session
// ended. No session has its number, so none is charged for them.
const tcpRetiredOwner = 255

// retireTombstones hands an ended owner's tombstones to tcpRetiredOwner.
func (stack *Stack) retireTombstones(owner int) {
	for index := range stack.tcpTombstones[:] {
		if tombstone := &stack.tcpTombstones[index]; tombstone.used && int(tombstone.owner) == owner {
			tombstone.owner = tcpRetiredOwner
		}
	}
}

// retireTimeWait moves the TIME_WAIT endpoint of an ended owner into a
// retired tombstone, freeing its slot; with no tombstone free it stays an
// endpoint, under tcpRetiredOwner, until its interval ends.
func (stack *Stack) retireTimeWait(entry *tcpEndpoint) {
	for index := range stack.tcpTombstones[:] {
		tombstone := &stack.tcpTombstones[index]
		if tombstone.used {
			continue
		}
		*tombstone = tcpTombstone{used: true, owner: tcpRetiredOwner, local: entry.local, port: entry.port,
			remote: entry.remote, remotePort: entry.remotePort, reuse: entry.reuse, sndNxt: entry.sndNxt, rcvNxt: entry.rcvNxt,
			deadline: entry.deadline}
		stack.releaseTCP(entry)
		return
	}
	entry.owner = tcpRetiredOwner
}
