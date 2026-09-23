package vfs

import (
	"encoding/binary"
	"goos/kernel/ipc"
	"goos/kernel/ramfs"
)

const (
	ProtocolID      = uint64(0x47564631)
	ProtocolVersion = uint16(2)
	protocolHeader  = 32
	MaxProtocolPath = 192
	MaxProtocolData = ipc.MaxPayload - protocolHeader - MaxProtocolPath
	replyHeader     = 32
	MaxReplyData    = ipc.MaxPayload - replyHeader
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

type Reply struct {
	Operation Operation
	Status    Status
	Kind      ramfs.Kind
	Handle    Handle
	Size      uint64
	Cursor    uint32
	Name      []byte
	Data      []byte
}

func EncodeReply(reply Reply, correlation uint64) (ipc.Message, bool) {
	if reply.Operation < OperationOpen || reply.Operation > OperationTruncate || correlation == 0 ||
		reply.Status > lastStatus || len(reply.Name) > ramfs.MaxName || len(reply.Data) > MaxReplyData ||
		replyHeader+len(reply.Name)+len(reply.Data) > ipc.MaxPayload {
		return ipc.Message{}, false
	}
	message := ipc.Message{ProtocolID: ProtocolID, Version: ProtocolVersion, Kind: ipc.KindReply, Correlation: correlation,
		PayloadSize: uint16(replyHeader + len(reply.Name) + len(reply.Data))}
	message.Payload[0] = byte(reply.Operation)
	message.Payload[1] = byte(reply.Status)
	message.Payload[2] = byte(reply.Kind)
	message.Payload[3] = byte(len(reply.Name))
	binary.LittleEndian.PutUint16(message.Payload[4:], uint16(len(reply.Data)))
	binary.LittleEndian.PutUint64(message.Payload[8:], uint64(reply.Handle))
	binary.LittleEndian.PutUint64(message.Payload[16:], reply.Size)
	binary.LittleEndian.PutUint32(message.Payload[24:], reply.Cursor)
	copy(message.Payload[replyHeader:], reply.Name)
	copy(message.Payload[replyHeader+len(reply.Name):], reply.Data)
	return message, true
}

func DecodeReply(message ipc.Message) (Reply, bool) {
	if message.ProtocolID != ProtocolID || message.Version != ProtocolVersion || message.Kind != ipc.KindReply ||
		message.Correlation == 0 || message.PayloadSize < replyHeader {
		return Reply{}, false
	}
	operation := Operation(message.Payload[0])
	status := Status(message.Payload[1])
	kind := ramfs.Kind(message.Payload[2])
	nameSize := int(message.Payload[3])
	dataSize := int(binary.LittleEndian.Uint16(message.Payload[4:]))
	if operation < OperationOpen || operation > OperationTruncate || status > lastStatus ||
		kind > ramfs.KindDirectory || nameSize > ramfs.MaxName || dataSize > MaxReplyData ||
		message.Payload[6] != 0 || message.Payload[7] != 0 || binary.LittleEndian.Uint32(message.Payload[28:]) != 0 ||
		replyHeader+nameSize+dataSize != int(message.PayloadSize) {
		return Reply{}, false
	}
	return Reply{Operation: operation, Status: status, Kind: kind,
		Handle: Handle(binary.LittleEndian.Uint64(message.Payload[8:])),
		Size:   binary.LittleEndian.Uint64(message.Payload[16:]), Cursor: binary.LittleEndian.Uint32(message.Payload[24:]),
		Name: message.Payload[replyHeader : replyHeader+nameSize],
		Data: message.Payload[replyHeader+nameSize : replyHeader+nameSize+dataSize]}, true
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
