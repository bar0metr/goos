package udpapi

import (
	"testing"

	"goos/kernel/ipc"
)

// FuzzDecode feeds arbitrary payloads and envelopes to both decoders. A
// decoded message must satisfy its opcode's rules and encode back to the
// same payload.
func FuzzDecode(f *testing.F) {
	for _, request := range validRequests() {
		var message ipc.Message
		correlation := uint64(1)
		if request.Opcode == OpRelease {
			correlation = 0
		}
		EncodeRequest(&request, correlation, &message)
		f.Add(message.Payload[:RequestSize], uint8(message.Kind), correlation)
	}
	for _, reply := range validReplies() {
		var message ipc.Message
		EncodeReply(&reply, 1, &message)
		f.Add(message.Payload[:ReplySize], uint8(ipc.KindReply), uint64(1))
	}
	f.Fuzz(func(t *testing.T, payload []byte, kind uint8, correlation uint64) {
		if len(payload) > ipc.MaxPayload {
			return
		}
		var in ipc.Message
		in.ProtocolID, in.Version, in.Kind, in.Correlation = ProtocolID, Version, ipc.Kind(kind), correlation
		in.PayloadSize = uint16(copy(in.Payload[:], payload))
		var request Request
		if got, ok := DecodeRequest(&in, &request); ok {
			var again ipc.Message
			if !EncodeRequest(&request, got, &again) || again.Payload != in.Payload {
				t.Fatalf("request %+v does not encode back", request)
			}
		}
		var reply Reply
		if got, ok := DecodeReply(&in, &reply); ok {
			var again ipc.Message
			if !EncodeReply(&reply, got, &again) || again.Payload != in.Payload {
				t.Fatalf("reply %+v does not encode back", reply)
			}
		}
	})
}
