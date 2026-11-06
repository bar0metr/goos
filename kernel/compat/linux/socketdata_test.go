package linux

import (
	"testing"

	"goos/userspace/linuxsockapi"
)

func newDataSession(t *testing.T) (NetEndpoint, []byte) {
	t.Helper()
	_, endpoint := newTestSession(t)
	rings := make([]byte, 4096)
	linuxsockapi.InitRings(rings)
	return endpoint, rings
}

// serviceTake pops every submit entry, as linuxsockd does.
func serviceTake(t *testing.T, rings []byte) []linuxsockapi.RingEntry {
	t.Helper()
	var out []linuxsockapi.RingEntry
	for {
		var entry linuxsockapi.RingEntry
		switch linuxsockapi.PopSubmit(rings, &entry) {
		case linuxsockapi.RingEmpty:
			return out
		case linuxsockapi.RingBroken:
			t.Fatal("submit ring broken")
		}
		out = append(out, entry)
	}
}

func servicePush(t *testing.T, rings []byte, entry linuxsockapi.RingEntry) {
	t.Helper()
	if linuxsockapi.PushComplete(rings, &entry) != linuxsockapi.RingOK {
		t.Fatalf("push %+v", entry)
	}
}

func TestTXSlotsCommitAndComeBack(t *testing.T) {
	endpoint, rings := newDataSession(t)
	stream := linuxsockapi.MakeSocket(endpoint.Session, 1, 2)
	other := linuxsockapi.MakeSocket(endpoint.Session, 1, 3)
	NewSocket(testFamily, endpoint.Session, stream, false)
	NewSocket(testFamily, endpoint.Session, other, false)
	// One socket takes at most its share; the other still finds slots.
	var slots []uint8
	for {
		slot, status := TXReserve(testFamily, endpoint.Session, stream)
		if status != DataOK {
			break
		}
		slots = append(slots, slot)
	}
	if len(slots) != linuxsockapi.SocketTXSlots || TXCredit(testFamily, endpoint.Session, stream) {
		t.Fatalf("%d slots for one socket", len(slots))
	}
	if !TXCredit(testFamily, endpoint.Session, other) {
		t.Fatal("one stream starved the other")
	}
	doorbells := 0
	for index, slot := range slots {
		bell, status := TXCommit(testFamily, endpoint.Session, rings, slot, uint32(100+index), false, [4]byte{}, 0)
		if status != DataOK {
			t.Fatalf("commit %d", status)
		}
		if bell {
			doorbells++
		}
	}
	if doorbells != 1 {
		t.Fatalf("%d doorbells for one batch", doorbells)
	}
	committed := serviceTake(t, rings)
	if len(committed) != len(slots) {
		t.Fatalf("%d entries", len(committed))
	}
	for index, entry := range committed {
		if entry.Kind != linuxsockapi.RingTX || entry.Slot != slots[index] || entry.Length != uint32(100+index) || entry.Socket != stream {
			t.Fatalf("entry %+v", entry)
		}
	}
	// A slot comes back only with its own generation.
	servicePush(t, rings, linuxsockapi.RingEntry{Kind: linuxsockapi.RingTXDone, Slot: committed[0].Slot, Generation: committed[0].Generation})
	if _, status := DrainComplete(testFamily, endpoint.Session, rings); status != DataOK || !TXCredit(testFamily, endpoint.Session, stream) {
		t.Fatal("slot not back")
	}
	servicePush(t, rings, linuxsockapi.RingEntry{Kind: linuxsockapi.RingTXDone, Slot: committed[1].Slot, Generation: committed[1].Generation + 1})
	if _, status := DrainComplete(testFamily, endpoint.Session, rings); status != DataBroken {
		t.Fatal("stale generation accepted")
	}
}

func TestTXAbortFreesAndDatagramCarriesDestination(t *testing.T) {
	endpoint, rings := newDataSession(t)
	socket := linuxsockapi.MakeSocket(endpoint.Session, 1, 2)
	NewSocket(testFamily, endpoint.Session, socket, true)
	slot, _ := TXReserve(testFamily, endpoint.Session, socket)
	TXAbort(testFamily, slot)
	again, _ := TXReserve(testFamily, endpoint.Session, socket)
	if again != slot {
		t.Fatal("aborted slot not free")
	}
	if _, status := TXCommit(testFamily, endpoint.Session, rings, again, 5, true, [4]byte{127, 0, 0, 1}, 53); status != DataOK {
		t.Fatal("commit")
	}
	entry := serviceTake(t, rings)[0]
	if entry.Flags != linuxsockapi.RingDestination || entry.Address != [4]byte{127, 0, 0, 1} || entry.Port != 53 {
		t.Fatalf("datagram entry %+v", entry)
	}
	if _, status := TXCommit(testFamily, endpoint.Session, rings, again, 5, false, [4]byte{}, 0); status != DataBroken {
		t.Fatal("committed a slot twice")
	}
}

func TestRXQueuesPerSocketInRingOrder(t *testing.T) {
	endpoint, rings := newDataSession(t)
	a := linuxsockapi.MakeSocket(endpoint.Session, 1, 2)
	b := linuxsockapi.MakeSocket(endpoint.Session, 1, 3)
	NewSocket(testFamily, endpoint.Session, a, false)
	NewSocket(testFamily, endpoint.Session, b, false)
	// Slots arrive out of index order and interleaved between sockets.
	for index, slot := range []uint8{5, 1, 7, 0} {
		owner := a
		if index%2 == 1 {
			owner = b
		}
		servicePush(t, rings, linuxsockapi.RingEntry{Kind: linuxsockapi.RingRXData, Slot: slot, Length: uint32(10 * (index + 1)), Socket: owner, Generation: uint32(index + 1)})
	}
	if _, status := DrainComplete(testFamily, endpoint.Session, rings); status != DataOK {
		t.Fatal("drain")
	}
	var pieces [linuxsockapi.RXSlots]RXPiece
	count, bytes := RXQueue(testFamily, endpoint.Session, a, &pieces)
	if count != 2 || bytes != 40 || pieces[0].Slot != 5 || pieces[1].Slot != 7 {
		t.Fatalf("a queue %d %d %+v", count, bytes, pieces[:count])
	}
	count, _ = RXQueue(testFamily, endpoint.Session, b, &pieces)
	if count != 2 || pieces[0].Slot != 1 || pieces[1].Slot != 0 {
		t.Fatalf("b queue %+v", pieces[:count])
	}
	// A partial read keeps the slot; reading it to the end hands it back.
	if bell, _ := RXConsume(testFamily, endpoint.Session, rings, 5, 4, false); bell || len(serviceTake(t, rings)) != 0 {
		t.Fatal("partial read handed the slot back")
	}
	count, bytes = RXQueue(testFamily, endpoint.Session, a, &pieces)
	if pieces[0].Offset != 4 || bytes != 36 {
		t.Fatalf("after partial %+v", pieces[:count])
	}
	if bell, _ := RXConsume(testFamily, endpoint.Session, rings, 5, 6, false); !bell {
		t.Fatal("no doorbell for a release")
	}
	released := serviceTake(t, rings)
	if len(released) != 1 || released[0].Kind != linuxsockapi.RingRelease || released[0].Slot != 5 || released[0].Generation != 1 {
		t.Fatalf("release %+v", released)
	}
	// Closing b hands its slots back; a keeps its own.
	linuxsockapi.TakeDoorbell(rings)
	if !RXDrop(testFamily, endpoint.Session, rings, b) || len(serviceTake(t, rings)) != 2 {
		t.Fatal("drop")
	}
	if count, _ := RXQueue(testFamily, endpoint.Session, a, &pieces); count != 1 {
		t.Fatal("a lost a slot")
	}
}

func TestRXForUnknownSocketGoesBackAndHeldSlotIsBroken(t *testing.T) {
	endpoint, rings := newDataSession(t)
	socket := linuxsockapi.MakeSocket(endpoint.Session, 1, 2)
	NewSocket(testFamily, endpoint.Session, socket, true)
	servicePush(t, rings, linuxsockapi.RingEntry{Kind: linuxsockapi.RingRXData, Slot: 2, Length: 5, Socket: linuxsockapi.MakeSocket(endpoint.Session, 9, 9), Generation: 3})
	if _, status := DrainComplete(testFamily, endpoint.Session, rings); status != DataOK || len(serviceTake(t, rings)) != 0 {
		t.Fatal("unknown socket's slot went back before the channel was drained")
	}
	if bell, status := ReleaseOrphans(testFamily, endpoint.Session, rings); status != DataOK || !bell {
		t.Fatal("orphan not released")
	}
	if released := serviceTake(t, rings); len(released) != 1 || released[0].Slot != 2 {
		t.Fatalf("release %+v", released)
	}
	// A datagram past the limit breaks the protocol.
	servicePush(t, rings, linuxsockapi.RingEntry{Kind: linuxsockapi.RingRXData, Slot: 3, Length: linuxsockapi.MaxDatagram + 1, Socket: socket, Generation: 1})
	if _, status := DrainComplete(testFamily, endpoint.Session, rings); status != DataBroken {
		t.Fatal("oversized datagram accepted")
	}
}

func TestRXPublishedTwiceIsBroken(t *testing.T) {
	endpoint, rings := newDataSession(t)
	socket := linuxsockapi.MakeSocket(endpoint.Session, 1, 2)
	NewSocket(testFamily, endpoint.Session, socket, false)
	entry := linuxsockapi.RingEntry{Kind: linuxsockapi.RingRXData, Slot: 4, Length: 5, Socket: socket, Generation: 1}
	servicePush(t, rings, entry)
	servicePush(t, rings, entry)
	if _, status := DrainComplete(testFamily, endpoint.Session, rings); status != DataBroken {
		t.Fatal("a held slot published again")
	}
}

func TestMergeReadiness(t *testing.T) {
	endpoint, rings := newDataSession(t)
	stream := linuxsockapi.MakeSocket(endpoint.Session, 1, 2)
	datagram := linuxsockapi.MakeSocket(endpoint.Session, 1, 3)
	NewSocket(testFamily, endpoint.Session, stream, false)
	NewSocket(testFamily, endpoint.Session, datagram, true)
	servicePush(t, rings, linuxsockapi.RingEntry{Kind: linuxsockapi.RingRXData, Slot: 0, Length: 7, Socket: stream, Generation: 1})
	servicePush(t, rings, linuxsockapi.RingEntry{Kind: linuxsockapi.RingRXData, Slot: 1, Length: 3, Socket: datagram, Generation: 1})
	servicePush(t, rings, linuxsockapi.RingEntry{Kind: linuxsockapi.RingRXData, Slot: 2, Length: 9, Socket: datagram, Generation: 1})
	DrainComplete(testFamily, endpoint.Session, rings)
	state := MergeReadiness(testFamily, endpoint.Session, false, linuxsockapi.Readiness{Socket: stream, Ready: linuxsockapi.ReadyOut, Readable: 5, State: linuxsockapi.EntrySend})
	if state.Ready != linuxsockapi.ReadyIn|linuxsockapi.ReadyOut || state.Readable != 12 {
		t.Fatalf("stream %+v", state)
	}
	state = MergeReadiness(testFamily, endpoint.Session, true, linuxsockapi.Readiness{Socket: datagram, Ready: linuxsockapi.ReadyOut, State: linuxsockapi.EntrySend})
	if state.Ready&linuxsockapi.ReadyIn == 0 || state.Readable != 3 {
		t.Fatalf("datagram %+v", state)
	}
	// Without slot credit a socket that sends through slots is not writable.
	for {
		if _, status := TXReserve(testFamily, endpoint.Session, stream); status != DataOK {
			break
		}
	}
	state = MergeReadiness(testFamily, endpoint.Session, false, linuxsockapi.Readiness{Socket: stream, Ready: linuxsockapi.ReadyOut, State: linuxsockapi.EntrySend})
	if state.Ready&linuxsockapi.ReadyOut != 0 {
		t.Fatalf("writable without credit %+v", state)
	}
}

func TestRXOfAcceptedChildWaitsForItsSocket(t *testing.T) {
	service, endpoint := newTestSession(t)
	rings := make([]byte, 4096)
	linuxsockapi.InitRings(rings)
	listener := linuxsockapi.MakeSocket(endpoint.Session, 1, 2)
	child := linuxsockapi.MakeSocket(endpoint.Session, 1, 3)
	NewSocket(testFamily, endpoint.Session, listener, false)
	_, correlation := issue(t, service, endpoint, testTask, linuxsockapi.Request{Opcode: linuxsockapi.OpAccept, Socket: listener})
	_, seen := service.request()
	service.reply(linuxsockapi.Reply{Opcode: linuxsockapi.OpAccept, Socket: listener, Value: child, Port: 4000}, seen)
	var reply linuxsockapi.Reply
	drain(t, service, endpoint, &reply)
	DeliverReply(testFamily, correlation, &reply)
	// The child's first bytes come before the accepting call installed it.
	servicePush(t, rings, linuxsockapi.RingEntry{Kind: linuxsockapi.RingRXData, Slot: 1, Length: 3, Socket: child, Generation: 1})
	if _, status := DrainComplete(testFamily, endpoint.Session, rings); status != DataOK {
		t.Fatal("drain")
	}
	if bell, _ := ReleaseOrphans(testFamily, endpoint.Session, rings); bell {
		t.Fatal("accepted child's data handed back")
	}
	NewSocket(testFamily, endpoint.Session, child, false)
	var pieces [linuxsockapi.RXSlots]RXPiece
	if count, bytes := RXQueue(testFamily, endpoint.Session, child, &pieces); count != 1 || bytes != 3 {
		t.Fatalf("child queue %d %d", count, bytes)
	}
}
