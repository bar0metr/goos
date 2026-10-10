package remoteapi

import (
	"encoding/binary"
	"goos/kernel/handle"
	"goos/kernel/ipc"
)

const WorkerProtocolID = uint64(0x47574f524b455231) // GWORKER1
const WorkerVersion = uint16(3)

type WorkerOperation uint8

const (
	WorkerLaunch WorkerOperation = iota + 1
	WorkerClose
	WorkerAuthority
)

// WorkerRequest is accepted only on the listener's private PID 1 endpoint.
// Launch moves policy memory. PID 1 creates ciphertext streams and returns the
// listener's input writer and output reader in the successful launch reply.
// The executable, arguments, grants, and worker endpoint are chosen by PID 1.
// Authority paths must match PID 1's current typed configuration.
type WorkerRequest struct {
	Operation                WorkerOperation
	Worker                   uint64
	HostPath, AuthorizedKeys []byte
}
type WorkerReply struct {
	Operation WorkerOperation
	Result    Result
	Worker    uint64
}

func EncodeWorkerRequest(request *WorkerRequest, attachments []handle.ID, correlation uint64, out *ipc.Message) bool {
	count := 0
	switch request.Operation {
	case WorkerLaunch:
		if request.Worker != 0 {
			return false
		}
		count = 1
	case WorkerClose:
		if request.Worker == 0 {
			return false
		}
	case WorkerAuthority:
		if request.Worker != 0 || !ValidHostPath(request.HostPath) || !ValidAuthorityPath(request.AuthorizedKeys) {
			return false
		}
	default:
		return false
	}
	if correlation == 0 || len(attachments) != count || request.Operation != WorkerAuthority && (len(request.HostPath) != 0 || len(request.AuthorizedKeys) != 0) {
		return false
	}
	for i, id := range attachments {
		if id == 0 {
			return false
		}
		for _, old := range attachments[:i] {
			if old == id {
				return false
			}
		}
	}
	*out = ipc.Message{ProtocolID: WorkerProtocolID, Version: WorkerVersion, Kind: ipc.KindRequest, Correlation: correlation, PayloadSize: uint16(16 + len(request.HostPath) + len(request.AuthorizedKeys)), HandleCount: uint8(count)}
	out.Payload[0], out.Payload[1], out.Payload[2] = byte(request.Operation), byte(len(request.HostPath)), byte(len(request.AuthorizedKeys))
	binary.LittleEndian.PutUint64(out.Payload[8:16], request.Worker)
	copy(out.Payload[16:], request.HostPath)
	copy(out.Payload[16+len(request.HostPath):], request.AuthorizedKeys)
	copy(out.Handles[:], attachments)
	return true
}
func DecodeWorkerRequest(in *ipc.Message, out *WorkerRequest) bool {
	if in.ProtocolID != WorkerProtocolID || in.Version != WorkerVersion || in.Kind != ipc.KindRequest || in.Correlation == 0 || in.PayloadSize < 16 || in.PayloadSize > 144 || int(in.HandleCount) > len(in.Handles) || !zero(in.Payload[3:8]) {
		return false
	}
	hostSize, keySize := int(in.Payload[1]), int(in.Payload[2])
	if hostSize > 64 || keySize > 64 || 16+hostSize+keySize != int(in.PayloadSize) {
		return false
	}
	request := WorkerRequest{Operation: WorkerOperation(in.Payload[0]), Worker: binary.LittleEndian.Uint64(in.Payload[8:16]), HostPath: in.Payload[16 : 16+hostSize], AuthorizedKeys: in.Payload[16+hostSize : in.PayloadSize]}
	var canonical ipc.Message
	if !EncodeWorkerRequest(&request, in.Handles[:in.HandleCount], in.Correlation, &canonical) {
		return false
	}
	*out = request
	return true
}
func EncodeWorkerReply(reply *WorkerReply, correlation uint64, out *ipc.Message) bool {
	if correlation == 0 || reply.Operation < WorkerLaunch || reply.Operation > WorkerAuthority || reply.Result > ResultPermissionDenied || reply.Result == ResultOK && reply.Worker == 0 && reply.Operation != WorkerAuthority || reply.Operation == WorkerAuthority && reply.Worker != 0 {
		return false
	}
	*out = ipc.Message{ProtocolID: WorkerProtocolID, Version: WorkerVersion, Kind: ipc.KindReply, Correlation: correlation, PayloadSize: 16}
	out.Payload[0], out.Payload[1] = byte(reply.Operation), byte(reply.Result)
	binary.LittleEndian.PutUint64(out.Payload[8:16], reply.Worker)
	return true
}
func DecodeWorkerReply(in *ipc.Message, correlation uint64, operation WorkerOperation, out *WorkerReply) bool {
	if in.ProtocolID != WorkerProtocolID || in.Version != WorkerVersion || in.Kind != ipc.KindReply || correlation == 0 || in.Correlation != correlation || in.PayloadSize != 16 || !zero(in.Payload[2:8]) {
		return false
	}
	reply := WorkerReply{Operation: WorkerOperation(in.Payload[0]), Result: Result(in.Payload[1]), Worker: binary.LittleEndian.Uint64(in.Payload[8:16])}
	var canonical ipc.Message
	if reply.Operation != operation || !EncodeWorkerReply(&reply, correlation, &canonical) {
		return false
	}
	count := workerReplyHandleCount(&reply)
	if int(in.HandleCount) != count {
		return false
	}
	if !validWorkerReplyHandles(in.Handles[:count]) {
		return false
	}
	*out = reply
	return true
}

// ValidHostPath confines private host-key storage to persistent state.
func ValidHostPath(path []byte) bool {
	return ValidAuthorityPath(path) && len(path) > len("/state/") && string(path[:len("/state/")]) == "/state/"
}

func ValidAuthorityPath(path []byte) bool {
	if len(path) < 2 || len(path) > 64 || path[0] != '/' || path[len(path)-1] == '/' {
		return false
	}
	start := 1
	for index := 1; index <= len(path); index++ {
		if index < len(path) && path[index] != '/' {
			if path[index] < 0x21 || path[index] > 0x7e || path[index] == '\\' {
				return false
			}
			continue
		}
		component := path[start:index]
		if len(component) == 0 || string(component) == "." || string(component) == ".." {
			return false
		}
		start = index + 1
	}
	return true
}
func EncodeWorkerAuthorityReply(reply *WorkerReply, attachments []handle.ID, correlation uint64, out *ipc.Message) bool {
	return reply.Operation == WorkerAuthority && reply.Worker == 0 && encodeWorkerGrantReply(reply, attachments, correlation, out)
}

func EncodeWorkerLaunchReply(reply *WorkerReply, attachments []handle.ID, correlation uint64, out *ipc.Message) bool {
	return reply.Operation == WorkerLaunch && encodeWorkerGrantReply(reply, attachments, correlation, out)
}

func encodeWorkerGrantReply(reply *WorkerReply, attachments []handle.ID, correlation uint64, out *ipc.Message) bool {
	if !EncodeWorkerReply(reply, correlation, out) {
		return false
	}
	count := workerReplyHandleCount(reply)
	if len(attachments) != count {
		return false
	}
	if !validWorkerReplyHandles(attachments) {
		return false
	}
	out.HandleCount = uint8(count)
	copy(out.Handles[:], attachments)
	return true
}

func workerReplyHandleCount(reply *WorkerReply) int {
	if reply.Result != ResultOK {
		return 0
	}
	switch reply.Operation {
	case WorkerLaunch:
		return 3
	case WorkerAuthority:
		return 2
	}
	return 0
}
func validWorkerReplyHandles(attachments []handle.ID) bool {
	for i, id := range attachments {
		if id == 0 {
			return false
		}
		for _, old := range attachments[:i] {
			if id == old {
				return false
			}
		}
	}
	return true
}
