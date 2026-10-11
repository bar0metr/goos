package config

import (
	"encoding/binary"
	"goos/kernel/ipc"
)

// GCF1 is the typed configuration protocol. Every request names an operation, a
// key and a typed value; there are no string commands and no untyped blobs, so
// a malformed message is rejected at the boundary rather than interpreted.
const (
	ProtocolID      = uint64(0x47434631)
	ProtocolVersion = uint16(2)
	requestHeader   = 32
	replyHeader     = 32
)

type Operation uint8

const (
	OperationGet Operation = iota + 1
	OperationSet
	OperationList
	OperationRevision
	// OperationSetDurable acknowledges persistence before publishing hostname.
	OperationSetDurable
)

type Request struct {
	Operation Operation
	Key       []byte
	Value     Value
	Cursor    uint32
}

type Reply struct {
	Operation Operation
	Status    Status
	Key       []byte
	Value     Value
	ReadOnly  bool
	Cursor    uint32
}

func EncodeRequest(request Request, correlation uint64) (ipc.Message, bool) {
	if request.Operation < OperationGet || request.Operation > OperationSetDurable || correlation == 0 {
		return ipc.Message{}, false
	}
	if len(request.Key) > MaxKey || int(request.Value.Size) > MaxText {
		return ipc.Message{}, false
	}
	if request.Operation != OperationList && request.Operation != OperationRevision && !validKey(request.Key) {
		return ipc.Message{}, false
	}
	if (request.Operation == OperationSet || request.Operation == OperationSetDurable) && !request.Value.valid() {
		return ipc.Message{}, false
	}
	if request.Operation == OperationRevision && !validRevisionRequest(&request) {
		return ipc.Message{}, false
	}
	payload := requestHeader + len(request.Key) + int(request.Value.Size)
	if payload > ipc.MaxPayload {
		return ipc.Message{}, false
	}
	message := ipc.Message{ProtocolID: ProtocolID, Version: ProtocolVersion, Kind: ipc.KindRequest, Correlation: correlation, PayloadSize: uint16(payload)}
	message.Payload[0] = byte(request.Operation)
	message.Payload[1] = byte(len(request.Key))
	message.Payload[2] = byte(request.Value.Kind)
	message.Payload[3] = request.Value.Size
	binary.LittleEndian.PutUint64(message.Payload[8:], request.Value.Uint)
	binary.LittleEndian.PutUint32(message.Payload[16:], request.Cursor)
	copy(message.Payload[requestHeader:], request.Key)
	copy(message.Payload[requestHeader+len(request.Key):], request.Value.Bytes())
	return message, true
}

func DecodeRequest(message ipc.Message) (Request, bool) {
	if message.ProtocolID != ProtocolID || message.Version != ProtocolVersion || message.Kind != ipc.KindRequest ||
		message.Correlation == 0 || message.HandleCount != 0 || message.PayloadSize < requestHeader {
		return Request{}, false
	}
	keySize, valueSize := int(message.Payload[1]), int(message.Payload[3])
	if message.Payload[4] != 0 || binary.LittleEndian.Uint32(message.Payload[4:]) != 0 ||
		binary.LittleEndian.Uint64(message.Payload[20:]) != 0 ||
		keySize > MaxKey || valueSize > MaxText || requestHeader+keySize+valueSize != int(message.PayloadSize) {
		return Request{}, false
	}
	operation := Operation(message.Payload[0])
	if operation < OperationGet || operation > OperationSetDurable {
		return Request{}, false
	}
	key := message.Payload[requestHeader : requestHeader+keySize]
	if operation != OperationList && operation != OperationRevision && !validKey(key) {
		return Request{}, false
	}
	request := Request{
		Operation: operation,
		Key:       key,
		Value:     Value{Kind: Kind(message.Payload[2]), Uint: binary.LittleEndian.Uint64(message.Payload[8:]), Size: uint8(valueSize)},
		Cursor:    binary.LittleEndian.Uint32(message.Payload[16:]),
	}
	copy(request.Value.Text[:], message.Payload[requestHeader+keySize:requestHeader+keySize+valueSize])
	if (operation == OperationSet || operation == OperationSetDurable) && !request.Value.valid() {
		return Request{}, false
	}
	if operation == OperationRevision && !validRevisionRequest(&request) {
		return Request{}, false
	}
	return request, true
}

func EncodeReply(reply Reply, correlation uint64) (ipc.Message, bool) {
	if reply.Operation < OperationGet || reply.Operation > OperationSetDurable || correlation == 0 ||
		len(reply.Key) > MaxKey || int(reply.Value.Size) > MaxText {
		return ipc.Message{}, false
	}
	if reply.Operation == OperationRevision && !validRevisionReply(&reply) {
		return ipc.Message{}, false
	}
	payload := replyHeader + len(reply.Key) + int(reply.Value.Size)
	if payload > ipc.MaxPayload {
		return ipc.Message{}, false
	}
	message := ipc.Message{ProtocolID: ProtocolID, Version: ProtocolVersion, Kind: ipc.KindReply, Correlation: correlation, PayloadSize: uint16(payload)}
	message.Payload[0] = byte(reply.Operation)
	message.Payload[1] = byte(reply.Status)
	message.Payload[2] = byte(len(reply.Key))
	message.Payload[3] = byte(reply.Value.Kind)
	message.Payload[4] = reply.Value.Size
	if reply.ReadOnly {
		message.Payload[5] = 1
	}
	binary.LittleEndian.PutUint64(message.Payload[8:], reply.Value.Uint)
	binary.LittleEndian.PutUint32(message.Payload[16:], reply.Cursor)
	copy(message.Payload[replyHeader:], reply.Key)
	copy(message.Payload[replyHeader+len(reply.Key):], reply.Value.Bytes())
	return message, true
}

func DecodeReply(message ipc.Message) (Reply, bool) {
	if message.ProtocolID != ProtocolID || message.Version != ProtocolVersion || message.Kind != ipc.KindReply ||
		message.Correlation == 0 || message.HandleCount != 0 || message.PayloadSize < replyHeader {
		return Reply{}, false
	}
	keySize, valueSize := int(message.Payload[2]), int(message.Payload[4])
	if message.Payload[5] > 1 || message.Payload[6] != 0 || message.Payload[7] != 0 ||
		binary.LittleEndian.Uint64(message.Payload[20:]) != 0 ||
		keySize > MaxKey || valueSize > MaxText || replyHeader+keySize+valueSize != int(message.PayloadSize) {
		return Reply{}, false
	}
	operation := Operation(message.Payload[0])
	if operation < OperationGet || operation > OperationSetDurable || Status(message.Payload[1]) > StatusPersistUnknown {
		return Reply{}, false
	}
	reply := Reply{
		Operation: operation,
		Status:    Status(message.Payload[1]),
		Key:       message.Payload[replyHeader : replyHeader+keySize],
		Value:     Value{Kind: Kind(message.Payload[3]), Uint: binary.LittleEndian.Uint64(message.Payload[8:]), Size: uint8(valueSize)},
		ReadOnly:  message.Payload[5] == 1,
		Cursor:    binary.LittleEndian.Uint32(message.Payload[16:]),
	}
	copy(reply.Value.Text[:], message.Payload[replyHeader+keySize:replyHeader+keySize+valueSize])
	if operation == OperationRevision && !validRevisionReply(&reply) {
		return Reply{}, false
	}
	return reply, true
}

// Serve applies one request to a store on behalf of a session. The session's
// scope and rights are the caller's authority; the message carries none.
func (store *Store) Serve(scope Scope, rights Rights, request Request) Reply {
	reply := Reply{Operation: request.Operation}
	switch request.Operation {
	case OperationGet:
		reply.Value, reply.Status = store.Get(scope, rights, request.Key)
		if reply.Status == StatusOK {
			reply.Key = request.Key
		}
	case OperationSet:
		reply.Status = store.Set(scope, rights, request.Key, request.Value)
	case OperationRevision:
		if !validRevisionRequest(&request) {
			reply.Status = StatusInvalidArgument
			break
		}
		revision, status := store.Revision(scope, rights)
		reply.Status = status
		if status == StatusOK {
			reply.Value = Uint(revision)
		}
	case OperationList:
		key, value, readOnly, cursor, status := store.At(scope, rights, int(request.Cursor))
		reply.Key, reply.Value, reply.ReadOnly, reply.Cursor, reply.Status = key, value, readOnly, uint32(cursor), status
	default:
		reply.Status = StatusInvalidArgument
	}
	return reply
}
