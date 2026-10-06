package network

const (
	MaxNeighbors   = 16
	MaxResolutions = 4

	neighborLifetime   = 60 * second
	resolutionInterval = second
	resolutionAttempts = 3
)

type neighbor struct {
	used    bool
	address IPv4
	mac     MAC
	expires uint64
}

// resolution is one next hop being asked for. Echoes waiting on it find it by
// their next hop; no frame is held while it runs.
type resolution struct {
	used     bool
	target   IPv4
	sent     uint8
	deadline uint64
}

func (stack *Stack) clearNeighbors() {
	for index := range stack.neighbors[:] {
		stack.neighbors[index] = neighbor{}
	}
	for index := range stack.resolutions[:] {
		stack.resolutions[index] = resolution{}
	}
}

func (stack *Stack) findNeighbor(address IPv4, now uint64) *neighbor {
	for index := range stack.neighbors[:] {
		entry := &stack.neighbors[index]
		if entry.used && entry.address == address && entry.expires > now {
			return entry
		}
	}
	return nil
}

func (stack *Stack) lookupNeighbor(address IPv4, now uint64) (MAC, bool) {
	if entry := stack.findNeighbor(address, now); entry != nil {
		return entry.mac, true
	}
	return MAC{}, false
}

// learnNeighbor refreshes the entry for the address, or takes a free or
// expired slot, or replaces the entry closest to expiry.
func (stack *Stack) learnNeighbor(address IPv4, mac MAC, now uint64) {
	slot := &stack.neighbors[0]
	for index := range stack.neighbors[:] {
		entry := &stack.neighbors[index]
		if entry.used && entry.address == address {
			slot = entry
			break
		}
		if entry.rank(now) < slot.rank(now) {
			slot = entry
		}
	}
	*slot = neighbor{used: true, address: address, mac: mac, expires: addTime(now, neighborLifetime)}
}

// rank orders replacement candidates: a free or expired slot first, then the
// entry that would expire soonest.
func (entry *neighbor) rank(now uint64) uint64 {
	if !entry.used || entry.expires <= now {
		return 0
	}
	return entry.expires
}

func (stack *Stack) findResolution(target IPv4) *resolution {
	for index := range stack.resolutions[:] {
		entry := &stack.resolutions[index]
		if entry.used && entry.target == target {
			return entry
		}
	}
	return nil
}

// resolve joins a running resolution for target or starts one. It reports
// false when all resolution slots are busy with other targets.
func (stack *Stack) resolve(target IPv4, now uint64) bool {
	if stack.findResolution(target) != nil {
		return true
	}
	for index := range stack.resolutions[:] {
		entry := &stack.resolutions[index]
		if entry.used {
			continue
		}
		*entry = resolution{used: true, target: target}
		stack.sendResolution(entry, now)
		return true
	}
	return false
}

func (stack *Stack) sendResolution(entry *resolution, now uint64) {
	stack.sendARP(broadcastMAC, arpPacket{
		operation: arpRequest,
		senderMAC: stack.mac,
		senderIP:  stack.address,
		targetIP:  entry.target,
	})
	entry.sent++
	entry.deadline = addTime(now, resolutionInterval)
}

// advanceResolutions retries each due resolution and gives up one interval
// after its last request.
func (stack *Stack) advanceResolutions(now uint64) {
	for index := range stack.resolutions[:] {
		entry := &stack.resolutions[index]
		if !entry.used || entry.deadline > now {
			continue
		}
		if entry.sent < resolutionAttempts {
			stack.sendResolution(entry, now)
			continue
		}
		target := entry.target
		*entry = resolution{}
		stack.failWaiting(target, ResultNeighborUnreachable)
	}
}

func (stack *Stack) sendARP(destination MAC, packet arpPacket) {
	frame := stack.frameBuffer()
	if frame == nil {
		return
	}
	putEthernet(frame, destination, stack.mac, etherTypeARP)
	putARP(frame[HeaderSize:], packet)
	stack.commitFrame(HeaderSize + arpSize)
}

// conflicts reports a packet that shows another host holding the candidate
// address, or probing for it at the same time as we are.
func (candidate probe) conflicts(packet arpPacket) bool {
	if packet.senderIP == candidate.candidate {
		return true
	}
	return packet.operation == arpRequest && packet.senderIP == zeroIPv4 && packet.targetIP == candidate.candidate
}

// receiveARP learns a mapping only for a host on our prefix that is talking
// to us, that we already know, or that we are resolving, so unrelated
// broadcast traffic cannot churn the table.
func (stack *Stack) receiveARP(payload []byte, now uint64) {
	packet, status := parseARP(payload)
	switch status {
	case parseMalformed:
		stack.countMalformed()
		return
	case parseUnsupported:
		stack.CountDrop()
		return
	}
	if !unicastMAC(packet.senderMAC) || packet.senderMAC == stack.mac {
		stack.CountDrop()
		return
	}
	if stack.probe.active && stack.probe.conflicts(packet) {
		stack.probeConflict(now)
		return
	}
	if stack.address == zeroIPv4 || !stack.onLink(packet.senderIP) || packet.senderIP == stack.address {
		return
	}
	toUs := packet.targetIP == stack.address
	pending := stack.findResolution(packet.senderIP)
	if toUs || pending != nil || stack.findNeighbor(packet.senderIP, now) != nil {
		stack.learnNeighbor(packet.senderIP, packet.senderMAC, now)
	}
	if pending != nil {
		*pending = resolution{}
		stack.sendWaiting(packet.senderIP, packet.senderMAC, now)
	}
	if toUs && packet.operation == arpRequest {
		stack.sendARP(packet.senderMAC, arpPacket{
			operation: arpReply,
			senderMAC: stack.mac,
			senderIP:  stack.address,
			targetMAC: packet.senderMAC,
			targetIP:  packet.senderIP,
		})
	}
}

// onLink reports a host address inside the applied prefix.
func (stack *Stack) onLink(address IPv4) bool {
	return stack.address != zeroIPv4 && hostAddress(address, stack.prefix) && samePrefix(address, stack.address, stack.prefix)
}
