package network

// TCP allocates its state as connections need it: a small control block per
// endpoint, a send ring on the first write, a receive ring when the handshake
// starts and reassembly spans when a segment arrives out of order. The
// constants below are ceilings and defaults, not table sizes; the tuning sets
// the limits in force, and the memory budgets bound the rings.
const (
	// MaxTCPSessions bounds the owners, one per client session.
	MaxTCPSessions = 64
	// MaxTCPEndpoints is the ceiling of max-endpoints and
	// DefaultTCPEndpoints its default. Both ends of a loopback connection
	// count.
	MaxTCPEndpoints     = 4096
	DefaultTCPEndpoints = 1024
	// MaxTCPEndpointsPerSession is the ordinary session quota, the
	// ceiling of max-endpoints-per-session. MaxTCPSessionQuota is the
	// largest quota a session can be granted explicitly; per-session tables
	// in tcpserve size to it.
	MaxTCPEndpointsPerSession = 8
	MaxTCPSessionQuota        = 512
	MaxTCPListeners           = 256
	// MaxTCPBacklog is the ceiling of listen-backlog and DefaultTCPBacklog
	// its default. A listen asking for more gets the tuned value.
	MaxTCPBacklog     = 4096
	DefaultTCPBacklog = 128
	// MaxTCPBuffer is the ceiling of the send-buffer-bytes and
	// recv-buffer-bytes settings. A ring starts at tcpBufferFloor bytes and
	// doubles as its stream needs room, up to the tuned size and while the
	// memory budgets allow.
	MaxTCPBuffer            = 4 << 20
	DefaultTCPSendBuffer    = 256 << 10
	DefaultTCPReceiveBuffer = 256 << 10
	tcpBufferFloor          = 4 << 10
	// TCPLoopbackMSS is the segment size between two endpoints of this
	// stack. Such a segment never touches a frame.
	TCPLoopbackMSS = 16 << 10
	// DefaultTCPMemoryBudget bounds the ring bytes of all connections and
	// DefaultTCPOwnerMemoryBudget those of one owner. An admitted loopback
	// connection costs 20 KiB at its starting rings and 8 KiB at the floor,
	// so one owner holds 512 loopback pairs at their starting rings with
	// 12 MiB left for growth. MaxTCPMemoryBudget keeps the rings, and the
	// garbage their growth leaves for the collector, inside netd's 64 MiB
	// page quota.
	DefaultTCPMemoryBudget      = 40 << 20
	DefaultTCPOwnerMemoryBudget = 32 << 20
	MaxTCPMemoryBudget          = 48 << 20
	minTCPMemoryBudget          = 64 << 10
	// DefaultTCPTimeWaitBudget bounds the TIME_WAIT tombstones and
	// DefaultTCPOwnerTimeWaitBudget the share of one owner.
	DefaultTCPTimeWaitBudget      = 8192
	DefaultTCPOwnerTimeWaitBudget = 2048
	tcpSlotBits                   = 12
	tcpGenerations                = 1 << (32 - tcpSlotBits)
	tcpInitialWindowBytes         = 14600
	tcpInitialSSThresh            = 65535
	// tcpMaxRetransmits bounds consecutive RTO retransmissions of one
	// synchronized connection. With the default RTO limits the connection
	// gives up after about six minutes without progress.
	tcpMaxRetransmits = 10
	// tcpOwners covers every owner byte: the sessions and tcpRetiredOwner.
	tcpOwners = 256
)

// The slot field of an ID must address every endpoint the tuning allows.
var _ [1<<tcpSlotBits - MaxTCPEndpoints]struct{}

// TCPEndpointID contains a registry slot and generation. A closed ID cannot
// address a later endpoint that reuses the slot.
type TCPEndpointID uint32

func (id TCPEndpointID) slot() int          { return int(id & (1<<tcpSlotBits - 1)) }
func (id TCPEndpointID) generation() uint32 { return uint32(id) >> tcpSlotBits }

// TCPState is the endpoint or connection state.
type TCPState uint8

const (
	TCPFree TCPState = iota
	TCPOpen
	TCPBound
	TCPListen
	TCPSynSent
	TCPSynReceived
	TCPEstablished
	TCPFinWait1
	TCPFinWait2
	TCPCloseWait
	TCPClosing
	TCPLastAck
	TCPTimeWait
	TCPClosed
)

// TCPTuple is a connection's fixed four-tuple.
type TCPTuple struct {
	Local      IPv4
	LocalPort  uint16
	Remote     IPv4
	RemotePort uint16
}

// tcpEndpoint is the control block. The rings and the reassembly spans hang
// off it and exist only while the connection needs them.
type tcpEndpoint struct {
	state      TCPState
	owner      uint8
	generation uint32
	// slot is the registry slot plus one. Zero marks a released endpoint
	// or a temporary one that carries a reply, so neither can arm a timer
	// or join an index.
	slot       int32
	local      IPv4
	port       uint16
	remote     IPv4
	remotePort uint16
	listener   TCPEndpointID
	accepted   bool
	backlog    uint16
	// reuse is SO_REUSEADDR as given at bind; a passive child takes its
	// listener's.
	reuse bool
	// synchronized records a completed three-way handshake. It survives
	// later transitions so a deferred Connect still reports success.
	synchronized bool

	// members marks the indexes the endpoint is linked into, hashes holds
	// the hash it was filed under in each, and links its neighbours.
	members uint8
	hashes  [tcpHashedIndexes]uint32
	links   [tcpLinkKinds]tcpLink
	// parent is the listener whose children list holds the endpoint, and
	// children, on a listener, the children not yet accepted, oldest first.
	parent   *tcpEndpoint
	children tcpList

	iss, irs       uint32
	sndUna, sndNxt uint32
	rcvNxt         uint32
	peerWindow     uint32
	sndWL1, sndWL2 uint32
	peerMSS        uint16
	localMSS       uint16
	// rcvScale shifts the windows this end advertises and sndScale those
	// the peer advertises (RFC 7323 2). Before the handshake completes,
	// rcvScale is the shift a SYN offers; both stay zero unless both SYNs
	// carried the option.
	rcvScale, sndScale uint8

	// sendRing and recvRing are the stream rings; tcp_ring.go describes the
	// indexing. charged is the ring bytes the owner's budget pays for, and
	// sendReserve the send floor charged from admission until the send side
	// ends; sendCharge describes how the two meet.
	sendRing    []byte
	sendReserve uint16
	sendSeq     uint32
	sendHead    uint32
	sendLen     uint32
	sendSent    uint32
	recvRing    []byte
	recvHead    uint32
	recvLen     uint32
	charged     uint32
	// reorder holds the out-of-order spans stored in recvRing past the
	// contiguous bytes; nil while every byte arrived in order.
	reorder *tcpReorder
	// rcvEdge is the right edge of the receive window the last ACK sent
	// advertised: the peer may send up to it.
	rcvEdge uint32
	// rcvEdgeKnown: an ACK has advertised rcvEdge.
	rcvEdgeKnown bool

	shutdownWrite bool
	orphan        bool
	finSent       bool
	finReceived   bool
	finSequence   uint32
	// peerFIN fixes the sequence number of the peer's FIN once a segment
	// carrying it has been accepted. No stream byte at or after peerFINAt is
	// delivered.
	peerFIN   bool
	peerFINAt uint32
	// l2Pending marks a SYN, data or FIN segment that could not be sent
	// because the next hop's link address was unknown. Neighbour resolution
	// either resends it or ends the connection.
	l2Pending bool
	// ackPending marks a pure ACK or window update that found the transmit
	// queue full; the endpoint then sits on stack.tcpAckPending. The first
	// Advance with a free frame sends it; a frame frees only when the driver
	// hands one back, which wakes netd. Without it a peer that saw a zero
	// window waits for its persist timer.
	ackPending bool
	// timeWaitHeld marks a TIME_WAIT connection the budget left an
	// endpoint, counted once in TCPStats.TimeWaitHeld.
	timeWaitHeld bool

	rto uint64
	// rtoDeadline and deadline are the endpoint's two timers. Only
	// setRTO and setDeadline change them, so the timer heap holds the
	// earlier of the two.
	rtoDeadline uint64
	srtt        uint64
	rttvar      uint64
	rttSeq      uint32
	rttAt       uint64
	retransmit  bool
	retries     uint8
	persist     uint8
	dupACKs     uint8
	cwnd        uint32
	ssthresh    uint32
	recover     uint32
	fastRecover bool
	challengeAt uint64

	deadline   uint64
	lastResult Result
	// pendingError is the transport error that ended the connection, kept
	// until TCPTakeError reads it.
	pendingError Result
}

// TCPStats are monotonic transport counters plus current state counts.
type TCPStats struct {
	RX, TX, Retransmits, RXReset, TXReset uint64
	DropChecksum, DropNoTCB, DropWindow   uint64
	DropMalformed                         uint64
	// DropMemory counts handshakes refused and segments dropped because a
	// memory budget had no room for their ring bytes.
	DropMemory uint64
	// MemoryCapped counts ring growths the memory budgets cut short. The
	// connection keeps working at the smaller ring and window.
	MemoryCapped uint64
	// DropBacklog counts SYNs a full listener dropped.
	DropBacklog uint64
	// SplitSegments counts data segments sent from both ends of the send
	// ring, which putTCP joins in the packet buffer.
	SplitSegments uint64
	// LocalSegments counts segments a local peer took without a packet.
	LocalSegments uint64
	// MatchProbes and ListenerProbes count the index entries the segment
	// lookups looked at. LookupProbes counts every entry a received
	// segment's lookups looked at, tombstones included, and
	// MaxSegmentProbes is the most one segment needed.
	MatchProbes      uint64
	ListenerProbes   uint64
	LookupProbes     uint64
	MaxSegmentProbes uint64
	// TimeWaitHeld counts TIME_WAIT connections that stayed endpoints
	// because the TIME_WAIT budget had no room for their tombstone.
	TimeWaitHeld uint64
	// Endpoints, Listeners and Connections count live endpoints; States
	// counts them by state. TimeWait counts tombstones, MemoryBytes the
	// ring bytes charged to the budgets.
	Endpoints, Listeners, Connections int
	TimeWait                          int
	MemoryBytes                       int
	States                            [TCPClosed + 1]int
}

func (stack *Stack) resetTCP() {
	stack.tcp = nil
	stack.tcpUsed = nil
	stack.tcpLive = 0
	stack.tcpTuples = tcpIndex{kind: tcpLinkTuple}
	stack.tcpPorts = tcpIndex{kind: tcpLinkPort}
	stack.tcpListeners = tcpIndex{kind: tcpLinkListen}
	stack.tcpOwnerLists = [tcpOwners]tcpList{}
	stack.tcpTimers = tcpTimers{}
	stack.tcpAckPending = nil
	stack.tcpAckSpare = nil
	stack.tcpTombstones = tcpTombstones{}
	stack.tcpQuota = [MaxTCPSessions]uint16{}
	stack.tcpOwnerBytes = [tcpOwners]int{}
	stack.tcpBytes = 0
	stack.tcpBudget = tcpBudget{}
	// The hash seed derives from the stack's random state without drawing
	// from it, so the ISS and port sequences do not shift.
	stack.tcpSeed = mix64(stack.random ^ 0x7cb3a1d5e0f29467)
	stack.tcpGeneration = 0
	stack.tcpStats = TCPStats{}
}

// tcpBudget holds the memory and TIME_WAIT budgets. Zero selects the
// default.
type tcpBudget struct {
	memory, ownerMemory     int
	timeWait, ownerTimeWait int
}

func budgetOr(value, fallback int) int {
	if value <= 0 {
		return fallback
	}
	return value
}

// SetTCPMemoryBudget sets the ring bytes all connections, and one owner's,
// may hold; zero restores the tuned value. A lowered budget takes nothing
// back: it caps the next ring growth and refuses the next handshake whose
// minimum would exceed it.
func (stack *Stack) SetTCPMemoryBudget(total, owner int) {
	stack.tcpBudget.memory, stack.tcpBudget.ownerMemory = total, owner
}

// TCPOwnerMemory reports the ring bytes charged to owner and the owner
// budget in force, so a caller can attribute a refused handshake or a
// capped ring to the owner that holds the memory.
func (stack *Stack) TCPOwnerMemory(owner int) (int, int) {
	if owner < 0 || owner >= tcpOwners {
		return 0, 0
	}
	return stack.tcpOwnerBytes[owner], stack.tcpOwnerMemoryBudget()
}

// SetTCPTimeWaitBudget sets the TIME_WAIT tombstones the stack, and one
// owner, may hold; zero restores a default. A connection with no room for a
// tombstone stays an endpoint in TIME_WAIT until its interval ends, so the
// owner's quota refuses new endpoints instead of the stack forgetting the
// four-tuple.
func (stack *Stack) SetTCPTimeWaitBudget(total, owner int) {
	stack.tcpBudget.timeWait, stack.tcpBudget.ownerTimeWait = total, owner
}

// SetTCPQuota sets how many endpoints owner may hold, 1 to
// MaxTCPSessionQuota. Zero restores the ordinary quota, the tuned
// max-endpoints-per-session. A quota above the ordinary capacity is an
// explicit grant and the tuning does not lower it; a smaller quota is capped
// by the tuning as well. Endpoints already open stay open.
func (stack *Stack) SetTCPQuota(owner, quota int) bool {
	if owner < 0 || owner >= MaxTCPSessions || quota < 0 || quota > MaxTCPSessionQuota {
		return false
	}
	stack.tcpQuota[owner] = uint16(quota)
	return true
}

// tcpOwnerQuota is the number of endpoints owner may hold now.
func (stack *Stack) tcpOwnerQuota(owner int) int {
	tuned := int(stack.tuning.TCP.SessionEndpoints)
	quota := int(stack.tcpQuota[owner])
	switch {
	case quota == 0:
		return tuned
	case quota > MaxTCPEndpointsPerSession:
		return quota
	}
	return min(quota, tuned)
}

func (stack *Stack) tcpEndpoint(owner int, id TCPEndpointID) (*tcpEndpoint, int) {
	slot := id.slot()
	if owner < 0 || owner >= MaxTCPSessions || slot >= len(stack.tcp) {
		return nil, -1
	}
	entry := stack.tcp[slot]
	if entry == nil || entry.state == TCPFree || entry.generation != id.generation() || int(entry.owner) != owner {
		return nil, -1
	}
	return entry, slot
}

func tcpID(entry *tcpEndpoint) TCPEndpointID {
	return TCPEndpointID(entry.generation<<tcpSlotBits | uint32(entry.slot-1))
}

// TCPOpen creates an endpoint. Busy means the global endpoint limit or the
// owner's quota is reached; an owner at its quota first gets the slot of one
// of its closed TIME_WAIT connections if a tombstone can take its four-tuple.
func (stack *Stack) TCPOpen(owner int) (TCPEndpointID, Result) {
	if owner < 0 || owner >= MaxTCPSessions {
		return 0, ResultInvalidArgument
	}
	if stack.tcpLive >= int(stack.tuning.TCP.MaxEndpoints) || stack.tcpOwnerLists[owner].count >= stack.tcpOwnerQuota(owner) {
		if !stack.recycleTimeWait(owner) {
			return 0, ResultBusy
		}
	}
	entry := stack.allocateTCP(owner)
	if entry == nil {
		return 0, ResultBusy
	}
	return tcpID(entry), ResultOK
}

func (stack *Stack) TCPBind(owner int, id TCPEndpointID, address IPv4, port uint16) (IPv4, uint16, Result) {
	return stack.TCPBindReuse(owner, id, address, port, false)
}

// TCPBindReuse binds with SO_REUSEADDR when reuse is set. A bind conflicts
// with any endpoint or TIME_WAIT tombstone on the same port whose address
// overlaps, equal or either one the wildcard, unless both carry reuse and
// the existing one is not listening. Port 0 picks a port nothing holds.
func (stack *Stack) TCPBindReuse(owner int, id TCPEndpointID, address IPv4, port uint16, reuse bool) (IPv4, uint16, Result) {
	entry, _ := stack.tcpEndpoint(owner, id)
	if entry == nil || entry.state != TCPOpen {
		return IPv4{}, 0, ResultInvalidArgument
	}
	if !stack.localAddress(address) {
		return IPv4{}, 0, ResultNoAddress
	}
	if port == 0 {
		port = stack.tcpEphemeralPort(address, IPv4{}, 0, entry)
		if port == 0 {
			return IPv4{}, 0, ResultBusy
		}
	} else if stack.tcpReuseConflict(entry, address, port, reuse) {
		return IPv4{}, 0, ResultAddressInUse
	}
	entry.local, entry.port, entry.state, entry.reuse = address, port, TCPBound, reuse
	stack.tcpPorts.insert(entry, stack.portHash(port))
	return address, port, ResultOK
}

// TCPListen makes a bound endpoint a listener that keeps up to backlog
// children no application has accepted. A backlog above the tuned
// listen-backlog gets the tuned value, as Linux clamps to somaxconn.
func (stack *Stack) TCPListen(owner int, id TCPEndpointID, backlog int) Result {
	entry, _ := stack.tcpEndpoint(owner, id)
	if entry == nil || entry.state != TCPBound || backlog < 1 {
		return ResultInvalidArgument
	}
	if stack.tcpListeners.count >= MaxTCPListeners {
		return ResultBusy
	}
	entry.state, entry.backlog = TCPListen, uint16(min(backlog, int(stack.tuning.TCP.ListenBacklog)))
	stack.tcpListeners.insert(entry, stack.portHash(entry.port))
	return ResultOK
}

// TCPAccept returns one established child once, oldest first. Busy means
// the deferred accept remains pending.
func (stack *Stack) TCPAccept(owner int, listener TCPEndpointID) (TCPEndpointID, TCPTuple, Result) {
	entry, _ := stack.tcpEndpoint(owner, listener)
	if entry == nil || entry.state != TCPListen {
		return 0, TCPTuple{}, ResultInvalidArgument
	}
	for child := entry.children.head; child != nil; child = child.links[tcpLinkChild].next {
		if child.state >= TCPEstablished && child.state < TCPClosed {
			stack.detachChild(child)
			child.accepted = true
			return tcpID(child), tcpTuple(child), ResultOK
		}
	}
	return 0, TCPTuple{}, ResultBusy
}

func (stack *Stack) TCPConnect(owner int, id TCPEndpointID, remote IPv4, port uint16, now uint64) (TCPTuple, Result) {
	entry, _ := stack.tcpEndpoint(owner, id)
	if entry == nil || (entry.state != TCPOpen && entry.state != TCPBound) || port == 0 {
		return TCPTuple{}, ResultInvalidArgument
	}
	route, result := stack.udpRoute(entry.local, remote)
	if result != ResultOK {
		return TCPTuple{}, result
	}
	local := entry.local
	if local == zeroIPv4 {
		local = route.source
	}
	chosen := entry.port
	if chosen == 0 {
		chosen = stack.tcpEphemeralPort(local, remote, port, entry)
		if chosen == 0 {
			return TCPTuple{}, ResultBusy
		}
	}
	if stack.tcpTupleConflict(entry, local, chosen, remote, port) {
		return TCPTuple{}, ResultAddressInUse
	}
	if !stack.allocateReceive(entry, route.path == udpLocal) {
		return TCPTuple{}, ResultBusy
	}
	if entry.state == TCPBound {
		stack.tcpPorts.remove(entry)
	}
	entry.local, entry.port, entry.remote, entry.remotePort = local, chosen, remote, port
	entry.iss = uint32(stack.next())
	entry.sndUna, entry.sndNxt = entry.iss, entry.iss+1
	entry.localMSS = stack.tcpPathMSS(route)
	entry.rcvScale = stack.tcpWindowScale()
	entry.state = TCPSynSent
	stack.indexConnection(entry)
	stack.setDeadline(entry, addTime(now, stack.tuning.TCP.ConnectTimeout))
	entry.rto = stack.tuning.TCP.RTOMin
	if stack.sendTCP(entry, tcpFlagSYN, entry.iss, 0, nil, now) && entry.state == TCPSynSent {
		stack.setRTO(entry, addTime(now, entry.rto))
	}
	return tcpTuple(entry), ResultOK
}

func tcpTuple(entry *tcpEndpoint) TCPTuple {
	return TCPTuple{Local: entry.local, LocalPort: entry.port, Remote: entry.remote, RemotePort: entry.remotePort}
}

// TCPWrite copies bytes into transport-owned storage. It may accept a prefix.
// A connection the transport ended reports why; Closed means the
// application shut down writing or the close completed normally. Busy means
// the send buffer is full or the memory budgets leave it no room to grow.
func (stack *Stack) TCPWrite(owner int, id TCPEndpointID, data []byte, now uint64) (int, Result) {
	entry, _ := stack.tcpEndpoint(owner, id)
	if entry != nil && entry.state == TCPClosed && entry.lastResult != ResultOK {
		return 0, entry.lastResult
	}
	if entry == nil || entry.shutdownWrite || (entry.state != TCPEstablished && entry.state != TCPCloseWait) {
		return 0, ResultClosed
	}
	if len(data) == 0 {
		return 0, ResultOK
	}
	room := stack.tcpSendRoom(entry)
	if room <= 0 {
		return 0, ResultBusy
	}
	if len(data) > room {
		data = data[:room]
	}
	stack.growSend(entry, int(entry.sendLen)+len(data))
	ringStore(entry.sendRing, int(entry.sendHead), int(entry.sendLen), data)
	entry.sendLen += uint32(len(data))
	stack.pumpTCP(entry, now)
	return len(data), ResultOK
}

// TCPRead returns contiguous bytes, Busy while a read must wait, or EOF after
// all bytes preceding a received FIN have been delivered.
func (stack *Stack) TCPRead(owner int, id TCPEndpointID, dst []byte, now uint64) (int, Result) {
	entry, _ := stack.tcpEndpoint(owner, id)
	if entry == nil || len(dst) == 0 {
		return 0, ResultInvalidArgument
	}
	if entry.recvLen == 0 {
		if entry.finReceived {
			return 0, ResultEOF
		}
		if entry.lastResult != ResultOK {
			return 0, entry.lastResult
		}
		return 0, ResultBusy
	}
	length := min(len(dst), int(entry.recvLen))
	ringLoad(dst, entry.recvRing, int(entry.recvHead), length)
	entry.recvHead = ringAdvance(int(entry.recvHead), length, len(entry.recvRing))
	entry.recvLen -= uint32(length)
	if entry.recvLen == 0 && entry.reorder == nil {
		entry.recvHead = 0
	}
	// A window update goes when the read moved the window's right edge by
	// a segment or half the buffer, whichever is less (RFC 1122 4.2.3.3);
	// smaller openings ride on the next ACK, so a reader that keeps up does
	// not send one update per read.
	if entry.state < TCPSynSent || entry.state >= TCPTimeWait {
		return length, ResultOK
	}
	edge := entry.rcvNxt + uint32(stack.tcpAdvertisedWindow(entry))
	step := min(tcpSMSS(entry), uint32(stack.tcpReceiveLimit(entry)/2))
	if !entry.rcvEdgeKnown || int32(edge-entry.rcvEdge) >= int32(step) {
		stack.sendTCP(entry, tcpFlagACK, entry.sndNxt, entry.rcvNxt, nil, now)
	}
	return length, ResultOK
}

func (stack *Stack) TCPShutdownWrite(owner int, id TCPEndpointID, now uint64) Result {
	entry, _ := stack.tcpEndpoint(owner, id)
	if entry == nil || entry.shutdownWrite || (entry.state != TCPEstablished && entry.state != TCPCloseWait) {
		return ResultClosed
	}
	entry.shutdownWrite = true
	stack.pumpTCP(entry, now)
	return ResultOK
}

// TCPClose starts graceful close for a synchronized connection and releases
// an unconnected endpoint or listener at once. A closing connection becomes an
// orphan: the stack releases it when the close completes, because its owner
// no longer holds the ID. A connection in TIME_WAIT leaves its four-tuple in
// a tombstone.
func (stack *Stack) TCPClose(owner int, id TCPEndpointID, now uint64) Result {
	entry, _ := stack.tcpEndpoint(owner, id)
	if entry == nil {
		return ResultInvalidArgument
	}
	switch entry.state {
	case TCPEstablished, TCPCloseWait:
		entry.orphan, entry.shutdownWrite = true, true
		stack.pumpTCP(entry, now)
		stack.settleTCP(entry)
	case TCPFinWait1, TCPClosing, TCPLastAck:
		entry.orphan = true
	case TCPFinWait2:
		entry.orphan = true
		stack.setDeadline(entry, addTime(now, stack.tuning.TCP.TimeWait))
	case TCPTimeWait:
		entry.orphan = true
		stack.settleTCP(entry)
	case TCPListen:
		stack.abortChildren(entry, now)
		stack.releaseTCP(entry)
	default:
		stack.releaseTCP(entry)
	}
	return ResultOK
}

// TCPCloseOwner ends every endpoint of a session. A connection in TIME_WAIT
// keeps its four-tuple protected, in a tombstone or, with the budget full,
// as an endpoint of tcpRetiredOwner, so a later session with the same owner
// number starts with its whole quota.
func (stack *Stack) TCPCloseOwner(owner int, now uint64) {
	if owner < 0 || owner >= MaxTCPSessions {
		return
	}
	stack.retireTombstones(owner)
	list := &stack.tcpOwnerLists[owner]
	for entry := list.head; entry != nil; {
		next := entry.links[tcpLinkOwner].next
		// A listener's release can release the children after it in the
		// list; start over from the head when next went.
		if entry.state == TCPTimeWait {
			stack.retireTimeWait(entry)
		} else {
			if entry.state >= TCPSynSent && entry.state < TCPTimeWait {
				stack.sendTCP(entry, tcpFlagRST|tcpFlagACK, entry.sndNxt, entry.rcvNxt, nil, now)
			}
			if entry.state == TCPListen {
				stack.abortChildren(entry, now)
			}
			stack.releaseTCP(entry)
		}
		if next != nil && next.slot == 0 {
			next = list.head
		}
		entry = next
	}
}

// abortChildren resets and releases the children of a listener that no
// application has accepted. Nobody else can ever reach them.
func (stack *Stack) abortChildren(listener *tcpEndpoint, now uint64) {
	for child := listener.children.head; child != nil; child = listener.children.head {
		if child.state >= TCPSynReceived && child.state < TCPTimeWait {
			stack.sendTCP(child, tcpFlagRST|tcpFlagACK, child.sndNxt, child.rcvNxt, nil, now)
		}
		if child.slot != 0 {
			stack.releaseTCP(child)
		} else if child.parent == listener {
			stack.detachChild(child)
		}
	}
}

// TCPConnectResult reports whether an active open has finished and how. A
// connection that completed its handshake reports OK in any later state, so a
// FIN or RST that arrives before the caller looks does not hide the success.
func (stack *Stack) TCPConnectResult(owner int, id TCPEndpointID) (TCPTuple, Result, bool) {
	entry, _ := stack.tcpEndpoint(owner, id)
	switch {
	case entry == nil:
		return TCPTuple{}, ResultInvalidArgument, true
	case entry.synchronized:
		return tcpTuple(entry), ResultOK, true
	case entry.state == TCPSynSent:
		return TCPTuple{}, ResultOK, false
	case entry.lastResult != ResultOK:
		return TCPTuple{}, entry.lastResult, true
	}
	return TCPTuple{}, ResultInvalidArgument, true
}

func (stack *Stack) TCPState(owner int, id TCPEndpointID) (TCPState, TCPTuple, Result) {
	entry, _ := stack.tcpEndpoint(owner, id)
	if entry == nil {
		return TCPFree, TCPTuple{}, ResultInvalidArgument
	}
	return entry.state, tcpTuple(entry), entry.lastResult
}

// closeTCP moves a connection to CLOSED with its final result and stops its
// timers. No application can observe an orphan or a passive child that was
// never accepted, so those are released instead, which also frees the
// listener's backlog.
func (stack *Stack) closeTCP(entry *tcpEndpoint, result Result) {
	if entry.orphan || (entry.listener != 0 && !entry.accepted) {
		stack.releaseTCP(entry)
		return
	}
	entry.lastResult, entry.state = result, TCPClosed
	stack.setDeadline(entry, 0)
	stack.setRTO(entry, 0)
	stack.freeSend(entry)
	if result != ResultOK {
		entry.pendingError = result
	}
}

// tcpSMSS is the sender maximum segment size: the smaller of the path MSS and
// the MSS the peer announced.
func tcpSMSS(entry *tcpEndpoint) uint32 {
	mss := min(entry.localMSS, entry.peerMSS)
	if mss == 0 {
		return tcpDefaultMSS
	}
	return uint32(mss)
}

// tcpInitialWindow is the RFC 6928 initial window, min(10*SMSS,
// max(2*SMSS, 14600)), or one segment after a SYN or SYN-ACK was retransmitted.
func tcpInitialWindow(entry *tcpEndpoint) uint32 {
	smss := tcpSMSS(entry)
	if entry.retries != 0 {
		return smss
	}
	return min(10*smss, max(2*smss, tcpInitialWindowBytes))
}

// TCPStats walks the live endpoints for the state counts; the transport
// paths never call it.
func (stack *Stack) TCPStats() TCPStats {
	stats := stack.tcpStats
	for index := stack.nextTCP(0); index >= 0; index = stack.nextTCP(index + 1) {
		state := stack.tcp[index].state
		stats.Endpoints++
		stats.States[state]++
		if state == TCPListen {
			stats.Listeners++
		} else if state >= TCPSynSent {
			stats.Connections++
		}
	}
	stats.TimeWait = stack.tcpTombstones.count
	stats.MemoryBytes = stack.tcpBytes
	return stats
}

// tcpPortTaken is the check behind automatic port choice. A bind to port 0
// takes a port nothing holds. A connect, as on Linux, may share a local port
// with other connections and TIME_WAIT four-tuples, which the tuple check
// keeps apart, but never with an endpoint bound or listening there.
func (stack *Stack) tcpPortTaken(skip *tcpEndpoint, address IPv4, port uint16, connect bool) bool {
	if !connect {
		return stack.tcpReuseConflict(skip, address, port, false)
	}
	taken := false
	for entry := stack.tcpPorts.bucket(stack.portHash(port)); entry != nil; entry = entry.links[tcpLinkPort].next {
		if entry == skip || entry.state < TCPBound || entry.state > TCPListen || entry.port != port {
			continue
		}
		if address == zeroIPv4 || entry.local == zeroIPv4 || entry.local == address {
			taken = true
			break
		}
	}
	if tcpCrossCheck && taken != stack.slowPortTaken(skip, address, port) {
		panic("tcp: port index disagrees with the endpoints")
	}
	return taken
}

// tcpReuseConflict is the Linux bind rule: an endpoint other than skip, in
// any state from bound on, or a tombstone, on port with an overlapping
// address conflicts unless reuse and the holder's reuse are both set and the
// holder is not listening.
func (stack *Stack) tcpReuseConflict(skip *tcpEndpoint, address IPv4, port uint16, reuse bool) bool {
	conflict := false
	for entry := stack.tcpPorts.bucket(stack.portHash(port)); entry != nil; entry = entry.links[tcpLinkPort].next {
		if entry == skip || entry.state < TCPBound || entry.port != port || !addressesOverlap(address, entry.local) {
			continue
		}
		if !reuse || !entry.reuse || entry.state == TCPListen {
			conflict = true
			break
		}
	}
	if !conflict {
		conflict = stack.tcpTombstones.portConflict(address, port, reuse, stack.portHash(port))
	}
	if tcpCrossCheck && conflict != stack.slowReuseConflict(skip, address, port, reuse) {
		panic("tcp: bind index disagrees with the endpoints")
	}
	return conflict
}

func addressesOverlap(a, b IPv4) bool { return a == zeroIPv4 || b == zeroIPv4 || a == b }

// tcpTupleConflict reports a four-tuple in use by another connection or
// still protected by a tombstone.
func (stack *Stack) tcpTupleConflict(skip *tcpEndpoint, local IPv4, localPort uint16, remote IPv4, remotePort uint16) bool {
	if stack.tcpTombstone(local, localPort, remote, remotePort) >= 0 {
		return true
	}
	var probes uint64
	entry := stack.lookupTuple(local, localPort, remote, remotePort, &probes)
	return entry != nil && entry != skip
}

func (stack *Stack) tcpEphemeralPort(local, remote IPv4, remotePort uint16, skip *tcpEndpoint) uint16 {
	first, last := uint32(stack.tuning.TCP.EphemeralMin), uint32(stack.tuning.TCP.EphemeralMax)
	if first == 0 || last < first {
		return 0
	}
	span := last - first + 1
	port := first + uint32(stack.next()%uint64(span))
	for attempt := uint32(0); attempt < span+4; attempt++ {
		if !stack.tcpPortTaken(skip, local, uint16(port), remotePort != 0) && !stack.tcpTupleConflict(skip, local, uint16(port), remote, remotePort) {
			return uint16(port)
		}
		if attempt < 4 {
			port = first + uint32(stack.next()%uint64(span))
		} else if port++; port > last {
			port = first
		}
	}
	return 0
}

// tcpPathMSS is the MSS a SYN announces: the MTU less the headers on the
// wire, TCPLoopbackMSS to a local peer. Reassembly stores segments in the
// receive ring, so no slot size bounds it.
func (stack *Stack) tcpPathMSS(route udpRouteResult) uint16 {
	if route.path == udpLocal {
		return TCPLoopbackMSS
	}
	mtu := int(stack.mtu)
	if mtu < ipv4HeaderSize+tcpHeaderSize+1 {
		return 1
	}
	return uint16(min(mtu-ipv4HeaderSize-tcpHeaderSize, 65535))
}

// TCPEndpointState is one endpoint's transport state, for diagnostics.
type TCPEndpointState struct {
	ID                     TCPEndpointID
	Owner                  uint8
	State                  TCPState
	LocalPort, RemotePort  uint16
	SendLen, SendSent      uint32
	SendHead, RecvLen      uint32
	RecvHead               uint32
	SendRing, RecvRing     uint32
	SndUna, SndNxt, RcvNxt uint32
	PeerWindow, CWnd       uint32
	ShutdownWrite, FINSent bool
	PendingError           Result
}

// TCPEndpointAt reports the endpoint in registry slot index, or false for a
// free slot or an index past the registry.
func (stack *Stack) TCPEndpointAt(index int) (TCPEndpointState, bool) {
	if index < 0 || index >= len(stack.tcp) || stack.tcp[index] == nil {
		return TCPEndpointState{}, false
	}
	e := stack.tcp[index]
	return TCPEndpointState{ID: tcpID(e), Owner: e.owner, State: e.state, LocalPort: e.port, RemotePort: e.remotePort,
		SendLen: e.sendLen, SendSent: e.sendSent, SendHead: e.sendHead, RecvLen: e.recvLen, RecvHead: e.recvHead,
		SendRing: uint32(len(e.sendRing)), RecvRing: uint32(len(e.recvRing)),
		SndUna: e.sndUna, SndNxt: e.sndNxt, RcvNxt: e.rcvNxt, PeerWindow: e.peerWindow, CWnd: e.cwnd,
		ShutdownWrite: e.shutdownWrite, FINSent: e.finSent, PendingError: e.pendingError}, true
}

// TCPEndpointCount is the size of the registry: one past the highest slot
// ever used.
func (stack *Stack) TCPEndpointCount() int { return len(stack.tcp) }
