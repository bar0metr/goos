// Package fsproto is the typed protocol between the VFS service and a
// filesystem service that runs in its own process (ADR 0096). It carries the
// operations of a namespace backend over a node-ID interface, so the VFS owns
// paths, mounts and open files while the filesystem service owns its on-disk
// format and the device it reads.
//
// Every message is one bounded IPC payload, and no message carries file data.
// The data of a read or a write lies in the file cache, a shared-memory object
// of CachePages pages both processes map: a request names a slot of it, and the
// bytes at the request's offset within that page (ADR 0098).
package fsproto

import (
	"encoding/binary"
	"goos/kernel/clock"
	"goos/kernel/ipc"
	"goos/kernel/ramfs"
)

const (
	ProtocolID = uint64(0x4746535052543031)
	// Version 2 moves file data from the messages to the file cache; version
	// 3 adds the no-space and corrupt statuses (ADR 0102); version 4 lets a
	// read or write span consecutive slots (ADR 0107).
	Version = uint16(4)
	// RequestDeadline bounds one exchange. A filesystem service that stalls
	// surfaces as an I/O error on its mounts rather than a hung namespace. It
	// is above the block protocol's own deadline, so a slow device is reported
	// by the service before the VFS gives up on the service.
	RequestDeadline = 6 * clock.TicksPerSecond

	requestHeader = 40
	replyHeader   = 32

	// PageSize is the size of one file-cache slot.
	PageSize = 4096
	// CachePages is the number of slots in the file cache.
	CachePages = 32
	// CacheSize is the size of the file cache mapping.
	CacheSize = CachePages * PageSize
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

// Request is one backend call. Node, Parent, Offset, Size, Slot and Name are
// used as the operation needs them; the rest are zero.
type Request struct {
	Operation Operation
	Node      ramfs.NodeID
	Parent    ramfs.NodeID
	// Offset is the file offset of a read or write, or the new size of a
	// truncation.
	Offset uint64
	// Size is the length of a read or write, or the cursor of a directory
	// listing.
	Size uint32
	// Slot is the first file-cache page a read fills or a write takes its
	// bytes from. The bytes start at Offset's position within that page and
	// run on through the slots after it, so one exchange moves up to the
	// rest of the cache.
	Slot uint32
	Name []byte
}

// Reply answers the request with the same correlation.
type Reply struct {
	Operation Operation
	Status    ramfs.Status
	Kind      ramfs.Kind
	// Node is the node an operation named or created, or the root of a mount.
	Node ramfs.NodeID
	// Kind and Value are the attributes of a stat or listed entry, and of the
	// node an open, create, or mkdir returns; a Kind of zero means none were
	// sent. For a read or a write, Value is the bytes it moved.
	Value uint64
	// Cursor is the position of the next directory entry.
	Cursor uint32
	Name   []byte
}

// validTransfer reports whether a read or write names a slot and stays within
// the cache from it.
func validTransfer(request Request) bool {
	if request.Operation != OperationRead && request.Operation != OperationWrite {
		return request.Slot == 0
	}
	return request.Slot < CachePages && request.Size != 0 &&
		request.Offset%PageSize+uint64(request.Size) <= uint64(CachePages-request.Slot)*PageSize
}

// SlotBytes returns the bytes of cache a read or write moves: Size bytes at
// Offset's position in the request's slot. The request must be valid.
func SlotBytes(cache []byte, request Request) ([]byte, bool) {
	if len(cache) != CacheSize || !validTransfer(request) || (request.Operation != OperationRead && request.Operation != OperationWrite) {
		return nil, false
	}
	start := uint64(request.Slot)*PageSize + request.Offset%PageSize
	return cache[start : start+uint64(request.Size)], true
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
		!validName(request.Name) || requestHeader+len(request.Name) > ipc.MaxPayload || !validTransfer(request) {
		return false
	}
	clearMessage(message)
	message.ProtocolID, message.Version, message.Kind, message.Correlation = ProtocolID, Version, ipc.KindRequest, correlation
	message.PayloadSize = uint16(requestHeader + len(request.Name))
	payload := message.Payload[:]
	payload[0] = byte(request.Operation)
	payload[1] = byte(len(request.Name))
	binary.LittleEndian.PutUint32(payload[4:], request.Slot)
	binary.LittleEndian.PutUint64(payload[8:], uint64(request.Node))
	binary.LittleEndian.PutUint64(payload[16:], uint64(request.Parent))
	binary.LittleEndian.PutUint64(payload[24:], request.Offset)
	binary.LittleEndian.PutUint32(payload[32:], request.Size)
	copy(payload[requestHeader:], request.Name)
	return true
}

// DecodeRequest validates a request. The returned slices alias the message.
func DecodeRequest(message *ipc.Message) (Request, bool) {
	if message == nil || message.ProtocolID != ProtocolID || message.Version != Version || message.Kind != ipc.KindRequest ||
		message.Correlation == 0 || message.HandleCount != 0 || message.PayloadSize < requestHeader || int(message.PayloadSize) > ipc.MaxPayload {
		return Request{}, false
	}
	payload := message.Payload[:]
	nameSize := int(payload[1])
	if !zero(payload[2:4]) || !zero(payload[36:40]) || nameSize > ramfs.MaxName || requestHeader+nameSize != int(message.PayloadSize) {
		return Request{}, false
	}
	request := Request{
		Operation: Operation(payload[0]),
		Slot:      binary.LittleEndian.Uint32(payload[4:]),
		Node:      ramfs.NodeID(binary.LittleEndian.Uint64(payload[8:])),
		Parent:    ramfs.NodeID(binary.LittleEndian.Uint64(payload[16:])),
		Offset:    binary.LittleEndian.Uint64(payload[24:]),
		Size:      binary.LittleEndian.Uint32(payload[32:]),
		Name:      payload[requestHeader : requestHeader+nameSize],
	}
	if !validOperation(request.Operation) || !validName(request.Name) || !validTransfer(request) {
		return Request{}, false
	}
	return request, true
}

func EncodeReply(reply Reply, correlation uint64, message *ipc.Message) bool {
	if message == nil || correlation == 0 || !validOperation(reply.Operation) || reply.Status > ramfs.LastStatus ||
		reply.Kind > ramfs.KindDirectory || len(reply.Name) > ramfs.MaxName || replyHeader+len(reply.Name) > ipc.MaxPayload {
		return false
	}
	clearMessage(message)
	message.ProtocolID, message.Version, message.Kind, message.Correlation = ProtocolID, Version, ipc.KindReply, correlation
	message.PayloadSize = uint16(replyHeader + len(reply.Name))
	payload := message.Payload[:]
	payload[0], payload[1], payload[2], payload[3] = byte(reply.Operation), byte(reply.Status), byte(reply.Kind), byte(len(reply.Name))
	binary.LittleEndian.PutUint64(payload[8:], uint64(reply.Node))
	binary.LittleEndian.PutUint64(payload[16:], reply.Value)
	binary.LittleEndian.PutUint32(payload[24:], reply.Cursor)
	copy(payload[replyHeader:], reply.Name)
	return true
}

// DecodeReply validates a reply. The returned slices alias the message.
func DecodeReply(message *ipc.Message) (Reply, bool) {
	if message == nil || message.ProtocolID != ProtocolID || message.Version != Version || message.Kind != ipc.KindReply ||
		message.Correlation == 0 || message.HandleCount != 0 || message.PayloadSize < replyHeader || int(message.PayloadSize) > ipc.MaxPayload {
		return Reply{}, false
	}
	payload := message.Payload[:]
	nameSize := int(payload[3])
	if !zero(payload[4:8]) || !zero(payload[28:32]) || nameSize > ramfs.MaxName || replyHeader+nameSize != int(message.PayloadSize) {
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
	}
	if !validOperation(reply.Operation) || reply.Status > ramfs.LastStatus || reply.Kind > ramfs.KindDirectory {
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
