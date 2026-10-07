package network

// The loopback interface, lo, owns 127.0.0.1/8 from Init on, whatever the
// link, the address configuration or DHCP do. The whole of 127.0.0.0/8 routes
// to it (RFC 1122): nothing sent there is framed, resolved with ARP or handed
// to the gateway, and a frame from the link carrying a loopback source or
// destination is dropped. Echo is its only user now; a later UDP or TCP
// socket uses the same route.

var (
	// LoopbackAddress and LoopbackPrefix are lo's address.
	LoopbackAddress = IPv4{127, 0, 0, 1}
	LoopbackPrefix  = uint8(8)
)

// LoopbackMTU is lo's MTU. No frame carries loopback traffic, so it is a
// logical limit for future sockets, not a device's.
const LoopbackMTU = 65536

// IsLoopback reports an address in 127.0.0.0/8.
func (address IPv4) IsLoopback() bool { return address[0] == 127 }

// loopbackHost reports a loopback address that names a host: neither
// 127.0.0.0 nor 127.255.255.255.
func loopbackHost(address IPv4) bool {
	host := address.uint32() &^ prefixMask(LoopbackPrefix)
	return address.IsLoopback() && host != 0 && host != ^prefixMask(LoopbackPrefix)
}

// echoLoopback answers an echo to lo at once, with zero RTT. It needs no
// link and no address.
func (stack *Stack) echoLoopback(session int, destination IPv4, sequence uint16) Result {
	if !loopbackHost(destination) {
		return ResultInvalidArgument
	}
	slot := &stack.sessions[session]
	if slot.phase != echoIdle {
		return ResultBusy
	}
	*slot = echoSession{destination: destination, local: destination, sequence: sequence}
	increment(&stack.loopback)
	stack.complete(session, ResultOK, destination, 0)
	return ResultOK
}
