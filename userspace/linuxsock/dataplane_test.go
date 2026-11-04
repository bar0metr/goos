package linuxsock

import (
	"bytes"
	"testing"

	"goos/userspace/linuxsockapi"
)

// The kernel's side of the rings, played by the tests.

func (h *harness) rings(family int) []byte {
	return h.pools[family][linuxsockapi.RingPage*4096 : (linuxsockapi.RingPage+1)*4096]
}

var txGeneration uint32

// commit copies data into TX slot and commits it for socket, then rings
// the doorbell.
func (h *harness) commit(family int, slot uint8, socket uint64, data []byte, destination bool, address [4]byte, port uint16) uint32 {
	h.t.Helper()
	offset := linuxsockapi.TXOffset(slot)
	copy(h.pools[family][offset:offset+linuxsockapi.SlotSize], data)
	txGeneration++
	entry := linuxsockapi.RingEntry{Kind: linuxsockapi.RingTX, Slot: slot, Length: uint32(len(data)), Socket: socket, Generation: txGeneration}
	if destination {
		entry.Flags, entry.Address, entry.Port = linuxsockapi.RingDestination, address, port
	}
	if linuxsockapi.PushSubmit(h.rings(family), &entry) != linuxsockapi.RingOK {
		h.t.Fatal("submit ring")
	}
	h.doorbell(family)
	return txGeneration
}

func (h *harness) doorbell(family int) {
	h.t.Helper()
	h.send(family, linuxsockapi.Request{Opcode: linuxsockapi.OpDoorbell})
}

// releaseRX hands an RX slot back.
func (h *harness) releaseRX(family int, slot uint8, generation uint32) {
	h.t.Helper()
	entry := linuxsockapi.RingEntry{Kind: linuxsockapi.RingRelease, Slot: slot, Generation: generation}
	if linuxsockapi.PushSubmit(h.rings(family), &entry) != linuxsockapi.RingOK {
		h.t.Fatal("submit ring")
	}
	h.doorbell(family)
}

// completions pops every complete-ring entry.
func (h *harness) completions(family int) []linuxsockapi.RingEntry {
	h.t.Helper()
	var out []linuxsockapi.RingEntry
	for {
		var entry linuxsockapi.RingEntry
		switch linuxsockapi.PopComplete(h.rings(family), &entry) {
		case linuxsockapi.RingEmpty:
			return out
		case linuxsockapi.RingBroken:
			h.t.Fatal("complete ring broken")
		}
		out = append(out, entry)
	}
}

func (h *harness) rxBytes(family int, entry linuxsockapi.RingEntry) []byte {
	offset := linuxsockapi.RXOffset(entry.Slot)
	return h.pools[family][offset : offset+uint64(entry.Length)]
}

// takeWrites removes every native write submitted so far, oldest first.
func (h *harness) takeWrites() []submitted {
	var writes []submitted
	for h.has(NativeWrite) {
		writes = append(writes, h.take(NativeWrite))
	}
	return writes
}

// readAhead removes the parked read ahead of endpoint.
func (h *harness) readAhead(endpoint uint32) submitted {
	h.t.Helper()
	for index, entry := range h.backend.log {
		if entry.request.Op == NativeRead && entry.request.Endpoint == endpoint {
			if entry.request.Flags&NativeNoWait != 0 {
				h.t.Fatal("a read ahead must park in netd")
			}
			h.backend.log = append(h.backend.log[:index], h.backend.log[index+1:]...)
			return entry
		}
	}
	h.t.Fatalf("no read ahead on %d", endpoint)
	return submitted{}
}

func pattern(length int, seed byte) []byte {
	data := make([]byte, length)
	for index := range data {
		data[index] = seed + byte(index*7)
	}
	return data
}

func TestStreamWritesPipelineWithOffsets(t *testing.T) {
	h := newHarness(t)
	f := h.family(1, 7)
	id := h.connected(f)
	h.wake(h.endpoint(id), MaskConnected|MaskWritable, 0)
	if state := h.readiness(f, id); state.State&linuxsockapi.EntrySend == 0 || state.Ready&linuxsockapi.ReadyOut == 0 {
		t.Fatalf("connected stream does not take sends %+v", state)
	}
	data := [][]byte{pattern(linuxsockapi.SlotSize, 1), pattern(linuxsockapi.SlotSize, 2), pattern(100, 3)}
	for slot, payload := range data {
		h.commit(f, uint8(slot), id, payload, false, [4]byte{}, 0)
	}
	writes := h.takeWrites()
	if len(writes) != 3 {
		t.Fatalf("%d writes, want all three at once", len(writes))
	}
	offset := uint32(0)
	for index, write := range writes {
		if write.request.Offset != offset || !bytes.Equal(write.data, data[index]) || write.request.Flags&NativeNoWait == 0 {
			t.Fatalf("write %d: offset %d length %d", index, write.request.Offset, len(write.data))
		}
		offset += uint32(len(write.data))
	}
	for _, write := range writes {
		h.complete(write, NativeCompletion{Length: uint32(len(write.data))})
	}
	done := h.completions(f)
	if len(done) != 3 {
		t.Fatalf("%d slots back", len(done))
	}
	for index, entry := range done {
		if entry.Kind != linuxsockapi.RingTXDone || entry.Slot != uint8(index) {
			t.Fatalf("done %+v", entry)
		}
	}
	if h.svc.families[f].txHeld != 0 || h.svc.Counters().BytesOut != 2*linuxsockapi.SlotSize+100 {
		t.Fatal("slots or bytes not accounted")
	}
	if h.notifications(f) == 0 {
		t.Fatal("no ReadyChanged for the slots handed back")
	}
}

func TestPartialWriteSendsAgainFromTheFirstUntakenByte(t *testing.T) {
	h := newHarness(t)
	f := h.family(1, 7)
	id := h.connected(f)
	endpoint := h.endpoint(id)
	h.wake(endpoint, MaskConnected|MaskWritable, 0)
	first, second := pattern(linuxsockapi.SlotSize, 1), pattern(5000, 9)
	h.commit(f, 0, id, first, false, [4]byte{}, 0)
	h.commit(f, 1, id, second, false, [4]byte{}, 0)
	writes := h.takeWrites()
	if len(writes) != 2 {
		t.Fatalf("%d writes", len(writes))
	}
	// netd took 1000 bytes of the first; the second started past that and
	// took nothing.
	h.complete(writes[0], NativeCompletion{Length: 1000})
	h.complete(writes[1], NativeCompletion{Length: 0})
	if h.has(NativeWrite) {
		t.Fatal("wrote again before netd had room")
	}
	if len(h.completions(f)) != 0 {
		t.Fatal("a slot went back before netd took all of it")
	}
	h.complete(h.take(NativeReady), NativeCompletion{Mask: MaskConnected | MaskWritable})
	resent := h.takeWrites()
	if len(resent) != 2 || resent[0].request.Offset != 1000 || !bytes.Equal(resent[0].data, first[1000:]) ||
		resent[1].request.Offset != linuxsockapi.SlotSize || !bytes.Equal(resent[1].data, second) {
		t.Fatalf("resent %d writes", len(resent))
	}
	h.complete(resent[0], NativeCompletion{Length: uint32(len(resent[0].data))})
	h.complete(resent[1], NativeCompletion{Length: uint32(len(resent[1].data))})
	if done := h.completions(f); len(done) != 2 {
		t.Fatalf("%d slots back", len(done))
	}
}

func TestTransportErrorAfterCommitBecomesPendingError(t *testing.T) {
	h := newHarness(t)
	f := h.family(1, 7)
	id := h.connected(f)
	h.wake(h.endpoint(id), MaskConnected|MaskWritable, 0)
	h.commit(f, 0, id, pattern(10, 1), false, [4]byte{}, 0)
	h.commit(f, 1, id, pattern(10, 2), false, [4]byte{}, 0)
	writes := h.takeWrites()
	h.complete(writes[0], NativeCompletion{Result: NativeReset})
	h.complete(writes[1], NativeCompletion{Result: NativeReset})
	if done := h.completions(f); len(done) != 2 {
		t.Fatalf("%d slots back after the error", len(done))
	}
	state := h.readiness(f, id)
	if state.State&linuxsockapi.EntrySend != 0 || state.Error != linuxsockapi.ResultReset || state.Ready&linuxsockapi.ReadyError == 0 {
		t.Fatalf("after reset %+v", state)
	}
	if reply := h.call(f, linuxsockapi.Request{Opcode: opWrite, Socket: id}); reply.Result != linuxsockapi.ResultReset {
		t.Fatalf("write after reset %d", reply.Result)
	}
	// A commit that raced the error goes back unsent.
	h.wake(h.endpoint(id), MaskConnected|MaskHungUp, 0)
	h.commit(f, 2, id, pattern(10, 3), false, [4]byte{}, 0)
	if h.has(NativeWrite) || len(h.completions(f)) != 1 {
		t.Fatal("late commit not handed back")
	}
}

func TestShutdownWriteFollowsQueuedBytes(t *testing.T) {
	h := newHarness(t)
	f := h.family(1, 7)
	id := h.connected(f)
	h.wake(h.endpoint(id), MaskConnected|MaskWritable, 0)
	h.commit(f, 0, id, pattern(100, 1), false, [4]byte{}, 0)
	if reply := h.call(f, linuxsockapi.Request{Opcode: opShutdown, Socket: id, Length: shutWrite}); reply.Result != linuxsockapi.ResultOK {
		t.Fatalf("SHUT_WR %d", reply.Result)
	}
	if h.has(NativeShutdownWrite) {
		t.Fatal("half-close before the queued bytes")
	}
	write := h.take(NativeWrite)
	h.complete(write, NativeCompletion{Length: 100})
	h.complete(h.take(NativeShutdownWrite), NativeCompletion{})
	if state := h.readiness(f, id); state.State&linuxsockapi.EntrySend != 0 {
		t.Fatal("a shut stream still takes sends")
	}
	if reply := h.call(f, linuxsockapi.Request{Opcode: opWrite, Socket: id}); reply.Result != linuxsockapi.ResultClosed {
		t.Fatalf("write after SHUT_WR %d", reply.Result)
	}
}

func TestCloseWithQueuedBytesDrainsFirst(t *testing.T) {
	h := newHarness(t)
	f := h.family(1, 7)
	id := h.connected(f)
	endpoint := h.endpoint(id)
	h.wake(endpoint, MaskConnected|MaskWritable, 0)
	h.commit(f, 0, id, pattern(100, 1), false, [4]byte{}, 0)
	if reply := h.call(f, linuxsockapi.Request{Opcode: opClose, Socket: id}); reply.Result != linuxsockapi.ResultOK {
		t.Fatalf("close %d", reply.Result)
	}
	if h.has(NativeClose) {
		t.Fatal("endpoint closed before its bytes went")
	}
	if state := h.readiness(f, id); state.Socket != 0 {
		t.Fatalf("entry of a closed socket %+v", state)
	}
	if reply := h.call(f, linuxsockapi.Request{Opcode: opGetName, Socket: id}); reply.Result != linuxsockapi.ResultStale {
		t.Fatalf("closed identity %d", reply.Result)
	}
	h.complete(h.take(NativeWrite), NativeCompletion{Length: 100})
	if closed := h.take(NativeClose); closed.request.Endpoint != endpoint {
		t.Fatalf("closed %d", closed.request.Endpoint)
	}
	if len(h.completions(f)) != 1 {
		t.Fatal("slot not handed back")
	}
}

func TestCloseReleasesEndpointAndIdentity(t *testing.T) {
	h := newHarness(t)
	f := h.family(1, 7)
	id := h.connected(f)
	endpoint := h.endpoint(id)
	if reply := h.call(f, linuxsockapi.Request{Opcode: opClose, Socket: id}); reply.Result != linuxsockapi.ResultOK {
		t.Fatalf("close %d", reply.Result)
	}
	if closed := h.take(NativeClose); closed.request.Endpoint != endpoint {
		t.Fatalf("closed %d", closed.request.Endpoint)
	}
	if state := h.readiness(f, id); state.Socket != 0 {
		t.Fatalf("entry not cleared %+v", state)
	}
	// netd answers the parked read ahead with Closed; its slot frees.
	h.complete(h.readAhead(endpoint), NativeCompletion{Result: NativeClosed})
	if h.svc.families[f].rxUsed != 0 {
		t.Fatal("read-ahead slot kept")
	}
	if reply := h.call(f, linuxsockapi.Request{Opcode: opRead, Socket: id}); reply.Result != linuxsockapi.ResultStale {
		t.Fatalf("closed identity %d", reply.Result)
	}
	again := h.open(f, linuxsockapi.KindStream)
	oldIndex, _ := linuxsockapi.SocketIndex(id)
	newIndex, _ := linuxsockapi.SocketIndex(again)
	if oldIndex != newIndex || again == id {
		t.Fatalf("reuse %x %x", id, again)
	}
}

func TestReadAheadPublishesSlotsInOrder(t *testing.T) {
	h := newHarness(t)
	f := h.family(1, 7)
	id := h.connected(f)
	endpoint := h.endpoint(id)
	first := pattern(NativeSlotSize, 1)
	h.complete(h.readAhead(endpoint), NativeCompletion{Data: first})
	// A full slot means more may follow: the core parks more reads, as
	// many as the socket's share and the reserve allow.
	reads := 0
	for h.has(NativeRead) {
		h.readAhead(endpoint)
		reads++
	}
	if reads != linuxsockapi.SocketRXSlots-1 {
		t.Fatalf("%d reads ahead after a full slot", reads)
	}
	published := h.completions(f)
	if len(published) != 1 || published[0].Kind != linuxsockapi.RingRXData || published[0].Socket != id ||
		!bytes.Equal(h.rxBytes(f, published[0]), first) {
		t.Fatalf("published %+v", published)
	}
	if h.notifications(f) == 0 {
		t.Fatal("no ReadyChanged for the published slot")
	}
	// The kernel holds the bytes, so the entry no longer counts them.
	h.wake(endpoint, MaskConnected|MaskWritable, 0)
	if state := h.readiness(f, id); state.Ready&linuxsockapi.ReadyIn != 0 {
		t.Fatalf("readable with nothing in netd %+v", state)
	}
	h.releaseRX(f, published[0].Slot, published[0].Generation)
	if h.svc.families[f].rxPub != 0 {
		t.Fatal("released slot still published")
	}
	// A release with a stale generation breaks the protocol.
	h.releaseRX(f, published[0].Slot, published[0].Generation)
	if !h.dropped(f) {
		t.Fatal("double release accepted")
	}
}

func TestEndOfStreamFollowsItsBytes(t *testing.T) {
	h := newHarness(t)
	f := h.family(1, 7)
	id := h.connected(f)
	endpoint := h.endpoint(id)
	h.complete(h.readAhead(endpoint), NativeCompletion{Data: []byte("tail")})
	h.complete(h.readAhead(endpoint), NativeCompletion{Result: NativeEOF})
	if h.has(NativeRead) {
		t.Fatal("read ahead past the end")
	}
	published := h.completions(f)
	if len(published) != 1 {
		t.Fatalf("%d published", len(published))
	}
	state := h.readiness(f, id)
	if state.Ready&linuxsockapi.ReadyIn == 0 || state.Ready&linuxsockapi.ReadyReadHangup == 0 {
		t.Fatalf("end of stream not readable %+v", state)
	}
	// Until the kernel took the last slot, a read looks at the ring again.
	if reply := h.call(f, linuxsockapi.Request{Opcode: opRead, Socket: id}); reply.Result != linuxsockapi.ResultOK || reply.Flags != 0 {
		t.Fatalf("read before release %+v", reply)
	}
	h.releaseRX(f, published[0].Slot, published[0].Generation)
	if reply := h.call(f, linuxsockapi.Request{Opcode: opRead, Socket: id}); reply.Result != linuxsockapi.ResultOK || reply.Flags != linuxsockapi.FlagEOF {
		t.Fatalf("read at the end %+v", reply)
	}
}

func TestReadWaitsForReadAheadNetdHolds(t *testing.T) {
	h := newHarness(t)
	f := h.family(1, 7)
	id := h.connected(f)
	endpoint := h.endpoint(id)
	read := h.readAhead(endpoint)
	// Nothing in netd: the read would block.
	if reply := h.call(f, linuxsockapi.Request{Opcode: opRead, Socket: id}); reply.Result != linuxsockapi.ResultWouldBlock {
		t.Fatalf("empty read %d", reply.Result)
	}
	// netd says it holds bytes; the read waits for the read ahead.
	h.wake(endpoint, MaskConnected|MaskWritable|MaskReadable, 5)
	correlation := h.send(f, linuxsockapi.Request{Opcode: opRead, Socket: id})
	h.noReply(f, correlation)
	h.complete(read, NativeCompletion{Data: []byte("hello")})
	if reply := h.reply(f, correlation); reply.Result != linuxsockapi.ResultOK {
		t.Fatalf("read after read ahead %d", reply.Result)
	}
	if published := h.completions(f); len(published) != 1 || string(h.rxBytes(f, published[0])) != "hello" {
		t.Fatalf("published %+v", published)
	}
}

func TestReadWithoutReadAheadGoesToNetd(t *testing.T) {
	h := newHarness(t)
	f := h.family(1, 7)
	// Five sockets each take a read ahead; the sixth finds only the
	// reserve and must ask netd when the kernel reads.
	var last uint64
	for index := 0; index < linuxsockapi.RXSlots-linuxsockapi.RXReserve; index++ {
		h.connected(f)
	}
	last = h.connected(f)
	endpoint := h.endpoint(last)
	for _, entry := range h.backend.log {
		if entry.request.Op == NativeRead && entry.request.Endpoint == endpoint {
			t.Fatal("read ahead took the reserve")
		}
	}
	h.wake(endpoint, MaskConnected|MaskWritable|MaskReadable, 3)
	correlation := h.send(f, linuxsockapi.Request{Opcode: opRead, Socket: last})
	var direct submitted
	for index, entry := range h.backend.log {
		if entry.request.Op == NativeRead && entry.request.Endpoint == endpoint {
			direct = entry
			h.backend.log = append(h.backend.log[:index], h.backend.log[index+1:]...)
			break
		}
	}
	if direct.request.Endpoint != endpoint || direct.request.Flags&NativeNoWait == 0 {
		t.Fatalf("direct read %+v", direct.request)
	}
	h.complete(direct, NativeCompletion{Data: []byte("abc")})
	if reply := h.reply(f, correlation); reply.Result != linuxsockapi.ResultOK {
		t.Fatalf("direct read reply %d", reply.Result)
	}
	if published := h.completions(f); len(published) != 1 || published[0].Socket != last {
		t.Fatalf("published %+v", published)
	}
}

func TestStaleCommitGoesBackAndMalformedRingFails(t *testing.T) {
	h := newHarness(t)
	f := h.family(1, 7)
	h.commit(f, 3, linuxsockapi.MakeSocket(7, 9, 4), pattern(10, 1), false, [4]byte{}, 0)
	if done := h.completions(f); len(done) != 1 || done[0].Kind != linuxsockapi.RingTXDone || done[0].Slot != 3 {
		t.Fatalf("stale commit %+v", done)
	}
	if h.dropped(f) {
		t.Fatal("stale commit failed the family")
	}
	id := h.connected(f)
	h.commit(f, 4, id, pattern(10, 1), false, [4]byte{}, 0)
	// The same slot again, while the service holds it.
	h.commit(f, 4, id, pattern(10, 1), false, [4]byte{}, 0)
	if !h.dropped(f) {
		t.Fatal("slot committed twice")
	}
}

func TestDatagramsGoThroughSlots(t *testing.T) {
	h := newHarness(t)
	f := h.family(1, 7)
	id := h.open(f, linuxsockapi.KindDatagram)
	// Unbound: the readiness entry does not take sends; SendTo binds.
	if state := h.readiness(f, id); state.State&linuxsockapi.EntrySend != 0 {
		t.Fatal("unbound datagram takes sends")
	}
	correlation := h.send(f, linuxsockapi.Request{Opcode: opSendTo, Socket: id, Address: peerAddress, Port: 53})
	h.complete(h.take(NativeBind), NativeCompletion{Local: localAddress, LocalPort: 5000})
	if reply := h.reply(f, correlation); reply.Result != linuxsockapi.ResultOK {
		t.Fatalf("send check %d", reply.Result)
	}
	endpoint := h.endpoint(id)
	if state := h.readiness(f, id); state.State != linuxsockapi.EntrySend {
		t.Fatalf("bound datagram state %+v", state)
	}
	h.commit(f, 0, id, []byte("query"), true, peerAddress, 53)
	send := h.take(NativeSendTo)
	if send.request.Address != peerAddress || send.request.Port != 53 || string(send.data) != "query" {
		t.Fatalf("sendto %+v", send.request)
	}
	h.complete(send, NativeCompletion{})
	if done := h.completions(f); len(done) != 1 || done[0].Kind != linuxsockapi.RingTXDone {
		t.Fatalf("done %+v", done)
	}
	// A bound socket reads ahead: one datagram per slot, with its source.
	h.complete(h.readAhead(endpoint), NativeCompletion{Data: []byte("answer"), Full: 6, Address: peerAddress, Port: 53})
	published := h.completions(f)
	if len(published) != 1 || published[0].Address != peerAddress || published[0].Port != 53 || string(h.rxBytes(f, published[0])) != "answer" {
		t.Fatalf("published %+v", published)
	}
	if !h.has(NativeRead) {
		t.Fatal("no next read ahead")
	}
}

func TestBackendLossHandsBackQueuedSlots(t *testing.T) {
	h := newHarness(t)
	f := h.family(1, 7)
	id := h.connected(f)
	h.wake(h.endpoint(id), MaskConnected|MaskWritable, 0)
	h.backend.busy = true
	h.commit(f, 0, id, pattern(10, 1), false, [4]byte{}, 0)
	h.commit(f, 1, id, pattern(10, 2), false, [4]byte{}, 0)
	h.backend.busy = false
	h.svc.BackendDown(TCP, h.generation[TCP])
	if done := h.completions(f); len(done) != 2 {
		t.Fatalf("%d slots back after the loss", len(done))
	}
	if state := h.readiness(f, id); state.State&linuxsockapi.EntrySend != 0 || state.Ready&linuxsockapi.ReadyHangup == 0 {
		t.Fatalf("dead socket %+v", state)
	}
}

func TestBusyNativeSessionRetriesWrites(t *testing.T) {
	h := newHarness(t)
	f := h.family(1, 7)
	id := h.connected(f)
	h.wake(h.endpoint(id), MaskConnected|MaskWritable, 0)
	h.backend.busy = true
	h.commit(f, 0, id, []byte("abc"), false, [4]byte{}, 0)
	h.backend.busy = false
	if h.has(NativeWrite) {
		t.Fatal("write went while busy")
	}
	// Any later event pumps the queue.
	h.call(f, linuxsockapi.Request{Opcode: opGetName, Socket: id})
	write := h.take(NativeWrite)
	h.complete(write, NativeCompletion{Length: 3})
	if len(h.completions(f)) != 1 {
		t.Fatal("slot not back")
	}
}
