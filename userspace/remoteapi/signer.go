package remoteapi

import (
	"encoding/binary"
	"goos/kernel/ipc"
)

const (
	SignerProtocolID = uint64(0x47485349474e3100) // GHSIGN1
	SignerVersion    = uint16(1)
	signRequestSize  = 48
	signReplySize    = 80
)

// Data is the unchanged SHA-256 H supplied by the pinned SSH KEX.
type SignRequest struct {
	Generation uint64
	Data       [32]byte
}
type SignReply struct {
	Generation uint64
	Result     Result
	Signature  [64]byte
}

func EncodeSignRequest(request *SignRequest, correlation uint64, out *ipc.Message) bool {
	if request == nil || out == nil || correlation == 0 || request.Generation == 0 {
		return false
	}
	*out = ipc.Message{ProtocolID: SignerProtocolID, Version: SignerVersion, Kind: ipc.KindRequest, Correlation: correlation, PayloadSize: signRequestSize}
	out.Payload[0] = 1
	binary.LittleEndian.PutUint64(out.Payload[8:16], request.Generation)
	copy(out.Payload[16:48], request.Data[:])
	return true
}
func DecodeSignRequest(message *ipc.Message, out *SignRequest) bool {
	if message == nil || out == nil || message.ProtocolID != SignerProtocolID || message.Version != SignerVersion || message.Kind != ipc.KindRequest || message.Correlation == 0 || message.HandleCount != 0 || message.PayloadSize != signRequestSize || message.Payload[0] != 1 || !zero(message.Payload[1:8]) {
		return false
	}
	candidate := SignRequest{Generation: binary.LittleEndian.Uint64(message.Payload[8:16])}
	if candidate.Generation == 0 {
		return false
	}
	copy(candidate.Data[:], message.Payload[16:48])
	*out = candidate
	return true
}
func EncodeSignReply(reply *SignReply, correlation uint64, out *ipc.Message) bool {
	if reply == nil || out == nil || correlation == 0 || reply.Generation == 0 || reply.Result > ResultPermissionDenied || reply.Result != ResultOK && !zero(reply.Signature[:]) {
		return false
	}
	*out = ipc.Message{ProtocolID: SignerProtocolID, Version: SignerVersion, Kind: ipc.KindReply, Correlation: correlation, PayloadSize: signReplySize}
	out.Payload[0], out.Payload[1] = 1, byte(reply.Result)
	binary.LittleEndian.PutUint64(out.Payload[8:16], reply.Generation)
	copy(out.Payload[16:80], reply.Signature[:])
	return true
}
func DecodeSignReply(message *ipc.Message, correlation, generation uint64, out *SignReply) bool {
	if message == nil || out == nil || correlation == 0 || generation == 0 || message.ProtocolID != SignerProtocolID || message.Version != SignerVersion || message.Kind != ipc.KindReply || message.Correlation != correlation || message.HandleCount != 0 || message.PayloadSize != signReplySize || message.Payload[0] != 1 || message.Payload[1] > byte(ResultPermissionDenied) || !zero(message.Payload[2:8]) {
		return false
	}
	candidate := SignReply{Generation: binary.LittleEndian.Uint64(message.Payload[8:16]), Result: Result(message.Payload[1])}
	if candidate.Generation != generation {
		return false
	}
	copy(candidate.Signature[:], message.Payload[16:80])
	if candidate.Result != ResultOK && !zero(candidate.Signature[:]) {
		return false
	}
	*out = candidate
	return true
}
