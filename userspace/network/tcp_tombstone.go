package network

const (
	// tcpTombstonesPerOwner bounds the TIME_WAIT four-tuples one owner may
	// have moved out of its endpoint slots. With one share per session the
	// table can never fill, so no owner can use up another's share.
	tcpTombstonesPerOwner = MaxTCPEndpointsPerSession
	tcpTombstoneSlots     = MaxTCPSessions * tcpTombstonesPerOwner
)

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
	if free < 0 || free >= len(stack.tcpTombstones) || held >= tcpTombstonesPerOwner {
		return -1
	}
	entry := &stack.tcp[oldest]
	stack.tcpTombstones[free] = tcpTombstone{used: true, owner: entry.owner, local: entry.local, port: entry.port,
		remote: entry.remote, remotePort: entry.remotePort, sndNxt: entry.sndNxt, rcvNxt: entry.rcvNxt,
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
