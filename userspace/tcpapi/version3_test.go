package tcpapi

import (
	"testing"

	"goos/kernel/ipc"
)

func TestLayouts(t *testing.T) {
	if Layout3.TXPool() != 128<<10 || Layout3.RXPool() != 128<<10 || Layout3.TXPoolPages() != 32 || Layout3.Offset(7) != 7*16384 {
		t.Fatalf("GTCP 3 pools %+v", Layout3)
	}
	if Layout2.TXPoolPages() != TXPoolPages || Layout2.RXPoolPages() != RXPoolPages || Layout2.Offset(3) != Offset(3) {
		t.Fatalf("GTCP 2 pools %+v", Layout2)
	}
	if Layout3.TXSlots > 8 || Layout3.RXSlots > 8 || PipelinedWrites3*SlotSize3 < 64<<10 {
		t.Fatal("GTCP 3 slots")
	}
	for version, want := range map[uint16]bool{1: false, Version: true, Version3: true, Version4: true, 5: false} {
		if _, ok := LayoutFor(version); ok != want {
			t.Fatalf("LayoutFor(%d) %v", version, ok)
		}
	}
}

func TestVersion3RequestRoundTrip(t *testing.T) {
	for _, request := range []Request{
		{Opcode: OpWrite, Endpoint: 1, Slot: TXSlots3 - 1, Length: SlotSize3, Flags: FlagNoWait, Offset: 0xfffffff0},
		{Opcode: OpWrite, Endpoint: 1, Slot: 4, Length: 1},
		{Opcode: OpRead, Endpoint: 1, Length: SlotSize3},
		{Opcode: OpRead, Endpoint: 1, Length: 1, Flags: FlagNoWait | FlagPeek},
		{Opcode: OpRelease, Slot: RXSlots3 - 1},
		{Opcode: OpWaitReady, Endpoint: 1, Mask: ReadyReadable},
		{Opcode: OpConnect, Endpoint: 1, Address: [4]byte{10, 0, 0, 1}, Port: 80, Flags: FlagNoWait},
	} {
		correlation := uint64(3)
		if request.Opcode == OpRelease {
			correlation = 0
		}
		var message ipc.Message
		if !Layout3.EncodeRequest(&request, correlation, &message) || message.Version != Version3 || message.PayloadSize != RequestSize3 {
			t.Fatalf("encode %+v", request)
		}
		back := wireRoundTrip(t, &message)
		var decoded Request
		if got, ok := Layout3.DecodeRequest(&back, &decoded); !ok || got != correlation || decoded != request {
			t.Fatalf("decode %+v: %+v %v", request, decoded, ok)
		}
		if _, ok := DecodeRequest(&back, &decoded); ok {
			t.Fatalf("GTCP 2 decoded a GTCP 3 request %+v", request)
		}
	}
}

func TestVersion3RequestRejects(t *testing.T) {
	for _, request := range []Request{
		{Opcode: OpWrite, Endpoint: 1, Slot: TXSlots3, Length: 1},
		{Opcode: OpWrite, Endpoint: 1, Length: SlotSize3 + 1},
		{Opcode: OpRead, Endpoint: 1, Length: SlotSize3 + 1},
		{Opcode: OpRead, Endpoint: 1},
		{Opcode: OpRelease, Slot: RXSlots3},
		{Opcode: OpRead, Endpoint: 1, Length: 1, Offset: 1},
		{Opcode: OpClose, Endpoint: 1, Offset: 1},
	} {
		var message ipc.Message
		if Layout3.EncodeRequest(&request, 1, &message) || Layout3.EncodeRequest(&request, 0, &message) {
			t.Fatalf("encoded %+v", request)
		}
	}
	// GTCP 2 carries no offset and keeps its 4 KiB slots.
	for _, request := range []Request{
		{Opcode: OpWrite, Endpoint: 1, Length: 1, Offset: 1},
		{Opcode: OpWrite, Endpoint: 1, Length: SlotSize + 1},
		{Opcode: OpWrite, Endpoint: 1, Slot: TXSlots, Length: 1},
		{Opcode: OpRead, Endpoint: 1, Length: SlotSize + 1},
	} {
		var message ipc.Message
		if EncodeRequest(&request, 1, &message) {
			t.Fatalf("GTCP 2 encoded %+v", request)
		}
	}
	var forged Layout
	if forged.EncodeRequest(&Request{Opcode: OpOpen}, 1, &ipc.Message{}) {
		t.Fatal("zero layout encoded")
	}
	bigger := Layout3
	bigger.SlotSize = 1 << 15
	if bigger.EncodeRequest(&Request{Opcode: OpRead, Endpoint: 1, Length: 1 << 15}, 1, &ipc.Message{}) {
		t.Fatal("forged layout encoded")
	}

	var decodedRelease Request
	valid := Request{Opcode: OpWrite, Endpoint: 1, Slot: 2, Length: 100, Offset: 77}
	for name, mutate := range map[string]func(*ipc.Message){
		"version 2":    func(m *ipc.Message) { m.Version = Version },
		"version 4":    func(m *ipc.Message) { m.Version = 4 },
		"short":        func(m *ipc.Message) { m.PayloadSize = RequestSize },
		"long":         func(m *ipc.Message) { m.PayloadSize = RequestSize3 + 1 },
		"reserved 3":   func(m *ipc.Message) { m.Payload[3] = 1 },
		"reserved 23":  func(m *ipc.Message) { m.Payload[23] = 1 },
		"slot":         func(m *ipc.Message) { m.Payload[1] = TXSlots3 },
		"length":       func(m *ipc.Message) { m.Payload[14], m.Payload[15] = 0x01, 0x40 },
		"notification": func(m *ipc.Message) { m.Kind, m.Correlation = ipc.KindNotification, 0 },
	} {
		var message ipc.Message
		Layout3.EncodeRequest(&valid, 5, &message)
		mutate(&message)
		var decoded Request
		if _, ok := Layout3.DecodeRequest(&message, &decoded); ok {
			t.Fatalf("accepted %s", name)
		}
	}
	// A request carries the RX slots it returns; a GTCP 2 one cannot, and
	// a Release names its slot in Slot.
	carrying := Request{Opcode: OpRead, Endpoint: 1, Length: 100, Release: 0x81}
	var carried ipc.Message
	if !Layout3.EncodeRequest(&carrying, 5, &carried) {
		t.Fatal("a read returning slots refused")
	}
	if _, ok := Layout3.DecodeRequest(&carried, &decodedRelease); !ok || decodedRelease != carrying {
		t.Fatalf("returned slots %+v", decodedRelease)
	}
	if Layout2.EncodeRequest(&Request{Opcode: OpRead, Endpoint: 1, Length: 1, Release: 1}, 5, &carried) {
		t.Fatal("GTCP 2 encoded returned slots")
	}
	if Layout3.EncodeRequest(&Request{Opcode: OpRelease, Slot: 1, Release: 1}, 0, &carried) {
		t.Fatal("a Release that returns more slots encoded")
	}
	var legacy ipc.Message
	EncodeRequest(&Request{Opcode: OpRead, Endpoint: 1, Length: 1}, 5, &legacy)
	legacy.Payload[2] = 1
	if _, ok := DecodeRequest(&legacy, &Request{}); ok {
		t.Fatal("GTCP 2 decoded returned slots")
	}
	// A GTCP 2 request fails a GTCP 3 decoder.
	var message ipc.Message
	EncodeRequest(&Request{Opcode: OpRead, Endpoint: 1, Length: 1}, 5, &message)
	if _, ok := Layout3.DecodeRequest(&message, &Request{}); ok {
		t.Fatal("GTCP 3 decoded a GTCP 2 request")
	}
}

func TestVersion3ReplyRoundTripAndRejects(t *testing.T) {
	for _, reply := range []Reply{
		{Opcode: OpRead, Endpoint: 1, Slot: RXSlots3 - 1, Length: SlotSize3},
		{Opcode: OpWrite, Endpoint: 1, Slot: TXSlots3 - 1, Length: SlotSize3},
		{Opcode: OpWrite, Result: ResultWouldBlock, Endpoint: 1, Slot: 5},
		{Opcode: OpWrite, Result: ResultInvalidArgument, Endpoint: 1, Slot: 5},
		{Opcode: OpReady, Endpoint: 1, Mask: ReadyWritable, Value: 3, Room: 9},
	} {
		var message ipc.Message
		if !Layout3.EncodeReply(&reply, 9, &message) || message.Version != Version3 {
			t.Fatalf("encode %+v", reply)
		}
		back := wireRoundTrip(t, &message)
		var decoded Reply
		if got, ok := Layout3.DecodeReply(&back, &decoded); !ok || got != 9 || decoded != reply {
			t.Fatalf("decode %+v: %+v", reply, decoded)
		}
		if _, ok := DecodeReply(&back, &decoded); ok {
			t.Fatalf("GTCP 2 decoded a GTCP 3 reply %+v", reply)
		}
	}
	for _, reply := range []Reply{
		{Opcode: OpRead, Endpoint: 1, Slot: RXSlots3, Length: 1},
		{Opcode: OpRead, Endpoint: 1, Length: SlotSize3 + 1},
		{Opcode: OpWrite, Endpoint: 1, Slot: TXSlots3, Length: 1},
		{Opcode: OpWrite, Endpoint: 1, Length: SlotSize3 + 1},
	} {
		var message ipc.Message
		if Layout3.EncodeReply(&reply, 9, &message) {
			t.Fatalf("encoded %+v", reply)
		}
	}
	var message ipc.Message
	if EncodeReply(&Reply{Opcode: OpRead, Endpoint: 1, Length: SlotSize + 1}, 9, &message) {
		t.Fatal("GTCP 2 reply past its slot")
	}
}

func FuzzDecode3(f *testing.F) {
	for _, request := range []Request{
		{Opcode: OpWrite, Endpoint: 1, Slot: 7, Length: SlotSize3, Flags: FlagNoWait, Offset: 1 << 20},
		{Opcode: OpRead, Endpoint: 1, Length: SlotSize3},
		{Opcode: OpOpen},
	} {
		var message ipc.Message
		Layout3.EncodeRequest(&request, 1, &message)
		f.Add(message.Payload[:RequestSize3], uint8(message.Kind), uint64(1))
	}
	var message ipc.Message
	Layout3.EncodeReply(&Reply{Opcode: OpRead, Endpoint: 1, Slot: 7, Length: SlotSize3}, 1, &message)
	f.Add(message.Payload[:ReplySize], uint8(ipc.KindReply), uint64(1))
	f.Fuzz(func(t *testing.T, payload []byte, kind uint8, correlation uint64) {
		if len(payload) > ipc.MaxPayload {
			return
		}
		var message ipc.Message
		message.ProtocolID, message.Version, message.Kind, message.Correlation = ProtocolID, Version3, ipc.Kind(kind), correlation
		message.PayloadSize = uint16(copy(message.Payload[:], payload))
		var request Request
		if got, ok := Layout3.DecodeRequest(&message, &request); ok {
			var encoded ipc.Message
			if !Layout3.EncodeRequest(&request, got, &encoded) || encoded.Payload != message.Payload {
				t.Fatalf("request %+v does not encode back", request)
			}
			if request.Slot >= TXSlots3 && request.Opcode == OpWrite || uint32(request.Length) > SlotSize3 {
				t.Fatalf("request %+v outside the pools", request)
			}
		}
		var reply Reply
		if got, ok := Layout3.DecodeReply(&message, &reply); ok {
			var encoded ipc.Message
			if !Layout3.EncodeReply(&reply, got, &encoded) || encoded.Payload != message.Payload {
				t.Fatalf("reply %+v does not encode back", reply)
			}
		}
	})
}
