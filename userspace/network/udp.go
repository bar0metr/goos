package network

// UDP endpoints (ADR 0195). An endpoint is a local address and port, an
// optional connected peer and a queue of received datagrams. The service
// that runs the stack names an owner for each endpoint, one per client
// session, and every call checks it: one owner can neither see nor touch
// another's endpoints.
//
// The endpoint registry and the receive buffer pool grow on demand up to
// the capacities below (udp_registry.go); UDPTuning bounds how much of
// them is in use. Received payloads wait in the pool, shared by all
// endpoints, and each endpoint's queue holds indexes into it. The stack
// counts each owner's endpoints and queued buffers, so neither the quota
// nor the buffer share check walks the registry.
//
// Delivery is unambiguous by construction. Bind refuses a port another
// endpoint holds on the same address or on the wildcard, and a wildcard bind
// refuses a port held on any address, unless both endpoints carry
// SO_REUSEADDR. A datagram goes to one endpoint: among those bound to its
// destination address, or else among those bound to the wildcard, the one
// bound last, as Linux does; the others receive nothing while it stays. A
// connected endpoint then takes it only from its peer.

const (
	// MaxUDPSessions bounds the owners, one per client session.
	MaxUDPSessions = 8
	// MaxUDPEndpoints is the ceiling of max-endpoints and
	// DefaultUDPEndpoints its default. MaxUDPSessionEndpoints is the ceiling
	// of max-endpoints-per-session, the ordinary session quota.
	// MaxUDPSessionQuota is the largest quota a session can be granted
	// explicitly, the same as TCP's so one control field serves both;
	// max-endpoints still bounds what a session holds.
	MaxUDPEndpoints        = 4096
	DefaultUDPEndpoints    = 1024
	MaxUDPSessionEndpoints = 8
	MaxUDPSessionQuota     = 512
	// UDPQueueCapacity is the capacity behind recv-queue-depth.
	UDPQueueCapacity = 16
	// UDPPayloadCapacity is the capacity behind max-payload and the size of
	// each receive buffer.
	UDPPayloadCapacity = 4096
	// UDPReceiveBuffers bounds the datagrams queued on all endpoints, and
	// UDPSessionBuffers one owner's share of them, so one session flooded
	// with datagrams leaves half the pool to the others. The pool allocates
	// a 4 KiB buffer when no free one is left and keeps it for reuse: 512
	// buffers take at most 2 MiB.
	UDPReceiveBuffers = 512
	UDPSessionBuffers = UDPReceiveBuffers / 2
	// UDPPendingSends holds datagrams waiting for ARP to resolve their next
	// hop.
	UDPPendingSends = 8

	udpWirePayload = MaxFrame - HeaderSize - ipv4HeaderSize - udpHeaderSize
	udpSlotBits    = 12
	udpGenerations = 1 << (32 - udpSlotBits)
)

// The slot field of an ID must address every endpoint the registry holds,
// and a queue entry every buffer.
var (
	_ [1<<udpSlotBits - MaxUDPEndpoints]struct{}
	_ [1<<16 - UDPReceiveBuffers]struct{}
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
	// reuse is SO_REUSEADDR as given at bind, and bound orders binds for
	// delivery among endpoints that share a port by reuse.
	reuse bool
	bound uint32
	queue [UDPQueueCapacity]uint16
	head  uint8
	count uint8
	// indexed marks an endpoint filed in udpPorts under hash, and portPrev
	// and portNext link its bucket.
	indexed            bool
	hash               uint32
	portPrev, portNext *udpEndpoint
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
	// LookupProbes counts the endpoints delivery looked at, and
	// MaxLookupProbes is the most one datagram needed.
	LookupProbes    uint64
	MaxLookupProbes uint64
	Endpoints       int
}

// UDPReceived describes one datagram UDPReceive took off a queue.
type UDPReceived struct {
	Source      IPv4
	SourcePort  uint16
	Destination IPv4
	Length      int
}

func (stack *Stack) resetUDP() {
	stack.udp, stack.udpUsed, stack.udpLive = nil, nil, 0
	stack.udpPorts = udpPortIndex{}
	stack.udpBuffers, stack.udpFree = nil, nil
	stack.udpOwnerBuffers = [MaxUDPSessions]int{}
	stack.udpOwnerEndpoints = [MaxUDPSessions]int{}
	// The seed derives from the random state without drawing from it.
	stack.udpSeed = mix64(stack.random ^ 0x3f84d5b5b5470917)
	for index := range stack.udpPending[:] {
		stack.udpPending[index].used = false
	}
	for index := range stack.udpQuota[:] {
		stack.udpQuota[index] = 0
	}
	stack.udpGeneration = 0
	stack.udpBinds = 0
	stack.udpStats = UDPStats{}
}

// SetUDPQuota sets how many endpoints owner may hold, as SetTCPQuota does
// for TCP.
func (stack *Stack) SetUDPQuota(owner, quota int) bool {
	if owner < 0 || owner >= MaxUDPSessions || quota < 0 || quota > MaxUDPSessionQuota {
		return false
	}
	stack.udpQuota[owner] = uint16(quota)
	return true
}

func (stack *Stack) udpOwnerQuota(owner int) int {
	tuned := int(stack.tuning.UDP.SessionEndpoints)
	quota := int(stack.udpQuota[owner])
	switch {
	case quota == 0:
		return tuned
	case quota > MaxUDPSessionEndpoints:
		return quota
	}
	return min(quota, tuned)
}

// endpoint returns owner's endpoint id names, or nil.
func (stack *Stack) endpoint(owner int, id EndpointID) (*udpEndpoint, int) {
	slot := id.slot()
	if owner < 0 || owner >= MaxUDPSessions || slot >= len(stack.udp) {
		return nil, -1
	}
	entry := stack.udp[slot]
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
	if stack.udpLive >= int(stack.tuning.UDP.MaxEndpoints) || stack.udpOwnerEndpoints[owner] >= stack.udpOwnerQuota(owner) {
		return 0, ResultBusy
	}
	free := stack.allocateUDP()
	if free < 0 {
		return 0, ResultBusy
	}
	stack.udpGeneration++
	if stack.udpGeneration == udpGenerations {
		stack.udpGeneration = 1
	}
	entry := stack.udp[free]
	*entry = udpEndpoint{state: UDPOpen, owner: uint8(owner), generation: stack.udpGeneration}
	stack.udpUsed[free/64] |= 1 << (free % 64)
	stack.udpLive++
	stack.udpOwnerEndpoints[owner]++
	return EndpointID(stack.udpGeneration<<udpSlotBits | uint32(free)), ResultOK
}

// UDPClose frees an endpoint and the datagrams queued on it.
func (stack *Stack) UDPClose(owner int, id EndpointID) Result {
	entry, slot := stack.endpoint(owner, id)
	if entry == nil {
		return ResultInvalidArgument
	}
	stack.closeEndpoint(entry, slot)
	return ResultOK
}

// UDPCloseOwner closes every endpoint of an owner whose session ended.
func (stack *Stack) UDPCloseOwner(owner int) {
	if owner < 0 || owner >= MaxUDPSessions || stack.udpOwnerEndpoints[owner] == 0 {
		return
	}
	for index := stack.nextUDP(0); index >= 0; index = stack.nextUDP(index + 1) {
		if int(stack.udp[index].owner) == owner {
			stack.closeEndpoint(stack.udp[index], index)
		}
	}
}

func (stack *Stack) closeEndpoint(entry *udpEndpoint, slot int) {
	for entry.count > 0 {
		stack.releaseBuffer(entry.owner, entry.queue[entry.head])
		entry.head = (entry.head + 1) % UDPQueueCapacity
		entry.count--
	}
	stack.udpPorts.remove(entry)
	stack.udpOwnerEndpoints[entry.owner]--
	stack.udpLive--
	stack.udpUsed[slot/64] &^= 1 << (slot % 64)
	generation := entry.generation
	*entry = udpEndpoint{generation: generation}
}

// UDPBind gives an open endpoint a local address and port. The address is
// the wildcard, the interface's current address or a loopback host address;
// port 0 picks a free ephemeral port. A port that would make delivery
// ambiguous is AddressInUse.
func (stack *Stack) UDPBind(owner int, id EndpointID, address IPv4, port uint16) (IPv4, uint16, Result) {
	return stack.UDPBindReuse(owner, id, address, port, false)
}

// UDPBindReuse binds with SO_REUSEADDR when reuse is set: a bind that would
// conflict succeeds when every endpoint it overlaps carries reuse too.
func (stack *Stack) UDPBindReuse(owner int, id EndpointID, address IPv4, port uint16, reuse bool) (IPv4, uint16, Result) {
	entry, _ := stack.endpoint(owner, id)
	if entry == nil || entry.state != UDPOpen {
		return IPv4{}, 0, ResultInvalidArgument
	}
	if !stack.localAddress(address) {
		increment(&stack.udpStats.BindFail)
		return IPv4{}, 0, ResultNoAddress
	}
	if port == 0 {
		port = stack.ephemeralPort()
		if port == 0 {
			increment(&stack.udpStats.BindFail)
			return IPv4{}, 0, ResultBusy
		}
	} else if stack.portConflict(entry, address, port, reuse) {
		increment(&stack.udpStats.BindFail)
		return IPv4{}, 0, ResultAddressInUse
	}
	entry.local, entry.port, entry.state, entry.reuse = address, port, UDPBound, reuse
	stack.markBound(entry)
	return address, port, ResultOK
}

// UDPConnect fixes an endpoint's peer. It sends nothing. From then on the
// endpoint sends only to the peer and keeps only what the peer sends. An
// endpoint without a port gets an ephemeral one and the local address the
// peer is reached from now. An endpoint bound explicitly keeps its binding:
// one bound to the wildcard stays on the wildcard, picks its source address
// at each send and so follows an address change. The returned address is
// the source the peer is reached from now. A connected endpoint cannot be
// connected again.
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
		entry.local, entry.port = route.source, local
		stack.markBound(entry)
	}
	entry.remote, entry.remotePort, entry.state = remote, port, UDPConnected
	return route.source, entry.port, ResultOK
}

// localAddress reports an address an endpoint may bind.
func (stack *Stack) localAddress(address IPv4) bool {
	return address == zeroIPv4 || loopbackHost(address) || stack.address != zeroIPv4 && address == stack.address
}

// portConflict reports a bound endpoint other than skip on port whose
// address overlaps address, equal to it or either one the wildcard, unless
// reuse and the endpoint's reuse are both set. Port 68 belongs to the DHCP
// client while the interface runs DHCP.
func (stack *Stack) portConflict(skip *udpEndpoint, address IPv4, port uint16, reuse bool) bool {
	if port == portClient && stack.config.Mode == ModeDHCP {
		return true
	}
	for entry := stack.udpBucket(port); entry != nil; entry = entry.portNext {
		if entry == skip || entry.port != port {
			continue
		}
		if (entry.local == zeroIPv4 || address == zeroIPv4 || entry.local == address) && (!reuse || !entry.reuse) {
			return true
		}
	}
	return false
}

// markBound stamps an endpoint that just got its port with the bind order
// delivery uses and files it under the port.
func (stack *Stack) markBound(entry *udpEndpoint) {
	stack.udpBinds++
	entry.bound = stack.udpBinds
	stack.udpPorts.insert(entry, stack.udpPortHash(entry.port))
}

func (stack *Stack) portBound(port uint16) bool {
	for entry := stack.udpBucket(port); entry != nil; entry = entry.portNext {
		if entry.port == port {
			return true
		}
	}
	return false
}

// ephemeralPort draws a random port in the tuned ephemeral range that no
// endpoint holds, or returns 0 when there is none. After four random draws
// it walks the range from the last one, and at most MaxUDPEndpoints ports
// are taken, so the walk ends within MaxUDPEndpoints+1 steps unless the
// range is that small, and then within the range. Each step reads one
// bucket of the port index.
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
	buffer := stack.udpBuffers[entry.queue[entry.head]]
	if len(dst) < int(buffer.length) {
		return UDPReceived{}, false
	}
	received := UDPReceived{Source: buffer.source, SourcePort: buffer.sourcePort, Destination: buffer.destination,
		Length: copy(dst, buffer.data[:buffer.length])}
	stack.releaseBuffer(entry.owner, entry.queue[entry.head])
	entry.head = (entry.head + 1) % UDPQueueCapacity
	entry.count--
	return received, true
}

// UDPReady is an endpoint's readiness mask.
type UDPReady uint8

const (
	// UDPReadyReadable: a datagram is queued, perhaps an empty one.
	UDPReadyReadable UDPReady = 1 << iota
	// UDPReadyWritable: a send is not refused for lack of local resources
	// the endpoint holds. A UDP endpoint holds none, so it is always set.
	UDPReadyWritable

	UDPReadyAll = UDPReadyWritable<<1 - 1
)

// UDPReadiness is the mask, the length of the next datagram and the number
// queued.
type UDPReadiness struct {
	Mask   UDPReady
	Next   uint32
	Queued uint32
}

// UDPReadiness reports owner's endpoint's readiness. It changes nothing.
func (stack *Stack) UDPReadiness(owner int, id EndpointID) (UDPReadiness, Result) {
	entry, _ := stack.endpoint(owner, id)
	if entry == nil {
		return UDPReadiness{}, ResultInvalidArgument
	}
	ready := UDPReadiness{Mask: UDPReadyWritable, Queued: uint32(entry.count)}
	if entry.count > 0 {
		ready.Mask |= UDPReadyReadable
		ready.Next = uint32(stack.udpBuffers[entry.queue[entry.head]].length)
	}
	return ready, ResultOK
}

// UDPPeek copies the oldest datagram queued on an endpoint into dst and
// leaves it queued. It reports false as UDPReceive does.
func (stack *Stack) UDPPeek(owner int, id EndpointID, dst []byte) (UDPReceived, bool) {
	entry, _ := stack.endpoint(owner, id)
	if entry == nil || entry.count == 0 {
		return UDPReceived{}, false
	}
	buffer := stack.udpBuffers[entry.queue[entry.head]]
	if len(dst) < int(buffer.length) {
		return UDPReceived{}, false
	}
	return UDPReceived{Source: buffer.source, SourcePort: buffer.sourcePort, Destination: buffer.destination,
		Length: copy(dst, buffer.data[:buffer.length])}, true
}

// deliverUDP queues one checked datagram on the endpoint it belongs to, or
// drops it. Datagrams from the link, from lo and to the interface's own
// address all arrive here. A full queue or buffer pool drops the newcomer
// and keeps what is queued.
func (stack *Stack) deliverUDP(source IPv4, sourcePort uint16, destination IPv4, destinationPort uint16, payload []byte) {
	entry := stack.udpMatch(destination, destinationPort)
	if entry == nil {
		increment(&stack.udpStats.DropNoEndpoint)
		return
	}
	if entry.state == UDPConnected && (entry.remote != source || entry.remotePort != sourcePort) {
		increment(&stack.udpStats.DropNoEndpoint)
		return
	}
	if len(payload) > int(stack.tuning.UDP.MaxPayload) {
		increment(&stack.udpStats.DropTooLong)
		return
	}
	if entry.count >= stack.tuning.UDP.ReceiveQueue || entry.count >= UDPQueueCapacity ||
		stack.udpOwnerBuffers[entry.owner] >= UDPSessionBuffers {
		increment(&stack.udpStats.DropQueueFull)
		return
	}
	free := stack.takeBuffer()
	if free < 0 {
		increment(&stack.udpStats.DropQueueFull)
		return
	}
	buffer := stack.udpBuffers[free]
	buffer.used, buffer.source, buffer.sourcePort, buffer.destination = true, source, sourcePort, destination
	buffer.length = uint16(copy(buffer.data[:], payload))
	entry.queue[(entry.head+entry.count)%UDPQueueCapacity] = uint16(free)
	entry.count++
	stack.udpOwnerBuffers[entry.owner]++
	increment(&stack.udpStats.RX)
}

// udpMatch finds the endpoint bound to destination and port, or else the one
// bound to the wildcard on port, or returns nil. Among endpoints that share
// the binding by reuse, the one bound last wins. It reads the port's bucket
// and counts the endpoints it looked at.
func (stack *Stack) udpMatch(destination IPv4, port uint16) *udpEndpoint {
	var exact, wildcard *udpEndpoint
	probes := uint64(0)
	for entry := stack.udpBucket(port); entry != nil; entry = entry.portNext {
		probes++
		if entry.port != port {
			continue
		}
		if entry.local == destination && (exact == nil || later(entry.bound, exact.bound)) {
			exact = entry
		}
		if entry.local == zeroIPv4 && (wildcard == nil || later(entry.bound, wildcard.bound)) {
			wildcard = entry
		}
	}
	stack.udpStats.LookupProbes += probes
	stack.udpStats.MaxLookupProbes = max(stack.udpStats.MaxLookupProbes, probes)
	if exact == nil {
		exact = wildcard
	}
	if udpCrossCheck && exact != stack.slowUDPMatch(destination, port) {
		panic("udp: port index disagrees with the endpoints")
	}
	return exact
}

// slowUDPMatch is udpMatch by a walk of every endpoint, for the cross-check.
func (stack *Stack) slowUDPMatch(destination IPv4, port uint16) *udpEndpoint {
	var exact, wildcard *udpEndpoint
	for index := stack.nextUDP(0); index >= 0; index = stack.nextUDP(index + 1) {
		entry := stack.udp[index]
		if entry.state < UDPBound || entry.port != port {
			continue
		}
		if entry.local == destination && (exact == nil || later(entry.bound, exact.bound)) {
			exact = entry
		}
		if entry.local == zeroIPv4 && (wildcard == nil || later(entry.bound, wildcard.bound)) {
			wildcard = entry
		}
	}
	if exact == nil {
		exact = wildcard
	}
	return exact
}

// later compares bind stamps across the wrap of the counter.
func later(a, b uint32) bool { return int32(a-b) > 0 }

// UDPStats reports the UDP counters and the endpoints open now.
func (stack *Stack) UDPStats() UDPStats {
	stats := stack.udpStats
	stats.Endpoints = stack.udpLive
	return stats
}
