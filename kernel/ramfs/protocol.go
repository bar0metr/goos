package ramfs

import (
	"encoding/binary"
	"goos/kernel/ipc"
)

const (
	ProtocolID      = uint64(0x47524631)
	ProtocolVersion = uint16(1)
	requestHeader   = 32
	MaxInlineData   = ipc.MaxPayload - requestHeader - MaxName
)

type Operation uint8

const (
	OperationCreate Operation = iota + 1
	OperationOpen
	OperationRead
	OperationWrite
	OperationTruncate
	OperationMkdir
	OperationReadDir
	OperationRename
	OperationUnlink
	OperationStat
)

type Request struct {
	Operation Operation
	Node      NodeID
	Parent    NodeID
	Offset    uint64
	Size      uint32
	Name      []byte
	Data      []byte
}

func EncodeRequest(request Request, correlation uint64) (ipc.Message, bool) {
	if request.Operation < OperationCreate || request.Operation > OperationStat || correlation == 0 || request.Offset > uint64(^uint32(0)) || !validProtocolName(request.Name) || len(request.Data) > MaxInlineData {
		return ipc.Message{}, false
	}
	payloadSize := requestHeader + len(request.Name) + len(request.Data)
	if payloadSize > ipc.MaxPayload {
		return ipc.Message{}, false
	}
	message := ipc.Message{ProtocolID: ProtocolID, Version: ProtocolVersion, Kind: ipc.KindRequest, Correlation: correlation, PayloadSize: uint16(payloadSize)}
	message.Payload[0] = byte(request.Operation)
	message.Payload[1] = byte(len(request.Name))
	binary.LittleEndian.PutUint16(message.Payload[2:], uint16(len(request.Data)))
	binary.LittleEndian.PutUint64(message.Payload[8:], uint64(request.Node))
	binary.LittleEndian.PutUint64(message.Payload[16:], uint64(request.Parent))
	binary.LittleEndian.PutUint32(message.Payload[24:], uint32(request.Offset))
	binary.LittleEndian.PutUint32(message.Payload[28:], request.Size)
	copy(message.Payload[requestHeader:], request.Name)
	copy(message.Payload[requestHeader+len(request.Name):], request.Data)
	return message, true
}

func DecodeRequest(message ipc.Message) (Request, bool) {
	if message.ProtocolID != ProtocolID || message.Version != ProtocolVersion || message.Kind != ipc.KindRequest || message.Correlation == 0 || message.PayloadSize < requestHeader {
		return Request{}, false
	}
	nameSize := int(message.Payload[1])
	dataSize := int(binary.LittleEndian.Uint16(message.Payload[2:]))
	if message.Payload[3] != 0 || binary.LittleEndian.Uint32(message.Payload[4:]) != 0 || nameSize > MaxName || dataSize > MaxInlineData || requestHeader+nameSize+dataSize != int(message.PayloadSize) {
		return Request{}, false
	}
	operation := Operation(message.Payload[0])
	name := message.Payload[requestHeader : requestHeader+nameSize]
	if operation < OperationCreate || operation > OperationStat || !validProtocolName(name) {
		return Request{}, false
	}
	return Request{
		Operation: operation,
		Node:      NodeID(binary.LittleEndian.Uint64(message.Payload[8:])),
		Parent:    NodeID(binary.LittleEndian.Uint64(message.Payload[16:])),
		Offset:    uint64(binary.LittleEndian.Uint32(message.Payload[24:])),
		Size:      binary.LittleEndian.Uint32(message.Payload[28:]),
		Name:      name,
		Data:      message.Payload[requestHeader+nameSize : requestHeader+nameSize+dataSize],
	}, true
}

func validProtocolName(name []byte) bool {
	return len(name) == 0 || validName(name)
}
