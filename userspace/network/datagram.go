package network

// Datagrams are single UDP exchanges for a privileged client of the service:
// send one datagram from a fresh random port, take at most one reply from the
// destination's address and port to that port, and stop listening at the
// deadline. The DNS resolver is the only client. There is no general socket:
// nothing listens on a port that no datagram opened.

const (
	MaxDatagrams = 8
	// DatagramPayload bounds a payload either way: the EDNS(0) size the
	// resolver advertises (RFC 9715), small enough that no datagram needs
	// fragmenting on a 1500-byte link.
	DatagramPayload = 1232
	// ephemeralFirst starts the dynamic port range (RFC 6335) source ports
	// come from.
	ephemeralFirst = 49152
)

type datagramPhase uint8

const (
	datagramIdle datagramPhase = iota
	datagramResolving
	datagramSent
	// datagramDone holds a result until the service takes it, as echoDone
	// does.
	datagramDone
)

type datagram struct {
	phase           datagramPhase
	result          Result
	destination     IPv4
	nextHop         IPv4
	local           IPv4
	sourcePort      uint16
	destinationPort uint16
	length          uint16
	deadline        uint64
	data            [DatagramPayload]byte
}

// clear frees the slot without zeroing its buffer, which length bounds.
func (entry *datagram) clear() {
	entry.phase, entry.result, entry.length = datagramIdle, ResultOK, 0
	entry.destination, entry.nextHop, entry.local = IPv4{}, IPv4{}, IPv4{}
	entry.sourcePort, entry.destinationPort, entry.deadline = 0, 0, 0
}

func (entry *datagram) waiting() bool {
	return entry.phase == datagramResolving || entry.phase == datagramSent
}

// DatagramResult is one finished datagram exchange. Length is the reply's
// size and is set only for ResultOK.
type DatagramResult struct {
	Slot   int
	Result Result
	Source IPv4
	Port   uint16
	Length int
}

// SendDatagram sends payload to destination:port from slot and listens for one
// reply until now+timeout. It checks arguments, then the slot, then the
// interface, like Echo. ResultOK means exactly one DatagramResult follows
// unless CancelDatagram drops the slot. Loopback has no UDP listener, so a
// loopback destination is RemoteUnreachable without touching the link.
func (stack *Stack) SendDatagram(slot int, destination IPv4, port uint16, payload []byte, timeout, now uint64) Result {
	if slot < 0 || slot >= MaxDatagrams || port == 0 || len(payload) == 0 || len(payload) > DatagramPayload || timeout == 0 {
		return ResultInvalidArgument
	}
	if destination.IsLoopback() {
		return ResultRemoteUnreachable
	}
	if !destination.IsUnicast() {
		return ResultInvalidArgument
	}
	entry := &stack.datagrams[slot]
	if entry.phase != datagramIdle {
		return ResultBusy
	}
	switch {
	case stack.state == StateFailed:
		return stack.failResult
	case !stack.link:
		return ResultDown
	case stack.address == zeroIPv4:
		return ResultNoAddress
	case stack.subnetSpecial(destination):
		return ResultInvalidArgument
	case destination == stack.address:
		return ResultRemoteUnreachable
	case ipv4HeaderSize+udpHeaderSize+len(payload) > int(stack.mtu):
		return ResultInvalidArgument
	}
	nextHop, result := stack.route(destination)
	if result != ResultOK {
		return result
	}
	entry.phase = datagramSent
	entry.destination, entry.nextHop, entry.local = destination, nextHop, stack.address
	entry.destinationPort = port
	entry.sourcePort = stack.freePort()
	entry.deadline = addTime(now, timeout)
	entry.length = uint16(copy(entry.data[:], payload))
	if mac, ok := stack.lookupNeighbor(nextHop, now); ok {
		stack.sendDatagram(entry, mac)
		return ResultOK
	}
	if !stack.resolve(nextHop, now) {
		entry.clear()
		return ResultBusy
	}
	entry.phase = datagramResolving
	return ResultOK
}

// freePort draws a random port in the dynamic range that no datagram in
// flight uses. Eight slots in 16384 ports make a clash rare; after a few
// draws it walks up from the last one, so the loop is bounded.
func (stack *Stack) freePort() uint16 {
	port := uint16(ephemeralFirst + stack.next()%(65536-ephemeralFirst))
	for attempt := 0; attempt < 65536-ephemeralFirst; attempt++ {
		if !stack.portInUse(port) {
			return port
		}
		if attempt < 4 {
			port = uint16(ephemeralFirst + stack.next()%(65536-ephemeralFirst))
			continue
		}
		port++
		if port < ephemeralFirst {
			port = ephemeralFirst
		}
	}
	return port
}

func (stack *Stack) portInUse(port uint16) bool {
	for index := range stack.datagrams[:] {
		entry := &stack.datagrams[index]
		if entry.phase != datagramIdle && entry.sourcePort == port {
			return true
		}
	}
	return false
}

// sendDatagram frames the payload once the next hop is known. A full
// transmit ring loses it, and the exchange times out.
func (stack *Stack) sendDatagram(entry *datagram, mac MAC) {
	entry.phase = datagramSent
	frame := stack.frameBuffer()
	if frame == nil {
		return
	}
	const udpAt = HeaderSize + ipv4HeaderSize
	length := int(entry.length)
	copy(frame[udpAt+udpHeaderSize:], entry.data[:length])
	putUDP(frame[udpAt:], entry.local, entry.destination, entry.sourcePort, entry.destinationPort, length)
	stack.identification++
	putIPv4Header(frame[HeaderSize:], stack.tuning.TTL, entry.local, entry.destination, protocolUDP,
		stack.identification, udpHeaderSize+length)
	putEthernet(frame, mac, stack.mac, etherTypeIPv4)
	stack.commitFrame(udpAt + udpHeaderSize + length)
}

// receiveDatagram takes a UDP reply for an open exchange: from the address
// and port it was sent to, to the port and address it was sent from. Anything
// else is a drop.
func (stack *Stack) receiveDatagram(packet *ipv4Packet, reply *udpDatagram) {
	for index := range stack.datagrams[:] {
		entry := &stack.datagrams[index]
		if entry.phase != datagramSent || reply.destinationPort != entry.sourcePort ||
			reply.sourcePort != entry.destinationPort || packet.source != entry.destination ||
			packet.destination != entry.local {
			continue
		}
		if len(reply.payload) > DatagramPayload {
			stack.CountDrop()
			return
		}
		entry.length = uint16(copy(entry.data[:], reply.payload))
		entry.phase, entry.result = datagramDone, ResultOK
		return
	}
	stack.CountDrop()
}

func (stack *Stack) finishDatagram(entry *datagram, result Result) {
	entry.phase, entry.result, entry.length = datagramDone, result, 0
}

func (stack *Stack) sendWaitingDatagrams(nextHop IPv4, mac MAC) {
	for index := range stack.datagrams[:] {
		entry := &stack.datagrams[index]
		if entry.phase == datagramResolving && entry.nextHop == nextHop {
			stack.sendDatagram(entry, mac)
		}
	}
}

func (stack *Stack) failWaitingDatagrams(nextHop IPv4, result Result) {
	for index := range stack.datagrams[:] {
		entry := &stack.datagrams[index]
		if entry.phase == datagramResolving && entry.nextHop == nextHop {
			stack.finishDatagram(entry, result)
		}
	}
}

func (stack *Stack) failDatagrams(result Result) {
	for index := range stack.datagrams[:] {
		if stack.datagrams[index].waiting() {
			stack.finishDatagram(&stack.datagrams[index], result)
		}
	}
}

func (stack *Stack) advanceDatagrams(now uint64) {
	for index := range stack.datagrams[:] {
		entry := &stack.datagrams[index]
		if entry.waiting() && entry.deadline <= now {
			stack.finishDatagram(entry, ResultTimeout)
		}
	}
}

// DatagramCompletion takes one finished exchange, copying a reply into dst,
// which must hold DatagramPayload bytes. Slots are scanned from a rotating
// start so one busy slot cannot starve the others.
func (stack *Stack) DatagramCompletion(dst []byte) (DatagramResult, bool) {
	if len(dst) < DatagramPayload {
		return DatagramResult{}, false
	}
	for step := 0; step < MaxDatagrams; step++ {
		number := (stack.datagramCursor + step) % MaxDatagrams
		entry := &stack.datagrams[number]
		if entry.phase != datagramDone {
			continue
		}
		done := DatagramResult{Slot: number, Result: entry.result}
		if entry.result == ResultOK {
			done.Source, done.Port = entry.destination, entry.destinationPort
			done.Length = copy(dst, entry.data[:entry.length])
		}
		entry.clear()
		stack.datagramCursor = (number + 1) % MaxDatagrams
		return done, true
	}
	return DatagramResult{}, false
}

// CancelDatagram forgets a slot's exchange without a result, for a client
// that has gone away. A late reply to its port is then a drop.
func (stack *Stack) CancelDatagram(slot int) {
	if slot >= 0 && slot < MaxDatagrams {
		stack.datagrams[slot].clear()
	}
}
