package udpapi

import (
	"testing"

	"goos/kernel/ipc"
)

// roundTrip passes a message through the wire codec, as a channel does.
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

var peer = [addressSize]byte{10, 0, 2, 2}

func validRequests() []Request {
	return []Request{
		{Opcode: OpOpen},
		{Opcode: OpBind, Endpoint: 0x101},
		{Opcode: OpBind, Endpoint: 0x101, Address: [4]byte{127, 0, 0, 1}, Port: 9000},
		{Opcode: OpConnect, Endpoint: 0x101, Address: peer, Port: 53},
		{Opcode: OpSend, Endpoint: 0x101, Slot: TXSlots - 1, Length: MaxPayload},
		{Opcode: OpSend, Endpoint: 0x101},
		{Opcode: OpSendTo, Endpoint: 0x101, Slot: 1, Length: 3, Address: peer, Port: 7},
		{Opcode: OpRecvFrom, Endpoint: 0x101},
		{Opcode: OpRelease, Slot: RXSlots - 1},
		{Opcode: OpClose, Endpoint: 0x101},
	}
}

func TestRequestRoundTrip(t *testing.T) {
	for _, request := range validRequests() {
		correlation := uint64(9)
		if request.Opcode == OpRelease {
			correlation = 0
		}
		var message ipc.Message
		if !EncodeRequest(&request, correlation, &message) {
			t.Fatalf("encode %+v", request)
		}
		var decoded Request
		got, ok := DecodeRequest(roundTrip(t, &message), &decoded)
		if !ok || got != correlation || decoded != request {
			t.Fatalf("decoded %+v %d %v from %+v", decoded, got, ok, request)
		}
	}
}

func TestRequestRefusesUnusedFields(t *testing.T) {
	for _, request := range []Request{
		{Opcode: 0},
		{Opcode: lastOpcode + 1, Endpoint: 1},
		{Opcode: OpOpen, Endpoint: 1},
		{Opcode: OpOpen, Port: 1},
		{Opcode: OpBind},
		{Opcode: OpBind, Endpoint: 1, Slot: 1},
		{Opcode: OpConnect, Endpoint: 1, Port: 53},
		{Opcode: OpConnect, Endpoint: 1, Address: peer},
		{Opcode: OpSend, Endpoint: 1, Slot: TXSlots},
		{Opcode: OpSend, Endpoint: 1, Length: MaxPayload + 1},
		{Opcode: OpSend, Endpoint: 1, Address: peer, Port: 7},
		{Opcode: OpSendTo, Endpoint: 1, Address: peer},
		{Opcode: OpRecvFrom, Endpoint: 1, Length: 1},
		{Opcode: OpRecvFrom},
		{Opcode: OpRelease, Slot: RXSlots},
		{Opcode: OpRelease, Endpoint: 1},
		{Opcode: OpClose, Endpoint: 1, Slot: 1},
	} {
		var message ipc.Message
		if EncodeRequest(&request, 1, &message) || EncodeRequest(&request, 0, &message) {
			t.Errorf("encoded %+v", request)
		}
	}
}

func TestRequestEnvelope(t *testing.T) {
	send := Request{Opcode: OpSend, Endpoint: 1}
	var message ipc.Message
	if EncodeRequest(&send, 0, &message) {
		t.Fatal("request without a correlation")
	}
	release := Request{Opcode: OpRelease}
	if EncodeRequest(&release, 3, &message) {
		t.Fatal("release with a correlation")
	}
	EncodeRequest(&send, 3, &message)
	var decoded Request
	for name, mutate := range map[string]func(*ipc.Message){
		"protocol":      func(m *ipc.Message) { m.ProtocolID++ },
		"version":       func(m *ipc.Message) { m.Version++ },
		"kind":          func(m *ipc.Message) { m.Kind = ipc.KindReply },
		"correlation":   func(m *ipc.Message) { m.Correlation = 0 },
		"size":          func(m *ipc.Message) { m.PayloadSize++ },
		"handles":       func(m *ipc.Message) { m.HandleCount, m.Handles[0] = 1, 5 },
		"reserved":      func(m *ipc.Message) { m.Payload[requestReserved] = 1 },
		"reserved high": func(m *ipc.Message) { m.Payload[requestReserved+1] = 1 },
	} {
		copied := message
		mutate(&copied)
		if _, ok := DecodeRequest(&copied, &decoded); ok {
			t.Errorf("%s accepted", name)
		}
	}
	EncodeRequest(&release, 0, &message)
	message.Kind, message.Correlation = ipc.KindRequest, 4
	if _, ok := DecodeRequest(&message, &decoded); ok {
		t.Fatal("release as a request accepted")
	}
}

func validReplies() []Reply {
	local := [4]byte{10, 0, 2, 15}
	return []Reply{
		{Opcode: OpOpen, Endpoint: 0x101},
		{Opcode: OpOpen, Result: ResultBusy},
		{Opcode: OpBind, Endpoint: 0x101, Port: 49152},
		{Opcode: OpBind, Endpoint: 0x101, Address: local, Port: 9000},
		{Opcode: OpBind, Endpoint: 0x101, Result: ResultAddressInUse},
		{Opcode: OpConnect, Endpoint: 0x101, Address: peer, Port: 53, Local: local, LocalPort: 50000},
		{Opcode: OpSend, Endpoint: 0x101, Slot: 2},
		{Opcode: OpSendTo, Endpoint: 0x101, Slot: 3, Result: ResultNoRoute},
		{Opcode: OpRecvFrom, Endpoint: 0x101, Slot: 7, Address: peer, Port: 53, Length: 0, Local: local},
		{Opcode: OpRecvFrom, Endpoint: 0x101, Result: ResultClosed},
		{Opcode: OpClose, Endpoint: 0x101},
	}
}

func TestReplyRoundTrip(t *testing.T) {
	for _, reply := range validReplies() {
		var message ipc.Message
		if !EncodeReply(&reply, 11, &message) {
			t.Fatalf("encode %+v", reply)
		}
		var decoded Reply
		got, ok := DecodeReply(roundTrip(t, &message), &decoded)
		if !ok || got != 11 || decoded != reply {
			t.Fatalf("decoded %+v from %+v", decoded, reply)
		}
	}
}

func TestReplyRefusesUnusedFields(t *testing.T) {
	for _, reply := range []Reply{
		{Opcode: OpRelease, Endpoint: 1},
		{Opcode: OpOpen},
		{Opcode: OpOpen, Endpoint: 1, Port: 1},
		{Opcode: OpBind, Endpoint: 1},
		{Opcode: OpBind, Endpoint: 1, Result: ResultBusy, Port: 1},
		{Opcode: OpConnect, Endpoint: 1, Address: peer, Port: 53},
		{Opcode: OpSend, Endpoint: 1, Slot: TXSlots},
		{Opcode: OpSend, Endpoint: 1, Length: 1},
		{Opcode: OpRecvFrom, Endpoint: 1, Slot: RXSlots, Address: peer, Local: peer},
		{Opcode: OpRecvFrom, Endpoint: 1, Address: peer, Local: peer, LocalPort: 1},
		{Opcode: OpRecvFrom, Endpoint: 1, Address: peer, Local: peer, Length: MaxPayload + 1},
		{Opcode: OpRecvFrom, Endpoint: 1, Result: ResultClosed, Slot: 1},
		{Opcode: OpClose, Endpoint: 1, Result: ResultClosed},
		{Opcode: OpClose, Endpoint: 1, Result: lastResult + 1},
	} {
		var message ipc.Message
		if EncodeReply(&reply, 1, &message) {
			t.Errorf("encoded %+v", reply)
		}
	}
	good := validReplies()[0]
	var message ipc.Message
	if EncodeReply(&good, 0, &message) {
		t.Fatal("reply without a correlation")
	}
	EncodeReply(&good, 5, &message)
	message.Payload[replyTail] = 1
	var decoded Reply
	if _, ok := DecodeReply(&message, &decoded); ok {
		t.Fatal("nonzero tail accepted")
	}
}

func TestSlots(t *testing.T) {
	var slots Slots
	slots.Reset(RXSlots)
	for index := uint8(0); index < RXSlots; index++ {
		slot, ok := slots.Free()
		if !ok || slot != index || !slots.Lend(slot) {
			t.Fatalf("slot %d", index)
		}
	}
	if _, ok := slots.Free(); ok {
		t.Fatal("free slot past the pool")
	}
	if slots.Lend(3) || slots.Lend(RXSlots) {
		t.Fatal("lent twice or past the pool")
	}
	if !slots.Return(3) || slots.Return(3) || slots.Return(RXSlots) {
		t.Fatal("return checks")
	}
	if slots.Lent(3) || !slots.Lent(4) {
		t.Fatal("ownership")
	}
	if slot, ok := slots.Free(); !ok || slot != 3 {
		t.Fatal("returned slot not free")
	}
}

func TestPoolGeometry(t *testing.T) {
	if TXPoolPages*pageSize != TXSlots*SlotSize || RXPoolPages*pageSize != RXSlots*SlotSize || SlotSize < MaxPayload {
		t.Fatal("pool sizes")
	}
	if Offset(RXSlots-1)+SlotSize != RXPoolPages*pageSize {
		t.Fatal("last slot past the pool")
	}
}
