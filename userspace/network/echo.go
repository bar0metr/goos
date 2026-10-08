package network

import "encoding/binary"

const (
	MaxSessions = 8
	// EchoPayload is the ICMP data the stack sends: an eight-byte random
	// token, the session and sequence, then a fixed fill pattern.
	EchoPayload    = 32
	MinEchoTimeout = 100 * millisecond
	MaxEchoTimeout = 5000 * millisecond

	echoTokenSize = 8
	echoFrameSize = HeaderSize + ipv4HeaderSize + icmpHeaderSize + EchoPayload
)

type echoPhase uint8

const (
	echoIdle echoPhase = iota
	echoResolving
	echoSent
	// echoDone holds a completion until the service takes it, which keeps the
	// session busy so completions never outnumber sessions.
	echoDone
)

// Completion is the single outcome of one accepted Echo.
type Completion struct {
	Session   int
	Result    Result
	Responder IPv4
	Sequence  uint16
	// RTT is in nanoseconds and is set only for ResultOK.
	RTT uint64
}

type echoSession struct {
	phase       echoPhase
	destination IPv4
	nextHop     IPv4
	local       IPv4
	identifier  uint16
	sequence    uint16
	token       [echoTokenSize]byte
	deadline    uint64
	sentAt      uint64
	completion  Completion
}

// Echo starts one ICMP echo for a session. Arguments are checked first, then
// session availability, then interface state, so a client sees a stable error
// for a request it cannot fix by retrying. ResultOK means exactly one
// Completion follows unless CancelSession drops the session first.
//
// The identifier is a per-boot random base plus the session number, so a
// reply names its session directly; the payload token still has to match.
// An echo to our own address, or anywhere in 127.0.0.0/8, completes at once
// with zero RTT and never reaches the wire.
func (stack *Stack) Echo(session int, destination IPv4, sequence uint16, timeout uint64, now uint64) Result {
	if session < 0 || session >= MaxSessions || timeout < MinEchoTimeout || timeout > MaxEchoTimeout {
		return ResultInvalidArgument
	}
	if destination.IsLoopback() {
		return stack.echoLoopback(session, destination, sequence)
	}
	if !destination.IsUnicast() {
		return ResultInvalidArgument
	}
	slot := &stack.sessions[session]
	if slot.phase != echoIdle {
		return ResultBusy
	}
	if stack.state == StateFailed {
		return stack.failResult
	}
	if !stack.link {
		return ResultDown
	}
	if stack.address == zeroIPv4 {
		return ResultNoAddress
	}
	if stack.subnetSpecial(destination) {
		return ResultInvalidArgument
	}
	nextHop, result := stack.route(destination)
	if result != ResultOK {
		return result
	}
	*slot = echoSession{
		destination: destination,
		nextHop:     nextHop,
		local:       stack.address,
		identifier:  stack.echoBase + uint16(session),
		sequence:    sequence,
		deadline:    addTime(now, timeout),
	}
	binary.LittleEndian.PutUint64(slot.token[:], stack.next())
	if destination == stack.address {
		stack.complete(session, ResultOK, destination, 0)
		return ResultOK
	}
	if mac, ok := stack.lookupNeighbor(nextHop, now); ok {
		stack.sendEcho(session, mac, now)
		return ResultOK
	}
	if !stack.resolve(nextHop, now) {
		*slot = echoSession{}
		return ResultBusy
	}
	slot.phase = echoResolving
	return ResultOK
}

// subnetSpecial reports the network or directed broadcast address of the
// applied prefix, which name no single host.
func (stack *Stack) subnetSpecial(address IPv4) bool {
	return samePrefix(address, stack.address, stack.prefix) && !hostAddress(address, stack.prefix)
}

// route picks the connected prefix first, then the default gateway.
func (stack *Stack) route(destination IPv4) (IPv4, Result) {
	if samePrefix(destination, stack.address, stack.prefix) {
		return destination, ResultOK
	}
	if stack.gateway != zeroIPv4 {
		return stack.gateway, ResultOK
	}
	return IPv4{}, ResultNoRoute
}

func (session *echoSession) payloadByte(number, index int) byte {
	switch {
	case index < echoTokenSize:
		return session.token[index]
	case index == echoTokenSize:
		return byte(number)
	case index == echoTokenSize+1:
		return byte(session.sequence >> 8)
	case index == echoTokenSize+2:
		return byte(session.sequence)
	}
	return byte(index)
}

func (session *echoSession) payloadMatches(number int, payload []byte) bool {
	if len(payload) != EchoPayload {
		return false
	}
	for index := range payload {
		if payload[index] != session.payloadByte(number, index) {
			return false
		}
	}
	return true
}

// sendEcho builds the request once the next hop is known. The send time is
// taken here, so resolution delay is not counted in the RTT. A full transmit
// ring loses the request; the echo then times out.
func (stack *Stack) sendEcho(number int, mac MAC, now uint64) {
	session := &stack.sessions[number]
	session.phase = echoSent
	session.sentAt = now
	frame := stack.frameBuffer()
	if frame == nil {
		return
	}
	putEthernet(frame, mac, stack.mac, etherTypeIPv4)
	message := frame[HeaderSize+ipv4HeaderSize : echoFrameSize]
	data := message[icmpHeaderSize:]
	for index := range data {
		data[index] = session.payloadByte(number, index)
	}
	putICMPEcho(message, icmpEchoRequest, session.identifier, session.sequence, data)
	stack.identification++
	putIPv4Header(frame[HeaderSize:], stack.tuning.TTL, session.local, session.destination, protocolICMP, stack.identification,
		icmpHeaderSize+EchoPayload)
	stack.commitFrame(echoFrameSize)
}

// sendWaiting sends every echo and UDP datagram that was waiting for next
// hop to resolve.
func (stack *Stack) sendWaiting(nextHop IPv4, mac MAC, now uint64) {
	for index := range stack.sessions[:] {
		session := &stack.sessions[index]
		if session.phase == echoResolving && session.nextHop == nextHop {
			stack.sendEcho(index, mac, now)
		}
	}
	stack.sendPendingUDP(nextHop, mac)
	stack.sendPendingTCP(nextHop, now)
}

func (stack *Stack) failWaiting(nextHop IPv4, result Result) {
	for index := range stack.sessions[:] {
		session := &stack.sessions[index]
		if session.phase == echoResolving && session.nextHop == nextHop {
			stack.complete(index, result, IPv4{}, 0)
		}
	}
	stack.dropPendingUDP(nextHop, false)
	stack.failPendingTCP(nextHop, result)
}

func (stack *Stack) failEchoes(result Result) {
	for index := range stack.sessions[:] {
		phase := stack.sessions[index].phase
		if phase == echoResolving || phase == echoSent {
			stack.complete(index, result, IPv4{}, 0)
		}
	}
}

func (stack *Stack) advanceEchoes(now uint64) {
	for index := range stack.sessions[:] {
		session := &stack.sessions[index]
		if (session.phase == echoResolving || session.phase == echoSent) && session.deadline <= now {
			stack.complete(index, ResultTimeout, IPv4{}, 0)
		}
	}
}

func (stack *Stack) complete(number int, result Result, responder IPv4, rtt uint64) {
	session := &stack.sessions[number]
	session.completion = Completion{
		Session:   number,
		Result:    result,
		Responder: responder,
		Sequence:  session.sequence,
		RTT:       rtt,
	}
	session.phase = echoDone
}

// Completion takes one finished echo. Sessions are scanned from a rotating
// start so one busy session cannot starve the others.
func (stack *Stack) Completion() (Completion, bool) {
	for step := 0; step < MaxSessions; step++ {
		number := (stack.cursor + step) % MaxSessions
		session := &stack.sessions[number]
		if session.phase != echoDone {
			continue
		}
		completion := session.completion
		*session = echoSession{}
		stack.cursor = (number + 1) % MaxSessions
		return completion, true
	}
	return Completion{}, false
}

// CancelSession forgets a session's echo without a completion, for a client
// that has gone away.
func (stack *Stack) CancelSession(session int) {
	if session < 0 || session >= MaxSessions {
		return
	}
	stack.sessions[session] = echoSession{}
}

// sessionFor maps an echo identifier back to the session that sent it.
func (stack *Stack) sessionFor(identifier uint16) (int, *echoSession) {
	number := int(identifier - stack.echoBase)
	if number >= MaxSessions || stack.sessions[number].phase != echoSent {
		return 0, nil
	}
	return number, &stack.sessions[number]
}

func (stack *Stack) receiveICMP(ether ethernetFrame, packet ipv4Packet, now uint64) {
	message, ok := parseICMP(packet.payload)
	if !ok {
		stack.countMalformed()
		return
	}
	switch message.kind {
	case icmpEchoRequest:
		if ether.destination == broadcastMAC {
			stack.CountDrop()
			return
		}
		stack.answerEcho(ether.source, packet, message, now)
	case icmpEchoReply:
		stack.matchReply(packet, message, now)
	case icmpUnreachable:
		stack.matchError(packet, message, ResultRemoteUnreachable)
	case icmpTimeExceeded:
		stack.matchError(packet, message, ResultTimeExceeded)
	default:
		stack.CountDrop()
	}
}

func (stack *Stack) matchReply(packet ipv4Packet, message icmpMessage, now uint64) {
	number, session := stack.sessionFor(message.identifier)
	if session == nil || message.code != 0 || packet.source != session.destination ||
		packet.destination != session.local || message.sequence != session.sequence ||
		!session.payloadMatches(number, message.body) {
		return
	}
	rtt := uint64(0)
	if now > session.sentAt {
		rtt = now - session.sentAt
	}
	stack.complete(number, ResultOK, packet.source, rtt)
}

// matchError reports an ICMP error against the echo it quotes. Only eight
// bytes of the echo are quoted, so the token cannot be checked; the quoted
// addresses, identifier and sequence have to match instead.
func (stack *Stack) matchError(packet ipv4Packet, message icmpMessage, result Result) {
	quoted, ok := parseQuotedEcho(message.body)
	if !ok {
		return
	}
	number, session := stack.sessionFor(quoted.identifier)
	if session == nil || packet.destination != session.local || quoted.source != session.local ||
		quoted.destination != session.destination || quoted.sequence != session.sequence {
		return
	}
	stack.complete(number, result, packet.source, 0)
}

// answerEcho replies to the frame's sender directly, which is the right next
// hop whether the requester is on the link or behind a router. The reply rate
// is capped per one-second window so a flood cannot fill the transmit ring; a
// cap of zero answers nothing. Replies to our own echoes are not counted.
func (stack *Stack) answerEcho(source MAC, packet ipv4Packet, message icmpMessage, now uint64) {
	if message.code != 0 || stack.subnetSpecial(packet.source) {
		stack.CountDrop()
		return
	}
	length := ipv4HeaderSize + icmpHeaderSize + len(message.body)
	if length > int(stack.mtu) {
		stack.CountDrop()
		return
	}
	if now < stack.replyWindow || now-stack.replyWindow >= second {
		stack.replyWindow, stack.replyCount = now, 0
	}
	if stack.replyCount >= stack.tuning.EchoReplyRate {
		stack.CountDrop()
		return
	}
	frame := stack.frameBuffer()
	if frame == nil {
		return
	}
	stack.replyCount++
	putEthernet(frame, source, stack.mac, etherTypeIPv4)
	putICMPEcho(frame[HeaderSize+ipv4HeaderSize:], icmpEchoReply, message.identifier, message.sequence, message.body)
	stack.identification++
	putIPv4Header(frame[HeaderSize:], stack.tuning.TTL, stack.address, packet.source, protocolICMP, stack.identification,
		icmpHeaderSize+len(message.body))
	stack.commitFrame(HeaderSize + length)
}
