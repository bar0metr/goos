package linuxsockapi

import (
	"testing"

	"goos/kernel/ipc"
)

func FuzzDecode(f *testing.F) {
	for _, request := range validRequests() {
		correlation := uint64(1)
		if notification(request.Opcode) {
			correlation = 0
		}
		var message ipc.Message
		EncodeRequest(&request, correlation, &message)
		f.Add(message.Payload[:RequestSize], uint8(message.Kind), correlation)
	}
	for _, reply := range validReplies() {
		var message ipc.Message
		EncodeReply(&reply, 1, &message)
		f.Add(message.Payload[:ReplySize], uint8(message.Kind), uint64(1))
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

func FuzzReadiness(f *testing.F) {
	f.Add(make([]byte, ReadinessBytes), 0)
	f.Fuzz(func(t *testing.T, page []byte, index int) {
		if state, ok := Read(page, index, 2); ok && (state.Ready&^allReady != 0 || state.Error > lastResult) {
			t.Fatalf("accepted %+v", state)
		}
	})
}
