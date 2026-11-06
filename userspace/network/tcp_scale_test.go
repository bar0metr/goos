package network

import (
	"bytes"
	"fmt"
	"math/rand"
	"testing"
)

// scaleOwners spread the endpoints of many connections over owners at the
// largest quota: servers on the first half, clients on the second.
const scaleOwners = 10

// TestTCPManyLoopbackConnections holds 300 established loopback
// connections at once, moves data on each, and checks that a received
// segment's lookups stay a few probes however many connections exist.
func TestTCPManyLoopbackConnections(t *testing.T) {
	const connections = 300
	const perOwner = connections / (scaleOwners / 2)
	h := newHarness(t)
	for owner := range scaleOwners {
		if !h.stack.SetTCPQuota(owner, MaxTCPSessionQuota) {
			t.Fatal("quota")
		}
	}
	var listeners [scaleOwners / 2]TCPEndpointID
	for owner := range listeners {
		listeners[owner], _ = h.stack.TCPOpen(owner)
		if _, _, result := h.stack.TCPBind(owner, listeners[owner], LoopbackAddress, uint16(7000+owner)); result != ResultOK {
			t.Fatalf("bind %d", result)
		}
		if result := h.stack.TCPListen(owner, listeners[owner], 1000); result != ResultOK {
			t.Fatalf("listen %d", result)
		}
	}
	type pair struct{ client, server TCPEndpointID }
	pairs := make([]pair, connections)
	for index := range pairs {
		server := index / perOwner
		client := scaleOwners/2 + server
		id, result := h.stack.TCPOpen(client)
		if result != ResultOK {
			t.Fatalf("connection %d open %d", index, result)
		}
		if _, result := h.stack.TCPConnect(client, id, LoopbackAddress, uint16(7000+server), h.now); result != ResultOK {
			t.Fatalf("connection %d connect %d", index, result)
		}
		accepted, _, result := h.stack.TCPAccept(server, listeners[server])
		if result != ResultOK {
			t.Fatalf("connection %d accept %d", index, result)
		}
		pairs[index] = pair{client: id, server: accepted}
	}
	stats := h.stack.TCPStats()
	if stats.States[TCPEstablished] != 2*connections || stats.Listeners != len(listeners) {
		t.Fatalf("established %d listeners %d", stats.States[TCPEstablished], stats.Listeners)
	}
	h.checkDeadline()

	buffer := make([]byte, 64)
	for index, pair := range pairs {
		client, server := scaleOwners/2+index/perOwner, index/perOwner
		message := []byte(fmt.Sprintf("request %d", index))
		if n, result := h.stack.TCPWrite(client, pair.client, message, h.now); result != ResultOK || n != len(message) {
			t.Fatalf("connection %d write %d %d", index, n, result)
		}
		n, result := h.stack.TCPRead(server, pair.server, buffer, h.now)
		if result != ResultOK || !bytes.Equal(buffer[:n], message) {
			t.Fatalf("connection %d read %q %d", index, buffer[:n], result)
		}
		h.stack.TCPWrite(server, pair.server, buffer[:n], h.now)
		if n, _ = h.stack.TCPRead(client, pair.client, buffer, h.now); !bytes.Equal(buffer[:n], message) {
			t.Fatalf("connection %d echo %q", index, buffer[:n])
		}
	}
	stats = h.stack.TCPStats()
	if stats.MaxSegmentProbes > 8 || stats.LookupProbes > 2*stats.RX {
		t.Fatalf("lookups: at most %d probes a segment, %d probes over %d segments", stats.MaxSegmentProbes, stats.LookupProbes, stats.RX)
	}
	t.Logf("%d segments, %d probes, at most %d a segment, %d ring bytes", stats.RX, stats.LookupProbes, stats.MaxSegmentProbes, stats.MemoryBytes)

	for index, pair := range pairs {
		client, server := scaleOwners/2+index/perOwner, index/perOwner
		h.stack.TCPClose(client, pair.client, h.now)
		h.stack.TCPClose(server, pair.server, h.now)
	}
	h.advance(second)
	stats = h.stack.TCPStats()
	if stats.Endpoints != len(listeners) || stats.TimeWait != connections || stats.MemoryBytes != 0 {
		t.Fatalf("after close: endpoints %d tombstones %d ring bytes %d", stats.Endpoints, stats.TimeWait, stats.MemoryBytes)
	}
	h.runUntil(h.now + h.stack.tuning.TCP.TimeWait)
	if stats = h.stack.TCPStats(); stats.TimeWait != 0 {
		t.Fatalf("tombstones after TIME_WAIT %d", stats.TimeWait)
	}
}

// The quotas fail explicitly and give their room back: the owner's quota,
// then the global endpoint limit, each refuses with Busy and takes the next
// open once an endpoint closes, in the freed slot with a new generation.
func TestTCPQuotaFillFailReleaseReuse(t *testing.T) {
	h := newHarness(t)
	const granted = 64
	h.stack.SetTCPQuota(1, granted)
	ids := make([]TCPEndpointID, granted)
	for index := range ids {
		var result Result
		if ids[index], result = h.stack.TCPOpen(1); result != ResultOK {
			t.Fatalf("open %d: %d", index, result)
		}
	}
	if _, result := h.stack.TCPOpen(1); result != ResultBusy {
		t.Fatalf("open past the quota %d", result)
	}
	h.stack.TCPClose(1, ids[10], h.now)
	reused, result := h.stack.TCPOpen(1)
	if result != ResultOK || reused.slot() != ids[10].slot() || reused == ids[10] {
		t.Fatalf("reuse %d: %x after %x", result, reused, ids[10])
	}
	if _, _, result := h.stack.TCPState(1, ids[10]); result != ResultInvalidArgument {
		t.Fatalf("stale ID %d", result)
	}

	h.stack.tuning.TCP.MaxEndpoints = 100
	held := 0
	for owner := 2; owner < 10; owner++ {
		h.stack.SetTCPQuota(owner, granted)
		for {
			if _, result := h.stack.TCPOpen(owner); result != ResultOK {
				if result != ResultBusy {
					t.Fatalf("owner %d open %d", owner, result)
				}
				break
			}
			held++
		}
	}
	if total := h.stack.TCPStats().Endpoints; total != 100 || held != 100-granted {
		t.Fatalf("endpoints %d, others held %d", total, held)
	}
	h.stack.TCPClose(1, ids[0], h.now)
	if _, result := h.stack.TCPOpen(9); result != ResultOK {
		t.Fatalf("open after a release %d", result)
	}
	h.stack.TCPCloseOwner(1, h.now)
	if total := h.stack.TCPStats().Endpoints; total != 100-granted+1 {
		t.Fatalf("endpoints after the owner ended %d", total)
	}
	h.checkDeadline()
}

// A listener keeps at most its backlog of children no one accepted; SYNs
// past it get nothing and the peer's retransmission gets in once an accept
// makes room. A listen asking for more than the tuning gets the tuning.
func TestTCPBacklogSaturationAndRecovery(t *testing.T) {
	h := readyWithPeer(t)
	listener, _ := h.stack.TCPOpen(0)
	h.stack.TCPBind(0, listener, IPv4{}, 8100)
	if result := h.stack.TCPListen(0, listener, 100_000); result != ResultOK {
		t.Fatal(result)
	}
	if entry := h.stack.tcp[listener.slot()]; entry.backlog != DefaultTCPBacklog {
		t.Fatalf("backlog %d", entry.backlog)
	}
	h.stack.TCPClose(0, listener, h.now)
	listener, _ = h.stack.TCPOpen(0)
	h.stack.TCPBind(0, listener, IPv4{}, 8100)
	h.stack.TCPListen(0, listener, 4)
	syn := func(port uint16) []tcpSegment {
		h.tcpFrom(TCPTuple{Local: hostIP, LocalPort: 8100, Remote: peerIP, RemotePort: port},
			tcpSegment{sequence: 1000, flags: tcpFlagSYN, window: 65535, mss: 1460})
		return h.tcpSent()
	}
	for port := uint16(40000); port < 40006; port++ {
		answered := len(syn(port)) == 1
		if answered != (port < 40004) {
			t.Fatalf("SYN from %d answered %v", port, answered)
		}
	}
	if children := h.stack.listenerChildren(listener); children != 4 || h.stack.TCPStats().DropBacklog != 2 {
		t.Fatalf("children %d dropped %d", children, h.stack.TCPStats().DropBacklog)
	}
	// Complete one handshake and accept it: the backlog has room again.
	child := h.stack.tcp[h.stack.tcp[listener.slot()].children.head.slot-1]
	h.tcpFrom(tcpTuple(child), tcpSegment{sequence: 1001, acknowledgment: child.sndNxt, flags: tcpFlagACK, window: 65535})
	if _, _, result := h.stack.TCPAccept(0, listener); result != ResultOK {
		t.Fatalf("accept %d", result)
	}
	if sent := syn(40004); len(sent) != 1 || sent[0].flags != tcpFlagSYN|tcpFlagACK {
		t.Fatalf("retransmitted SYN after an accept %+v", sent)
	}
	if children := h.stack.listenerChildren(listener); children != 4 {
		t.Fatalf("children %d", children)
	}
}

// The memory budgets refuse a handshake with no room for its receive ring
// and send floor, and a write or a ring growth past them; closing gives the
// bytes back.
func TestTCPMemoryBudget(t *testing.T) {
	h := readyWithPeer(t)
	h.stack.SetTCPMemoryBudget(64<<10, 6*tcpBufferFloor)
	var ids []TCPEndpointID
	for port := uint16(1); ; port++ {
		id, _ := h.stack.TCPOpen(0)
		h.learn(peerIP, peerMAC)
		if _, result := h.stack.TCPConnect(0, id, peerIP, port, h.now); result != ResultOK {
			if result != ResultBusy || len(ids) != 3 || h.stack.TCPStats().DropMemory != 1 {
				t.Fatalf("connect %d after %d: %d, drops %d", port, len(ids), result, h.stack.TCPStats().DropMemory)
			}
			h.stack.TCPClose(0, id, h.now)
			break
		}
		ids = append(ids, id)
	}
	h.tcpSent()
	if bytes := h.stack.TCPStats().MemoryBytes; bytes != 6*tcpBufferFloor {
		t.Fatalf("charged %d", bytes)
	}
	// Another owner has its own share of the global budget.
	other, _ := h.stack.TCPOpen(1)
	if _, result := h.stack.TCPConnect(1, other, peerIP, 99, h.now); result != ResultOK {
		t.Fatalf("other owner connect %d", result)
	}
	h.tcpSent()
	h.stack.TCPClose(0, ids[0], h.now)
	h.tcpSent()
	if bytes := h.stack.tcpOwnerBytes[0]; bytes != 4*tcpBufferFloor {
		t.Fatalf("owner bytes after a close %d", bytes)
	}

	// A write takes the paid send floor and what the owner's budget lets
	// the send ring grow to past it.
	h.stack.SetTCPMemoryBudget(0, 0)
	id, entry := establishedWithPeer(t, h, 50200)
	entry.peerWindow = 0
	h.stack.SetTCPMemoryBudget(0, h.stack.tcpOwnerBytes[0]+10000)
	written, result := h.stack.TCPWrite(0, id, make([]byte, 50000), h.now)
	if result != ResultOK || written != tcpBufferFloor+10000 || h.stack.tcpMemoryLeft(0) != 0 {
		t.Fatalf("write %d %d, left %d", written, result, h.stack.tcpMemoryLeft(0))
	}
	if _, result := h.stack.TCPWrite(0, id, []byte("x"), h.now); result != ResultBusy {
		t.Fatalf("write with the budget spent %d", result)
	}
	if ready, _ := h.stack.TCPReadiness(0, id); ready.Mask&TCPReadyWritable != 0 {
		t.Fatalf("writable with the budget spent %+v", ready)
	}
	h.checkDeadline()
}

// The receive ring doubles while the stream fills more than half of it, up
// to the tuned buffer; the advertised window follows.
func TestTCPReceiveRingGrows(t *testing.T) {
	h := readyWithPeer(t)
	id, entry := establishedWithPeer(t, h, 50201)
	if len(entry.recvRing) != tcpBufferFloor {
		t.Fatalf("first ring %d", len(entry.recvRing))
	}
	h.stack.tuning.TCP.ReceiveBuffer = 64 << 10
	var stream []byte
	for len(stream) < 60<<10 {
		window := h.stack.tcpAdvertisedWindow(entry)
		chunk := pattern(len(stream), min(1400, window))
		h.tcpFromPeer(entry, peerData(entry, 0, chunk, 0))
		h.tcpSent()
		stream = append(stream, chunk...)
		if len(entry.recvRing) > 64<<10 {
			t.Fatalf("ring %d past the tuning", len(entry.recvRing))
		}
	}
	if len(entry.recvRing) != 64<<10 || entry.charged != uint32(len(entry.recvRing)+tcpBufferFloor) {
		t.Fatalf("ring %d charged %d", len(entry.recvRing), entry.charged)
	}
	got := make([]byte, len(stream))
	if n, result := h.stack.TCPRead(0, id, got, h.now); result != ResultOK || !bytes.Equal(got[:n], stream) {
		t.Fatalf("read %d %d", n, result)
	}
}

// Reassembly holds far more than eight segments: twenty arrive in reverse
// order around a missing first one and all join the stream when it comes.
func TestTCPReorderBeyondEightSegments(t *testing.T) {
	h := readyWithPeer(t)
	id, entry := establishedWithPeer(t, h, 50202)
	fixedRings(h, entry)
	const size, count = 500, 21
	stream := pattern(0, size*count)
	for index := count - 1; index >= 1; index-- {
		h.tcpFromPeer(entry, peerData(entry, uint32(index*size), stream[index*size:(index+1)*size], 0))
		if sent := h.tcpSent(); len(sent) != 1 || sent[0].acknowledgment != entry.rcvNxt || int(sent[0].window) != testRing {
			t.Fatalf("segment %d: ACK %+v", index, sent)
		}
	}
	if entry.recvLen != 0 || entry.heldBytes() != size*(count-1) || len(entry.reorder.spans) != 1 {
		t.Fatalf("held %d bytes in %d spans", entry.heldBytes(), len(entry.reorder.spans))
	}
	h.tcpFromPeer(entry, peerData(entry, 0, stream[:size], 0))
	if entry.recvLen != size*count || entry.heldBytes() != 0 {
		t.Fatalf("promoted %d", entry.recvLen)
	}
	got := make([]byte, len(stream))
	if n, result := h.stack.TCPRead(0, id, got, h.now); result != ResultOK || !bytes.Equal(got[:n], stream) {
		t.Fatalf("read %d %d", n, result)
	}
}

// One-byte segments with gaps fill the span list; past its bound a new gap
// is dropped, and held bytes stay correct.
func TestTCPReorderSpanBound(t *testing.T) {
	h := readyWithPeer(t)
	id, entry := establishedWithPeer(t, h, 50203)
	fixedRings(h, entry)
	limit := tcpReorderSpans(testRing)
	stream := pattern(0, 4*limit+8)
	for index := 0; index <= limit; index++ {
		offset := uint32(2 + 2*index)
		h.tcpFromPeer(entry, peerData(entry, offset, stream[offset:offset+1], 0))
	}
	h.tcpSent()
	if len(entry.reorder.spans) != limit || h.stack.tcpStats.DropWindow != 1 {
		t.Fatalf("spans %d drops %d", len(entry.reorder.spans), h.stack.tcpStats.DropWindow)
	}
	// The rest of the stream in one segment fills every gap.
	h.tcpFromPeer(entry, peerData(entry, 0, stream, 0))
	h.tcpSent()
	got := make([]byte, len(stream))
	if n, result := h.stack.TCPRead(0, id, got, h.now); result != ResultOK || !bytes.Equal(got[:n], stream) {
		t.Fatalf("read %d %d", n, result)
	}
}

// The heap agrees with a plain map of deadlines under random operations.
func TestTCPTimerHeap(t *testing.T) {
	random := rand.New(rand.NewSource(7))
	var timers tcpTimers
	model := map[int32]uint64{}
	for step := 0; step < 20000; step++ {
		ref := int32(random.Intn(300))
		switch random.Intn(4) {
		case 0:
			timers.set(ref, 0)
			delete(model, ref)
		default:
			at := uint64(1 + random.Intn(1000))
			timers.set(ref, at)
			model[ref] = at
		}
		if step%97 == 0 && len(model) != 0 {
			ref, at, _ := timers.earliest()
			least := ^uint64(0)
			for _, value := range model {
				least = min(least, value)
			}
			if at != least || model[ref] != at {
				t.Fatalf("step %d: earliest %d at %d, want %d", step, ref, at, least)
			}
			timers.set(ref, 0)
			delete(model, ref)
		}
		if !timers.valid() || len(timers.nodes) != len(model) {
			t.Fatalf("step %d: heap invalid or %d nodes for %d", step, len(timers.nodes), len(model))
		}
	}
	for ref, at := range model {
		if timers.at(ref) != at {
			t.Fatalf("ref %d at %d, want %d", ref, timers.at(ref), at)
		}
	}
}

// Many connections with retransmission timers staggered in time: each
// retransmits once at its own deadline, and Deadline names the next one.
func TestTCPTimersRunInDeadlineOrder(t *testing.T) {
	h := readyWithPeer(t)
	h.stack.SetTCPQuota(0, MaxTCPSessionQuota)
	const count = 40
	entries := make([]*tcpEndpoint, count)
	for index := range entries {
		id, entry := establishedWithPeer(t, h, uint16(51000+index))
		entry.peerWindow = 60000
		h.now += 10 * millisecond
		h.stack.TCPWrite(0, id, []byte{byte(index)}, h.now)
		entries[index] = entry
	}
	h.tcpSent()
	first := entries[0].rtoDeadline
	if deadline, ok := h.stack.Deadline(); !ok || deadline != first {
		t.Fatalf("deadline %d, first RTO %d", deadline, first)
	}
	for index, entry := range entries {
		h.now = entry.rtoDeadline
		h.stack.Advance(h.now)
		sent := h.tcpSent()
		if len(sent) != 1 || sent[0].destinationPort != 7 || sent[0].sourcePort != uint16(51000+index) {
			t.Fatalf("connection %d: retransmits %+v", index, sent)
		}
		h.checkDeadline()
	}
}

// FuzzTCPReassembly delivers a stream as overlapping, duplicated and
// reordered segments, with reads in between, and checks that the reader
// only ever sees the stream's prefix and, after a last full copy with the
// FIN, all of it and then EOF.
func FuzzTCPReassembly(f *testing.F) {
	f.Add([]byte{3, 200, 1, 9, 50, 0, 7, 255, 4, 4, 120, 3})
	f.Add([]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0})
	f.Add(bytes.Repeat([]byte{17, 33, 2}, 40))
	f.Fuzz(func(t *testing.T, plan []byte) {
		if len(plan) < 3 {
			return
		}
		stack := Stack{tuning: DefaultTuning()}
		stack.tuning.TCP.ReceiveBuffer = testRing
		entry := tcpEndpoint{state: TCPEstablished, rcvNxt: uint32(plan[0]) << 24, sndNxt: 50, localMSS: 1460, peerMSS: 1460}
		start := entry.rcvNxt
		length := 1 + int(plan[1])*40
		stream := pattern(0, length)
		var got []byte
		check := func() {
			if !bytes.Equal(got, stream[:len(got)]) {
				t.Fatalf("read bytes differ from the stream at %d", len(got))
			}
			if entry.reorder != nil {
				for index, span := range entry.reorder.spans {
					if !seqGT(span.start, entry.rcvNxt) || (index > 0 && !seqGT(span.start, entry.reorder.spans[index-1].end)) {
						t.Fatalf("spans %v at %d", entry.reorder.spans, entry.rcvNxt)
					}
				}
			}
		}
		read := func(count int) {
			count = min(count, int(entry.recvLen))
			out := make([]byte, count)
			ringLoad(out, entry.recvRing, int(entry.recvHead), count)
			entry.recvHead = ringAdvance(int(entry.recvHead), count, len(entry.recvRing))
			entry.recvLen -= uint32(count)
			got = append(got, out...)
		}
		for index := 2; index+2 < len(plan); index += 3 {
			offset := int(plan[index]) * length / 256
			size := 1 + int(plan[index+1])*8
			end := min(offset+size, length)
			flags := uint8(tcpFlagACK)
			if end == length && plan[index+2]&1 != 0 {
				flags |= tcpFlagFIN
			}
			stack.handlePayload(&entry, tcpSegment{sequence: start + uint32(offset), flags: flags, payload: stream[offset:end]}, second)
			if plan[index+2]&6 == 6 {
				read(int(plan[index+2]) * 16)
			}
			check()
		}
		stack.handlePayload(&entry, tcpSegment{sequence: start, flags: tcpFlagACK | tcpFlagFIN, payload: stream}, second)
		read(length)
		for !entry.finReceived && len(got) < length {
			// The window held back part of the last copy; send the rest.
			sent := len(got)
			stack.handlePayload(&entry, tcpSegment{sequence: start + uint32(sent), flags: tcpFlagACK | tcpFlagFIN, payload: stream[sent:]}, second)
			read(length)
			if len(got) == sent {
				t.Fatalf("no progress at %d of %d", sent, length)
			}
		}
		read(length)
		check()
		if len(got) != length || !entry.finReceived || entry.rcvNxt != start+uint32(length)+1 || entry.reorder != nil {
			t.Fatalf("got %d of %d, FIN %v, rcvNxt %d", len(got), length, entry.finReceived, entry.rcvNxt-start)
		}
	})
}
