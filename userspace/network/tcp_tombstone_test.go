package network

import "testing"

// timeWait is what a connection closed first leaves behind.
type timeWait struct {
	id             TCPEndpointID
	tuple          TCPTuple
	sndNxt, rcvNxt uint32
}

// peerTimeWait connects owner to peerIP:remotePort, closes first and has the
// peer finish the close, leaving the four-tuple in TIME_WAIT: in a
// tombstone, or in the endpoint when the budget has no room.
func (h *harness) peerTimeWait(owner int, remotePort uint16) timeWait {
	h.t.Helper()
	id, result := h.stack.TCPOpen(owner)
	if result != ResultOK {
		h.t.Fatalf("open %d", result)
	}
	tuple, result := h.connectPeer(owner, id, peerIP, remotePort)
	if result != ResultOK {
		h.t.Fatalf("connect %d", result)
	}
	entry, _ := h.stack.tcpEndpoint(owner, id)
	h.stack.TCPClose(owner, id, h.now)
	if fin := h.tcpSent(); len(fin) != 1 || fin[0].flags&tcpFlagFIN == 0 {
		h.t.Fatalf("FIN %+v", fin)
	}
	left := timeWait{id: id, tuple: tuple, sndNxt: entry.sndNxt, rcvNxt: entry.rcvNxt + 1}
	h.tcpFromPeer(entry, tcpSegment{sequence: entry.rcvNxt, acknowledgment: entry.sndNxt, flags: tcpFlagACK | tcpFlagFIN, window: 65535})
	h.tcpSent()
	if h.stack.tcpTombstone(tuple.Local, tuple.LocalPort, tuple.Remote, tuple.RemotePort) < 0 && (entry.state != TCPTimeWait || !entry.orphan) {
		h.t.Fatalf("no TIME_WAIT: state %d orphan %v", entry.state, entry.orphan)
	}
	return left
}

// connectPeer runs an active open against a peer that answers the SYN.
func (h *harness) connectPeer(owner int, id TCPEndpointID, remote IPv4, remotePort uint16) (TCPTuple, Result) {
	h.t.Helper()
	// Timers may have run past the neighbour's lifetime.
	h.learn(peerIP, peerMAC)
	h.learn(gatewayIP, gatewayMAC)
	tuple, result := h.stack.TCPConnect(owner, id, remote, remotePort, h.now)
	if result != ResultOK {
		return tuple, result
	}
	syn := h.tcpSent()
	if len(syn) != 1 || syn[0].flags != tcpFlagSYN {
		h.t.Fatalf("SYN %+v", syn)
	}
	entry, _ := h.stack.tcpEndpoint(owner, id)
	h.tcpFrom(tuple, tcpSegment{sequence: 5000, acknowledgment: syn[0].sequence + 1, flags: tcpFlagSYN | tcpFlagACK, window: 65535, mss: 1460})
	h.tcpSent()
	if entry.state != TCPEstablished {
		h.t.Fatalf("state %d after SYN-ACK", entry.state)
	}
	return tuple, ResultOK
}

// tcpFrom delivers a segment from the remote end of tuple.
func (h *harness) tcpFrom(tuple TCPTuple, segment tcpSegment) {
	h.t.Helper()
	segment.sourcePort, segment.destinationPort = tuple.RemotePort, tuple.LocalPort
	var bytes [MTU]byte
	length := putTCP(bytes[:], tuple.Remote, tuple.Local, segment)
	mac := peerMAC
	if tuple.Remote == gatewayIP {
		mac = gatewayMAC
	}
	h.receive(ipv4Frame(hostMAC, mac, tuple.Remote, tuple.Local, protocolTCP, bytes[:length]))
}

// timeWaits counts owner's endpoints in TIME_WAIT.
func (h *harness) timeWaits(owner int) int {
	count := 0
	for entry := h.stack.tcpOwnerLists[owner].head; entry != nil; entry = entry.links[tcpLinkOwner].next {
		if entry.state == TCPTimeWait {
			count++
		}
	}
	return count
}

func (h *harness) tombstones(owner int) int { return h.stack.tcpTombstones.owned[owner] }

func (h *harness) tombstoneOf(tuple TCPTuple) *tcpTombstone {
	index := h.stack.tcpTombstone(tuple.Local, tuple.LocalPort, tuple.Remote, tuple.RemotePort)
	if index < 0 {
		return nil
	}
	return &h.stack.tcpTombstones.slots[index]
}

// A connection closed first leaves its endpoint as it enters TIME_WAIT: the
// owner's quota is free at once and the four-tuple stays protected.
func TestTCPTimeWaitTombstoneKeepsFourTuple(t *testing.T) {
	h := readyWithPeer(t)
	h.stack.tuning.TCP.SessionEndpoints = 1
	old := h.peerTimeWait(1, 53)
	if _, _, result := h.stack.TCPState(1, old.id); result != ResultInvalidArgument {
		t.Fatalf("closed ID still valid: %d", result)
	}
	id, result := h.stack.TCPOpen(1)
	if result != ResultOK || id.slot() != old.id.slot() || id == old.id {
		t.Fatalf("open %d: new ID %x, old %x", result, id, old.id)
	}
	if h.timeWaits(1) != 0 || h.tombstones(1) != 1 {
		t.Fatalf("TIME_WAIT %d tombstones %d", h.timeWaits(1), h.tombstones(1))
	}

	// With the old local port as the only choice, the old four-tuple is
	// refused rather than recreated.
	tuple := old.tuple
	h.stack.tuning.TCP.EphemeralMin, h.stack.tuning.TCP.EphemeralMax = tuple.LocalPort, tuple.LocalPort
	if _, result := h.stack.TCPConnect(1, id, peerIP, 53, h.now); result != ResultBusy {
		t.Fatalf("connect over the protected tuple: %d", result)
	}
	// TIME_WAIT keeps the port bound: without reuse on both sides a bind
	// fails, as on Linux.
	if _, _, result := h.stack.TCPBindReuse(1, id, tuple.Local, tuple.LocalPort, true); result != ResultAddressInUse {
		t.Fatalf("bind over a TIME_WAIT without reuse %d", result)
	}
	tombstone := h.tombstoneOf(tuple)
	if tombstone == nil {
		t.Fatal("no tombstone")
	}
	tombstone.reuse = true
	if _, _, result := h.stack.TCPBind(1, id, tuple.Local, tuple.LocalPort); result != ResultAddressInUse {
		t.Fatalf("bind without reuse %d", result)
	}
	if _, _, result := h.stack.TCPBindReuse(1, id, tuple.Local, tuple.LocalPort, true); result != ResultOK {
		t.Fatalf("bind %d", result)
	}
	if _, result := h.stack.TCPConnect(1, id, peerIP, 53, h.now); result != ResultAddressInUse {
		t.Fatalf("explicit port over the protected tuple: %d", result)
	}
	if segments := h.tcpSent(); len(segments) != 0 {
		t.Fatalf("refused connects sent %+v", segments)
	}
	h.stack.TCPClose(1, id, h.now)

	// With a second port the allocator must take that one, whatever its
	// random start.
	h.stack.tuning.TCP.EphemeralMax = tuple.LocalPort + 1
	h.stack.tuning.TCP.SessionEndpoints = MaxTCPEndpointsPerSession
	for round := 0; round < 4; round++ {
		id, _ = h.stack.TCPOpen(1)
		next, result := h.connectPeer(1, id, peerIP, 53)
		if result != ResultOK || next.LocalPort != tuple.LocalPort+1 {
			t.Fatalf("round %d: %+v %d", round, next, result)
		}
		// An abort leaves no TIME_WAIT behind for the next round.
		h.abort(1, id)
	}

	// Another remote port or another remote address makes another tuple.
	h.stack.tuning.TCP.EphemeralMax = tuple.LocalPort
	id, _ = h.stack.TCPOpen(1)
	if next, result := h.connectPeer(1, id, peerIP, 54); result != ResultOK || next.LocalPort != tuple.LocalPort {
		t.Fatalf("other remote port: %+v %d", next, result)
	}
	id, _ = h.stack.TCPOpen(1)
	if next, result := h.connectPeer(1, id, gatewayIP, 53); result != ResultOK || next.LocalPort != tuple.LocalPort {
		t.Fatalf("other remote address: %+v %d", next, result)
	}
	if h.tombstones(1) != 1 {
		t.Fatalf("tombstones %d", h.tombstones(1))
	}
}

// abort ends a connection with a reset from the peer.
func (h *harness) abort(owner int, id TCPEndpointID) {
	h.t.Helper()
	entry, _ := h.stack.tcpEndpoint(owner, id)
	h.tcpFrom(tcpTuple(entry), tcpSegment{sequence: entry.rcvNxt, flags: tcpFlagRST})
	h.stack.TCPClose(owner, id, h.now)
	h.tcpSent()
	if entry.state != TCPFree {
		h.t.Fatalf("state %d after reset", entry.state)
	}
}

// Segments for a tombstone's tuple meet TIME_WAIT's rules: a retransmitted
// FIN is acknowledged with the old sequence numbers and restarts the
// interval; a delayed segment or a new SYN gets nothing, neither a reset nor
// a TCB.
func TestTCPTombstoneAnswersLikeTimeWait(t *testing.T) {
	h := readyWithPeer(t)
	old := h.peerTimeWait(1, 53)
	tombstone := h.tombstoneOf(old.tuple)
	if tombstone == nil || tombstone.sndNxt != old.sndNxt || tombstone.rcvNxt != old.rcvNxt {
		t.Fatalf("tombstone %+v, want %+v", tombstone, old)
	}
	h.now += second
	h.tcpFrom(old.tuple, tcpSegment{sequence: old.rcvNxt - 1, acknowledgment: old.sndNxt, flags: tcpFlagACK | tcpFlagFIN, window: 65535})
	ack := h.tcpSent()
	if len(ack) != 1 || ack[0].flags != tcpFlagACK || ack[0].sequence != old.sndNxt || ack[0].acknowledgment != old.rcvNxt {
		t.Fatalf("FIN answered with %+v", ack)
	}
	if tombstone.deadline != h.now+h.stack.tuning.TCP.TimeWait {
		t.Fatalf("deadline %d", tombstone.deadline)
	}
	before := h.stack.TCPStats()
	h.tcpFrom(old.tuple, tcpSegment{sequence: old.rcvNxt, acknowledgment: old.sndNxt, flags: tcpFlagACK, window: 65535, payload: []byte("late")})
	h.tcpFrom(old.tuple, tcpSegment{sequence: 9000, flags: tcpFlagSYN, window: 65535})
	h.tcpFrom(old.tuple, tcpSegment{sequence: old.rcvNxt, flags: tcpFlagRST})
	if segments := h.tcpSent(); len(segments) != 0 {
		t.Fatalf("answered delayed segments with %+v", segments)
	}
	after := h.stack.TCPStats()
	if after.Endpoints != before.Endpoints || after.TXReset != before.TXReset || h.tombstones(1) != 1 {
		t.Fatalf("stats %+v then %+v", before, after)
	}
}

func TestTCPTombstoneExpires(t *testing.T) {
	h := readyWithPeer(t)
	old := h.peerTimeWait(1, 53)
	id, _ := h.stack.TCPOpen(1)
	deadline, ok := h.stack.Deadline()
	if !ok || deadline > h.now+h.stack.tuning.TCP.TimeWait {
		t.Fatalf("tombstone timer %d %v", deadline, ok)
	}
	h.runUntil(h.now + h.stack.tuning.TCP.TimeWait)
	if h.tombstones(1) != 0 || h.stack.TCPStats().TimeWait != 0 {
		t.Fatal("tombstone outlived TIME_WAIT")
	}
	h.stack.tuning.TCP.EphemeralMin, h.stack.tuning.TCP.EphemeralMax = old.tuple.LocalPort, old.tuple.LocalPort
	if tuple, result := h.connectPeer(1, id, peerIP, 53); result != ResultOK || tuple != old.tuple {
		t.Fatalf("after expiry: %+v %d", tuple, result)
	}
}

// Each owner's share of tombstones is its own. With its share full, the
// owner's next TIME_WAIT stays an endpoint: the four-tuple keeps its
// protection and the owner's quota refuses the next open with Busy, while
// the other owners keep their shares.
func TestTCPTimeWaitBudgetPerOwner(t *testing.T) {
	h := readyWithPeer(t)
	h.stack.tuning.TCP.SessionEndpoints = 1
	h.stack.SetTCPTimeWaitBudget(100, 3)
	for round := 0; round < 3; round++ {
		h.peerTimeWait(1, uint16(60+round))
	}
	held := h.peerTimeWait(1, 70)
	if h.tombstones(1) != 3 || h.timeWaits(1) != 1 || h.stack.TCPStats().TimeWaitHeld != 1 {
		t.Fatalf("tombstones %d TIME_WAIT %d held %d", h.tombstones(1), h.timeWaits(1), h.stack.TCPStats().TimeWaitHeld)
	}
	if !h.stack.tcpTupleConflict(nil, held.tuple.Local, held.tuple.LocalPort, held.tuple.Remote, held.tuple.RemotePort) {
		t.Fatal("a full share dropped protection")
	}
	if _, result := h.stack.TCPOpen(1); result != ResultBusy {
		t.Fatalf("open past the tombstone share: %d", result)
	}
	// Owner 2 still has its own share.
	h.peerTimeWait(2, 53)
	if h.tombstones(2) != 1 || h.timeWaits(2) != 0 {
		t.Fatalf("owner 2 tombstones %d", h.tombstones(2))
	}
	// Room in the share lets the next open turn the held TIME_WAIT into a
	// tombstone and take its slot.
	h.stack.SetTCPTimeWaitBudget(100, 4)
	id, result := h.stack.TCPOpen(1)
	if result != ResultOK || h.tombstones(1) != 4 || h.timeWaits(1) != 0 {
		t.Fatalf("recycling open %d: tombstones %d TIME_WAIT %d", result, h.tombstones(1), h.timeWaits(1))
	}
	h.stack.TCPClose(1, id, h.now)
	h.runUntil(h.now + h.stack.tuning.TCP.TimeWait)
	if h.tombstones(1) != 0 || h.timeWaits(1) != 0 || h.stack.TCPStats().TimeWait != 0 {
		t.Fatal("expired protection kept")
	}
}

// The global budget bounds every owner together, and its exhaustion is
// explicit in the same way.
func TestTCPTimeWaitBudgetGlobal(t *testing.T) {
	h := readyWithPeer(t)
	h.stack.tuning.TCP.SessionEndpoints = 1
	h.stack.SetTCPTimeWaitBudget(2, 0)
	h.peerTimeWait(1, 53)
	h.peerTimeWait(2, 53)
	held := h.peerTimeWait(3, 53)
	if h.stack.TCPStats().TimeWait != 2 || h.timeWaits(3) != 1 {
		t.Fatalf("tombstones %d, owner 3 TIME_WAIT %d", h.stack.TCPStats().TimeWait, h.timeWaits(3))
	}
	if _, result := h.stack.TCPOpen(3); result != ResultBusy {
		t.Fatalf("owner 3 open with the budget full: %d", result)
	}
	if h.tombstoneOf(held.tuple) != nil || !h.stack.tcpTupleConflict(nil, held.tuple.Local, held.tuple.LocalPort, held.tuple.Remote, held.tuple.RemotePort) {
		t.Fatal("held TIME_WAIT lost its protection")
	}
}

// Ending a session keeps its TIME_WAIT four-tuples until they expire, the
// tombstones and a TIME_WAIT the application still held.
func TestTCPCloseOwnerKeepsTimeWait(t *testing.T) {
	h := readyWithPeer(t)
	old := h.peerTimeWait(1, 53)

	// A connection whose application shut down writing but still holds the
	// ID reaches TIME_WAIT as an endpoint.
	id, _ := h.stack.TCPOpen(1)
	open, _ := h.connectPeer(1, id, peerIP, 54)
	entry, _ := h.stack.tcpEndpoint(1, id)
	h.stack.TCPShutdownWrite(1, id, h.now)
	h.tcpSent()
	h.tcpFromPeer(entry, tcpSegment{sequence: entry.rcvNxt, acknowledgment: entry.sndNxt, flags: tcpFlagACK | tcpFlagFIN, window: 65535})
	h.tcpSent()
	if entry.state != TCPTimeWait || h.timeWaits(1) != 1 {
		t.Fatalf("held TIME_WAIT state %d", entry.state)
	}

	h.stack.TCPCloseOwner(1, h.now)
	// The four-tuples stay protected, no longer on the ended owner's count.
	if h.timeWaits(1) != 0 || h.tombstones(1) != 0 || h.tombstones(tcpRetiredOwner) != 2 {
		t.Fatalf("TIME_WAIT %d tombstones %d retired %d", h.timeWaits(1), h.tombstones(1), h.tombstones(tcpRetiredOwner))
	}
	for _, tuple := range []TCPTuple{old.tuple, open} {
		if !h.stack.tcpTupleConflict(nil, tuple.Local, tuple.LocalPort, tuple.Remote, tuple.RemotePort) {
			t.Fatalf("tuple %+v unprotected after the session ended", tuple)
		}
	}
	h.runUntil(h.now + h.stack.tuning.TCP.TimeWait)
	if h.stack.tcpTupleConflict(nil, old.tuple.Local, old.tuple.LocalPort, old.tuple.Remote, old.tuple.RemotePort) {
		t.Fatal("TIME_WAIT outlived its interval")
	}
}

// DNS-style clients open, exchange and close first, again and again. They
// keep going past the endpoint quota, every tuple distinct from the ones
// still protected, until the tombstone share and then the quota are full.
func TestTCPShortConnectionsPastTheLimit(t *testing.T) {
	h := readyWithPeer(t)
	const share = 16
	h.stack.SetTCPTimeWaitBudget(0, share)
	seen := map[TCPTuple]bool{}
	for round := 0; round < share+MaxTCPEndpointsPerSession; round++ {
		left := h.peerTimeWait(1, 53)
		if seen[left.tuple] {
			t.Fatalf("round %d reused a protected tuple %+v", round, left.tuple)
		}
		seen[left.tuple] = true
	}
	if h.tombstones(1) != share || h.timeWaits(1) != MaxTCPEndpointsPerSession {
		t.Fatalf("tombstones %d TIME_WAIT %d", h.tombstones(1), h.timeWaits(1))
	}
	if _, result := h.stack.TCPOpen(1); result != ResultBusy {
		t.Fatalf("open past every share: %d", result)
	}
	h.runUntil(h.now + h.stack.tuning.TCP.TimeWait)
	h.peerTimeWait(1, 53)
}
