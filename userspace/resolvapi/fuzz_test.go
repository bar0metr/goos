package resolvapi

import (
	"goos/kernel/ipc"
	"testing"
)

// FuzzDecodeRequest feeds the variable-length request body through the
// decoder, which must never panic and must hand on only names it could
// normalize.
func FuzzDecodeRequest(f *testing.F) {
	f.Add(byte(OpResolve), []byte("example.test"))
	f.Add(byte(OpResolve), []byte("LOCALHOST."))
	f.Add(byte(OpStatus), []byte{})
	f.Add(byte(OpResolve), make([]byte, 254))
	f.Fuzz(func(t *testing.T, opcode byte, name []byte) {
		if len(name) > ipc.MaxPayload-requestHeader {
			name = name[:ipc.MaxPayload-requestHeader]
		}
		message := ipc.Message{ProtocolID: ProtocolID, Version: Version, Kind: ipc.KindRequest, Correlation: 1,
			PayloadSize: uint16(requestHeader + len(name))}
		message.Payload[0], message.Payload[1] = opcode, byte(len(name))
		copy(message.Payload[requestHeader:], name)
		var request Request
		if _, ok := DecodeRequest(&message, &request); ok && request.Name.Size != 0 {
			var again ipc.Message
			if !EncodeRequest(&request, 1, &again) {
				t.Fatalf("decoded request does not encode: %q", request.Name.Bytes())
			}
		}
	})
}
