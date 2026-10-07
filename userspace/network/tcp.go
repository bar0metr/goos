package network

// TCP uses fixed endpoint, stream and reassembly storage. Runtime tuning can
// lower these limits but cannot resize them.
const (
	MaxTCPSessions            = 8
	MaxTCPEndpoints           = 32
	MaxTCPEndpointsPerSession = 8
	MaxTCPListeners           = 8
	MaxTCPBacklog             = 8
	MaxTCPOutOfOrderSegments  = 8
	TCPSendCapacity           = 16 * 1024
	TCPReceiveCapacity        = 16 * 1024
	TCPReorderPayload         = 1460
	tcpSlotBits               = 8
	tcpGenerations            = 1 << (32 - tcpSlotBits)
	tcpInitialWindow          = 10 * 1460
	tcpInitialSSThresh        = 65535
	tcpEventCapacity          = MaxTCPEndpoints * 2
)

// TCPEndpointID contains a table slot and generation. A closed ID cannot
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

// TCPOperation identifies an asynchronous completion.
type TCPOperation uint8

const (
	TCPConnected TCPOperation = iota + 1
	TCPReadable
	TCPWritable
	TCPAccepted
	TCPClosedEvent
)

// TCPEvent reports state changes to tcpserve without storing IPC state in the
// protocol stack.
type TCPEvent struct {
	Owner     int
	Endpoint  TCPEndpointID
	Operation TCPOperation
	Result    Result
}

// TCPTuple is a connection's fixed four-tuple.
type TCPTuple struct {
	Local      IPv4
	LocalPort  uint16
	Remote     IPv4
	RemotePort uint16
}

type tcpReorder struct {
	used bool
	seq  uint32
	len  uint16
	fin  bool
	data [TCPReorderPayload]byte
}

type tcpEndpoint struct {
	state      TCPState
	owner      uint8
	generation uint32
	local      IPv4
	port       uint16
	remote     IPv4
	remotePort uint16
	listener   TCPEndpointID
	accepted   bool
	backlog    uint8

	iss, irs       uint32
	sndUna, sndNxt uint32
	rcvNxt         uint32
	peerWindow     uint32
	peerMSS        uint16
	localMSS       uint16

	send     [TCPSendCapacity]byte
	sendSeq  uint32
	sendLen  uint16
	sendSent uint16
	receive  [TCPReceiveCapacity]byte
	recvLen  uint16
	reorder  [MaxTCPOutOfOrderSegments]tcpReorder

	shutdownWrite bool
	finSent       bool
	finReceived   bool
	finSequence   uint32

	rto         uint64
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
}

// TCPStats are monotonic transport counters plus current state counts.
type TCPStats struct {
	RX, TX, Retransmits, RXReset, TXReset uint64
	DropChecksum, DropNoTCB, DropWindow   uint64
	DropMalformed                         uint64
	Endpoints, Listeners, Connections     int
	States                                [TCPClosed + 1]int
}

func (stack *Stack) resetTCP() {
	for index := range stack.tcp[:] {
		stack.tcp[index] = tcpEndpoint{}
	}
	stack.tcpGeneration = 0
	stack.tcpStats = TCPStats{}
	stack.tcpEventHead, stack.tcpEventCount = 0, 0
}

func (stack *Stack) tcpEndpoint(owner int, id TCPEndpointID) (*tcpEndpoint, int) {
	slot := id.slot()
	if owner < 0 || owner >= MaxTCPSessions || slot < 0 || slot >= MaxTCPEndpoints {
		return nil, -1
	}
	entry := &stack.tcp[slot]
	if entry.state == TCPFree || entry.generation != id.generation() || int(entry.owner) != owner {
		return nil, -1
	}
	return entry, slot
}

func tcpID(entry *tcpEndpoint, slot int) TCPEndpointID {
	return TCPEndpointID(entry.generation<<tcpSlotBits | uint32(slot))
}

func (stack *Stack) TCPOpen(owner int) (TCPEndpointID, Result) {
	if owner < 0 || owner >= MaxTCPSessions {
		return 0, ResultInvalidArgument
	}
	total, mine, free := 0, 0, -1
	for index := range stack.tcp[:] {
		entry := &stack.tcp[index]
		if entry.state == TCPFree {
			if free < 0 {
				free = index
			}
			continue
		}
		total++
		if int(entry.owner) == owner {
			mine++
		}
	}
	if free < 0 || total >= int(stack.tuning.TCP.MaxEndpoints) || mine >= int(stack.tuning.TCP.SessionEndpoints) {
		return 0, ResultBusy
	}
	stack.tcpGeneration++
	if stack.tcpGeneration == tcpGenerations {
		stack.tcpGeneration = 1
	}
	entry := &stack.tcp[free]
	*entry = tcpEndpoint{state: TCPOpen, owner: uint8(owner), generation: stack.tcpGeneration,
		peerMSS: tcpDefaultMSS, cwnd: tcpInitialWindow, ssthresh: tcpInitialSSThresh, rto: stack.tuning.TCP.RTOMin}
	return tcpID(entry, free), ResultOK
}

func (stack *Stack) TCPBind(owner int, id TCPEndpointID, address IPv4, port uint16) (IPv4, uint16, Result) {
	entry, slot := stack.tcpEndpoint(owner, id)
	if entry == nil || entry.state != TCPOpen || !stack.localAddress(address) {
		return IPv4{}, 0, ResultInvalidArgument
	}
	if port == 0 {
		port = stack.tcpEphemeralPort(address, IPv4{}, 0, slot)
		if port == 0 {
			return IPv4{}, 0, ResultBusy
		}
	} else if stack.tcpBindConflict(slot, address, port) {
		return IPv4{}, 0, ResultAddressInUse
	}
	entry.local, entry.port, entry.state = address, port, TCPBound
	return address, port, ResultOK
}

func (stack *Stack) TCPListen(owner int, id TCPEndpointID, backlog int) Result {
	entry, _ := stack.tcpEndpoint(owner, id)
	if entry == nil || entry.state != TCPBound || backlog < 1 || backlog > int(stack.tuning.TCP.ListenBacklog) {
		return ResultInvalidArgument
	}
	listeners := 0
	for index := range stack.tcp[:] {
		if stack.tcp[index].state == TCPListen {
			listeners++
		}
	}
	if listeners >= MaxTCPListeners {
		return ResultBusy
	}
	entry.state, entry.backlog = TCPListen, uint8(backlog)
	return ResultOK
}

// TCPAccept returns one established child once. Busy means the deferred
// accept remains pending.
func (stack *Stack) TCPAccept(owner int, listener TCPEndpointID) (TCPEndpointID, TCPTuple, Result) {
	entry, _ := stack.tcpEndpoint(owner, listener)
	if entry == nil || entry.state != TCPListen {
		return 0, TCPTuple{}, ResultInvalidArgument
	}
	for index := range stack.tcp[:] {
		child := &stack.tcp[index]
		if child.listener == listener && child.state >= TCPEstablished && child.state < TCPClosed && !child.accepted {
			child.accepted = true
			return tcpID(child, index), tcpTuple(child), ResultOK
		}
	}
	return 0, TCPTuple{}, ResultBusy
}

func (stack *Stack) TCPConnect(owner int, id TCPEndpointID, remote IPv4, port uint16, now uint64) (TCPTuple, Result) {
	entry, slot := stack.tcpEndpoint(owner, id)
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
	if entry.port == 0 {
		entry.port = stack.tcpEphemeralPort(local, remote, port, slot)
		if entry.port == 0 {
			return TCPTuple{}, ResultBusy
		}
	}
	if stack.tcpTupleConflict(slot, local, entry.port, remote, port) {
		return TCPTuple{}, ResultAddressInUse
	}
	entry.local, entry.remote, entry.remotePort = local, remote, port
	entry.iss = uint32(stack.next())
	entry.sndUna, entry.sndNxt = entry.iss, entry.iss+1
	entry.localMSS = stack.tcpPathMSS(route)
	entry.state = TCPSynSent
	entry.deadline = addTime(now, stack.tuning.TCP.ConnectTimeout)
	entry.rto = stack.tuning.TCP.RTOMin
	if stack.sendTCP(entry, tcpFlagSYN, entry.iss, 0, nil, now) && entry.state == TCPSynSent {
		entry.rtoDeadline = addTime(now, entry.rto)
	}
	return tcpTuple(entry), ResultOK
}

func tcpTuple(entry *tcpEndpoint) TCPTuple {
	return TCPTuple{Local: entry.local, LocalPort: entry.port, Remote: entry.remote, RemotePort: entry.remotePort}
}

// TCPWrite copies bytes into transport-owned storage. It may accept a prefix.
func (stack *Stack) TCPWrite(owner int, id TCPEndpointID, data []byte, now uint64) (int, Result) {
	entry, _ := stack.tcpEndpoint(owner, id)
	if entry == nil || entry.shutdownWrite || (entry.state != TCPEstablished && entry.state != TCPCloseWait) {
		return 0, ResultClosed
	}
	if len(data) == 0 {
		return 0, ResultOK
	}
	limit := int(stack.tuning.TCP.SendBuffer)
	if limit > TCPSendCapacity {
		limit = TCPSendCapacity
	}
	free := limit - int(entry.sendLen)
	if free <= 0 {
		return 0, ResultBusy
	}
	if len(data) > free {
		data = data[:free]
	}
	copy(entry.send[entry.sendLen:], data)
	entry.sendLen += uint16(len(data))
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
	copy(dst, entry.receive[:length])
	copy(entry.receive[:], entry.receive[length:entry.recvLen])
	entry.recvLen -= uint16(length)
	stack.sendTCP(entry, tcpFlagACK, entry.sndNxt, entry.rcvNxt, nil, now)
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
// an unconnected endpoint or listener at once.
func (stack *Stack) TCPClose(owner int, id TCPEndpointID, now uint64) Result {
	entry, _ := stack.tcpEndpoint(owner, id)
	if entry == nil {
		return ResultInvalidArgument
	}
	switch entry.state {
	case TCPEstablished, TCPCloseWait:
		entry.shutdownWrite = true
		stack.pumpTCP(entry, now)
	default:
		stack.releaseTCP(entry)
	}
	return ResultOK
}

func (stack *Stack) TCPCloseOwner(owner int, now uint64) {
	for index := range stack.tcp[:] {
		entry := &stack.tcp[index]
		if entry.state != TCPFree && int(entry.owner) == owner {
			if entry.state >= TCPSynSent && entry.state < TCPTimeWait {
				stack.sendTCP(entry, tcpFlagRST|tcpFlagACK, entry.sndNxt, entry.rcvNxt, nil, now)
			}
			stack.releaseTCP(entry)
		}
	}
}

func (stack *Stack) TCPState(owner int, id TCPEndpointID) (TCPState, TCPTuple, Result) {
	entry, _ := stack.tcpEndpoint(owner, id)
	if entry == nil {
		return TCPFree, TCPTuple{}, ResultInvalidArgument
	}
	return entry.state, tcpTuple(entry), entry.lastResult
}

func (stack *Stack) TCPEvent() (TCPEvent, bool) {
	if stack.tcpEventCount == 0 {
		return TCPEvent{}, false
	}
	event := stack.tcpEvents[stack.tcpEventHead]
	stack.tcpEventHead = (stack.tcpEventHead + 1) % tcpEventCapacity
	stack.tcpEventCount--
	return event, true
}

func (stack *Stack) tcpEvent(entry *tcpEndpoint, operation TCPOperation, result Result) {
	if stack.tcpEventCount == tcpEventCapacity {
		return
	}
	slot := -1
	for index := range stack.tcp[:] {
		if entry == &stack.tcp[index] {
			slot = index
			break
		}
	}
	if slot < 0 {
		return
	}
	index := (stack.tcpEventHead + stack.tcpEventCount) % tcpEventCapacity
	stack.tcpEvents[index] = TCPEvent{Owner: int(entry.owner), Endpoint: tcpID(entry, slot), Operation: operation, Result: result}
	stack.tcpEventCount++
}

func (stack *Stack) releaseTCP(entry *tcpEndpoint) {
	generation := entry.generation
	*entry = tcpEndpoint{generation: generation}
}

func (stack *Stack) TCPStats() TCPStats {
	stats := stack.tcpStats
	for index := range stack.tcp[:] {
		state := stack.tcp[index].state
		if state == TCPFree {
			continue
		}
		stats.Endpoints++
		stats.States[state]++
		if state == TCPListen {
			stats.Listeners++
		} else if state >= TCPSynSent {
			stats.Connections++
		}
	}
	return stats
}

func (stack *Stack) tcpBindConflict(skip int, address IPv4, port uint16) bool {
	for index := range stack.tcp[:] {
		entry := &stack.tcp[index]
		if index == skip || entry.state < TCPBound || entry.state > TCPListen || entry.port != port {
			continue
		}
		if address == zeroIPv4 || entry.local == zeroIPv4 || entry.local == address {
			return true
		}
	}
	return false
}

func (stack *Stack) tcpTupleConflict(skip int, local IPv4, localPort uint16, remote IPv4, remotePort uint16) bool {
	for index := range stack.tcp[:] {
		entry := &stack.tcp[index]
		if index != skip && entry.state >= TCPSynSent && entry.local == local && entry.port == localPort &&
			entry.remote == remote && entry.remotePort == remotePort {
			return true
		}
	}
	return false
}

func (stack *Stack) tcpEphemeralPort(local, remote IPv4, remotePort uint16, skip int) uint16 {
	first, last := uint32(stack.tuning.TCP.EphemeralMin), uint32(stack.tuning.TCP.EphemeralMax)
	if first == 0 || last < first {
		return 0
	}
	span := last - first + 1
	port := first + uint32(stack.next()%uint64(span))
	for attempt := uint32(0); attempt < span+4; attempt++ {
		if !stack.tcpBindConflict(skip, local, uint16(port)) && !stack.tcpTupleConflict(skip, local, uint16(port), remote, remotePort) {
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

func (stack *Stack) tcpPathMSS(route udpRouteResult) uint16 {
	mtu := int(stack.mtu)
	if route.path == udpLocal {
		mtu = LoopbackMTU
	}
	if mtu < ipv4HeaderSize+tcpHeaderSize+1 {
		return 1
	}
	mss := mtu - ipv4HeaderSize - tcpHeaderSize
	if mss > TCPReorderPayload {
		mss = TCPReorderPayload
	}
	return uint16(mss)
}
