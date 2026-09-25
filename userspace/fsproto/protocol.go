// Package fsproto is the typed protocol between the VFS service and a
// filesystem service that runs in its own process (ADR 0096). It carries the
// operations of a namespace backend over a node-ID interface, so the VFS owns
// paths, mounts and open files while the filesystem service owns its on-disk
// format and the device it reads.
//
// Every message is one bounded IPC payload. File data moves in chunks of at
// most MaxRequestData bytes per write and MaxReplyData bytes per read; the
// caller splits larger transfers.
package fsproto

import (
	"encoding/binary"
	"goos/kernel/clock"
	"goos/kernel/ipc"
	"goos/kernel/ramfs"
)

const (
	ProtocolID = uint64(0x4746535052543031)
	Version    = uint16(1)
	// RequestDeadline bounds one exchange. A filesystem service that stalls
	// surfaces as an I/O error on its mounts rather than a hung namespace. It
	// is above the block protocol's own deadline, so a slow device is reported
	// by the service before the VFS gives up on the service.
	RequestDeadline = 6 * clock.TicksPerSecond

	requestHeader  = 40
	replyHeader    = 32
	MaxRequestData = ipc.MaxPayload - requestHeader
	MaxReplyData   = ipc.MaxPayload - replyHeader
)

type Operation uint8

const (
	// OperationMount returns the root of the volume the service has mounted,
	// or why it refused the volume.
	OperationMount Operation = iota + 1
	OperationOpen
	OperationCreate
	OperationMkdir
	OperationRead
	OperationWrite
	OperationTruncate
	OperationStat
	OperationReadDir
	OperationRename
	OperationUnlink
	// OperationFlush makes every completed write durable on the device.
	OperationFlush
)

// Request is one backend call. Node, Parent, Offset, Size, Name and Data are
// used as the operation needs them; the rest are zero.
type Request struct {
	Operation Operation
	Node      ramfs.NodeID
	Parent    ramfs.NodeID
	// Offset is the file offset of a read or write, or the new size of a
	// truncation.
	Offset uint64
	// Size is the length of a read, or the cursor of a directory listing.
	Size uint32
	Name []byte
	Data []byte
}

// Reply answers the request with the same correlation.
type Reply struct {
	Operation Operation
	Status    ramfs.Status
	Kind      ramfs.Kind
	// Node is the node an operation named or created, or the root of a mount.
	Node ramfs.NodeID
	// Value is the size of a stat or listed entry, or the bytes a write moved.
	Value uint64
	// Cursor is the position of the next directory entry.
	Cursor uint32
	Name   []byte
	Data   []byte
}

func validOperation(operation Operation) bool {
	return operation >= OperationMount && operation <= OperationFlush
}

// validName accepts an empty name, for operations that take none, or one a
// backend would accept.
func validName(name []byte) bool {
	if len(name) == 0 {
		return true
	}
	_, ok := ramfs.MakeName(name)
	return ok
}

func EncodeRequest(request Request, correlation uint64, message *ipc.Message) bool {
	if message == nil || correlation == 0 || !validOperation(request.Operation) || len(request.Name) > ramfs.MaxName ||
		!validName(request.Name) || requestHeader+len(request.Name)+len(request.Data) > ipc.MaxPayload ||
		request.Operation == OperationRead && request.Size > MaxReplyData {
		return false
	}
	clearMessage(message)
	message.ProtocolID, message.Version, message.Kind, message.Correlation = ProtocolID, Version, ipc.KindRequest, correlation
	message.PayloadSize = uint16(requestHeader + len(request.Name) + len(request.Data))
	payload := message.Payload[:]
	payload[0] = byte(request.Operation)
	payload[1] = byte(len(request.Name))
	binary.LittleEndian.PutUint16(payload[2:], uint16(len(request.Data)))
	binary.LittleEndian.PutUint64(payload[8:], uint64(request.Node))
	binary.LittleEndian.PutUint64(payload[16:], uint64(request.Parent))
	binary.LittleEndian.PutUint64(payload[24:], request.Offset)
	binary.LittleEndian.PutUint32(payload[32:], request.Size)
	copy(payload[requestHeader:], request.Name)
	copy(payload[requestHeader+len(request.Name):], request.Data)
	return true
}

// DecodeRequest validates a request. The returned slices alias the message.
func DecodeRequest(message *ipc.Message) (Request, bool) {
	if message == nil || message.ProtocolID != ProtocolID || message.Version != Version || message.Kind != ipc.KindRequest ||
		message.Correlation == 0 || message.HandleCount != 0 || message.PayloadSize < requestHeader || int(message.PayloadSize) > ipc.MaxPayload {
		return Request{}, false
	}
	payload := message.Payload[:]
	nameSize, dataSize := int(payload[1]), int(binary.LittleEndian.Uint16(payload[2:]))
	if !zero(payload[4:8]) || !zero(payload[36:40]) || nameSize > ramfs.MaxName || requestHeader+nameSize+dataSize != int(message.PayloadSize) {
		return Request{}, false
	}
	request := Request{
		Operation: Operation(payload[0]),
		Node:      ramfs.NodeID(binary.LittleEndian.Uint64(payload[8:])),
		Parent:    ramfs.NodeID(binary.LittleEndian.Uint64(payload[16:])),
		Offset:    binary.LittleEndian.Uint64(payload[24:]),
		Size:      binary.LittleEndian.Uint32(payload[32:]),
		Name:      payload[requestHeader : requestHeader+nameSize],
		Data:      payload[requestHeader+nameSize : requestHeader+nameSize+dataSize],
	}
	if !validOperation(request.Operation) || !validName(request.Name) || request.Operation == OperationRead && request.Size > MaxReplyData {
		return Request{}, false
	}
	return request, true
}

func EncodeReply(reply Reply, correlation uint64, message *ipc.Message) bool {
	if message == nil || correlation == 0 || !validOperation(reply.Operation) || reply.Status > ramfs.StatusIOError ||
		reply.Kind > ramfs.KindDirectory || len(reply.Name) > ramfs.MaxName || replyHeader+len(reply.Name)+len(reply.Data) > ipc.MaxPayload {
		return false
	}
	clearMessage(message)
	message.ProtocolID, message.Version, message.Kind, message.Correlation = ProtocolID, Version, ipc.KindReply, correlation
	message.PayloadSize = uint16(replyHeader + len(reply.Name) + len(reply.Data))
	payload := message.Payload[:]
	payload[0], payload[1], payload[2], payload[3] = byte(reply.Operation), byte(reply.Status), byte(reply.Kind), byte(len(reply.Name))
	binary.LittleEndian.PutUint16(payload[4:], uint16(len(reply.Data)))
	binary.LittleEndian.PutUint64(payload[8:], uint64(reply.Node))
	binary.LittleEndian.PutUint64(payload[16:], reply.Value)
	binary.LittleEndian.PutUint32(payload[24:], reply.Cursor)
	copy(payload[replyHeader:], reply.Name)
	copy(payload[replyHeader+len(reply.Name):], reply.Data)
	return true
}

// DecodeReply validates a reply. The returned slices alias the message.
func DecodeReply(message *ipc.Message) (Reply, bool) {
	if message == nil || message.ProtocolID != ProtocolID || message.Version != Version || message.Kind != ipc.KindReply ||
		message.Correlation == 0 || message.HandleCount != 0 || message.PayloadSize < replyHeader || int(message.PayloadSize) > ipc.MaxPayload {
		return Reply{}, false
	}
	payload := message.Payload[:]
	nameSize, dataSize := int(payload[3]), int(binary.LittleEndian.Uint16(payload[4:]))
	if !zero(payload[6:8]) || !zero(payload[28:32]) || nameSize > ramfs.MaxName || replyHeader+nameSize+dataSize != int(message.PayloadSize) {
		return Reply{}, false
	}
	reply := Reply{
		Operation: Operation(payload[0]),
		Status:    ramfs.Status(payload[1]),
		Kind:      ramfs.Kind(payload[2]),
		Node:      ramfs.NodeID(binary.LittleEndian.Uint64(payload[8:])),
		Value:     binary.LittleEndian.Uint64(payload[16:]),
		Cursor:    binary.LittleEndian.Uint32(payload[24:]),
		Name:      payload[replyHeader : replyHeader+nameSize],
		Data:      payload[replyHeader+nameSize : replyHeader+nameSize+dataSize],
	}
	if !validOperation(reply.Operation) || reply.Status > ramfs.StatusIOError || reply.Kind > ramfs.KindDirectory {
		return Reply{}, false
	}
	return reply, true
}

// Reserved bytes must reach the peer as zero even when the message still
// holds older data, and the decoders reject them otherwise.
func clearMessage(message *ipc.Message) {
	for index := range message.Payload {
		message.Payload[index] = 0
	}
	message.HandleCount = 0
	for index := range message.Handles {
		message.Handles[index] = 0
	}
}

func zero(data []byte) bool {
	for _, value := range data {
		if value != 0 {
			return false
		}
	}
	return true
}
