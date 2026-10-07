package network

const (
	millisecond = uint64(1_000_000)
	second      = 1000 * millisecond

	// TxQueueFrames bounds frames built but not yet taken by the service.
	TxQueueFrames = 8
	// minMTU is the datagram size every IPv4 host must accept, and the size
	// DHCP assumes.
	minMTU = 576

	maxCounter = ^uint64(0)
)

// Snapshot is the interface view a status request reports.
type Snapshot struct {
	State State
	Mode  Mode
	Link  bool
	MAC   MAC
	// Address, Prefix and Gateway are zero until an address is applied.
	Address IPv4
	Prefix  uint8
	Gateway IPv4
	// MTU is the largest IPv4 datagram the link carries, zero without a link.
	MTU uint16
	// LeaseRemaining is in whole seconds and is zero outside a DHCP lease.
	LeaseRemaining uint64
	RX             uint64
	TX             uint64
	Drop           uint64
	Malformed      uint64
	// Conflicts counts address-conflict events; ConflictRun is the current
	// run of conflicts without an address that held, and RateLimited reports
	// that the next claim waits for the rate limit.
	Conflicts   Conflicts
	ConflictRun uint8
	RateLimited bool
	// Loopback counts echoes answered on the loopback interface.
	Loopback uint64
	UDP      UDPStats
	TCP      TCPStats
}

// Stack is the whole state of one interface. It holds no pointers and every
// table and the transmit ring are fixed arrays, so a service can keep one in
// static storage.
//
// Every address passes the conflict detection in conflict.go before it goes
// live. The loopback range never reaches the link: see loopback.go.
type Stack struct {
	random     uint64
	state      State
	failResult Result
	config     Config
	tuning     Tuning
	link       bool
	mac        MAC
	mtu        uint16

	// address, prefix and gateway are the applied configuration. address is
	// nonzero exactly when the state is Ready.
	address IPv4
	prefix  uint8
	gateway IPv4

	identification uint16
	echoBase       uint16
	cursor         int

	rx, tx, drop, malformed uint64

	replyWindow uint64
	replyCount  uint16

	neighbors   [MaxNeighbors]neighbor
	resolutions [MaxResolutions]resolution
	sessions    [MaxSessions]echoSession
	probe       probe
	announcer   announcer
	dhcp        dhcpClient

	udp           [MaxUDPEndpoints]udpEndpoint
	udpBuffers    [UDPReceiveBuffers]udpBuffer
	udpPending    [UDPPendingSends]udpPending
	udpGeneration uint32
	udpStats      UDPStats

	tcp           [MaxTCPEndpoints]tcpEndpoint
	tcpGeneration uint32
	tcpStats      TCPStats

	// defended and defendedAt record the last defence of the live address.
	defended   bool
	defendedAt uint64
	// dns is the server list of the bound lease.
	dns           [MaxDNSServers]IPv4
	dnsCount      uint8
	dnsGeneration uint64
	conflicts     uint8
	rateLimited   bool
	counters      Conflicts
	loopback      uint64

	frames  [TxQueueFrames][MaxFrame]byte
	lengths [TxQueueFrames]uint16
	txHead  int
	txCount int
}

// Init resets the stack to Down with the default DHCP configuration and
// tuning. Fields are
// reset one by one: a literal of the whole struct would be a temporary larger
// than TinyGo keeps off the heap.
func (stack *Stack) Init(seed uint64) {
	stack.random = seed
	stack.state = StateDown
	stack.failResult = ResultOK
	stack.config = Config{Mode: ModeDHCP}
	stack.tuning = DefaultTuning()
	stack.link = false
	stack.mac = MAC{}
	stack.mtu = 0
	stack.clearAddress()
	stack.identification = uint16(stack.next())
	stack.echoBase = uint16(stack.next())
	stack.cursor = 0
	stack.rx, stack.tx, stack.drop, stack.malformed = 0, 0, 0, 0
	stack.replyWindow, stack.replyCount = 0, 0
	stack.clearNeighbors()
	for index := range stack.sessions[:] {
		stack.sessions[index] = echoSession{}
	}
	stack.probe = probe{}
	stack.announcer = announcer{}
	stack.dhcp = dhcpClient{}
	stack.resetUDP()
	stack.resetTCP()
	stack.defended, stack.defendedAt = false, 0
	stack.conflicts, stack.rateLimited = 0, false
	stack.counters = Conflicts{}
	stack.loopback = 0
	stack.dns, stack.dnsCount, stack.dnsGeneration = [MaxDNSServers]IPv4{}, 0, 0
	stack.flushFrames()
}

// next is splitmix64. It only has to make transaction IDs, jitter and echo
// tokens unpredictable to an off-path sender, given a seed from the service.
func (stack *Stack) next() uint64 {
	stack.random += 0x9e3779b97f4a7c15
	value := stack.random
	value = (value ^ value>>30) * 0xbf58476d1ce4e5b9
	value = (value ^ value>>27) * 0x94d049bb133111eb
	return value ^ value>>31
}

func increment(counter *uint64) {
	if *counter != maxCounter {
		*counter++
	}
}

func addTime(now, delay uint64) uint64 {
	if now > maxCounter-delay {
		return maxCounter
	}
	return now + delay
}

func (stack *Stack) countMalformed() { increment(&stack.malformed) }

// CountDrop adds a frame the service dropped before it reached the stack.
func (stack *Stack) CountDrop() { increment(&stack.drop) }

// LinkUp reports a ready driver with carrier. A new MAC means a different
// device, so everything learned on the old one is discarded. An invalid MAC
// or an MTU below the IPv4 minimum is a device fault.
func (stack *Stack) LinkUp(mac MAC, mtu uint16, now uint64) {
	if !unicastMAC(mac) || mtu < minMTU {
		stack.Fail(now)
		return
	}
	if mtu > MTU {
		mtu = MTU
	}
	if stack.link && stack.mac == mac && stack.state != StateFailed {
		stack.mtu = mtu
		return
	}
	if stack.link {
		stack.resetAddressing(ResultDriverFault)
		stack.flushFrames()
	}
	stack.link, stack.mac, stack.mtu = true, mac, mtu
	stack.startConfiguring(now)
}

// LinkDown reports lost carrier (ResultDown) or a dead driver
// (ResultDriverFault). Pending echoes complete with that reason; frames still
// queued for the old link are discarded.
func (stack *Stack) LinkDown(reason Result, now uint64) {
	if reason != ResultDriverFault {
		reason = ResultDown
	}
	stack.resetAddressing(reason)
	stack.flushFrames()
	stack.link = false
	stack.state = StateDown
}

// Fail records an unrecoverable device fault. The stack stays Failed until the
// next LinkUp or Configure.
func (stack *Stack) Fail(now uint64) {
	stack.resetAddressing(ResultDriverFault)
	stack.flushFrames()
	stack.state = StateFailed
	stack.failResult = ResultDriverFault
}

// Apply validates a configuration and a tuning together and applies both, or
// neither: an invalid value in either changes nothing. A tuning change alone
// restarts nothing; see Tuning for when each value takes effect.
func (stack *Stack) Apply(config Config, tuning Tuning, now uint64) Field {
	if field := config.Validate(); field != FieldNone {
		return field
	}
	if field := tuning.Validate(); field != FieldNone {
		return field
	}
	stack.tuning = tuning
	return stack.Configure(config, now)
}

// Tuning returns the policy in use.
func (stack *Stack) Tuning() Tuning { return stack.tuning }

// Configure validates and applies a configuration in one step. An invalid one
// changes nothing. A new mode or address discards neighbours and fails
// pending echoes with ResultReconfigured; a static prefix or gateway change on
// the same address applies in place. While Failed, even an identical
// configuration restarts address assignment, which is what a reload is for.
func (stack *Stack) Configure(config Config, now uint64) Field {
	if field := config.Validate(); field != FieldNone {
		return field
	}
	config = config.normalized()
	previous := stack.config
	if config == previous && stack.state != StateFailed {
		return FieldNone
	}
	stack.config = config
	if !stack.link {
		stack.resetAddressing(ResultReconfigured)
		stack.state = StateDown
		return FieldNone
	}
	if config.Mode != previous.Mode || config.Address != previous.Address || stack.state == StateFailed {
		stack.resetAddressing(ResultReconfigured)
		stack.startConfiguring(now)
		return FieldNone
	}
	if stack.state == StateReady {
		stack.prefix, stack.gateway = config.Prefix, config.Gateway
	}
	return FieldNone
}

func (stack *Stack) startConfiguring(now uint64) {
	stack.state = StateConfiguring
	if stack.config.Mode == ModeDHCP {
		stack.startDiscovery(now)
		return
	}
	stack.startProbe(stack.config.Address, now)
}

func (stack *Stack) clearAddress() {
	stack.address, stack.prefix, stack.gateway = IPv4{}, 0, IPv4{}
}

// resetAddressing removes the address and everything that depends on it.
// Echoes fail first so none can match a reply after the address is gone, and
// datagrams waiting for ARP are dropped. Loopback echoes never wait, so none
// are affected. UDP endpoints stay: one bound to the wildcard or to lo keeps
// working, and one bound to the old address refuses to send.
func (stack *Stack) resetAddressing(result Result) {
	stack.failEchoes(result)
	stack.dropPendingUDP(IPv4{}, true)
	stack.reconfigureTCP(result)
	stack.clearAddress()
	stack.clearNeighbors()
	stack.probe = probe{}
	stack.announcer = announcer{}
	stack.defended = false
	stack.dhcp.phase = dhcpOff
	stack.clearDNS()
	if stack.state == StateReady {
		stack.state = StateConfiguring
	}
}

// Receive processes one Ethernet frame. RX counts every call; a frame outside
// 14..1514 bytes is malformed and never truncated into something parseable.
func (stack *Stack) Receive(frame []byte, now uint64) {
	increment(&stack.rx)
	ether, ok := parseEthernet(frame)
	if !ok {
		stack.countMalformed()
		return
	}
	if !stack.link || stack.state == StateFailed {
		stack.CountDrop()
		return
	}
	if ether.destination != stack.mac && ether.destination != broadcastMAC {
		stack.CountDrop()
		return
	}
	if !unicastMAC(ether.source) || ether.source == stack.mac {
		stack.CountDrop()
		return
	}
	switch ether.etherType {
	case etherTypeARP:
		stack.receiveARP(ether.payload, now)
	case etherTypeIPv4:
		stack.receiveIPv4(ether, now)
	default:
		stack.CountDrop()
	}
}

func (stack *Stack) receiveIPv4(ether ethernetFrame, now uint64) {
	packet, status := parseIPv4(ether.payload)
	switch status {
	case parseMalformed:
		stack.countMalformed()
		return
	case parseUnsupported:
		stack.CountDrop()
		return
	}
	if !stack.acceptDestination(packet) || !packet.source.IsUnicast() || packet.source == stack.address {
		stack.CountDrop()
		return
	}
	switch packet.protocol {
	case protocolICMP:
		stack.receiveICMP(ether, packet, now)
	case protocolUDP:
		stack.receiveUDP(ether, packet, now)
	case protocolTCP:
		stack.receiveTCP(packet, now)
	default:
		stack.CountDrop()
	}
}

// acceptDestination admits our own address and, for DHCP only, limited
// broadcast. Before an address exists a server may also unicast to the
// address it is offering; the transaction ID and MAC checks still apply.
func (stack *Stack) acceptDestination(packet ipv4Packet) bool {
	if stack.address != zeroIPv4 && packet.destination == stack.address {
		return true
	}
	if packet.protocol != protocolUDP || !stack.dhcp.listening() {
		return false
	}
	if packet.destination == broadcastIPv4 {
		return true
	}
	return stack.address == zeroIPv4 && packet.destination.IsUnicast()
}

// receiveUDP hands DHCP's server-to-client datagrams to the DHCP client
// while the interface runs DHCP, and the rest to the endpoints. Only a
// datagram to the interface's address reaches an endpoint: the broadcasts
// and offers acceptDestination admits are DHCP's alone.
func (stack *Stack) receiveUDP(ether ethernetFrame, packet ipv4Packet, now uint64) {
	datagram, status := parseUDP(packet)
	switch status {
	case udpMalformed:
		stack.countMalformed()
		return
	case udpBadChecksum:
		increment(&stack.udpStats.DropChecksum)
		return
	}
	if datagram.sourcePort == portServer && datagram.destinationPort == portClient && stack.config.Mode == ModeDHCP {
		stack.receiveDHCP(ether.source, datagram.payload, now)
		return
	}
	if stack.address == zeroIPv4 || packet.destination != stack.address {
		stack.CountDrop()
		return
	}
	stack.deliverUDP(packet.source, datagram.sourcePort, packet.destination, datagram.destinationPort, datagram.payload)
}

// Advance runs every timer due at now. Each handler schedules its next
// deadline strictly after now, so one pass leaves nothing due.
func (stack *Stack) Advance(now uint64) {
	stack.advanceEchoes(now)
	stack.advanceResolutions(now)
	stack.advanceTCP(now)
	if stack.probe.active && stack.probe.deadline <= now {
		stack.advanceProbe(now)
	}
	if stack.announcer.active && stack.announcer.deadline <= now {
		stack.advanceAnnouncer(now)
	}
	if stack.dhcp.timed() && stack.dhcp.deadline <= now {
		stack.advanceDHCP(now)
	}
}

// Deadline returns the earliest pending timer, or false when only an
// incoming frame or request can change anything.
func (stack *Stack) Deadline() (uint64, bool) {
	var earliest timer
	for index := range stack.sessions[:] {
		session := &stack.sessions[index]
		if session.phase == echoResolving || session.phase == echoSent {
			earliest.consider(session.deadline)
		}
	}
	for index := range stack.resolutions[:] {
		if stack.resolutions[index].used {
			earliest.consider(stack.resolutions[index].deadline)
		}
	}
	if stack.probe.active {
		earliest.consider(stack.probe.deadline)
	}
	if stack.announcer.active {
		earliest.consider(stack.announcer.deadline)
	}
	if stack.dhcp.timed() {
		earliest.consider(stack.dhcp.deadline)
	}
	stack.tcpDeadline(&earliest)
	return earliest.deadline, earliest.found
}

type timer struct {
	deadline uint64
	found    bool
}

func (earliest *timer) consider(deadline uint64) {
	if !earliest.found || deadline < earliest.deadline {
		earliest.deadline, earliest.found = deadline, true
	}
}

// frameBuffer returns the next free transmit slot, or nil after counting a
// drop when the ring is full. The frame exists only once commitFrame runs.
func (stack *Stack) frameBuffer() []byte {
	if stack.txCount == TxQueueFrames {
		stack.CountDrop()
		return nil
	}
	return stack.frames[(stack.txHead+stack.txCount)%TxQueueFrames][:]
}

func (stack *Stack) commitFrame(length int) {
	index := (stack.txHead + stack.txCount) % TxQueueFrames
	frame := stack.frames[index][:]
	for position := length; position < minFrame; position++ {
		frame[position] = 0
	}
	if length < minFrame {
		length = minFrame
	}
	stack.lengths[index] = uint16(length)
	stack.txCount++
}

func (stack *Stack) flushFrames() {
	stack.txHead, stack.txCount = 0, 0
}

// NextFrame moves the oldest outgoing frame into dst and counts it as sent. A
// dst too short for that frame leaves it queued.
func (stack *Stack) NextFrame(dst []byte) (int, bool) {
	if stack.txCount == 0 {
		return 0, false
	}
	length := int(stack.lengths[stack.txHead])
	if len(dst) < length {
		return 0, false
	}
	copy(dst, stack.frames[stack.txHead][:length])
	stack.txHead = (stack.txHead + 1) % TxQueueFrames
	stack.txCount--
	increment(&stack.tx)
	return length, true
}

func (stack *Stack) PendingFrames() int { return stack.txCount }

func (stack *Stack) Snapshot(now uint64) Snapshot {
	snapshot := Snapshot{
		State:     stack.state,
		Mode:      stack.config.Mode,
		Link:      stack.link,
		MAC:       stack.mac,
		Address:   stack.address,
		Prefix:    stack.prefix,
		Gateway:   stack.gateway,
		RX:        stack.rx,
		TX:        stack.tx,
		Drop:      stack.drop,
		Malformed: stack.malformed,
		MTU:       stack.mtu,

		Conflicts:   stack.counters,
		ConflictRun: stack.conflicts,
		RateLimited: stack.rateLimited,
		Loopback:    stack.loopback,
		UDP:         stack.UDPStats(),
		TCP:         stack.TCPStats(),
	}
	if stack.address != zeroIPv4 && stack.dhcp.leased() && stack.dhcp.expiry > now {
		snapshot.LeaseRemaining = (stack.dhcp.expiry - now) / second
	}
	return snapshot
}
