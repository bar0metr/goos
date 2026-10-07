package tcpapi

import (
	"testing"

	"goos/kernel/ipc"
)

func FuzzDecode(f *testing.F) {
	requests := []Request{
		{Opcode: OpOpen},
		{Opcode: OpBind, Endpoint: 1, Address: [4]byte{127, 0, 0, 1}, Port: 80},
		{Opcode: OpListen, Endpoint: 1, Backlog: 2},
		{Opcode: OpAccept, Endpoint: 1},
		{Opcode: OpConnect, Endpoint: 1, Address: [4]byte{192, 0, 2, 1}, Port: 7},
		{Opcode: OpWrite, Endpoint: 1, Slot: 0, Length: SlotSize},
		{Opcode: OpRead, Endpoint: 1, Length: SlotSize},
		{Opcode: OpShutdownWrite, Endpoint: 1},
		{Opcode: OpClose, Endpoint: 1},
		{Opcode: OpRelease, Slot: 0},
	}
	for _, request := range requests {
		correlation := uint64(1)
		if request.Opcode == OpRelease {
			correlation = 0
		}
		var message ipc.Message
		EncodeRequest(&request, correlation, &message)
		f.Add(message.Payload[:RequestSize], uint8(message.Kind), correlation)
	}
	f.Fuzz(func(t *testing.T, payload []byte, kind uint8, correlation uint64) {
		if len(payload) > ipc.MaxPayload {
			return
		}
		var message ipc.Message
		message.ProtocolID, message.Version, message.Kind, message.Correlation = ProtocolID, Version, ipc.Kind(kind), correlation
		message.PayloadSize = uint16(copy(message.Payload[:], payload))
		var request Request
		if got, ok := DecodeRequest(&message, &request); ok {
			var encoded ipc.Message
			if !EncodeRequest(&request, got, &encoded) || encoded.Payload != message.Payload {
				t.Fatalf("request %+v does not encode back", request)
			}
		}
		var reply Reply
		if got, ok := DecodeReply(&message, &reply); ok {
			var encoded ipc.Message
			if !EncodeReply(&reply, got, &encoded) || encoded.Payload != message.Payload {
				t.Fatalf("reply %+v does not encode back", reply)
			}
		}
	})
}
