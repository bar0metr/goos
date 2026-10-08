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
	increment(&stack.tcpStats.RX)
	entry := stack.tcpMatch(packet.destination, segment.destinationPort, packet.source, segment.sourcePort)
	if entry == nil {
		if tombstone := stack.tcpTombstone(packet.destination, segment.destinationPort, packet.source, segment.sourcePort); tombstone != nil {
			stack.receiveTombstone(tombstone, segment, now)
			return
		}
		listener := stack.tcpListener(packet.destination, segment.destinationPort)
		if listener != nil && segment.flags&tcpFlagSYN != 0 && segment.flags&(tcpFlagACK|tcpFlagRST) == 0 {
			stack.passiveSYN(listener, packet, segment, now)
			return
		}
		increment(&stack.tcpStats.DropNoTCB)
		stack.resetUnknown(packet, segment, now)
		return
	}
	stack.handleTCP(entry, packet, segment, now)
}

func (stack *Stack) tcpMatch(local IPv4, localPort uint16, remote IPv4, remotePort uint16) *tcpEndpoint {
	for index := range stack.tcp[:] {
		entry := &stack.tcp[index]
		if entry.state >= TCPSynSent && entry.local == local && entry.port == localPort &&
			entry.remote == remote && entry.remotePort == remotePort {
			return entry
		}
	}
	return nil
}

func (stack *Stack) tcpListener(local IPv4, port uint16) *tcpEndpoint {
	var wildcard *tcpEndpoint
	for index := range stack.tcp[:] {
		entry := &stack.tcp[index]
		if entry.state != TCPListen || entry.port != port {
			continue
		}
		if entry.local == local {
			return entry
		}
		if entry.local == zeroIPv4 {
			wildcard = entry
		}
	}
	return wildcard
}

func (stack *Stack) listenerChildren(listener TCPEndpointID) int {
	count := 0
	for index := range stack.tcp[:] {
		if stack.tcp[index].listener == listener && stack.tcp[index].state != TCPFree && !stack.tcp[index].accepted {
			count++
		}
	}
	return count
}

func (stack *Stack) passiveSYN(listener *tcpEndpoint, packet ipv4Packet, segment tcpSegment, now uint64) {
	listenerSlot := -1
	for index := range stack.tcp[:] {
		if listener == &stack.tcp[index] {
			listenerSlot = index
			break
		}
	}
	if listenerSlot < 0 || stack.listenerChildren(tcpID(listener, listenerSlot)) >= int(listener.backlog) {
		return
	}
	id, result := stack.TCPOpen(int(listener.owner))
	if result != ResultOK {
		return
	}
	child, _ := stack.tcpEndpoint(int(listener.owner), id)
	route, result := stack.udpRoute(packet.destination, packet.source)
	if result != ResultOK {
		stack.releaseTCP(child)
		return
	}
	child.local, child.port = packet.destination, segment.destinationPort
	child.remote, child.remotePort = packet.source, segment.sourcePort
	child.listener = tcpID(listener, listenerSlot)
	child.irs, child.rcvNxt = segment.sequence, segment.sequence+1
	child.iss = uint32(stack.next())
	child.sndUna, child.sndNxt = child.iss, child.iss+1
	child.peerWindow, child.peerMSS = uint32(segment.window), segment.mss
	child.localMSS = stack.tcpPathMSS(route)
	child.state = TCPSynReceived
	child.deadline = addTime(now, stack.tuning.TCP.ConnectTimeout)
	child.rto = stack.tuning.TCP.RTOMin
	if stack.sendTCP(child, tcpFlagSYN|tcpFlagACK, child.iss, child.rcvNxt, nil, now) && child.state == TCPSynReceived {
		child.rtoDeadline = addTime(now, child.rto)
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
			entry.deadline = addTime(now, stack.tuning.TCP.TimeWait)
		}
		return
	}
	if entry.state < TCPEstablished || entry.state >= TCPClosed {
		return
	}
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
	entry.state, entry.synchronized, entry.rtoDeadline, entry.deadline = TCPEstablished, true, 0, 0
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
	entry.state, entry.synchronized, entry.rtoDeadline, entry.deadline = TCPEstablished, true, 0, 0
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
			entry.rtoDeadline = 0
			stack.pumpTCP(entry, now)
		}
		return true
	}
	entry.dupACKs, entry.retries = 0, 0
	oldUna := entry.sndUna
	entry.sndUna = acknowledgment
	if entry.sendLen != 0 && seqGT(acknowledgment, entry.sendSeq) {
		acked := int(acknowledgment - entry.sendSeq)
		if acked > int(entry.sendSent) {
			acked = int(entry.sendSent)
		}
		if acked > 0 {
			copy(entry.send[:], entry.send[acked:entry.sendLen])
			entry.sendLen -= uint16(acked)
			entry.sendSent -= uint16(acked)
			entry.sendSeq += uint32(acked)
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
		entry.rtoDeadline = 0
	} else {
		entry.rtoDeadline = addTime(now, entry.rto)
	}
	if entry.finSent && seqGT(acknowledgment, entry.finSequence) {
		switch entry.state {
		case TCPFinWait1:
			entry.state = TCPFinWait2
			if entry.orphan {
				entry.deadline = addTime(now, stack.tuning.TCP.TimeWait)
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
	if sequence == entry.rcvNxt {
		stack.appendTCPReceive(entry, payload)
		if len(payload) != 0 {
			stack.promoteTCP(entry)
		}
	} else if seqGT(sequence, entry.rcvNxt) && seqLT(sequence, entry.rcvNxt+window) {
		windowEnd := entry.rcvNxt + window
		if seqGT(sequence+uint32(len(payload)), windowEnd) {
			payload = payload[:windowEnd-sequence]
		}
		if len(payload) != 0 {
			stack.queueTCPReorder(entry, sequence, payload)
		}
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
	for index := range entry.reorder[:] {
		held := &entry.reorder[index]
		if !held.used {
			continue
		}
		if seqGE(held.seq, at) {
			*held = tcpReorder{}
		} else if seqGT(held.seq+uint32(held.len), at) {
			held.len = uint16(at - held.seq)
		}
	}
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

func (stack *Stack) appendTCPReceive(entry *tcpEndpoint, payload []byte) {
	free := stack.tcpReceiveFree(entry)
	if len(payload) > free {
		payload = payload[:free]
	}
	if len(payload) == 0 {
		return
	}
	copy(entry.receive[entry.recvLen:], payload)
	entry.recvLen += uint16(len(payload))
	entry.rcvNxt += uint32(len(payload))
}

func (stack *Stack) tcpReceiveFree(entry *tcpEndpoint) int {
	limit := int(stack.tuning.TCP.ReceiveBuffer)
	if limit > TCPReceiveCapacity {
		limit = TCPReceiveCapacity
	}
	return max(limit-int(entry.recvLen), 0)
}

func (stack *Stack) tcpAdvertisedWindow(entry *tcpEndpoint) int {
	free := stack.tcpReceiveFree(entry)
	for index := range entry.reorder[:] {
		if entry.reorder[index].used {
			free -= int(entry.reorder[index].len)
		}
	}
	return max(free, 0)
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

func (stack *Stack) queueTCPReorder(entry *tcpEndpoint, sequence uint32, payload []byte) {
	if len(payload) > TCPReorderPayload {
		payload = payload[:TCPReorderPayload]
	}
	for index := range entry.reorder[:] {
		held := &entry.reorder[index]
		if held.used && held.seq == sequence {
			if len(payload) > int(held.len) {
				held.len = uint16(copy(held.data[:], payload))
			}
			return
		}
	}
	for index := range entry.reorder[:] {
		held := &entry.reorder[index]
		if !held.used {
			held.used, held.seq = true, sequence
			held.len = uint16(copy(held.data[:], payload))
			return
		}
	}
	increment(&stack.tcpStats.DropWindow)
}

// promoteTCP appends held segments that have become contiguous.
func (stack *Stack) promoteTCP(entry *tcpEndpoint) {
	for {
		found := false
		for index := range entry.reorder[:] {
			held := &entry.reorder[index]
			if !held.used || seqGT(held.seq, entry.rcvNxt) {
				continue
			}
			offset := int(entry.rcvNxt - held.seq)
			if offset > int(held.len) {
				offset = int(held.len)
			}
			copyHeld := *held
			*held = tcpReorder{}
			data := clipToPeerFIN(entry, entry.rcvNxt, copyHeld.data[offset:copyHeld.len])
			stack.appendTCPReceive(entry, data)
			found = true
			break
		}
		if !found {
			return
		}
	}
}

// receiveFIN consumes the peer's FIN once every byte before it is in the
// receive stream.
func (stack *Stack) receiveFIN(entry *tcpEndpoint, now uint64) {
	if entry.finReceived {
		return
	}
	entry.finReceived = true
	entry.rcvNxt++
	for index := range entry.reorder[:] {
		entry.reorder[index] = tcpReorder{}
	}
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

func (stack *Stack) enterTimeWait(entry *tcpEndpoint, now uint64) {
	entry.state = TCPTimeWait
	entry.deadline = addTime(now, stack.tuning.TCP.TimeWait)
	entry.rtoDeadline = 0
}

func (stack *Stack) pumpTCP(entry *tcpEndpoint, now uint64) {
	if entry.state != TCPEstablished && entry.state != TCPCloseWait {
		return
	}
	for entry.sendSent < entry.sendLen {
		flight := entry.sndNxt - entry.sndUna
		limit := min(entry.peerWindow, entry.cwnd)
		if flight >= limit {
			break
		}
		length := min(int(entry.sendLen-entry.sendSent), int(tcpSMSS(entry)), int(limit-flight))
		if length <= 0 {
			break
		}
		start := entry.sendSent
		sequence := entry.sndNxt
		entry.sendSent += uint16(length)
		entry.sndNxt += uint32(length)
		if entry.rttAt == 0 {
			entry.rttAt, entry.rttSeq = now, entry.sndNxt
		}
		if !stack.sendTCP(entry, tcpFlagACK|tcpFlagPSH, sequence, entry.rcvNxt, entry.send[start:int(start)+length], now) {
			// The RTO timer pumps again, so a full TX queue or a missing
			// neighbour cannot strand buffered data.
			entry.sendSent -= uint16(length)
			entry.sndNxt -= uint32(length)
			if entry.rtoDeadline == 0 {
				entry.rtoDeadline = addTime(now, entry.rto)
			}
			break
		}
		// A loopback peer may have acknowledged or released the entry already.
		if entry.rtoDeadline == 0 && entry.sndUna != entry.sndNxt {
			entry.rtoDeadline = addTime(now, entry.rto)
		}
	}
	if entry.shutdownWrite && entry.sendLen == entry.sendSent && !entry.finSent {
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
			entry.rtoDeadline = addTime(now, entry.rto)
		}
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
			sent = stack.sendTCP(entry, tcpFlagACK|tcpFlagPSH, entry.sendSeq, entry.rcvNxt, entry.send[:length], now)
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
	route, result := stack.udpRoute(entry.local, entry.remote)
	if result != ResultOK {
		return false
	}
	segment := tcpSegment{sourcePort: entry.port, destinationPort: entry.remotePort, sequence: sequence,
		acknowledgment: acknowledgment, flags: flags, window: uint16(min(stack.tcpAdvertisedWindow(entry), 65535)),
		mss: entry.localMSS, payload: payload}
	if route.path == udpLocal {
		var bytes [MTU]byte
		length := putTCP(bytes[:], entry.local, entry.remote, segment)
		packet := ipv4Packet{source: entry.local, destination: entry.remote, protocol: protocolTCP, payload: bytes[:length]}
		stack.receiveTCP(packet, now)
		increment(&stack.tcpStats.TX)
		return true
	}
	mac, known := stack.lookupNeighbor(route.nextHop, now)
	if !known {
		if len(payload) != 0 || flags&(tcpFlagSYN|tcpFlagFIN) != 0 {
			entry.l2Pending = true
		}
		stack.resolve(route.nextHop, now)
		return false
	}
	frame := stack.frameBuffer()
	if frame == nil {
		return false
	}
	const tcpAt = HeaderSize + ipv4HeaderSize
	length := putTCP(frame[tcpAt:], entry.local, entry.remote, segment)
	stack.identification++
	putIPv4Header(frame[HeaderSize:], stack.tuning.TTL, entry.local, entry.remote, protocolTCP, stack.identification, length)
	putEthernet(frame, mac, stack.mac, etherTypeIPv4)
	stack.commitFrame(tcpAt + length)
	increment(&stack.tcpStats.TX)
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

func (stack *Stack) advanceTCP(now uint64) {
	stack.advanceTombstones(now)
	for index := range stack.tcp[:] {
		entry := &stack.tcp[index]
		if entry.state == TCPFree || entry.state == TCPClosed {
			continue
		}
		if (entry.state == TCPTimeWait || (entry.state == TCPFinWait2 && entry.orphan)) && entry.deadline != 0 && entry.deadline <= now {
			stack.releaseTCP(entry)
			continue
		}
		if (entry.state == TCPSynSent || entry.state == TCPSynReceived) && entry.deadline != 0 && entry.deadline <= now {
			if entry.state == TCPSynReceived {
				stack.releaseTCP(entry)
				continue
			}
			stack.closeTCP(entry, ResultTimeout)
			continue
		}
		if entry.rtoDeadline != 0 && entry.rtoDeadline <= now {
			if entry.sndUna == entry.sndNxt && entry.sendLen == 0 {
				entry.rtoDeadline = 0
				continue
			}
			if entry.peerWindow == 0 && entry.sendSent == 0 && entry.sendLen != 0 {
				if entry.persist == 8 {
					stack.closeTCP(entry, ResultTimeout)
					continue
				}
				entry.persist++
				stack.sendTCP(entry, tcpFlagACK, entry.sndNxt, entry.rcvNxt, entry.send[entry.sendSent:entry.sendSent+1], now)
				increment(&stack.tcpStats.Retransmits)
				entry.rto = min(max(entry.rto*2, stack.tuning.TCP.RTOMin), stack.tuning.TCP.RTOMax)
				entry.rtoDeadline = addTime(now, entry.rto)
				continue
			}
			if entry.state >= TCPEstablished && entry.retries >= tcpMaxRetransmits {
				stack.closeTCP(entry, ResultTimeout)
				continue
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
			entry.rtoDeadline = addTime(now, entry.rto)
		}
		if entry.peerWindow == 0 && entry.sendLen > entry.sendSent && entry.rtoDeadline == 0 {
			entry.rtoDeadline = addTime(now, entry.rto)
		}
	}
}

func (stack *Stack) tcpDeadline(earliest *timer) {
	stack.tombstoneDeadline(earliest)
	for index := range stack.tcp[:] {
		entry := &stack.tcp[index]
		if entry.deadline != 0 {
			earliest.consider(entry.deadline)
		}
		if entry.rtoDeadline != 0 {
			earliest.consider(entry.rtoDeadline)
		}
	}
}

func (stack *Stack) reconfigureTCP(result Result) {
	if result == ResultDown {
		return
	}
	stack.dropTombstones()
	for index := range stack.tcp[:] {
		entry := &stack.tcp[index]
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

// sendPendingTCP resends what waited for nextHop to resolve.
func (stack *Stack) sendPendingTCP(nextHop IPv4, now uint64) {
	for index := range stack.tcp[:] {
		entry := &stack.tcp[index]
		if !entry.l2Pending || !stack.tcpVia(entry, nextHop) {
			continue
		}
		entry.l2Pending = false
		switch entry.state {
		case TCPSynSent:
			if stack.sendTCP(entry, tcpFlagSYN, entry.iss, 0, nil, now) {
				entry.rtoDeadline = addTime(now, entry.rto)
			}
		case TCPSynReceived:
			if stack.sendTCP(entry, tcpFlagSYN|tcpFlagACK, entry.iss, entry.rcvNxt, nil, now) {
				entry.rtoDeadline = addTime(now, entry.rto)
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
	for index := range stack.tcp[:] {
		entry := &stack.tcp[index]
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
