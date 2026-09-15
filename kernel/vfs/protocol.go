package vfs

import (
	"encoding/binary"
	"goos/kernel/ipc"
)

const (
	ProtocolID      = uint64(0x47564631)
	ProtocolVersion = uint16(1)
	protocolHeader  = 32
	MaxProtocolPath = 192
	MaxProtocolData = ipc.MaxPayload - protocolHeader - MaxProtocolPath
)

type Operation uint8

const (
	OperationOpen Operation = iota + 1
	OperationClose
	OperationRead
	OperationWrite
	OperationSeek
	OperationCreate
	OperationMkdir
	OperationReadDir
	OperationRename
	OperationUnlink
	OperationStat
	OperationTruncate
)

type Request struct {
	Operation Operation
	Rights    Rights
	Handle    Handle
	Offset    uint64
	Size      uint32
	Path      []byte
	Data      []byte
}

func EncodeRequest(request Request, correlation uint64) (ipc.Message, bool) {
	if request.Operation < OperationOpen || request.Operation > OperationTruncate || correlation == 0 || len(request.Path) > MaxProtocolPath || len(request.Data) > MaxProtocolData || len(request.Path)+len(request.Data)+protocolHeader > ipc.MaxPayload {
		return ipc.Message{}, false
	}
	message := ipc.Message{ProtocolID: ProtocolID, Version: ProtocolVersion, Kind: ipc.KindRequest, Correlation: correlation, PayloadSize: uint16(protocolHeader + len(request.Path) + len(request.Data))}
	message.Payload[0] = byte(request.Operation)
	message.Payload[1] = byte(request.Rights)
	binary.LittleEndian.PutUint16(message.Payload[2:], uint16(len(request.Path)))
	binary.LittleEndian.PutUint16(message.Payload[4:], uint16(len(request.Data)))
	binary.LittleEndian.PutUint64(message.Payload[8:], uint64(request.Handle))
	binary.LittleEndian.PutUint64(message.Payload[16:], request.Offset)
	binary.LittleEndian.PutUint32(message.Payload[24:], request.Size)
	copy(message.Payload[protocolHeader:], request.Path)
	copy(message.Payload[protocolHeader+len(request.Path):], request.Data)
	return message, true
}

func DecodeRequest(message ipc.Message) (Request, bool) {
	if message.ProtocolID != ProtocolID || message.Version != ProtocolVersion || message.Kind != ipc.KindRequest || message.Correlation == 0 || message.PayloadSize < protocolHeader {
		return Request{}, false
	}
	pathSize := int(binary.LittleEndian.Uint16(message.Payload[2:]))
	dataSize := int(binary.LittleEndian.Uint16(message.Payload[4:]))
	if message.Payload[6] != 0 || message.Payload[7] != 0 || binary.LittleEndian.Uint32(message.Payload[28:]) != 0 || pathSize > MaxProtocolPath || dataSize > MaxProtocolData || protocolHeader+pathSize+dataSize != int(message.PayloadSize) {
		return Request{}, false
	}
	operation := Operation(message.Payload[0])
	rights := Rights(message.Payload[1])
	if operation < OperationOpen || operation > OperationTruncate || rights&^(Read|Write) != 0 {
		return Request{}, false
	}
	return Request{
		Operation: operation,
		Rights:    rights,
		Handle:    Handle(binary.LittleEndian.Uint64(message.Payload[8:])),
		Offset:    binary.LittleEndian.Uint64(message.Payload[16:]),
		Size:      binary.LittleEndian.Uint32(message.Payload[24:]),
		Path:      message.Payload[protocolHeader : protocolHeader+pathSize],
		Data:      message.Payload[protocolHeader+pathSize : protocolHeader+pathSize+dataSize],
	}, true
}
