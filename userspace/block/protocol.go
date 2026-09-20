// Package block defines the bounded service protocol for byte-addressable block
// storage. Hardware drivers remain behind the service Backend boundary.
package block

import (
	"encoding/binary"
	"goos/kernel/clock"
	"goos/kernel/ipc"
)

const (
	ProtocolID = uint64(0x47424c4b30303100)
	Version    = uint16(1)
	headerSize = 24
	// RequestDeadline bounds one storage exchange. A stalled peer must surface
	// an I/O error rather than block the caller indefinitely.
	RequestDeadline = 5 * clock.TicksPerSecond
	MaxData         = ipc.MaxPayload - headerSize
)

type Operation uint8

const (
	OperationInfo Operation = iota + 1
	OperationRead
	OperationWrite
	OperationFlush
)

type Status uint8

const (
	StatusOK Status = iota
	StatusInvalidArgument
	StatusOutOfRange
	StatusReadOnly
	StatusIOError
)

type Request struct {
	Operation Operation
	Offset    uint64
	Length    uint32
	Data      []byte
}

type Reply struct {
	Operation Operation
	Status    Status
	Capacity  uint64
	BlockSize uint32
	Data      []byte
}

func EncodeRequest(request Request, correlation uint64) (ipc.Message, bool) {
	var message ipc.Message
	ok := EncodeRequestInto(request, correlation, &message)
	return message, ok
}

func EncodeRequestInto(request Request, correlation uint64, message *ipc.Message) bool {
	if correlation == 0 || request.Operation < OperationInfo || request.Operation > OperationFlush || !validRequest(request) {
		return false
	}
	if message == nil {
		return false
	}
	// Clear the payload explicitly, like EncodeReplyInto: reserved bytes must
	// reach the peer as zero even when the message still holds older data. The
	// decoder rejects a request whose reserved bytes are dirty, so relying on
	// the caller's memory being zero would make encoding non-deterministic.
	for index := range message.Payload {
		message.Payload[index] = 0
	}
	message.ProtocolID, message.Version, message.Kind = ProtocolID, Version, ipc.KindRequest
	message.Correlation, message.PayloadSize, message.HandleCount = correlation, uint16(headerSize+len(request.Data)), 0
	for index := range message.Handles {
		message.Handles[index] = 0
	}
	message.Payload[0] = byte(request.Operation)
	binary.LittleEndian.PutUint64(message.Payload[8:16], request.Offset)
	binary.LittleEndian.PutUint32(message.Payload[16:20], request.Length)
	binary.LittleEndian.PutUint16(message.Payload[20:22], uint16(len(request.Data)))
	copy(message.Payload[headerSize:], request.Data)
	return true
}

func DecodeRequest(message ipc.Message) (Request, bool) {
	return DecodeRequestFrom(&message)
}

func DecodeRequestFrom(message *ipc.Message) (Request, bool) {
	if message == nil {
		return Request{}, false
	}
	if message.ProtocolID != ProtocolID || message.Version != Version || message.Kind != ipc.KindRequest || message.Correlation == 0 || message.HandleCount != 0 || message.PayloadSize < headerSize {
		return Request{}, false
	}
	if !zero(message.Payload[1:8]) || message.Payload[22] != 0 || message.Payload[23] != 0 {
		return Request{}, false
	}
	dataSize := int(binary.LittleEndian.Uint16(message.Payload[20:22]))
	if dataSize > MaxData || headerSize+dataSize != int(message.PayloadSize) {
		return Request{}, false
	}
	request := Request{Operation: Operation(message.Payload[0]), Offset: binary.LittleEndian.Uint64(message.Payload[8:16]), Length: binary.LittleEndian.Uint32(message.Payload[16:20]), Data: message.Payload[headerSize : headerSize+dataSize]}
	if request.Operation < OperationInfo || request.Operation > OperationFlush || !validRequest(request) {
		return Request{}, false
	}
	return request, true
}

func EncodeReply(reply Reply, correlation uint64) (ipc.Message, bool) {
	var message ipc.Message
	ok := EncodeReplyInto(reply, correlation, &message)
	return message, ok
}

func EncodeReplyInto(reply Reply, correlation uint64, message *ipc.Message) bool {
	if correlation == 0 || reply.Operation < OperationInfo || reply.Operation > OperationFlush || reply.Status > StatusIOError || len(reply.Data) > MaxData || reply.Operation != OperationRead && len(reply.Data) != 0 || reply.Status != StatusOK && len(reply.Data) != 0 {
		return false
	}
	if message == nil {
		return false
	}
	for index := range message.Payload {
		message.Payload[index] = 0
	}
	message.ProtocolID, message.Version, message.Kind = ProtocolID, Version, ipc.KindReply
	message.Correlation, message.PayloadSize, message.HandleCount = correlation, uint16(headerSize+len(reply.Data)), 0
	message.Payload[0], message.Payload[1] = byte(reply.Operation), byte(reply.Status)
	binary.LittleEndian.PutUint64(message.Payload[8:16], reply.Capacity)
	binary.LittleEndian.PutUint32(message.Payload[16:20], reply.BlockSize)
	binary.LittleEndian.PutUint16(message.Payload[20:22], uint16(len(reply.Data)))
	copy(message.Payload[headerSize:], reply.Data)
	return true
}

func DecodeReply(message ipc.Message) (Reply, bool) {
	return DecodeReplyFrom(&message)
}

func DecodeReplyFrom(message *ipc.Message) (Reply, bool) {
	if message == nil {
		return Reply{}, false
	}
	if message.ProtocolID != ProtocolID || message.Version != Version || message.Kind != ipc.KindReply || message.Correlation == 0 || message.HandleCount != 0 || message.PayloadSize < headerSize || !zero(message.Payload[2:8]) || message.Payload[22] != 0 || message.Payload[23] != 0 {
		return Reply{}, false
	}
	dataSize := int(binary.LittleEndian.Uint16(message.Payload[20:22]))
	reply := Reply{Operation: Operation(message.Payload[0]), Status: Status(message.Payload[1]), Capacity: binary.LittleEndian.Uint64(message.Payload[8:16]), BlockSize: binary.LittleEndian.Uint32(message.Payload[16:20]), Data: message.Payload[headerSize : headerSize+dataSize]}
	if reply.Operation < OperationInfo || reply.Operation > OperationFlush || reply.Status > StatusIOError || dataSize > MaxData || headerSize+dataSize != int(message.PayloadSize) || reply.Operation != OperationRead && dataSize != 0 || reply.Status != StatusOK && dataSize != 0 {
		return Reply{}, false
	}
	return reply, true
}

func validRequest(request Request) bool {
	switch request.Operation {
	case OperationInfo, OperationFlush:
		return request.Offset == 0 && request.Length == 0 && len(request.Data) == 0
	case OperationRead:
		return request.Length != 0 && request.Length <= MaxData && len(request.Data) == 0
	case OperationWrite:
		return request.Length != 0 && request.Length == uint32(len(request.Data)) && len(request.Data) <= MaxData
	default:
		return false
	}
}

func zero(value []byte) bool {
	for _, item := range value {
		if item != 0 {
			return false
		}
	}
	return true
}
