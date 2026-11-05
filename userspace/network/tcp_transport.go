package network

func (stack *Stack) receiveTCP(packet ipv4Packet, now uint64) {
	segment, status := parseTCP(packet)
	switch status {
	case tcpMalformed:
		increment(&stack.tcpStats.DropMalformed)
		return
	case tcpBadChecksum:
		increment(&stack.tcpStats.DropChecksum)
		return
	}
	stack.receiveSegment(packet, segment, now)
}

// receiveSegment runs one parsed segment, or one a local sender handed over
// whole, through the state machine. packet carries the addresses only. The
// lookups go through the tuple, tombstone and listener indexes; none walks
// the endpoints.
func (stack *Stack) receiveSegment(packet ipv4Packet, segment tcpSegment, now uint64) {
	increment(&stack.tcpStats.RX)
	var probes uint64
	entry := stack.lookupTuple(packet.destination, segment.destinationPort, packet.source, segment.sourcePort, &probes)
	stack.tcpStats.MatchProbes += probes
	if tcpCrossCheck && entry != stack.slowMatch(packet.destination, segment.destinationPort, packet.source, segment.sourcePort) {
		panic("tcp: tuple index disagrees with the endpoints")
	}
	if entry != nil {
		stack.noteProbes(probes)
		stack.handleTCP(entry, packet, segment, now)
		stack.armPersist(entry, now)
		stack.settleTCP(entry)
		return
	}
	if tomb := stack.lookupTombstone(packet.destination, segment.destinationPort, packet.source, segment.sourcePort, &probes); tomb >= 0 {
		stack.noteProbes(probes)
		stack.receiveTombstone(tomb, segment, now)
		return
	}
	before := probes
	listener := stack.tcpListener(packet.destination, segment.destinationPort, &probes)
	stack.tcpStats.ListenerProbes += probes - before
	stack.noteProbes(probes)
	if listener != nil && segment.flags&tcpFlagSYN != 0 && segment.flags&(tcpFlagACK|tcpFlagRST) == 0 {
		stack.passiveSYN(listener, packet, segment, now)
		return
	}
	increment(&stack.tcpStats.DropNoTCB)
	stack.resetUnknown(packet, segment, now)
}

// noteProbes records the index entries one received segment's lookups
// looked at.
func (stack *Stack) noteProbes(probes uint64) {
	stack.tcpStats.LookupProbes += probes
	stack.tcpStats.MaxSegmentProbes = max(stack.tcpStats.MaxSegmentProbes, probes)
}

// tcpListener finds the listener for a local address and port: one bound
// to the address itself before one bound to the wildcard.
func (stack *Stack) tcpListener(local IPv4, port uint16, probes *uint64) *tcpEndpoint {
	var exact, wildcard *tcpEndpoint
	for entry := stack.tcpListeners.bucket(stack.portHash(port)); entry != nil; entry = entry.links[tcpLinkListen].next {
		*probes++
		if entry.port != port {
			continue
		}
		if entry.local == local {
			exact = entry
			break
		}
		if entry.local == zeroIPv4 {
			wildcard = entry
		}
	}
	if exact == nil {
		exact = wildcard
	}
	if tcpCrossCheck && exact != stack.slowListener(local, port) {
		panic("tcp: listener index disagrees with the endpoints")
	}
	return exact
}

// passiveSYN opens a child for a SYN to a listener with room in its
// backlog. A full backlog or exhausted budgets drop the SYN; the peer
// retransmits it.
func (stack *Stack) passiveSYN(listener *tcpEndpoint, packet ipv4Packet, segment tcpSegment, now uint64) {
	if listener.children.count >= int(listener.backlog) {
		increment(&stack.tcpStats.DropBacklog)
		return
	}
	id, result := stack.TCPOpen(int(listener.owner))
	if result != ResultOK {
		increment(&stack.tcpStats.DropBacklog)
		return
	}
	child := stack.tcp[id.slot()]
	route, result := stack.udpRoute(packet.destination, packet.source)
	if result != ResultOK || !stack.allocateReceive(child, route.path == udpLocal) {
		stack.releaseTCP(child)
		return
	}
	child.local, child.port = packet.destination, segment.destinationPort
	child.remote, child.remotePort = packet.source, segment.sourcePort
	child.listener = tcpID(listener)
	child.reuse = listener.reuse
	child.irs, child.rcvNxt = segment.sequence, segment.sequence+1
	child.iss = uint32(stack.next())
	child.sndUna, child.sndNxt = child.iss, child.iss+1
	child.peerWindow, child.peerMSS = uint32(segment.window), segment.mss
	child.localMSS = stack.tcpPathMSS(route)
	child.state = TCPSynReceived
	stack.indexConnection(child)
	stack.attachChild(listener, child)
	stack.setDeadline(child, addTime(now, stack.tuning.TCP.ConnectTimeout))
	child.rto = stack.tuning.TCP.RTOMin
	if stack.sendTCP(child, tcpFlagSYN|tcpFlagACK, child.iss, child.rcvNxt, nil, now) && child.state == TCPSynReceived {
		stack.setRTO(child, addTime(now, child.rto))
	}
}

func (stack *Stack) handleTCP(entry *tcpEndpoint, packet ipv4Packet, segment tcpSegment, now uint64) {
	if segment.flags&tcpFlagRST != 0 {
		stack.handleRST(entry, segment, now)
		return
	}
	switch entry.state {
	case TCPSynSent:
		stack.handleSynSent(entry, segment, now)
		return
	case TCPSynReceived:
		stack.handleSynReceived(entry, segment, now)
		return
	case TCPTimeWait:
		if segment.flags&tcpFlagFIN != 0 && segment.sequence+uint32(len(segment.payload))+1 == entry.rcvNxt {
			stack.sendTCP(entry, tcpFlagACK, entry.sndNxt, entry.rcvNxt, nil, now)
			stack.setDeadline(entry, addTime(now, stack.tuning.TCP.TimeWait))
		}
		return
	}
	if entry.state < TCPEstablished || entry.state >= TCPClosed {
		return
	}
	// The handshake gave the connection its ring; a control block built
	// some other way gets one here.
	stack.allocateReceive(entry, false)
	if !stack.tcpSegmentAcceptable(entry, segment) {
		stack.sendTCP(entry, tcpFlagACK, entry.sndNxt, entry.rcvNxt, nil, now)
		return
	}
	if segment.flags&tcpFlagACK == 0 {
		stack.sendTCP(entry, tcpFlagACK, entry.sndNxt, entry.rcvNxt, nil, now)
		return
	}
	if stack.handleACK(entry, segment, now) {
		stack.handlePayload(entry, segment, now)
	}
}

func (stack *Stack) handleSynSent(entry *tcpEndpoint, segment tcpSegment, now uint64) {
	if segment.flags&(tcpFlagSYN|tcpFlagACK) != tcpFlagSYN|tcpFlagACK || segment.acknowledgment != entry.sndNxt {
		return
	}
	entry.irs, entry.rcvNxt = segment.sequence, segment.sequence+1
	entry.sndUna = segment.acknowledgment
	entry.sendSeq = entry.sndNxt
	entry.peerWindow, entry.peerMSS = uint32(segment.window), segment.mss
	entry.sndWL1, entry.sndWL2 = segment.sequence, segment.acknowledgment
	entry.cwnd, entry.retries = tcpInitialWindow(entry), 0
	entry.state, entry.synchronized = TCPEstablished, true
	stack.setRTO(entry, 0)
	stack.setDeadline(entry, 0)
	stack.sendTCP(entry, tcpFlagACK, entry.sndNxt, entry.rcvNxt, nil, now)
}

func (stack *Stack) handleSynReceived(entry *tcpEndpoint, segment tcpSegment, now uint64) {
	if segment.flags&tcpFlagSYN != 0 && segment.sequence == entry.irs {
		stack.sendTCP(entry, tcpFlagSYN|tcpFlagACK, entry.iss, entry.rcvNxt, nil, now)
		return
	}
	if segment.flags&tcpFlagACK == 0 || segment.acknowledgment != entry.sndNxt || segment.sequence != entry.rcvNxt {
		return
	}
	entry.sndUna = segment.acknowledgment
	entry.sendSeq = entry.sndNxt
	entry.peerWindow = uint32(segment.window)
	entry.sndWL1, entry.sndWL2 = segment.sequence, segment.acknowledgment
	entry.cwnd, entry.retries = tcpInitialWindow(entry), 0
	entry.state, entry.synchronized = TCPEstablished, true
	stack.setRTO(entry, 0)
	stack.setDeadline(entry, 0)
}

func (stack *Stack) handleRST(entry *tcpEndpoint, segment tcpSegment, now uint64) {
	valid := entry.state == TCPSynSent && segment.flags&tcpFlagACK != 0 && segment.acknowledgment == entry.sndNxt
	if entry.state >= TCPSynReceived && entry.state < TCPTimeWait {
		valid = segment.sequence == entry.rcvNxt
	}
	if !valid {
		window := uint32(stack.tcpAdvertisedWindow(entry))
		if entry.state >= TCPSynReceived && entry.state < TCPTimeWait &&
			seqGE(segment.sequence, entry.rcvNxt) && seqLT(segment.sequence, entry.rcvNxt+window) &&
			(entry.challengeAt == 0 || now-entry.challengeAt >= second) {
			entry.challengeAt = now
			stack.sendTCP(entry, tcpFlagACK, entry.sndNxt, entry.rcvNxt, nil, now)
		}
		return
	}
	result := ResultReset
	if entry.state == TCPSynSent {
		result = ResultConnectionRefused
	}
	increment(&stack.tcpStats.RXReset)
	stack.closeTCP(entry, result)
}

// handleACK applies the acknowledgment and window of a segment that passed
// the sequence check. It returns false when the segment's payload and FIN
// must not be processed.
func (stack *Stack) handleACK(entry *tcpEndpoint, segment tcpSegment, now uint64) bool {
	acknowledgment, window := segment.acknowledgment, uint32(segment.window)
	if seqGT(acknowledgment, entry.sndNxt) {
		stack.sendTCP(entry, tcpFlagACK, entry.sndNxt, entry.rcvNxt, nil, now)
		return false
	}
	if seqLT(acknowledgment, entry.sndUna) {
		return true
	}
	oldWindow := entry.peerWindow
	if seqLT(entry.sndWL1, segment.sequence) || (entry.sndWL1 == segment.sequence && seqLE(entry.sndWL2, acknowledgment)) {
		entry.peerWindow, entry.sndWL1, entry.sndWL2 = window, segment.sequence, acknowledgment
		if window != 0 {
			entry.persist = 0
		}
	}
	smss := tcpSMSS(entry)
	if acknowledgment == entry.sndUna {
		pure := len(segment.payload) == 0 && segment.flags&tcpFlagFIN == 0
		if pure && window == oldWindow && entry.sendSent != 0 {
			entry.dupACKs++
			if entry.dupACKs == 3 {
				flight := entry.sndNxt - entry.sndUna
				entry.ssthresh = max(flight/2, 2*smss)
				entry.cwnd = entry.ssthresh + 3*smss
				entry.recover = entry.sndNxt
				entry.fastRecover = true
				stack.retransmitTCP(entry, now)
			} else if entry.dupACKs > 3 && entry.fastRecover {
				entry.cwnd += smss
				stack.pumpTCP(entry, now)
			}
		}
		if entry.peerWindow > oldWindow {
			stack.setRTO(entry, 0)
			stack.pumpTCP(entry, now)
			// A FIN the peer dropped at a closed window still waits for
			// its ACK; without the timer nothing would resend it.
			if entry.rtoDeadline == 0 && entry.sndUna != entry.sndNxt {
				entry.rtoDeadline = addTime(now, entry.rto)
			}
		}
		return true
	}
	entry.dupACKs, entry.retries = 0, 0
	oldUna := entry.sndUna
	entry.sndUna = acknowledgment
	if entry.sendLen != 0 && seqGT(acknowledgment, entry.sendSeq) {
		acked := min(acknowledgment-entry.sendSeq, entry.sendSent)
		if acked > 0 {
			entry.sendHead = ringAdvance(int(entry.sendHead), int(acked), len(entry.sendRing))
			entry.sendLen -= acked
			entry.sendSent -= acked
			entry.sendSeq += acked
			if entry.sendLen == 0 {
				entry.sendHead = 0
			}
		}
	}
	if entry.rttAt != 0 && seqGE(acknowledgment, entry.rttSeq) && !entry.retransmit {
		stack.updateRTO(entry, now-entry.rttAt)
		entry.rttAt = 0
	}
	entry.retransmit = false
	if entry.fastRecover {
		if seqLT(acknowledgment, entry.recover) {
			entry.cwnd = entry.ssthresh + 3*smss
			stack.retransmitTCP(entry, now)
		} else {
			entry.fastRecover = false
			entry.cwnd = entry.ssthresh
		}
	} else if entry.cwnd < entry.ssthresh {
		entry.cwnd += min(acknowledgment-oldUna, smss)
	} else {
		entry.cwnd += max(smss*smss/max(entry.cwnd, 1), uint32(1))
	}
	if entry.sndUna == entry.sndNxt {
		stack.setRTO(entry, 0)
	} else {
		stack.setRTO(entry, addTime(now, entry.rto))
	}
	if entry.finSent && seqGT(acknowledgment, entry.finSequence) {
		switch entry.state {
		case TCPFinWait1:
			entry.state = TCPFinWait2
			if entry.orphan {
				stack.setDeadline(entry, addTime(now, stack.tuning.TCP.TimeWait))
			}
		case TCPClosing:
			stack.enterTimeWait(entry, now)
		case TCPLastAck:
			stack.closeTCP(entry, ResultOK)
			return false
		}
	}
	stack.pumpTCP(entry, now)
	return true
}

func (stack *Stack) handlePayload(entry *tcpEndpoint, segment tcpSegment, now uint64) {
	fin := segment.flags&tcpFlagFIN != 0
	if entry.finReceived {
		// The stream ended at the peer's FIN. A retransmission only needs an ACK.
		if len(segment.payload) != 0 || fin {
			stack.sendTCP(entry, tcpFlagACK, entry.sndNxt, entry.rcvNxt, nil, now)
		}
		return
	}
	stack.allocateReceive(entry, false)
	window := uint32(stack.tcpAdvertisedWindow(entry))
	if fin {
		stack.notePeerFIN(entry, segment.sequence+uint32(len(segment.payload)), window)
	}
	sequence := segment.sequence
	payload := segment.payload
	if seqLT(sequence, entry.rcvNxt) {
		trim := int(entry.rcvNxt - sequence)
		if trim >= len(payload) {
			payload = nil
		} else {
			payload = payload[trim:]
		}
		sequence = entry.rcvNxt
	}
	payload = clipToPeerFIN(entry, sequence, payload)
	if sequence == entry.rcvNxt || (seqGT(sequence, entry.rcvNxt) && seqLT(sequence, entry.rcvNxt+window)) {
		windowEnd := entry.rcvNxt + window
		if seqGT(sequence+uint32(len(payload)), windowEnd) {
			payload = payload[:windowEnd-sequence]
		}
		if !stack.storeTCP(entry, sequence, payload) {
			increment(&stack.tcpStats.DropWindow)
		}
		stack.growReceive(entry)
	} else if len(payload) != 0 {
		increment(&stack.tcpStats.DropWindow)
	}
	if entry.peerFIN && entry.rcvNxt == entry.peerFINAt {
		stack.receiveFIN(entry, now)
	}
	if len(segment.payload) != 0 || fin {
		stack.sendTCP(entry, tcpFlagACK, entry.sndNxt, entry.rcvNxt, nil, now)
	}
}

// notePeerFIN records a FIN only when its own sequence number lies in the
// current receive window. Payload clipping does not decide FIN acceptability.
// The first accepted FIN fixes the stream end. Held bytes past it are discarded.
func (stack *Stack) notePeerFIN(entry *tcpEndpoint, at, window uint32) {
	if entry.peerFIN || window == 0 || !seqGE(at, entry.rcvNxt) || !seqLT(at, entry.rcvNxt+window) {
		return
	}
	entry.peerFIN, entry.peerFINAt = true, at
	entry.clipHeld(at)
}

// clipToPeerFIN drops the bytes of payload, which starts at sequence, that
// lie at or after a known FIN.
func clipToPeerFIN(entry *tcpEndpoint, sequence uint32, payload []byte) []byte {
	if !entry.peerFIN || !seqGT(sequence+uint32(len(payload)), entry.peerFINAt) {
		return payload
	}
	if seqGE(sequence, entry.peerFINAt) {
		return nil
	}
	return payload[:entry.peerFINAt-sequence]
}

// tcpReceiveFree is the receive window: buffer room past the contiguous
// bytes. Held out-of-order bytes sit inside it, so holding them never
// moves the window's right edge back.
func (stack *Stack) tcpReceiveFree(entry *tcpEndpoint) int {
	return max(stack.tcpReceiveLimit(entry)-int(entry.recvLen), 0)
}

func (stack *Stack) tcpAdvertisedWindow(entry *tcpEndpoint) int {
	return stack.tcpReceiveFree(entry)
}

// tcpOfferedWindow is the window a segment advertises. The right edge moves
// only by a segment or half the buffer, whichever is less (RFC 1122
// 4.2.3.3): an ACK that offered each few hundred bytes a reader freed would
// make the sender cut the stream into segments that small.
func (stack *Stack) tcpOfferedWindow(entry *tcpEndpoint) int {
	free := stack.tcpAdvertisedWindow(entry)
	if !entry.rcvEdgeKnown || !seqGE(entry.rcvEdge, entry.rcvNxt) {
		return free
	}
	step := min(tcpSMSS(entry), uint32(stack.tcpReceiveLimit(entry)/2))
	if int32(entry.rcvNxt+uint32(free)-entry.rcvEdge) >= int32(step) {
		return free
	}
	return min(int(entry.rcvEdge-entry.rcvNxt), free)
}

func (stack *Stack) tcpSegmentAcceptable(entry *tcpEndpoint, segment tcpSegment) bool {
	length := uint32(len(segment.payload))
	if segment.flags&tcpFlagSYN != 0 {
		length++
	}
	if segment.flags&tcpFlagFIN != 0 {
		length++
	}
	window := uint32(stack.tcpAdvertisedWindow(entry))
	if length == 0 {
		if window == 0 {
			return segment.sequence == entry.rcvNxt
		}
		return seqGE(segment.sequence, entry.rcvNxt) && seqLT(segment.sequence, entry.rcvNxt+window)
	}
	if window == 0 {
		return false
	}
	last := segment.sequence + length - 1
	windowEnd := entry.rcvNxt + window
	return (seqGE(segment.sequence, entry.rcvNxt) && seqLT(segment.sequence, windowEnd)) ||
		(seqGE(last, entry.rcvNxt) && seqLT(last, windowEnd))
}

// receiveFIN consumes the peer's FIN once every byte before it is in the
// receive stream.
func (stack *Stack) receiveFIN(entry *tcpEndpoint, now uint64) {
	if entry.finReceived {
		return
	}
	entry.finReceived = true
	entry.rcvNxt++
	entry.reorder = nil
	switch entry.state {
	case TCPEstablished:
		entry.state = TCPCloseWait
	case TCPFinWait1:
		entry.state = TCPClosing
		if entry.finSent && entry.sndUna == entry.sndNxt {
			stack.enterTimeWait(entry, now)
		}
	case TCPFinWait2:
		stack.enterTimeWait(entry, now)
	}
}

// enterTimeWait starts the interval. Every byte sent was acknowledged, so
// the send ring goes; the receive ring stays for bytes not yet read.
func (stack *Stack) enterTimeWait(entry *tcpEndpoint, now uint64) {
	entry.state = TCPTimeWait
	stack.setDeadline(entry, addTime(now, stack.tuning.TCP.TimeWait))
	stack.setRTO(entry, 0)
	stack.freeSend(entry)
}

func (stack *Stack) pumpTCP(entry *tcpEndpoint, now uint64) {
	if entry.state != TCPEstablished && entry.state != TCPCloseWait {
		return
	}
	local := stack.tcpLocal(entry)
	for entry.sendSent < entry.sendLen {
		flight := entry.sndNxt - entry.sndUna
		limit := min(entry.peerWindow, entry.cwnd)
		if flight >= limit {
			break
		}
		length := min(int(entry.sendLen-entry.sendSent), int(tcpSMSS(entry)), int(limit-flight))
		if local {
			length = min(length, entry.sendContiguous(int(entry.sendSent)))
		}
		if length <= 0 {
			break
		}
		start := entry.sendSent
		sequence := entry.sndNxt
		entry.sendSent += uint32(length)
		entry.sndNxt += uint32(length)
		if entry.rttAt == 0 {
			entry.rttAt, entry.rttSeq = now, entry.sndNxt
		}
		if !stack.sendTCPData(entry, tcpFlagACK|tcpFlagPSH, sequence, int(start), length, now) {
			// The RTO timer pumps again, so a full TX queue or a missing
			// neighbour cannot strand buffered data.
			if entry.sendSent >= uint32(length) {
				entry.sendSent -= uint32(length)
				entry.sndNxt -= uint32(length)
			}
			if entry.rtoDeadline == 0 {
				stack.setRTO(entry, addTime(now, entry.rto))
			}
			break
		}
		// A loopback peer may have acknowledged or released the entry already.
		if entry.rtoDeadline == 0 && entry.sndUna != entry.sndNxt {
			stack.setRTO(entry, addTime(now, entry.rto))
		}
	}
	if entry.shutdownWrite && entry.sendLen == entry.sendSent && !entry.finSent &&
		(entry.state == TCPEstablished || entry.state == TCPCloseWait) {
		entry.finSent, entry.finSequence = true, entry.sndNxt
		entry.sndNxt++
		if entry.state == TCPEstablished {
			entry.state = TCPFinWait1
		} else {
			entry.state = TCPLastAck
		}
		// Arm the timer even when the FIN could not be sent; it is resent on
		// expiry. A loopback peer may have acknowledged or released the entry.
		stack.sendTCP(entry, tcpFlagFIN|tcpFlagACK, entry.finSequence, entry.rcvNxt, nil, now)
		if entry.finSent && entry.sndUna != entry.sndNxt {
			stack.setRTO(entry, addTime(now, entry.rto))
		}
	}
	stack.armPersist(entry, now)
}

// armPersist starts the persist timer of a connection whose peer closed
// its window while bytes wait to be sent.
func (stack *Stack) armPersist(entry *tcpEndpoint, now uint64) {
	if entry.slot != 0 && entry.peerWindow == 0 && entry.sendLen > entry.sendSent && entry.rtoDeadline == 0 &&
		entry.state >= TCPEstablished && entry.state < TCPTimeWait {
		stack.setRTO(entry, addTime(now, entry.rto))
	}
}

// retransmitTCP resends the oldest unacknowledged segment. The counter
// records only segments that left the stack.
func (stack *Stack) retransmitTCP(entry *tcpEndpoint, now uint64) {
	entry.retransmit = true
	sent := false
	switch entry.state {
	case TCPSynSent:
		sent = stack.sendTCP(entry, tcpFlagSYN, entry.iss, 0, nil, now)
	case TCPSynReceived:
		sent = stack.sendTCP(entry, tcpFlagSYN|tcpFlagACK, entry.iss, entry.rcvNxt, nil, now)
	default:
		if entry.sendSent != 0 {
			length := min(int(entry.sendSent), int(tcpSMSS(entry)))
			if stack.tcpLocal(entry) {
				length = min(length, entry.sendContiguous(0))
			}
			sent = stack.sendTCPData(entry, tcpFlagACK|tcpFlagPSH, entry.sendSeq, 0, length, now)
		} else if entry.finSent && entry.sndUna == entry.finSequence {
			sent = stack.sendTCP(entry, tcpFlagFIN|tcpFlagACK, entry.finSequence, entry.rcvNxt, nil, now)
		} else {
			stack.pumpTCP(entry, now)
		}
	}
	if sent {
		increment(&stack.tcpStats.Retransmits)
	}
}

func (stack *Stack) updateRTO(entry *tcpEndpoint, sample uint64) {
	if sample == 0 {
		sample = 1
	}
	if entry.srtt == 0 {
		entry.srtt, entry.rttvar = sample, sample/2
	} else {
		delta := entry.srtt
		if sample > delta {
			delta = sample - delta
		} else {
			delta -= sample
		}
		entry.rttvar = (3*entry.rttvar + delta) / 4
		entry.srtt = (7*entry.srtt + sample) / 8
	}
	entry.rto = entry.srtt + max(4*entry.rttvar, millisecond)
	entry.rto = min(max(entry.rto, stack.tuning.TCP.RTOMin), stack.tuning.TCP.RTOMax)
}

func (stack *Stack) sendTCP(entry *tcpEndpoint, flags uint8, sequence, acknowledgment uint32, payload []byte, now uint64) bool {
	return stack.sendTCPParts(entry, flags, sequence, acknowledgment, payload, nil, now)
}

// sendTCPParts sends a segment whose payload continues in tail.
func (stack *Stack) sendTCPParts(entry *tcpEndpoint, flags uint8, sequence, acknowledgment uint32, payload, tail []byte, now uint64) bool {
	route, result := stack.udpRoute(entry.local, entry.remote)
	if result != ResultOK {
		return false
	}
	segment := tcpSegment{sourcePort: entry.port, destinationPort: entry.remotePort, sequence: sequence,
		acknowledgment: acknowledgment, flags: flags, window: uint16(min(stack.tcpOfferedWindow(entry), 65535)),
		mss: entry.localMSS, payload: payload, tail: tail}

	if route.path == udpLocal {
		// No wire lies between the two ends: the segment goes to the same
		// receive state machine as a parsed one, without a header or a
		// checksum to build and check. A segment whose payload wraps the
		// send ring never comes here; sendTCPData splits it.
		if len(tail) != 0 {
			return false
		}
		if stack.localWire != nil {
			// Tests compare the logical path against the parsed one.
			length := putTCP(stack.localWire, entry.local, entry.remote, segment)
			entry.advertised(flags, acknowledgment, segment.window)
			stack.receiveTCP(ipv4Packet{source: entry.local, destination: entry.remote, protocol: protocolTCP, payload: stack.localWire[:length]}, now)
			increment(&stack.tcpStats.TX)
			return true
		}
		if flags&tcpFlagSYN == 0 {
			segment.mss = tcpDefaultMSS
		}
		packet := ipv4Packet{source: entry.local, destination: entry.remote, protocol: protocolTCP}
		increment(&stack.tcpStats.TX)
		increment(&stack.tcpStats.LocalSegments)
		entry.advertised(flags, acknowledgment, segment.window)
		stack.receiveSegment(packet, segment, now)
		return true
	}
	mac, known := stack.lookupNeighbor(route.nextHop, now)
	if !known {
		if len(payload)+len(tail) != 0 || flags&(tcpFlagSYN|tcpFlagFIN) != 0 {
			entry.l2Pending = true
		}
		stack.resolve(route.nextHop, now)
		return false
	}
	frame := stack.frameBuffer()
	if frame == nil {
		if len(payload)+len(tail) == 0 && flags == tcpFlagACK && entry.slot != 0 && !entry.ackPending {
			entry.ackPending = true
			stack.tcpAckPending = append(stack.tcpAckPending, entry)
		}
		return false
	}
	const tcpAt = HeaderSize + ipv4HeaderSize
	length := putTCP(frame[tcpAt:], entry.local, entry.remote, segment)
	stack.identification++
	putIPv4Header(frame[HeaderSize:], stack.tuning.TTL, entry.local, entry.remote, protocolTCP, stack.identification, length)
	putEthernet(frame, mac, stack.mac, etherTypeIPv4)
	stack.commitFrame(tcpAt + length)
	increment(&stack.tcpStats.TX)
	entry.advertised(flags, acknowledgment, segment.window)
	return true
}

func (stack *Stack) resetUnknown(packet ipv4Packet, segment tcpSegment, now uint64) {
	if segment.flags&tcpFlagRST != 0 {
		return
	}
	entry := tcpEndpoint{local: packet.destination, port: segment.destinationPort,
		remote: packet.source, remotePort: segment.sourcePort, localMSS: tcpDefaultMSS}
	flags, sequence, acknowledgment := uint8(tcpFlagRST|tcpFlagACK), uint32(0), segment.sequence+uint32(len(segment.payload))
	if segment.flags&tcpFlagSYN != 0 {
		acknowledgment++
	}
	if segment.flags&tcpFlagFIN != 0 {
		acknowledgment++
	}
	if segment.flags&tcpFlagACK != 0 {
		flags, sequence, acknowledgment = tcpFlagRST, segment.acknowledgment, 0
	}
	if stack.sendTCP(&entry, flags, sequence, acknowledgment, nil, now) {
		increment(&stack.tcpStats.TXReset)
	}
}

// setRTO and setDeadline change an endpoint's timers and keep the heap
// entry at the earlier of the two.
func (stack *Stack) setRTO(entry *tcpEndpoint, at uint64) {
	entry.rtoDeadline = at
	stack.rearmTCP(entry)
}

func (stack *Stack) setDeadline(entry *tcpEndpoint, at uint64) {
	entry.deadline = at
	stack.rearmTCP(entry)
}

func (stack *Stack) rearmTCP(entry *tcpEndpoint) {
	if entry.slot != 0 {
		stack.tcpTimers.set(entry.slot, tcpTimerKey(entry))
	}
}

// tcpTimerKey is the earlier of an endpoint's two deadlines, zero for none.
func tcpTimerKey(entry *tcpEndpoint) uint64 {
	switch {
	case entry.deadline == 0:
		return entry.rtoDeadline
	case entry.rtoDeadline == 0:
		return entry.deadline
	}
	return min(entry.deadline, entry.rtoDeadline)
}

// advanceTCP runs the due timers in deadline order. It touches only
// endpoints with a due timer or a pending ACK.
func (stack *Stack) advanceTCP(now uint64) {
	stack.advanceTombstones(now)
	stack.flushPendingACKs(now)
	for {
		ref, at, ok := stack.tcpTimers.earliest()
		if !ok || at > now {
			break
		}
		entry := stack.tcp[ref-1]
		stack.expireTCP(entry, now)
		// A deadline no state handles would come round again at once.
		if entry.slot == ref && tcpTimerKey(entry) != 0 && tcpTimerKey(entry) <= now {
			if entry.deadline != 0 && entry.deadline <= now {
				stack.setDeadline(entry, 0)
			}
			if entry.rtoDeadline != 0 && entry.rtoDeadline <= now {
				stack.setRTO(entry, 0)
			}
		}
		stack.settleTCP(entry)
	}
	if tcpCrossCheck {
		if mismatch := stack.checkTCP(); mismatch != "" {
			panic("tcp: " + mismatch)
		}
	}
}

// expireTCP runs an endpoint's due timers.
func (stack *Stack) expireTCP(entry *tcpEndpoint, now uint64) {
	if entry.state == TCPFree || entry.state == TCPClosed {
		return
	}
	if (entry.state == TCPTimeWait || (entry.state == TCPFinWait2 && entry.orphan)) && entry.deadline != 0 && entry.deadline <= now {
		stack.releaseTCP(entry)
		return
	}
	if (entry.state == TCPSynSent || entry.state == TCPSynReceived) && entry.deadline != 0 && entry.deadline <= now {
		if entry.state == TCPSynReceived {
			stack.releaseTCP(entry)
			return
		}
		stack.closeTCP(entry, ResultTimeout)
		return
	}
	if entry.rtoDeadline != 0 && entry.rtoDeadline <= now {
		if entry.sndUna == entry.sndNxt && entry.sendLen == 0 {
			stack.setRTO(entry, 0)
			return
		}
		if entry.peerWindow == 0 && entry.sendSent == 0 && entry.sendLen != 0 {
			if entry.persist == 8 {
				stack.closeTCP(entry, ResultTimeout)
				return
			}
			entry.persist++
			stack.sendTCPData(entry, tcpFlagACK, entry.sndNxt, int(entry.sendSent), 1, now)
			increment(&stack.tcpStats.Retransmits)
			entry.rto = min(max(entry.rto*2, stack.tuning.TCP.RTOMin), stack.tuning.TCP.RTOMax)
			stack.setRTO(entry, addTime(now, entry.rto))
			return
		}
		if entry.state >= TCPEstablished && entry.retries >= tcpMaxRetransmits {
			stack.closeTCP(entry, ResultTimeout)
			return
		}
		entry.retries++
		if entry.state >= TCPEstablished {
			smss := tcpSMSS(entry)
			entry.fastRecover = false
			entry.ssthresh = max((entry.sndNxt-entry.sndUna)/2, 2*smss)
			entry.cwnd = smss
		}
		stack.retransmitTCP(entry, now)
		entry.rto = min(max(entry.rto*2, stack.tuning.TCP.RTOMin), stack.tuning.TCP.RTOMax)
		stack.setRTO(entry, addTime(now, entry.rto))
	}
	stack.armPersist(entry, now)
}

// flushPendingACKs sends the ACKs a full transmit queue held back, while
// frames are free.
func (stack *Stack) flushPendingACKs(now uint64) {
	if len(stack.tcpAckPending) == 0 || stack.txCount >= TxQueueFrames {
		return
	}
	pending := stack.tcpAckPending
	stack.tcpAckPending = stack.tcpAckSpare[:0]
	for index, entry := range pending {
		if stack.txCount >= TxQueueFrames {
			stack.tcpAckPending = append(stack.tcpAckPending, pending[index:]...)
			break
		}
		if entry.slot == 0 || !entry.ackPending {
			continue
		}
		entry.ackPending = false
		if entry.state >= TCPEstablished && entry.state < TCPClosed {
			stack.sendTCP(entry, tcpFlagACK, entry.sndNxt, entry.rcvNxt, nil, now)
		}
	}
	clear(pending)
	stack.tcpAckSpare = pending[:0]
}

func (stack *Stack) tcpDeadline(earliest *timer) {
	stack.tombstoneDeadline(earliest)
	if _, at, ok := stack.tcpTimers.earliest(); ok {
		earliest.consider(at)
	}
}

// reconfigureTCP ends what the address change took away. It walks every
// endpoint; it runs once per reconfiguration, not per segment.
func (stack *Stack) reconfigureTCP(result Result) {
	if result == ResultDown {
		return
	}
	stack.dropTombstones()
	for index := stack.nextTCP(0); index >= 0; index = stack.nextTCP(index + 1) {
		entry := stack.tcp[index]
		if entry.state == TCPBound || entry.state == TCPListen {
			if entry.local != zeroIPv4 && !entry.local.IsLoopback() {
				stack.releaseTCP(entry)
			}
			continue
		}
		if entry.state < TCPSynSent || entry.local.IsLoopback() {
			continue
		}
		stack.closeTCP(entry, result)
	}
}

// sendPendingTCP resends what waited for nextHop to resolve. It walks every
// endpoint once per resolved neighbour.
func (stack *Stack) sendPendingTCP(nextHop IPv4, now uint64) {
	for index := stack.nextTCP(0); index >= 0; index = stack.nextTCP(index + 1) {
		entry := stack.tcp[index]
		if !entry.l2Pending || !stack.tcpVia(entry, nextHop) {
			continue
		}
		entry.l2Pending = false
		switch entry.state {
		case TCPSynSent:
			if stack.sendTCP(entry, tcpFlagSYN, entry.iss, 0, nil, now) {
				stack.setRTO(entry, addTime(now, entry.rto))
			}
		case TCPSynReceived:
			if stack.sendTCP(entry, tcpFlagSYN|tcpFlagACK, entry.iss, entry.rcvNxt, nil, now) {
				stack.setRTO(entry, addTime(now, entry.rto))
			}
		default:
			if entry.sendSent != 0 {
				stack.retransmitTCP(entry, now)
			} else if entry.finSent && entry.sndUna == entry.finSequence {
				stack.sendTCP(entry, tcpFlagFIN|tcpFlagACK, entry.finSequence, entry.rcvNxt, nil, now)
			} else {
				stack.pumpTCP(entry, now)
			}
		}
	}
}

// failPendingTCP ends the connections whose segments waited for nextHop when
// its resolution failed. Connections that merely route through it stay.
func (stack *Stack) failPendingTCP(nextHop IPv4, result Result) {
	for index := stack.nextTCP(0); index >= 0; index = stack.nextTCP(index + 1) {
		entry := stack.tcp[index]
		if entry.l2Pending && stack.tcpVia(entry, nextHop) {
			entry.l2Pending = false
			stack.closeTCP(entry, result)
		}
	}
}

func (stack *Stack) tcpVia(entry *tcpEndpoint, nextHop IPv4) bool {
	if entry.state < TCPSynSent || entry.state >= TCPClosed {
		return false
	}
	route, result := stack.udpRoute(entry.local, entry.remote)
	return result == ResultOK && route.path != udpLocal && route.nextHop == nextHop
}

// advertised records the window edge a sent ACK told the peer.
func (entry *tcpEndpoint) advertised(flags uint8, acknowledgment uint32, window uint16) {
	if flags&tcpFlagACK != 0 {
		entry.rcvEdge, entry.rcvEdgeKnown = acknowledgment+uint32(window), true
		entry.ackPending = false
	}
}
