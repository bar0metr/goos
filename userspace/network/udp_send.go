package network

// Sending UDP. A datagram takes one of three paths. One to 127.0.0.0/8 or to
// the interface's own address is delivered to the endpoint table at once:
// it never reaches ARP, the transmit ring or the driver, and lo needs no
// device at all. Anything else goes out of the interface through the usual
// route and ARP, with a checksum over the pseudo-header. A datagram whose
// next hop is not resolved yet waits in a small pending table and is framed
// when ARP answers, or dropped when it gives up.
//
// Acceptance is local: ResultOK means the stack framed, queued or delivered
// the datagram, not that a peer received it.

type udpPath uint8

const (
	udpLocal udpPath = iota
	udpWire
)

type udpRouteResult struct {
	path    udpPath
	source  IPv4
	nextHop IPv4
}

type udpPending struct {
	used            bool
	nextHop         IPv4
	source          IPv4
	destination     IPv4
	sourcePort      uint16
	destinationPort uint16
	length          uint16
	data            [udpWirePayload]byte
}

// udpRoute picks the path and the source address for a datagram from an
// endpoint bound to local. Arguments are checked first, then the binding,
// then the interface, as Echo does. An endpoint bound to an address the
// interface no longer has is Reconfigured until it is closed: it never
// moves to the new address.
func (stack *Stack) udpRoute(local, destination IPv4) (udpRouteResult, Result) {
	var route udpRouteResult
	if destination.IsLoopback() {
		if !loopbackHost(destination) {
			return route, ResultInvalidArgument
		}
	} else if !destination.IsUnicast() {
		return route, ResultInvalidArgument
	}
	if local != zeroIPv4 && !loopbackHost(local) && local != stack.address {
		return route, ResultReconfigured
	}
	route.source = local
	switch {
	case destination.IsLoopback():
		if local == zeroIPv4 {
			route.source = LoopbackAddress
		}
		return route, ResultOK
	case stack.address != zeroIPv4 && destination == stack.address:
		if local == zeroIPv4 {
			route.source = stack.address
		}
		return route, ResultOK
	case local.IsLoopback():
		return route, ResultInvalidArgument
	case stack.state == StateFailed:
		return route, stack.failResult
	case !stack.link:
		return route, ResultDown
	case stack.address == zeroIPv4:
		return route, ResultNoAddress
	case stack.subnetSpecial(destination):
		return route, ResultInvalidArgument
	}
	nextHop, result := stack.route(destination)
	if result != ResultOK {
		return route, result
	}
	route.path, route.source, route.nextHop = udpWire, stack.address, nextHop
	return route, ResultOK
}

// UDPSend sends payload to a connected endpoint's peer.
func (stack *Stack) UDPSend(owner int, id EndpointID, payload []byte, now uint64) Result {
	entry, _ := stack.endpoint(owner, id)
	if entry == nil || entry.state != UDPConnected {
		return ResultInvalidArgument
	}
	return stack.sendUDP(entry, entry.remote, entry.remotePort, payload, now)
}

// UDPSendTo sends payload from an endpoint that is not connected, binding
// it to the wildcard and an ephemeral port first if it has no port. Port 0
// is no destination.
func (stack *Stack) UDPSendTo(owner int, id EndpointID, destination IPv4, port uint16, payload []byte, now uint64) Result {
	entry, _ := stack.endpoint(owner, id)
	if entry == nil || entry.state == UDPConnected || port == 0 {
		return ResultInvalidArgument
	}
	return stack.sendUDP(entry, destination, port, payload, now)
}

// sendUDP checks everything that can refuse the datagram before it binds an
// ephemeral port, so a refused send leaves the endpoint as it was.
func (stack *Stack) sendUDP(entry *udpEndpoint, destination IPv4, port uint16, payload []byte, now uint64) Result {
	if len(payload) > int(stack.tuning.UDP.MaxPayload) {
		return ResultInvalidArgument
	}
	route, result := stack.udpRoute(entry.local, destination)
	if result != ResultOK {
		return result
	}
	var mac MAC
	pending := -1
	if route.path == udpWire {
		if ipv4HeaderSize+udpHeaderSize+len(payload) > int(stack.mtu) {
			return ResultInvalidArgument
		}
		var known bool
		if mac, known = stack.lookupNeighbor(route.nextHop, now); known {
			if stack.txCount == TxQueueFrames {
				return ResultBusy
			}
		} else {
			if pending = stack.freePending(); pending < 0 || !stack.resolve(route.nextHop, now) {
				return ResultBusy
			}
		}
	}
	if entry.state == UDPOpen {
		local := stack.ephemeralPort()
		if local == 0 {
			increment(&stack.udpStats.BindFail)
			return ResultBusy
		}
		entry.port, entry.state = local, UDPBound
	}
	increment(&stack.udpStats.TX)
	switch {
	case route.path == udpLocal:
		stack.deliverUDP(route.source, entry.port, destination, port, payload)
	case pending < 0:
		stack.frameUDP(mac, route.source, destination, entry.port, port, payload)
	default:
		held := &stack.udpPending[pending]
		held.used, held.nextHop, held.source, held.destination = true, route.nextHop, route.source, destination
		held.sourcePort, held.destinationPort = entry.port, port
		held.length = uint16(copy(held.data[:], payload))
	}
	return ResultOK
}

func (stack *Stack) freePending() int {
	for index := range stack.udpPending[:] {
		if !stack.udpPending[index].used {
			return index
		}
	}
	return -1
}

// frameUDP builds one Ethernet frame with a checksummed UDP datagram. A full
// transmit ring counts a drop.
func (stack *Stack) frameUDP(mac MAC, source, destination IPv4, sourcePort, destinationPort uint16, payload []byte) {
	frame := stack.frameBuffer()
	if frame == nil {
		return
	}
	const udpAt = HeaderSize + ipv4HeaderSize
	length := copy(frame[udpAt+udpHeaderSize:], payload)
	putUDP(frame[udpAt:], source, destination, sourcePort, destinationPort, length)
	stack.identification++
	putIPv4Header(frame[HeaderSize:], stack.tuning.TTL, source, destination, protocolUDP, stack.identification,
		udpHeaderSize+length)
	putEthernet(frame, mac, stack.mac, etherTypeIPv4)
	stack.commitFrame(udpAt + udpHeaderSize + length)
}

func (stack *Stack) sendPendingUDP(nextHop IPv4, mac MAC) {
	for index := range stack.udpPending[:] {
		held := &stack.udpPending[index]
		if held.used && held.nextHop == nextHop {
			stack.frameUDP(mac, held.source, held.destination, held.sourcePort, held.destinationPort, held.data[:held.length])
			held.used = false
		}
	}
}

// dropPendingUDP discards datagrams waiting for nextHop, or for every next
// hop when all is set, counting each as a drop.
func (stack *Stack) dropPendingUDP(nextHop IPv4, all bool) {
	for index := range stack.udpPending[:] {
		held := &stack.udpPending[index]
		if held.used && (all || held.nextHop == nextHop) {
			held.used = false
			stack.CountDrop()
		}
	}
}
