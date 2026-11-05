package network

import "goos/internal/validation"

// tcpCrossCheck makes a validation build compare every index lookup with a
// walk of the registry, and each Advance the whole index, timer and budget
// state with the endpoints. A disagreement panics.
const tcpCrossCheck = validation.Enabled

func (stack *Stack) slowMatch(local IPv4, localPort uint16, remote IPv4, remotePort uint16) *tcpEndpoint {
	for index := stack.nextTCP(0); index >= 0; index = stack.nextTCP(index + 1) {
		entry := stack.tcp[index]
		if entry.state >= TCPSynSent && entry.local == local && entry.port == localPort &&
			entry.remote == remote && entry.remotePort == remotePort {
			return entry
		}
	}
	return nil
}

func (stack *Stack) slowListener(local IPv4, port uint16) *tcpEndpoint {
	var wildcard *tcpEndpoint
	for index := stack.nextTCP(0); index >= 0; index = stack.nextTCP(index + 1) {
		entry := stack.tcp[index]
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

func (stack *Stack) slowPortTaken(skip *tcpEndpoint, address IPv4, port uint16) bool {
	for index := stack.nextTCP(0); index >= 0; index = stack.nextTCP(index + 1) {
		entry := stack.tcp[index]
		if entry != skip && entry.state >= TCPBound && entry.state <= TCPListen && entry.port == port && addressesOverlap(address, entry.local) {
			return true
		}
	}
	return false
}

func (stack *Stack) slowReuseConflict(skip *tcpEndpoint, address IPv4, port uint16, reuse bool) bool {
	for index := stack.nextTCP(0); index >= 0; index = stack.nextTCP(index + 1) {
		entry := stack.tcp[index]
		if entry == skip || entry.state < TCPBound || entry.port != port || !addressesOverlap(address, entry.local) {
			continue
		}
		if !reuse || !entry.reuse || entry.state == TCPListen {
			return true
		}
	}
	tombs := &stack.tcpTombstones
	for index := range tombs.slots {
		tomb := &tombs.slots[index]
		if tomb.used && tomb.port == port && addressesOverlap(address, tomb.local) && (!reuse || !tomb.reuse) {
			return true
		}
	}
	return false
}

// checkTCP compares the indexes, timers, lists and budgets with the
// endpoints and tombstones they describe. It returns the first mismatch, or
// "" when everything agrees.
func (stack *Stack) checkTCP() string {
	var owners [tcpOwners]int
	var bytes [tcpOwners]int
	live, tuples, ports, listeners, total := 0, 0, 0, 0, 0
	for slot, entry := range stack.tcp {
		used := stack.tcpUsed[slot/64]&(1<<(slot%64)) != 0
		if (entry != nil) != used {
			return "registry mark disagrees with slot"
		}
		if entry == nil {
			if stack.tcpTimers.at(int32(slot+1)) != 0 {
				return "timer left for a free slot"
			}
			continue
		}
		live++
		if entry.slot != int32(slot+1) || entry.state == TCPFree {
			return "endpoint records the wrong slot"
		}
		owners[entry.owner]++
		bytes[entry.owner] += int(entry.charged)
		total += int(entry.charged)
		if int(entry.charged) != len(entry.sendRing)+len(entry.recvRing) {
			return "charge disagrees with the rings"
		}
		connection := entry.state >= TCPSynSent
		if (entry.members&(1<<tcpLinkTuple) != 0) != connection {
			return "tuple index membership disagrees with state"
		}
		if connection {
			tuples++
			var probes uint64
			if stack.lookupTuple(entry.local, entry.port, entry.remote, entry.remotePort, &probes) != entry {
				return "tuple lookup misses a connection"
			}
		}
		if (entry.members&(1<<tcpLinkPort) != 0) != (entry.state >= TCPBound) {
			return "port index membership disagrees with state"
		}
		if entry.state >= TCPBound {
			ports++
		}
		if (entry.members&(1<<tcpLinkListen) != 0) != (entry.state == TCPListen) {
			return "listener index membership disagrees with state"
		}
		if entry.state == TCPListen {
			listeners++
			for child := entry.children.head; child != nil; child = child.links[tcpLinkChild].next {
				if child.parent != entry || child.accepted || child.listener != tcpID(entry) {
					return "child list holds a stranger"
				}
			}
		} else if entry.children.count != 0 {
			return "children on an endpoint that is not listening"
		}
		if entry.parent != nil && entry.parent.state != TCPListen {
			return "child of an endpoint that is not listening"
		}
		if stack.tcpTimers.at(entry.slot) != tcpTimerKey(entry) {
			return "timer heap disagrees with the endpoint deadlines"
		}
		if entry.ackPending && !stack.ackQueued(entry) {
			return "pending ACK missing from the queue"
		}
		if entry.reorder != nil {
			for index, span := range entry.reorder.spans {
				if !seqGT(span.start, entry.rcvNxt) || !seqGT(span.end, span.start) ||
					int(span.end-entry.rcvNxt) > len(entry.recvRing)-int(entry.recvLen) {
					return "reassembly span outside the window"
				}
				if index > 0 && !seqGT(span.start, entry.reorder.spans[index-1].end) {
					return "reassembly spans overlap or touch"
				}
			}
		}
	}
	switch {
	case live != stack.tcpLive:
		return "live count disagrees with the registry"
	case tuples != stack.tcpTuples.count || ports != stack.tcpPorts.count || listeners != stack.tcpListeners.count:
		return "index count disagrees with the endpoints"
	case total != stack.tcpBytes:
		return "memory total disagrees with the charges"
	case !stack.tcpTimers.valid() || !stack.tcpTombstones.timers.valid():
		return "timer heap out of order"
	}
	for owner := range owners {
		if owners[owner] != stack.tcpOwnerLists[owner].count || bytes[owner] != stack.tcpOwnerBytes[owner] {
			return "owner list or budget disagrees with the endpoints"
		}
	}
	return stack.checkTombstones()
}

func (stack *Stack) ackQueued(entry *tcpEndpoint) bool {
	for _, queued := range stack.tcpAckPending {
		if queued == entry {
			return true
		}
	}
	return false
}

func (stack *Stack) checkTombstones() string {
	tombs := &stack.tcpTombstones
	var owned [tcpOwners]int
	count := 0
	for index := range tombs.slots {
		tomb := &tombs.slots[index]
		if !tomb.used {
			if tombs.timers.at(int32(index)) != 0 {
				return "timer left for a free tombstone"
			}
			continue
		}
		count++
		owned[tomb.owner]++
		if tombs.timers.at(int32(index)) != max(tomb.deadline, 1) {
			return "tombstone timer disagrees with its deadline"
		}
		var probes uint64
		if stack.lookupTombstone(tomb.local, tomb.port, tomb.remote, tomb.remotePort, &probes) != int32(index) {
			return "tombstone lookup misses a four-tuple"
		}
		if stack.slowMatch(tomb.local, tomb.port, tomb.remote, tomb.remotePort) != nil {
			return "tombstone shares a four-tuple with a connection"
		}
	}
	if count != tombs.count || owned != tombs.owned {
		return "tombstone counts disagree with the table"
	}
	return ""
}
