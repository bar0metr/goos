package network

// UDP endpoints (ADR 0195). An endpoint is a local address and port, an
// optional connected peer and a queue of received datagrams. The service
// that runs the stack names an owner for each endpoint, one per client
// session, and every call checks it: one owner can neither see nor touch
// another's endpoints.
//
// Every table is a fixed array sized by the constants below. UDPTuning sets
// how much of each one is in use at run time and never resizes anything.
// Received payloads wait in a pool of buffers shared by all endpoints; each
// endpoint's queue holds indexes into it.
//
// Delivery is unambiguous by construction. Bind refuses a port another
// endpoint holds on the same address or on the wildcard, and a wildcard bind
// refuses a port held on any address, so a datagram matches at most one
// endpoint: the one bound to its destination address, or else the one bound
// to the wildcard. A connected endpoint then takes it only from its peer.

const (
	// MaxUDPSessions bounds the owners, one per client session.
	MaxUDPSessions = 8
	// MaxUDPEndpoints and MaxUDPSessionEndpoints are the capacities behind
	// the max-endpoints and max-endpoints-per-session settings.
	MaxUDPEndpoints        = 32
	MaxUDPSessionEndpoints = 8
	// UDPQueueCapacity is the capacity behind recv-queue-depth.
	UDPQueueCapacity = 16
	// UDPPayloadCapacity is the capacity behind max-payload and the size of
	// each receive buffer.
	UDPPayloadCapacity = 4096
	// UDPReceiveBuffers holds the datagrams queued on all endpoints, and
	// UDPSessionBuffers bounds one owner's share of them, so one session
	// flooded with datagrams leaves half the pool to the others.
	UDPReceiveBuffers = 32
	UDPSessionBuffers = UDPReceiveBuffers / 2
	// UDPPendingSends holds datagrams waiting for ARP to resolve their next
	// hop.
	UDPPendingSends = 8

	udpWirePayload = MaxFrame - HeaderSize - ipv4HeaderSize - udpHeaderSize
	udpSlotBits    = 8
	udpGenerations = 1 << (32 - udpSlotBits)
)

// EndpointID names one endpoint: a generation above the table slot. A
// closed endpoint's slot gets a new generation when it is opened again, so
// an ID kept after Close matches nothing. Zero is never an ID.
type EndpointID uint32

func (id EndpointID) slot() int          { return int(id & (1<<udpSlotBits - 1)) }
func (id EndpointID) generation() uint32 { return uint32(id) >> udpSlotBits }

// UDPState is an endpoint's place in its life: open after Open, bound once
// it has a port, connected once it has a peer.
type UDPState uint8

const (
	UDPFree UDPState = iota
	UDPOpen
	UDPBound
	UDPConnected
)

type udpEndpoint struct {
	state      UDPState
	owner      uint8
	generation uint32
	// local is zero for the wildcard.
	local      IPv4
	port       uint16
	remote     IPv4
	remotePort uint16
	queue      [UDPQueueCapacity]uint8
	head       uint8
	count      uint8
}

type udpBuffer struct {
	used        bool
	source      IPv4
	destination IPv4
	sourcePort  uint16
	length      uint16
	data        [UDPPayloadCapacity]byte
}

// UDPStats are the UDP counters a status request reports. Endpoints is the
// number open now; the rest only grow and saturate.
type UDPStats struct {
	RX             uint64
	TX             uint64
	DropNoEndpoint uint64
	DropQueueFull  uint64
	DropChecksum   uint64
	DropTooLong    uint64
	BindFail       uint64
	Endpoints      int
}

// UDPReceived describes one datagram UDPReceive took off a queue.
type UDPReceived struct {
	Source      IPv4
	SourcePort  uint16
	Destination IPv4
	Length      int
}

func (stack *Stack) resetUDP() {
	for index := range stack.udp[:] {
		stack.udp[index] = udpEndpoint{}
	}
	for index := range stack.udpBuffers[:] {
		stack.udpBuffers[index].used = false
	}
	for index := range stack.udpPending[:] {
		stack.udpPending[index].used = false
	}
	stack.udpGeneration = 0
	stack.udpStats = UDPStats{}
}

// endpoint returns owner's endpoint id names, or nil.
func (stack *Stack) endpoint(owner int, id EndpointID) (*udpEndpoint, int) {
	slot := id.slot()
	if owner < 0 || owner >= MaxUDPSessions || slot >= MaxUDPEndpoints {
		return nil, -1
	}
	entry := &stack.udp[slot]
	if entry.state == UDPFree || entry.generation != id.generation() || int(entry.owner) != owner {
		return nil, -1
	}
	return entry, slot
}

// UDPOpen creates an unbound endpoint for owner. It is Busy when the service
// or the owner already has as many endpoints as the tuning allows.
func (stack *Stack) UDPOpen(owner int) (EndpointID, Result) {
	if owner < 0 || owner >= MaxUDPSessions {
		return 0, ResultInvalidArgument
	}
	total, mine, free := 0, 0, -1
	for index := range stack.udp[:] {
		entry := &stack.udp[index]
		if entry.state == UDPFree {
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
	if free < 0 || total >= int(stack.tuning.UDP.MaxEndpoints) || mine >= int(stack.tuning.UDP.SessionEndpoints) {
		return 0, ResultBusy
	}
	stack.udpGeneration++
	if stack.udpGeneration == udpGenerations {
		stack.udpGeneration = 1
	}
	entry := &stack.udp[free]
	*entry = udpEndpoint{state: UDPOpen, owner: uint8(owner), generation: stack.udpGeneration}
	return EndpointID(stack.udpGeneration<<udpSlotBits | uint32(free)), ResultOK
}

// UDPClose frees an endpoint and the datagrams queued on it.
func (stack *Stack) UDPClose(owner int, id EndpointID) Result {
	entry, _ := stack.endpoint(owner, id)
	if entry == nil {
		return ResultInvalidArgument
	}
	stack.closeEndpoint(entry)
	return ResultOK
}

// UDPCloseOwner closes every endpoint of an owner whose session ended.
func (stack *Stack) UDPCloseOwner(owner int) {
	for index := range stack.udp[:] {
		entry := &stack.udp[index]
		if entry.state != UDPFree && int(entry.owner) == owner {
			stack.closeEndpoint(entry)
		}
	}
}

func (stack *Stack) closeEndpoint(entry *udpEndpoint) {
	for entry.count > 0 {
		stack.udpBuffers[entry.queue[entry.head]].used = false
		entry.head = (entry.head + 1) % UDPQueueCapacity
		entry.count--
	}
	generation := entry.generation
	*entry = udpEndpoint{generation: generation}
}

// UDPBind gives an open endpoint a local address and port. The address is
// the wildcard, the interface's current address or a loopback host address;
// port 0 picks a free ephemeral port. A port that would make delivery
// ambiguous is AddressInUse.
func (stack *Stack) UDPBind(owner int, id EndpointID, address IPv4, port uint16) (IPv4, uint16, Result) {
	entry, slot := stack.endpoint(owner, id)
	if entry == nil || entry.state != UDPOpen {
		return IPv4{}, 0, ResultInvalidArgument
	}
	if !stack.localAddress(address) {
		increment(&stack.udpStats.BindFail)
		return IPv4{}, 0, ResultInvalidArgument
	}
	if port == 0 {
		port = stack.ephemeralPort()
		if port == 0 {
			increment(&stack.udpStats.BindFail)
			return IPv4{}, 0, ResultBusy
		}
	} else if stack.portConflict(slot, address, port) {
		increment(&stack.udpStats.BindFail)
		return IPv4{}, 0, ResultAddressInUse
	}
	entry.local, entry.port, entry.state = address, port, UDPBound
	return address, port, ResultOK
}

// UDPConnect fixes an endpoint's peer. It sends nothing: it picks the local
// address the peer is reached from, binds an ephemeral port if the endpoint
// has none, and from then on the endpoint sends only to the peer and keeps
// only what the peer sends. A connected endpoint cannot be connected again.
func (stack *Stack) UDPConnect(owner int, id EndpointID, remote IPv4, port uint16, now uint64) (IPv4, uint16, Result) {
	entry, _ := stack.endpoint(owner, id)
	if entry == nil || entry.state == UDPConnected || port == 0 {
		return IPv4{}, 0, ResultInvalidArgument
	}
	route, result := stack.udpRoute(entry.local, remote)
	if result != ResultOK {
		return IPv4{}, 0, result
	}
	if entry.state == UDPOpen {
		local := stack.ephemeralPort()
		if local == 0 {
			increment(&stack.udpStats.BindFail)
			return IPv4{}, 0, ResultBusy
		}
		entry.port = local
	}
	entry.local, entry.remote, entry.remotePort, entry.state = route.source, remote, port, UDPConnected
	return entry.local, entry.port, ResultOK
}

// localAddress reports an address an endpoint may bind.
func (stack *Stack) localAddress(address IPv4) bool {
	return address == zeroIPv4 || loopbackHost(address) || stack.address != zeroIPv4 && address == stack.address
}

// portConflict reports a bound endpoint other than skip on port whose
// address overlaps address: equal to it, or either one the wildcard. Port 68
// belongs to the DHCP client while the interface runs DHCP.
func (stack *Stack) portConflict(skip int, address IPv4, port uint16) bool {
	if port == portClient && stack.config.Mode == ModeDHCP {
		return true
	}
	for index := range stack.udp[:] {
		entry := &stack.udp[index]
		if index == skip || entry.state < UDPBound || entry.port != port {
			continue
		}
		if entry.local == zeroIPv4 || address == zeroIPv4 || entry.local == address {
			return true
		}
	}
	return false
}

func (stack *Stack) portBound(port uint16) bool {
	for index := range stack.udp[:] {
		if stack.udp[index].state >= UDPBound && stack.udp[index].port == port {
			return true
		}
	}
	return false
}

// ephemeralPort draws a random port in the tuned ephemeral range that no
// endpoint holds, or returns 0 when there is none. After four random draws
// it walks the range from the last one, and at most MaxUDPEndpoints ports
// are taken, so the walk ends within MaxUDPEndpoints+1 steps unless the
// range is that small, and then within the range.
func (stack *Stack) ephemeralPort() uint16 {
	first, last := uint32(stack.tuning.UDP.EphemeralMin), uint32(stack.tuning.UDP.EphemeralMax)
	if first == 0 || last < first {
		return 0
	}
	span := last - first + 1
	port := first + uint32(stack.next()%uint64(span))
	for attempt := uint32(0); attempt < span+4; attempt++ {
		if !stack.portBound(uint16(port)) {
			return uint16(port)
		}
		if attempt < 4 {
			port = first + uint32(stack.next()%uint64(span))
			continue
		}
		port++
		if port > last {
			port = first
		}
	}
	return 0
}

// UDPQueued reports how many datagrams wait on a bound endpoint.
func (stack *Stack) UDPQueued(owner int, id EndpointID) (int, Result) {
	entry, _ := stack.endpoint(owner, id)
	if entry == nil || entry.state < UDPBound {
		return 0, ResultInvalidArgument
	}
	return int(entry.count), ResultOK
}

// UDPReceive moves the oldest datagram queued on an endpoint into dst. It
// reports false when there is none, or when dst is shorter than it, which
// leaves it queued.
func (stack *Stack) UDPReceive(owner int, id EndpointID, dst []byte) (UDPReceived, bool) {
	entry, _ := stack.endpoint(owner, id)
	if entry == nil || entry.count == 0 {
		return UDPReceived{}, false
	}
	buffer := &stack.udpBuffers[entry.queue[entry.head]]
	if len(dst) < int(buffer.length) {
		return UDPReceived{}, false
	}
	received := UDPReceived{Source: buffer.source, SourcePort: buffer.sourcePort, Destination: buffer.destination,
		Length: copy(dst, buffer.data[:buffer.length])}
	buffer.used = false
	entry.head = (entry.head + 1) % UDPQueueCapacity
	entry.count--
	return received, true
}

// deliverUDP queues one checked datagram on the endpoint it belongs to, or
// drops it. Datagrams from the link, from lo and to the interface's own
// address all arrive here. A full queue or buffer pool drops the newcomer
// and keeps what is queued.
func (stack *Stack) deliverUDP(source IPv4, sourcePort uint16, destination IPv4, destinationPort uint16, payload []byte) {
	slot := stack.udpMatch(destination, destinationPort)
	if slot < 0 {
		increment(&stack.udpStats.DropNoEndpoint)
		return
	}
	entry := &stack.udp[slot]
	if entry.state == UDPConnected && (entry.remote != source || entry.remotePort != sourcePort) {
		increment(&stack.udpStats.DropNoEndpoint)
		return
	}
	if len(payload) > int(stack.tuning.UDP.MaxPayload) {
		increment(&stack.udpStats.DropTooLong)
		return
	}
	if entry.count >= stack.tuning.UDP.ReceiveQueue || entry.count >= UDPQueueCapacity ||
		stack.ownerBuffers(entry.owner) >= UDPSessionBuffers {
		increment(&stack.udpStats.DropQueueFull)
		return
	}
	free := -1
	for index := range stack.udpBuffers[:] {
		if !stack.udpBuffers[index].used {
			free = index
			break
		}
	}
	if free < 0 {
		increment(&stack.udpStats.DropQueueFull)
		return
	}
	buffer := &stack.udpBuffers[free]
	buffer.used, buffer.source, buffer.sourcePort, buffer.destination = true, source, sourcePort, destination
	buffer.length = uint16(copy(buffer.data[:], payload))
	entry.queue[(entry.head+entry.count)%UDPQueueCapacity] = uint8(free)
	entry.count++
	increment(&stack.udpStats.RX)
}

func (stack *Stack) ownerBuffers(owner uint8) int {
	held := 0
	for index := range stack.udp[:] {
		if stack.udp[index].state != UDPFree && stack.udp[index].owner == owner {
			held += int(stack.udp[index].count)
		}
	}
	return held
}

// udpMatch finds the endpoint bound to destination and port, or else the one
// bound to the wildcard on port, or returns -1.
func (stack *Stack) udpMatch(destination IPv4, port uint16) int {
	wildcard := -1
	for index := range stack.udp[:] {
		entry := &stack.udp[index]
		if entry.state < UDPBound || entry.port != port {
			continue
		}
		if entry.local == destination {
			return index
		}
		if entry.local == zeroIPv4 && wildcard < 0 {
			wildcard = index
		}
	}
	return wildcard
}

// UDPStats reports the UDP counters and the endpoints open now.
func (stack *Stack) UDPStats() UDPStats {
	stats := stack.udpStats
	for index := range stack.udp[:] {
		if stack.udp[index].state != UDPFree {
			stats.Endpoints++
		}
	}
	return stats
}
