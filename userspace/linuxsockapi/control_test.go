package linuxsockapi

import (
	"testing"

	"goos/kernel/handle"
	"goos/kernel/ipc"
)

func validControlRequests() []struct {
	request ControlRequest
	handles []handle.ID
} {
	return []struct {
		request ControlRequest
		handles []handle.ID
	}{
		{ControlRequest{Opcode: ControlAddFamily, Session: 7, Token: 99}, []handle.ID{11, 12}},
		{ControlRequest{Opcode: ControlRemoveFamily, Token: 99}, nil},
		{ControlRequest{Opcode: ControlSetTCP, Generation: 3}, []handle.ID{21, 22, 23}},
		{ControlRequest{Opcode: ControlSetUDP, Generation: 4}, []handle.ID{31, 32, 33}},
		{ControlRequest{Opcode: ControlStatus}, nil},
	}
}

func TestControlRequestsRoundTrip(t *testing.T) {
	for _, test := range validControlRequests() {
		var message ipc.Message
		if !EncodeControlRequest(&test.request, test.handles, 5, &message) {
			t.Fatalf("encode %+v", test.request)
		}
		var decoded ControlRequest
		got := roundTrip(t, &message)
		correlation, ok := DecodeControlRequest(got, &decoded)
		if !ok || correlation != 5 || decoded != test.request || int(got.HandleCount) != len(test.handles) {
			t.Fatalf("decode %+v: %+v %v", test.request, decoded, ok)
		}
		for index, id := range test.handles {
			if got.Handles[index] != id {
				t.Fatalf("handle %d of %+v", index, test.request)
			}
		}
		if op, attributed, known := ControlRequestOpcode(got); !known || op != test.request.Opcode || attributed != 5 {
			t.Fatalf("opcode of %+v", test.request)
		}
	}
}

func TestControlRequestsRejected(t *testing.T) {
	bad := []struct {
		request ControlRequest
		handles []handle.ID
	}{
		{ControlRequest{Opcode: ControlAddFamily, Token: 99}, []handle.ID{11, 12}},
		{ControlRequest{Opcode: ControlAddFamily, Session: 7}, []handle.ID{11, 12}},
		{ControlRequest{Opcode: ControlAddFamily, Session: 7, Token: 99}, []handle.ID{11}},
		{ControlRequest{Opcode: ControlAddFamily, Session: 7, Token: 99}, []handle.ID{11, 0}},
		{ControlRequest{Opcode: ControlRemoveFamily}, nil},
		{ControlRequest{Opcode: ControlRemoveFamily, Token: 1, Generation: 1}, nil},
		{ControlRequest{Opcode: ControlSetTCP}, []handle.ID{1, 2, 3}},
		{ControlRequest{Opcode: ControlSetUDP, Generation: 1, Token: 1}, []handle.ID{1, 2, 3}},
		{ControlRequest{Opcode: ControlSetUDP, Generation: 1}, []handle.ID{1, 2}},
		{ControlRequest{Opcode: ControlStatus, Token: 1}, nil},
		{ControlRequest{Opcode: lastControlOpcode + 1}, nil},
		{ControlRequest{}, nil},
	}
	for _, test := range bad {
		var message ipc.Message
		if EncodeControlRequest(&test.request, test.handles, 1, &message) {
			t.Fatalf("encoded %+v with %d handles", test.request, len(test.handles))
		}
	}
	var message ipc.Message
	request := ControlRequest{Opcode: ControlStatus}
	if EncodeControlRequest(&request, nil, 0, &message) {
		t.Fatal("encoded a zero correlation")
	}
	EncodeControlRequest(&request, nil, 1, &message)
	for _, mutate := range []func(*ipc.Message){
		func(m *ipc.Message) { m.Payload[1] = 1 },
		func(m *ipc.Message) { m.Payload[ControlRequestSize-1] = 1 },
		func(m *ipc.Message) { m.HandleCount = 1; m.Handles[0] = 5 },
		func(m *ipc.Message) { m.Version++ },
		func(m *ipc.Message) { m.Kind = ipc.KindNotification },
		func(m *ipc.Message) { m.Correlation = 0 },
		func(m *ipc.Message) { m.PayloadSize-- },
	} {
		copy := message
		mutate(&copy)
		var decoded ControlRequest
		if _, ok := DecodeControlRequest(&copy, &decoded); ok {
			t.Fatalf("decoded a malformed request %+v", copy)
		}
	}
}

func TestControlRepliesRoundTrip(t *testing.T) {
	replies := []ControlReply{
		{Opcode: ControlAddFamily, Token: 99},
		{Opcode: ControlAddFamily, Result: ControlBusy, Token: 99},
		{Opcode: ControlRemoveFamily, Result: ControlNotFound, Token: 1},
		{Opcode: ControlSetTCP, Generation: 3},
		{Opcode: ControlSetUDP, Result: ControlStale, Generation: 3},
		{Opcode: ControlStatus, Families: 2, Sockets: 700, SocketLimit: 1024, FamilyLimit: 512, FamilyReserve: 32,
			FamilySockets: [StatusFamilies]uint16{0: 512, 3: 188}, Backends: BackendTCP | BackendUDP,
			Counters: Counters{Opens: 1, Closes: 2, Connects: 3, Accepts: 4, BytesIn: 5, BytesOut: 6, DatagramsIn: 7,
				DatagramsOut: 8, WouldBlock: 9, NativeFailures: 10, SessionFailures: 11, BackendLosses: 12,
				Notifications: 13, DeferredNotifications: 14}},
		{Opcode: ControlStatus, Result: ControlInvalidArgument},
	}
	for _, reply := range replies {
		var message ipc.Message
		if !EncodeControlReply(&reply, 8, &message) {
			t.Fatalf("encode %+v", reply)
		}
		var decoded ControlReply
		if !DecodeControlReply(roundTrip(t, &message), 8, reply.Opcode, &decoded) || decoded != reply {
			t.Fatalf("decode %+v: %+v", reply, decoded)
		}
		if DecodeControlReply(&message, 9, reply.Opcode, &decoded) {
			t.Fatal("accepted another correlation")
		}
	}
	for _, reply := range []ControlReply{
		{Opcode: ControlAddFamily, Families: 1},
		{Opcode: ControlSetTCP, Token: 1},
		{Opcode: ControlStatus, Result: ControlBusy, Sockets: 1},
		{Opcode: ControlRemoveFamily, Token: 1, FamilySockets: [StatusFamilies]uint16{2: 1}},
		{Opcode: ControlStatus, Backends: 4},
		{Opcode: ControlStatus, Result: lastControlResult + 1},
		{Opcode: 0},
	} {
		var message ipc.Message
		if EncodeControlReply(&reply, 1, &message) {
			t.Fatalf("encoded %+v", reply)
		}
	}
}

func FuzzDecodeControl(f *testing.F) {
	for _, test := range validControlRequests() {
		var message ipc.Message
		EncodeControlRequest(&test.request, test.handles, 1, &message)
		f.Add(message.Payload[:ControlRequestSize], uint8(message.Kind), uint8(message.HandleCount))
	}
	status := ControlReply{Opcode: ControlStatus, Families: 1, Counters: Counters{Opens: 3}}
	var message ipc.Message
	EncodeControlReply(&status, 1, &message)
	f.Add(message.Payload[:ControlReplySize], uint8(message.Kind), uint8(0))
	f.Fuzz(func(t *testing.T, payload []byte, kind uint8, handles uint8) {
		if len(payload) > ipc.MaxPayload || handles > ipc.MaxAttachedHandles {
			return
		}
		var message ipc.Message
		message.ProtocolID, message.Version, message.Kind, message.Correlation = ControlProtocolID, ControlVersion, ipc.Kind(kind), 1
		message.PayloadSize = uint16(copy(message.Payload[:], payload))
		message.HandleCount = handles
		for index := range message.Handles[:handles] {
			message.Handles[index] = handle.ID(index + 1)
		}
		var request ControlRequest
		if got, ok := DecodeControlRequest(&message, &request); ok {
			var encoded ipc.Message
			if !EncodeControlRequest(&request, message.Handles[:handles], got, &encoded) || encoded.Payload != message.Payload {
				t.Fatalf("request %+v does not encode back", request)
			}
		}
		var reply ControlReply
		if len(payload) > 0 && DecodeControlReply(&message, 1, ControlOpcode(payload[0]), &reply) {
			var encoded ipc.Message
			if !EncodeControlReply(&reply, 1, &encoded) || encoded.Payload != message.Payload {
				t.Fatalf("reply %+v does not encode back", reply)
			}
		}
	})
}
