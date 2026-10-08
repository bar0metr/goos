package network

import "testing"

// peerTimeWait connects owner to peerIP:remotePort, closes first and has the
// peer finish the close, leaving an orphan in TIME_WAIT.
func (h *harness) peerTimeWait(owner int, remotePort uint16) (TCPEndpointID, TCPTuple) {
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
	h.tcpFromPeer(entry, tcpSegment{sequence: entry.rcvNxt, acknowledgment: entry.sndNxt, flags: tcpFlagACK | tcpFlagFIN, window: 65535})
	h.tcpSent()
	if entry.state != TCPTimeWait || !entry.orphan {
		h.t.Fatalf("state %d orphan %v", entry.state, entry.orphan)
	}
	return id, tuple
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

func (h *harness) timeWaits(owner int) int {
	count := 0
	for index := range h.stack.tcp {
		if h.stack.tcp[index].state == TCPTimeWait && int(h.stack.tcp[index].owner) == owner {
			count++
		}
	}
	return count
}

func (h *harness) tombstones(owner int) int {
	count := 0
	for index := range h.stack.tcpTombstones {
		if h.stack.tcpTombstones[index].used && int(h.stack.tcpTombstones[index].owner) == owner {
			count++
		}
	}
	return count
}

// recycle opens an endpoint for owner while it sits at its limit, so the
// slot of its oldest TIME_WAIT is reused.
func (h *harness) recycle(owner int) TCPEndpointID {
	h.t.Helper()
	id, result := h.stack.TCPOpen(owner)
	if result != ResultOK {
		h.t.Fatalf("recycling open %d", result)
	}
	return id
}

func TestTCPRecycledTimeWaitKeepsFourTuple(t *testing.T) {
	h := readyWithPeer(t)
	h.stack.tuning.TCP.SessionEndpoints = 1
	oldID, old := h.peerTimeWait(1, 53)
	id := h.recycle(1)
	if _, _, result := h.stack.TCPState(1, oldID); result != ResultInvalidArgument {
		t.Fatalf("stale ID still valid: %d", result)
	}
	if id.slot() != oldID.slot() || id == oldID {
		t.Fatalf("new ID %x, old %x", id, oldID)
	}
	if h.timeWaits(1) != 0 || h.tombstones(1) != 1 {
		t.Fatalf("TIME_WAIT %d tombstones %d", h.timeWaits(1), h.tombstones(1))
	}

	// With the old local port as the only choice, the old four-tuple is
	// refused rather than recreated.
	h.stack.tuning.TCP.EphemeralMin, h.stack.tuning.TCP.EphemeralMax = old.LocalPort, old.LocalPort
	if _, result := h.stack.TCPConnect(1, id, peerIP, 53, h.now); result != ResultBusy {
		t.Fatalf("connect over the protected tuple: %d", result)
	}
	if _, _, result := h.stack.TCPBind(1, id, old.Local, old.LocalPort); result != ResultOK {
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
	h.stack.tuning.TCP.EphemeralMax = old.LocalPort + 1
	h.stack.tuning.TCP.SessionEndpoints = MaxTCPEndpointsPerSession
	for round := 0; round < 4; round++ {
		id, _ = h.stack.TCPOpen(1)
		tuple, result := h.connectPeer(1, id, peerIP, 53)
		if result != ResultOK || tuple.LocalPort != old.LocalPort+1 {
			t.Fatalf("round %d: %+v %d", round, tuple, result)
		}
		// An abort leaves no TIME_WAIT behind for the next round.
		h.abort(1, id)
	}

	// Another remote port or another remote address makes another tuple.
	h.stack.tuning.TCP.EphemeralMax = old.LocalPort
	id, _ = h.stack.TCPOpen(1)
	if tuple, result := h.connectPeer(1, id, peerIP, 54); result != ResultOK || tuple.LocalPort != old.LocalPort {
		t.Fatalf("other remote port: %+v %d", tuple, result)
	}
	id, _ = h.stack.TCPOpen(1)
	if tuple, result := h.connectPeer(1, id, gatewayIP, 53); result != ResultOK || tuple.LocalPort != old.LocalPort {
		t.Fatalf("other remote address: %+v %d", tuple, result)
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

// Segments for a recycled tuple meet TIME_WAIT's rules: a retransmitted FIN
// is acknowledged with the old sequence numbers and restarts the interval;
// a delayed segment or a new SYN gets nothing, neither a reset nor a TCB.
func TestTCPTombstoneAnswersLikeTimeWait(t *testing.T) {
	h := readyWithPeer(t)
	h.stack.tuning.TCP.SessionEndpoints = 1
	oldID, old := h.peerTimeWait(1, 53)
	entry := h.stack.tcp[oldID.slot()]
	h.recycle(1)
	tombstone := h.stack.tcpTombstone(old.Local, old.LocalPort, old.Remote, old.RemotePort)
	if tombstone == nil || tombstone.sndNxt != entry.sndNxt || tombstone.rcvNxt != entry.rcvNxt {
		t.Fatalf("tombstone %+v", tombstone)
	}
	h.now += second
	h.tcpFrom(old, tcpSegment{sequence: entry.rcvNxt - 1, acknowledgment: entry.sndNxt, flags: tcpFlagACK | tcpFlagFIN, window: 65535})
	ack := h.tcpSent()
	if len(ack) != 1 || ack[0].flags != tcpFlagACK || ack[0].sequence != entry.sndNxt || ack[0].acknowledgment != entry.rcvNxt {
		t.Fatalf("FIN answered with %+v", ack)
	}
	if tombstone.deadline != h.now+h.stack.tuning.TCP.TimeWait {
		t.Fatalf("deadline %d", tombstone.deadline)
	}
	before := h.stack.TCPStats()
	h.tcpFrom(old, tcpSegment{sequence: entry.rcvNxt, acknowledgment: entry.sndNxt, flags: tcpFlagACK, window: 65535, payload: []byte("late")})
	h.tcpFrom(old, tcpSegment{sequence: 9000, flags: tcpFlagSYN, window: 65535})
	h.tcpFrom(old, tcpSegment{sequence: entry.rcvNxt, flags: tcpFlagRST})
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
	h.stack.tuning.TCP.SessionEndpoints = 1
	_, old := h.peerTimeWait(1, 53)
	id := h.recycle(1)
	deadline, ok := h.stack.Deadline()
	if !ok || deadline > h.now+h.stack.tuning.TCP.TimeWait {
		t.Fatalf("tombstone timer %d %v", deadline, ok)
	}
	h.runUntil(h.now + h.stack.tuning.TCP.TimeWait)
	if h.tombstones(1) != 0 {
		t.Fatal("tombstone outlived TIME_WAIT")
	}
	h.stack.tuning.TCP.EphemeralMin, h.stack.tuning.TCP.EphemeralMax = old.LocalPort, old.LocalPort
	if tuple, result := h.connectPeer(1, id, peerIP, 53); result != ResultOK || tuple != old {
		t.Fatalf("after expiry: %+v %d", tuple, result)
	}
}

// One owner never recycles another's TIME_WAIT, and each owner's share of
// tombstones is its own: when it is full the owner waits, deterministically,
// while the others keep theirs.
func TestTCPTombstoneSharesPerOwner(t *testing.T) {
	h := readyWithPeer(t)
	h.stack.tuning.TCP.SessionEndpoints = 1
	h.peerTimeWait(1, 53)
	held, result := h.stack.TCPOpen(2)
	if result != ResultOK {
		t.Fatal(result)
	}
	if _, result := h.stack.TCPOpen(2); result != ResultBusy {
		t.Fatalf("owner 2 past its limit: %d", result)
	}
	if h.timeWaits(1) != 1 || h.tombstones(2) != 0 {
		t.Fatal("owner 2 took owner 1's TIME_WAIT")
	}
	if state, _, result := h.stack.TCPState(2, held); result != ResultOK || state != TCPOpen {
		t.Fatalf("held endpoint %d %d", state, result)
	}
	h.stack.TCPClose(2, held, h.now)

	// Owner 1 recycles until its share is full.
	for round := 0; round < tcpTombstonesPerOwner; round++ {
		id := h.recycle(1)
		h.stack.TCPClose(1, id, h.now)
		h.peerTimeWait(1, uint16(60+round))
	}
	if h.tombstones(1) != tcpTombstonesPerOwner || h.timeWaits(1) != 1 {
		t.Fatalf("tombstones %d TIME_WAIT %d", h.tombstones(1), h.timeWaits(1))
	}
	if _, result := h.stack.TCPOpen(1); result != ResultBusy {
		t.Fatalf("open past the tombstone share: %d", result)
	}
	if h.timeWaits(1) != 1 {
		t.Fatal("a full share dropped protection")
	}
	// Owner 2 still has its own share.
	h.peerTimeWait(2, 53)
	h.recycle(2)
	if h.tombstones(2) != 1 {
		t.Fatalf("owner 2 tombstones %d", h.tombstones(2))
	}
	h.runUntil(h.now + h.stack.tuning.TCP.TimeWait)
	if h.tombstones(1) != 0 || h.timeWaits(1) != 0 {
		t.Fatal("expired protection kept")
	}
	h.recycle(1)
}

// Ending a session keeps its TIME_WAIT connections until they expire.
func TestTCPCloseOwnerKeepsTimeWait(t *testing.T) {
	h := readyWithPeer(t)
	_, old := h.peerTimeWait(1, 53)
	h.stack.TCPCloseOwner(1, h.now)
	if h.timeWaits(1) != 1 {
		t.Fatal("session end released TIME_WAIT")
	}
	if !h.stack.tcpTupleConflict(-1, old.Local, old.LocalPort, old.Remote, old.RemotePort) {
		t.Fatal("tuple unprotected after the session ended")
	}
	h.runUntil(h.now + h.stack.tuning.TCP.TimeWait)
	if h.timeWaits(1) != 0 {
		t.Fatal("TIME_WAIT outlived its interval")
	}
}

// DNS-style clients open, exchange and close first, again and again. Past
// the endpoint limit they keep going on recycled slots, every tuple distinct
// from the ones still protected, until the share of tombstones is full.
func TestTCPShortConnectionsPastTheLimit(t *testing.T) {
	h := readyWithPeer(t)
	seen := map[TCPTuple]bool{}
	limit := MaxTCPEndpointsPerSession + tcpTombstonesPerOwner
	for round := 0; round < limit; round++ {
		_, tuple := h.peerTimeWait(1, 53)
		if seen[tuple] {
			t.Fatalf("round %d reused a protected tuple %+v", round, tuple)
		}
		seen[tuple] = true
	}
	if _, result := h.stack.TCPOpen(1); result != ResultBusy {
		t.Fatalf("open past every share: %d", result)
	}
	h.runUntil(h.now + h.stack.tuning.TCP.TimeWait)
	h.peerTimeWait(1, 53)
}
