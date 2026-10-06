package network

import (
	"encoding/binary"
	"math/bits"
)

const (
	dhcpFixedSize   = 236
	dhcpCookie      = 0x63825363
	dhcpOptionsAt   = dhcpFixedSize + 4
	dhcpSnameAt     = 44
	dhcpSnameSize   = 64
	dhcpFileAt      = 108
	dhcpFileSize    = 128
	dhcpChaddrAt    = 28
	dhcpFlagsAt     = 10
	dhcpBroadcast   = 0x8000
	bootRequest     = 1
	bootReply       = 2
	hardwareEther   = 1
	hardwareLength  = 6
	dhcpMinimumSize = 300 // the BOOTP message size older servers expect

	dhcpDiscover = 1
	dhcpOffer    = 2
	dhcpRequest  = 3
	dhcpDecline  = 4
	dhcpAck      = 5
	dhcpNak      = 6

	optionPad          = 0
	optionSubnetMask   = 1
	optionRouter       = 3
	optionRequestedIP  = 50
	optionLeaseTime    = 51
	optionOverload     = 52
	optionMessageType  = 53
	optionServerID     = 54
	optionParameters   = 55
	optionMaxMessage   = 57
	optionRenewalTime  = 58
	optionRebindTime   = 59
	optionEnd          = 255
	overloadFile       = 1
	overloadSname      = 2
	overloadBoth       = 3
	infiniteLease      = 0xffffffff
	minLeaseSeconds    = 30
	dhcpInitialBackoff = 4 * second
	dhcpMaxBackoff     = 64 * second
	dhcpJitter         = second
	dhcpRequestRetries = 4
	// dhcpDeclineWait is the RFC 2131 minimum before trying again after a
	// conflict.
	dhcpDeclineWait = 10 * second
	// dhcpMinRetry is the RFC 2131 floor on RENEW and REBIND retransmission.
	dhcpMinRetry = 60 * second
)

type dhcpPhase uint8

const (
	dhcpOff dhcpPhase = iota
	dhcpSelecting
	dhcpRequesting
	dhcpProbing
	dhcpBound
	dhcpRenewing
	dhcpRebinding
	dhcpBackoff
)

// dhcpLease is a validated OFFER or ACK. Times are in nanoseconds.
type dhcpLease struct {
	address IPv4
	prefix  uint8
	router  IPv4
	server  IPv4
	length  uint64
	renew   uint64
	rebind  uint64
}

type dhcpClient struct {
	phase    dhcpPhase
	xid      uint32
	started  uint64
	deadline uint64
	backoff  uint64
	retries  uint8
	// requested is when the latest REQUEST left; a lease runs from then.
	requested uint64
	lease     dhcpLease
	// serverMAC is the Ethernet source of the ACK: the hop through which the
	// server reached us, and so the hop a unicast RENEW goes back through.
	serverMAC MAC
	renewAt   uint64
	rebindAt  uint64
	expiry    uint64
}

func (client *dhcpClient) listening() bool {
	switch client.phase {
	case dhcpSelecting, dhcpRequesting, dhcpRenewing, dhcpRebinding:
		return true
	}
	return false
}

func (client *dhcpClient) timed() bool { return client.phase != dhcpOff && client.phase != dhcpProbing }

func (client *dhcpClient) leased() bool {
	return client.phase == dhcpBound || client.phase == dhcpRenewing || client.phase == dhcpRebinding
}

// dhcpMessage holds the fields of a server reply this client uses. seen
// tracks options already read, so a repeated option fails the message
// instead of overriding the first.
type dhcpMessage struct {
	kind     uint8
	yiaddr   IPv4
	server   IPv4
	prefix   uint8
	router   IPv4
	lease    uint32
	renew    uint32
	rebind   uint32
	overload uint8
	seen     uint16
}

const (
	seenKind uint16 = 1 << iota
	seenServer
	seenMask
	seenRouter
	seenLease
	seenRenew
	seenRebind
	seenOverload
)

// parseDHCP decodes a BOOTP reply for our transaction. A wrong transaction
// ID or hardware address marks another client's traffic, which counts as a
// drop.
func parseDHCP(data []byte, xid uint32, mac MAC) (dhcpMessage, parseStatus) {
	var message dhcpMessage
	if len(data) < dhcpOptionsAt {
		return message, parseMalformed
	}
	if data[0] != bootReply || data[1] != hardwareEther || data[2] != hardwareLength ||
		binary.BigEndian.Uint32(data[dhcpFixedSize:dhcpOptionsAt]) != dhcpCookie {
		return message, parseMalformed
	}
	var chaddr MAC
	copy(chaddr[:], data[dhcpChaddrAt:])
	if binary.BigEndian.Uint32(data[4:8]) != xid || chaddr != mac {
		return message, parseUnsupported
	}
	copy(message.yiaddr[:], data[16:20])
	if !message.parseOptions(data[dhcpOptionsAt:], true) {
		return message, parseMalformed
	}
	// RFC 2131 orders an overloaded message as options, then file, then sname.
	if message.overload&overloadFile != 0 && !message.parseOptions(data[dhcpFileAt:dhcpFileAt+dhcpFileSize], false) {
		return message, parseMalformed
	}
	if message.overload&overloadSname != 0 && !message.parseOptions(data[dhcpSnameAt:dhcpSnameAt+dhcpSnameSize], false) {
		return message, parseMalformed
	}
	if message.seen&seenKind == 0 {
		return message, parseMalformed
	}
	return message, parseOK
}

// parseOptions walks one option area. Each step advances at least one byte,
// so the loop is bounded by the area length. The area must end with an End
// option; an option that runs past the area is malformed.
func (message *dhcpMessage) parseOptions(area []byte, top bool) bool {
	index := 0
	for index < len(area) {
		code := area[index]
		if code == optionPad {
			index++
			continue
		}
		if code == optionEnd {
			return true
		}
		if index+2 > len(area) {
			return false
		}
		start := index + 2
		end := start + int(area[index+1])
		if end > len(area) {
			return false
		}
		if !message.option(code, area[start:end], top) {
			return false
		}
		index = end
	}
	return false
}

func (message *dhcpMessage) mark(flag uint16) bool {
	if message.seen&flag != 0 {
		return false
	}
	message.seen |= flag
	return true
}

func (message *dhcpMessage) option(code uint8, value []byte, top bool) bool {
	switch code {
	case optionMessageType:
		if len(value) != 1 || !message.mark(seenKind) {
			return false
		}
		message.kind = value[0]
	case optionServerID:
		if len(value) != 4 || !message.mark(seenServer) {
			return false
		}
		copy(message.server[:], value)
	case optionSubnetMask:
		if len(value) != 4 || !message.mark(seenMask) {
			return false
		}
		mask := binary.BigEndian.Uint32(value)
		if ^mask&(^mask+1) != 0 {
			return false
		}
		message.prefix = uint8(bits.OnesCount32(mask))
	case optionRouter:
		if len(value) < 4 || len(value)%4 != 0 || !message.mark(seenRouter) {
			return false
		}
		copy(message.router[:], value[:4])
	case optionLeaseTime, optionRenewalTime, optionRebindTime:
		return message.seconds(code, value)
	case optionOverload:
		if !top || len(value) != 1 || value[0] < overloadFile || value[0] > overloadBoth || !message.mark(seenOverload) {
			return false
		}
		message.overload = value[0]
	}
	return true
}

func (message *dhcpMessage) seconds(code uint8, value []byte) bool {
	if len(value) != 4 {
		return false
	}
	seconds := binary.BigEndian.Uint32(value)
	switch code {
	case optionLeaseTime:
		message.lease = seconds
		return message.mark(seenLease)
	case optionRenewalTime:
		message.renew = seconds
		return message.mark(seenRenew)
	}
	message.rebind = seconds
	return message.mark(seenRebind)
}

// validLease applies the policy an OFFER or ACK must meet before this client
// acts on it: a server identifier, a usable mask, a finite lease of at least
// 30 seconds, a host address inside the prefix, an optional router inside it,
// and T1 < T2 < lease.
func (message *dhcpMessage) validLease() (dhcpLease, bool) {
	const want = seenServer | seenMask | seenLease
	var lease dhcpLease
	if message.seen&want != want || !message.server.IsUnicast() {
		return lease, false
	}
	if message.lease == infiniteLease || message.lease < minLeaseSeconds {
		return lease, false
	}
	if !hostAddress(message.yiaddr, message.prefix) {
		return lease, false
	}
	if message.seen&seenRouter != 0 {
		router := message.router
		if !hostAddress(router, message.prefix) || !samePrefix(router, message.yiaddr, message.prefix) ||
			router == message.yiaddr {
			return lease, false
		}
		lease.router = router
	}
	length := uint64(message.lease) * second
	renew, rebind := length/2, length/8*7
	if message.seen&seenRenew != 0 {
		renew = uint64(message.renew) * second
	}
	if message.seen&seenRebind != 0 {
		rebind = uint64(message.rebind) * second
	}
	if renew == 0 || renew >= rebind || rebind >= length {
		return lease, false
	}
	lease.address, lease.prefix, lease.server = message.yiaddr, message.prefix, message.server
	lease.length, lease.renew, lease.rebind = length, renew, rebind
	return lease, true
}

// jitter spreads retransmissions by up to one second either way. Every base
// is at least four seconds, so the result is always in the future.
func (stack *Stack) jitter(base uint64) uint64 {
	return base - dhcpJitter + stack.next()%(2*dhcpJitter+1)
}

func (stack *Stack) newTransaction(now uint64) {
	stack.dhcp.xid = uint32(stack.next())
	stack.dhcp.started = now
	stack.dhcp.backoff = dhcpInitialBackoff
	stack.dhcp.retries = 0
}

func (stack *Stack) startDiscovery(now uint64) {
	stack.dhcp = dhcpClient{phase: dhcpSelecting}
	stack.newTransaction(now)
	stack.sendDiscover(now)
}

func (stack *Stack) sendDiscover(now uint64) {
	stack.sendDHCP(dhcpOutgoing{kind: dhcpDiscover}, now)
	stack.dhcp.deadline = addTime(now, stack.jitter(stack.dhcp.backoff))
}

func (stack *Stack) sendSelectingRequest(now uint64) {
	lease := &stack.dhcp.lease
	stack.sendDHCP(dhcpOutgoing{kind: dhcpRequest, requested: lease.address, server: lease.server}, now)
	stack.dhcp.requested = now
	stack.dhcp.deadline = addTime(now, stack.jitter(stack.dhcp.backoff))
}

// sendLeaseRequest renews (unicast to the server) or rebinds (broadcast to
// any server) the address we hold.
func (stack *Stack) sendLeaseRequest(now uint64) {
	outgoing := dhcpOutgoing{kind: dhcpRequest, client: stack.address}
	limit := stack.dhcp.expiry
	if stack.dhcp.phase == dhcpRenewing {
		outgoing.destination, outgoing.link = stack.dhcp.lease.server, stack.dhcp.serverMAC
		limit = stack.dhcp.rebindAt
	}
	stack.sendDHCP(outgoing, now)
	stack.dhcp.requested = now
	wait := (limit - now) / 2
	if wait < dhcpMinRetry {
		wait = dhcpMinRetry
	}
	stack.dhcp.deadline = addTime(now, wait)
	if stack.dhcp.deadline > limit {
		stack.dhcp.deadline = limit
	}
}

func (stack *Stack) startLeaseRequest(phase dhcpPhase, now uint64) {
	stack.dhcp.phase = phase
	stack.newTransaction(now)
	stack.sendLeaseRequest(now)
}

func (stack *Stack) advanceDHCP(now uint64) {
	client := &stack.dhcp
	switch client.phase {
	case dhcpSelecting:
		client.backoff = nextBackoff(client.backoff)
		stack.sendDiscover(now)
	case dhcpRequesting:
		client.retries++
		if client.retries > dhcpRequestRetries {
			stack.startDiscovery(now)
			return
		}
		client.backoff = nextBackoff(client.backoff)
		stack.sendSelectingRequest(now)
	case dhcpBound, dhcpRenewing, dhcpRebinding:
		stack.leaseTimers(now)
	case dhcpBackoff:
		stack.startDiscovery(now)
	}
}

func nextBackoff(backoff uint64) uint64 {
	if backoff >= dhcpMaxBackoff/2 {
		return dhcpMaxBackoff
	}
	return backoff * 2
}

// leaseTimers moves the lease to the phase that now calls for. It is safe to
// run late: a lease that reached T1, T2 or expiry while the stack was busy
// goes straight to the right phase, and every deadline it sets is after now.
func (stack *Stack) leaseTimers(now uint64) {
	client := &stack.dhcp
	switch {
	case now >= client.expiry:
		stack.loseLease(now)
	case now >= client.rebindAt:
		if client.phase != dhcpRebinding {
			stack.startLeaseRequest(dhcpRebinding, now)
			return
		}
		stack.sendLeaseRequest(now)
	case now >= client.renewAt:
		if client.phase != dhcpRenewing {
			stack.startLeaseRequest(dhcpRenewing, now)
			return
		}
		stack.sendLeaseRequest(now)
	default:
		client.phase = dhcpBound
		client.deadline = client.renewAt
	}
}

func (stack *Stack) startLease(lease dhcpLease) {
	client := &stack.dhcp
	client.lease = lease
	client.renewAt = addTime(client.requested, lease.renew)
	client.rebindAt = addTime(client.requested, lease.rebind)
	client.expiry = addTime(client.requested, lease.length)
}

// loseLease removes the address when the lease ends or a server refuses it,
// then starts over.
func (stack *Stack) loseLease(now uint64) {
	stack.resetAddressing(ResultReconfigured)
	stack.startDiscovery(now)
}

func (stack *Stack) dhcpProbePassed(candidate IPv4, now uint64) {
	lease := stack.dhcp.lease
	stack.applyAddress(candidate, lease.prefix, lease.router)
	stack.dhcp.phase = dhcpBound
	stack.leaseTimers(now)
}

func (stack *Stack) dhcpDecline(candidate IPv4, now uint64) {
	stack.sendDHCP(dhcpOutgoing{kind: dhcpDecline, requested: candidate, server: stack.dhcp.lease.server}, now)
	stack.dhcp.phase = dhcpBackoff
	stack.dhcp.deadline = addTime(now, dhcpDeclineWait)
}

func (stack *Stack) receiveDHCP(source MAC, data []byte, now uint64) {
	client := &stack.dhcp
	if stack.config.Mode != ModeDHCP || !client.listening() {
		stack.CountDrop()
		return
	}
	message, status := parseDHCP(data, client.xid, stack.mac)
	switch status {
	case parseMalformed:
		stack.countMalformed()
		return
	case parseUnsupported:
		stack.CountDrop()
		return
	}
	switch client.phase {
	case dhcpSelecting:
		stack.receiveOffer(message, now)
	case dhcpRequesting:
		stack.receiveRequestReply(source, message, now)
	default:
		stack.receiveLeaseReply(source, message, now)
	}
}

func (stack *Stack) receiveOffer(message dhcpMessage, now uint64) {
	lease, ok := message.validLease()
	if message.kind != dhcpOffer || !ok {
		stack.CountDrop()
		return
	}
	client := &stack.dhcp
	client.phase = dhcpRequesting
	client.lease = lease
	client.backoff = dhcpInitialBackoff
	client.retries = 0
	stack.sendSelectingRequest(now)
}

func (stack *Stack) receiveRequestReply(source MAC, message dhcpMessage, now uint64) {
	client := &stack.dhcp
	if message.seen&seenServer == 0 || message.server != client.lease.server {
		stack.CountDrop()
		return
	}
	switch message.kind {
	case dhcpNak:
		stack.startDiscovery(now)
	case dhcpAck:
		lease, ok := message.validLease()
		if !ok || lease.address != client.lease.address {
			stack.CountDrop()
			return
		}
		stack.startLease(lease)
		client.serverMAC = source
		client.phase = dhcpProbing
		stack.startProbe(lease.address, now)
	default:
		stack.CountDrop()
	}
}

// receiveLeaseReply handles the answer to a RENEW or REBIND. While renewing
// only our server may answer; while rebinding any server may, and the one
// that answers becomes our server.
func (stack *Stack) receiveLeaseReply(source MAC, message dhcpMessage, now uint64) {
	client := &stack.dhcp
	if message.seen&seenServer == 0 || client.phase == dhcpRenewing && message.server != client.lease.server {
		stack.CountDrop()
		return
	}
	switch message.kind {
	case dhcpNak:
		stack.loseLease(now)
	case dhcpAck:
		lease, ok := message.validLease()
		if !ok {
			stack.CountDrop()
			return
		}
		if lease.address != stack.address {
			stack.loseLease(now)
			return
		}
		stack.startLease(lease)
		client.serverMAC = source
		stack.prefix, stack.gateway = lease.prefix, lease.router
		client.phase = dhcpBound
		stack.leaseTimers(now)
	default:
		stack.CountDrop()
	}
}

// dhcpOutgoing describes one client message. A zero client address means we
// hold no address: the message goes from 0.0.0.0 to limited broadcast with
// the BOOTP broadcast flag set, since we cannot receive unicast yet.
type dhcpOutgoing struct {
	kind        uint8
	client      IPv4
	requested   IPv4
	server      IPv4
	destination IPv4
	link        MAC
}

var dhcpParameterList = [...]byte{optionSubnetMask, optionRouter, optionLeaseTime, optionServerID,
	optionRenewalTime, optionRebindTime}

func (stack *Stack) sendDHCP(outgoing dhcpOutgoing, now uint64) {
	frame := stack.frameBuffer()
	if frame == nil {
		return
	}
	if outgoing.destination == zeroIPv4 {
		outgoing.destination, outgoing.link = broadcastIPv4, broadcastMAC
	}
	const udpAt = HeaderSize + ipv4HeaderSize
	const dhcpAt = udpAt + udpHeaderSize
	message := frame[dhcpAt : dhcpAt+dhcpMinimumSize]
	for index := range message {
		message[index] = 0
	}
	message[0], message[1], message[2] = bootRequest, hardwareEther, hardwareLength
	binary.BigEndian.PutUint32(message[4:8], stack.dhcp.xid)
	elapsed := uint64(0)
	if now > stack.dhcp.started {
		elapsed = (now - stack.dhcp.started) / second
	}
	if elapsed > 0xffff {
		elapsed = 0xffff
	}
	binary.BigEndian.PutUint16(message[8:10], uint16(elapsed))
	if outgoing.client == zeroIPv4 && outgoing.kind != dhcpDecline {
		binary.BigEndian.PutUint16(message[dhcpFlagsAt:dhcpFlagsAt+2], dhcpBroadcast)
	}
	copy(message[12:16], outgoing.client[:])
	copy(message[dhcpChaddrAt:], stack.mac[:])
	binary.BigEndian.PutUint32(message[dhcpFixedSize:dhcpOptionsAt], dhcpCookie)
	options := message[dhcpOptionsAt:]
	options[0], options[1], options[2] = optionMessageType, 1, outgoing.kind
	at := 3
	if outgoing.requested != zeroIPv4 {
		at = putOption(options, at, optionRequestedIP, outgoing.requested[:])
	}
	if outgoing.server != zeroIPv4 {
		at = putOption(options, at, optionServerID, outgoing.server[:])
	}
	if outgoing.kind != dhcpDecline {
		at = putOption(options, at, optionParameters, dhcpParameterList[:])
		options[at], options[at+1] = optionMaxMessage, 2
		binary.BigEndian.PutUint16(options[at+2:at+4], MTU)
		at += 4
	}
	options[at] = optionEnd

	putUDP(frame[udpAt:], outgoing.client, outgoing.destination, portClient, portServer, dhcpMinimumSize)
	stack.identification++
	putIPv4Header(frame[HeaderSize:], outgoing.client, outgoing.destination, protocolUDP, stack.identification,
		udpHeaderSize+dhcpMinimumSize)
	putEthernet(frame, outgoing.link, stack.mac, etherTypeIPv4)
	stack.commitFrame(dhcpAt + dhcpMinimumSize)
}

// putOption writes one option at offset at. The options this client sends
// are a fixed set far shorter than the space after the BOOTP header.
func putOption(options []byte, at int, code uint8, value []byte) int {
	options[at], options[at+1] = code, byte(len(value))
	copy(options[at+2:], value)
	return at + 2 + len(value)
}
