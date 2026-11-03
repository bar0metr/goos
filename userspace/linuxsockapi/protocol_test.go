package linuxsockapi

import (
	"sync"
	"testing"

	"goos/kernel/ipc"
)

func roundTrip(t *testing.T, message *ipc.Message) *ipc.Message {
	t.Helper()
	var wire [ipc.WireMaxSize]byte
	size, ok := ipc.EncodeFrom(message, wire[:])
	if !ok {
		t.Fatal("wire encode")
	}
	var out ipc.Message
	if !ipc.DecodeInto(wire[:size], &out) {
		t.Fatal("wire decode")
	}
	return &out
}

var (
	socket = MakeSocket(3, 7, 5)
	peer   = [4]byte{10, 0, 2, 2}
	local  = [4]byte{10, 0, 2, 15}
)

func validRequests() []Request {
	return []Request{
		{Opcode: OpOpen, Length: KindStream},
		{Opcode: OpOpen, Length: KindDatagram},
		{Opcode: OpBind, Socket: socket},
		{Opcode: OpBind, Socket: socket, Flags: FlagReuseAddr, Address: [4]byte{127, 0, 0, 1}, Port: 8080},
		{Opcode: OpListen, Socket: socket, Length: 4096},
		{Opcode: OpAccept, Socket: socket},
		{Opcode: OpConnect, Socket: socket, Address: peer, Port: 80},
		{Opcode: OpRead, Socket: socket, Length: SlotSize, Flags: FlagPeek},
		{Opcode: OpRead, Socket: socket, Length: 1, Flags: FlagDiscard},
		{Opcode: OpWrite, Socket: socket, Slot: TXSlots - 1, Length: SlotSize},
		{Opcode: OpWrite, Socket: socket},
		{Opcode: OpSendTo, Socket: socket, Slot: 2, Length: 12, Address: peer, Port: 53},
		{Opcode: OpShutdown, Socket: socket, Length: 2},
		{Opcode: OpGetSockName, Socket: socket},
		{Opcode: OpGetPeerName, Socket: socket},
		{Opcode: OpGetOption, Socket: socket, Level: 1, Name: 4},
		{Opcode: OpSetOption, Socket: socket, Level: 6, Name: 1, Value: 1},
		{Opcode: OpGetPendingError, Socket: socket},
		{Opcode: OpQueryReady, Socket: socket},
		{Opcode: OpClose, Socket: socket},
		{Opcode: OpCancel, Value: 99},
		{Opcode: OpRelease, Socket: socket, Slot: RXSlots - 1, Length: 10, Value: SlotSize - 10},
		{Opcode: OpReadyChanged},
	}
}

func TestRequestsRoundTrip(t *testing.T) {
	for _, request := range validRequests() {
		correlation := uint64(42)
		if notification(request.Opcode) {
			correlation = 0
		}
		var message ipc.Message
		if !EncodeRequest(&request, correlation, &message) {
			t.Fatalf("encode %+v", request)
		}
		var decoded Request
		got, ok := DecodeRequest(roundTrip(t, &message), &decoded)
		if !ok || got != correlation || decoded != request {
			t.Fatalf("decode %+v: %+v %d %v", request, decoded, got, ok)
		}
	}
}

func TestRequestsRefuseUnusedFields(t *testing.T) {
	bad := []Request{
		{Opcode: 0},
		{Opcode: lastOpcode + 1},
		{Opcode: OpOpen, Length: 3},
		{Opcode: OpOpen, Length: KindStream, Socket: socket},
		{Opcode: OpBind, Socket: 0},
		{Opcode: OpBind, Socket: MakeSocket(1, 1, MaxSockets)},
		{Opcode: OpBind, Socket: socket, Flags: FlagPeek},
		{Opcode: OpAccept, Socket: socket, Port: 1},
		{Opcode: OpConnect, Socket: socket, Address: peer},
		{Opcode: OpRead, Socket: socket},
		{Opcode: OpRead, Socket: socket, Length: SlotSize + 1},
		{Opcode: OpWrite, Socket: socket, Slot: TXSlots},
		{Opcode: OpShutdown, Socket: socket, Length: 3},
		{Opcode: OpGetOption, Socket: socket, Value: 1},
		{Opcode: OpCancel},
		{Opcode: OpRelease, Socket: socket, Slot: RXSlots},
		{Opcode: OpRelease, Socket: socket, Length: 10, Value: SlotSize - 9},
		{Opcode: OpReadyChanged, Socket: socket},
	}
	for _, request := range bad {
		var message ipc.Message
		if EncodeRequest(&request, 1, &message) || EncodeRequest(&request, 0, &message) {
			t.Fatalf("encoded %+v", request)
		}
	}
	request := Request{Opcode: OpClose, Socket: socket}
	var message ipc.Message
	if EncodeRequest(&request, 0, &message) {
		t.Fatal("request without correlation")
	}
	notice := Request{Opcode: OpReadyChanged}
	if EncodeRequest(&notice, 5, &message) {
		t.Fatal("notification with correlation")
	}
}

func TestDecodeRequestRefusesReservedBytes(t *testing.T) {
	request := Request{Opcode: OpConnect, Socket: socket, Address: peer, Port: 80}
	for _, offset := range []int{offThird, offGap, offGap + 1, offExtra, RequestSize - 1} {
		var message ipc.Message
		EncodeRequest(&request, 7, &message)
		message.Payload[offset] = 1
		var decoded Request
		if _, ok := DecodeRequest(&message, &decoded); ok {
			t.Fatalf("accepted reserved byte %d", offset)
		}
	}
	var message ipc.Message
	EncodeRequest(&request, 7, &message)
	message.Kind = ipc.KindNotification
	var decoded Request
	if _, ok := DecodeRequest(&message, &decoded); ok {
		t.Fatal("request as notification")
	}
	EncodeRequest(&request, 7, &message)
	message.Version++
	if _, ok := DecodeRequest(&message, &decoded); ok {
		t.Fatal("other version")
	}
}

func validReplies() []Reply {
	child := MakeSocket(3, 8, 6)
	return []Reply{
		{Opcode: OpOpen, Socket: socket},
		{Opcode: OpOpen, Result: ResultFamilyLimit},
		{Opcode: OpBind, Socket: socket, Local: local, LocalPort: 49152},
		{Opcode: OpBind, Socket: socket, Result: ResultAddressInUse},
		{Opcode: OpListen, Socket: socket},
		{Opcode: OpAccept, Socket: socket, Value: child, Address: peer, Port: 40000, Local: local, LocalPort: 80},
		{Opcode: OpAccept, Socket: socket, Result: ResultWouldBlock},
		{Opcode: OpConnect, Socket: socket, Address: peer, Port: 80, Local: local, LocalPort: 50000},
		{Opcode: OpConnect, Socket: socket, Result: ResultInProgress},
		{Opcode: OpRead, Socket: socket, Slot: 3, Length: 100, Flags: FlagTruncated, Value: 200, Address: peer, Port: 53},
		{Opcode: OpRead, Socket: socket, Flags: FlagEOF},
		{Opcode: OpRead, Socket: socket, Result: ResultWouldBlock},
		{Opcode: OpWrite, Socket: socket, Slot: 1, Length: 4000},
		{Opcode: OpWrite, Socket: socket, Slot: 1, Result: ResultClosed},
		{Opcode: OpSendTo, Socket: socket, Slot: 0, Result: ResultMessageSize},
		{Opcode: OpShutdown, Socket: socket},
		{Opcode: OpGetSockName, Socket: socket, Local: local, LocalPort: 1},
		{Opcode: OpGetPeerName, Socket: socket, Address: peer, Port: 2},
		{Opcode: OpGetPeerName, Socket: socket, Result: ResultNotConnected},
		{Opcode: OpGetOption, Socket: socket, Value: 16384},
		{Opcode: OpSetOption, Socket: socket, Result: ResultNoOption},
		{Opcode: OpGetPendingError, Socket: socket, Value: uint64(ResultRefused)},
		{Opcode: OpQueryReady, Socket: socket, Value: uint64(ReadyIn | ReadyReadHangup), Extra: 12},
		{Opcode: OpClose, Socket: socket},
		{Opcode: OpCancel},
	}
}

func TestRepliesRoundTrip(t *testing.T) {
	for _, reply := range validReplies() {
		var message ipc.Message
		if !EncodeReply(&reply, 9, &message) {
			t.Fatalf("encode %+v", reply)
		}
		var decoded Reply
		got, ok := DecodeReply(roundTrip(t, &message), &decoded)
		if !ok || got != 9 || decoded != reply {
			t.Fatalf("decode %+v: %+v %v", reply, decoded, ok)
		}
	}
}

func TestRepliesRefuseUnusedFields(t *testing.T) {
	bad := []Reply{
		{Opcode: OpRelease},
		{Opcode: OpReadyChanged},
		{Opcode: OpOpen, Result: lastResult + 1, Socket: socket},
		{Opcode: OpOpen, Result: ResultFamilyLimit, Socket: socket},
		{Opcode: OpListen, Socket: socket, Value: 1},
		{Opcode: OpAccept, Socket: socket, Value: 0},
		{Opcode: OpAccept, Socket: socket, Result: ResultWouldBlock, Value: socket},
		{Opcode: OpRead, Socket: socket, Result: ResultWouldBlock, Slot: 1},
		{Opcode: OpRead, Socket: socket, Slot: RXSlots, Length: 1},
		{Opcode: OpRead, Socket: socket, Slot: 1},
		{Opcode: OpWrite, Socket: socket, Slot: TXSlots},
		{Opcode: OpWrite, Socket: socket, Result: ResultClosed, Length: 1},
		{Opcode: OpQueryReady, Socket: socket, Value: 1 << 10},
		{Opcode: OpGetPendingError, Socket: socket, Value: uint64(lastResult) + 1},
		{Opcode: OpCancel, Socket: socket},
		{Opcode: OpClose},
	}
	for _, reply := range bad {
		var message ipc.Message
		if EncodeReply(&reply, 1, &message) {
			t.Fatalf("encoded %+v", reply)
		}
	}
	reply := Reply{Opcode: OpClose, Socket: socket}
	var message ipc.Message
	if EncodeReply(&reply, 0, &message) {
		t.Fatal("reply without correlation")
	}
}

func TestDecodeReplyRefusesReservedBytes(t *testing.T) {
	reply := Reply{Opcode: OpConnect, Socket: socket, Address: peer, Port: 80, Local: local, LocalPort: 50000}
	for _, offset := range []int{offGap, offGap + 1, offName + 2, offName + 3} {
		var message ipc.Message
		EncodeReply(&reply, 7, &message)
		message.Payload[offset] = 1
		var decoded Reply
		if _, ok := DecodeReply(&message, &decoded); ok {
			t.Fatalf("accepted reserved byte %d", offset)
		}
	}
}

func TestSocketIdentity(t *testing.T) {
	id := MakeSocket(0xdeadbeef, 0x123456, 31)
	if index, ok := SocketIndex(id); !ok || index != 31 || SessionOf(id) != 0xdeadbeef {
		t.Fatalf("identity %#x", id)
	}
	if _, ok := SocketIndex(0); ok {
		t.Fatal("zero identity")
	}
	if _, ok := SocketIndex(MakeSocket(1, 1, 32)); ok {
		t.Fatal("index past the family table")
	}
}

func TestPoolLayout(t *testing.T) {
	if TXOffset(0) != pageSize || RXOffset(0) != 9*pageSize || PoolPages != 17 || RXOffset(RXSlots-1)+SlotSize != PoolBytes {
		t.Fatalf("layout tx %d rx %d pages %d", TXOffset(0), RXOffset(0), PoolPages)
	}
	if ReadinessBytes > pageSize {
		t.Fatal("readiness page overflows")
	}
}

func TestReadinessPublishAndRead(t *testing.T) {
	page := make([]byte, pageSize)
	InitReadiness(page, 3)
	if Magic(page) != ReadinessMagic || Session(page) != 3 {
		t.Fatal("header")
	}
	state := Readiness{Socket: socket, Ready: ReadyIn | ReadyOut, Readable: 77, Error: ResultReset}
	Publish(page, 5, state)
	got, ok := Read(page, 5, 4)
	if !ok || got != state {
		t.Fatalf("read %+v %v", got, ok)
	}
	if empty, ok := Read(page, 6, 4); !ok || empty != (Readiness{}) {
		t.Fatalf("empty entry %+v", empty)
	}
	if _, ok := Read(page, MaxSockets, 4); ok {
		t.Fatal("entry past the table")
	}
	page[entryOffset(5)+offEntryReady] = 0xff
	if _, ok := Read(page, 5, 4); ok {
		t.Fatal("accepted an undefined ready bit")
	}
	*word(page, entryOffset(7)+offEntrySequence) = 1
	if _, ok := Read(page, 7, 4); ok {
		t.Fatal("accepted an entry that never settles")
	}
}

func TestReadinessToken(t *testing.T) {
	page := make([]byte, pageSize)
	InitReadiness(page, 1)
	if !ClaimToken(page) || ClaimToken(page) {
		t.Fatal("token claimed twice")
	}
	TakeToken(page)
	if !ClaimToken(page) {
		t.Fatal("token not claimable after the kernel took it")
	}
	DropToken(page)
	if !ClaimToken(page) {
		t.Fatal("token not claimable after a failed send")
	}
}

// The kernel reads entries while the service rewrites them; a read never
// returns a mix of two states.
func TestReadinessReadsAreConsistent(t *testing.T) {
	page := make([]byte, pageSize)
	InitReadiness(page, 1)
	var group sync.WaitGroup
	group.Add(1)
	go func() {
		defer group.Done()
		for round := uint32(1); round <= 20000; round++ {
			Publish(page, 0, Readiness{Socket: uint64(round), Readable: round, Ready: Ready(round) & allReady})
		}
	}()
	for reads := 0; reads < 20000; reads++ {
		if state, ok := Read(page, 0, 1000); ok && (uint64(state.Readable) != state.Socket || state.Ready != Ready(state.Readable)&allReady) {
			t.Fatalf("torn read %+v", state)
		}
	}
	group.Wait()
}
