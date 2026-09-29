// Package fsproto is the typed protocol between the VFS service and a
// filesystem service that runs in its own process (ADR 0096). It carries the
// operations of a namespace backend over a node-ID interface, so the VFS owns
// paths, mounts and open files while the filesystem service owns its on-disk
// format and the device it reads.
//
// Every message is one bounded IPC payload, and no message carries file data.
// The data of a read or a write lies in the file cache, a shared-memory object
// of CachePages pages both processes map: a request names a slot of it, and the
// bytes at the request's offset within that page (ADR 0098). A name too long
// for a message travels in the page after the slots, NamePage (ADR 0110).
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
	// read or write span consecutive slots (ADR 0107); version 5 carries
	// names of up to 255 bytes, the long ones in the name page (ADR 0110);
	// version 6 adds the timeout, peer-died, and unsupported statuses; version
	// 7 widens name lengths for the 1024-byte native name limit; version 8
	// adds stable open-object acquire/release operations (ADR 0119); version 9
	// carries fixed-width native timestamps and attributes; version 10 adds
	// filesystem capacity and read-only state; version 11 adds whole-file
	// replacement (ADR 0129).
	Version = uint16(11)
	// RequestDeadline bounds one exchange. A filesystem service that stalls
	// surfaces as an I/O error on its mounts rather than a hung namespace. It
	// is above the block protocol's own deadline, so a slow device is reported
	// by the service before the VFS gives up on the service.
	RequestDeadline = 6 * clock.TicksPerSecond

	requestHeader = 40
	replyHeader   = 88

	// PageSize is the size of one file-cache slot.
	PageSize = 4096
	// CachePages is the number of slots in the file cache.
	CachePages = 32
	// NamePage is the page after the slots, which carries a name too long for
	// a message. One exchange is in flight at a time, so one page serves both
	// directions: the VFS writes a request's name there before sending it, and
	// the filesystem service a listed name before answering.
	NamePage = CachePages
	// CacheSize is the size of the file cache mapping: the slots and the name
	// page.
	CacheSize = (CachePages + 1) * PageSize

	// Names up to these lengths travel in the message.
	maxRequestName = ipc.MaxPayload - requestHeader
	maxReplyName   = ipc.MaxPayload - replyHeader
	// flagNameInCache marks a message whose name is in the name page.
	flagNameInCache = 1
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
	OperationAcquire
	OperationRelease
	// OperationFlush makes every completed write durable on the device.
	OperationFlush
	OperationFilesystemInfo
	// OperationReplace gives Node the name of the existing file Name in
	// Parent and removes that file.
	OperationReplace
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
	Cursor              uint32
	Name                []byte
	Created             ramfs.Timestamp
	Modified            ramfs.Timestamp
	Attributes          ramfs.Attributes
	AttributesAvailable bool
	Filesystem          ramfs.FilesystemInfo
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
	return operation >= OperationMount && operation <= OperationReplace
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

// EncodeRequest encodes request into message. A name too long for the
// message is written to the name page of cache, which must then be the file
// cache mapping.
func EncodeRequest(request Request, correlation uint64, message *ipc.Message, cache []byte) bool {
	if message == nil || correlation == 0 || !validOperation(request.Operation) || len(request.Name) > ramfs.MaxName ||
		!validName(request.Name) || !validTransfer(request) {
		return false
	}
	inline := len(request.Name) <= maxRequestName
	if !inline && !placeName(cache, request.Name) {
		return false
	}
	clearMessage(message)
	message.ProtocolID, message.Version, message.Kind, message.Correlation = ProtocolID, Version, ipc.KindRequest, correlation
	message.PayloadSize = requestHeader
	payload := message.Payload[:]
	payload[0] = byte(request.Operation)
	binary.LittleEndian.PutUint16(payload[1:], uint16(len(request.Name)))
	binary.LittleEndian.PutUint32(payload[4:], request.Slot)
	binary.LittleEndian.PutUint64(payload[8:], uint64(request.Node))
	binary.LittleEndian.PutUint64(payload[16:], uint64(request.Parent))
	binary.LittleEndian.PutUint64(payload[24:], request.Offset)
	binary.LittleEndian.PutUint32(payload[32:], request.Size)
	if inline {
		message.PayloadSize += uint16(copy(payload[requestHeader:], request.Name))
	} else {
		payload[3] = flagNameInCache
	}
	return true
}

// DecodeRequest validates a request. Its name aliases the message, or, when
// it came through the name page, a copy of it in name: the page is shared,
// so what is checked must be what is used.
func DecodeRequest(message *ipc.Message, cache []byte, name *[ramfs.MaxName]byte) (Request, bool) {
	if message == nil || message.ProtocolID != ProtocolID || message.Version != Version || message.Kind != ipc.KindRequest ||
		message.Correlation == 0 || message.HandleCount != 0 || message.PayloadSize < requestHeader || int(message.PayloadSize) > ipc.MaxPayload {
		return Request{}, false
	}
	payload := message.Payload[:]
	nameSize := int(binary.LittleEndian.Uint16(payload[1:]))
	cached := payload[3] == flagNameInCache
	if payload[3]&^flagNameInCache != 0 || !zero(payload[36:40]) || nameSize > ramfs.MaxName {
		return Request{}, false
	}
	request := Request{
		Operation: Operation(payload[0]),
		Slot:      binary.LittleEndian.Uint32(payload[4:]),
		Node:      ramfs.NodeID(binary.LittleEndian.Uint64(payload[8:])),
		Parent:    ramfs.NodeID(binary.LittleEndian.Uint64(payload[16:])),
		Offset:    binary.LittleEndian.Uint64(payload[24:]),
		Size:      binary.LittleEndian.Uint32(payload[32:]),
	}
	switch {
	case cached:
		if int(message.PayloadSize) != requestHeader || nameSize <= maxRequestName {
			return Request{}, false
		}
		copied, ok := takeName(cache, nameSize, name)
		if !ok {
			return Request{}, false
		}
		request.Name = copied
	case requestHeader+nameSize != int(message.PayloadSize):
		return Request{}, false
	default:
		request.Name = payload[requestHeader : requestHeader+nameSize]
	}
	if !validOperation(request.Operation) || !validName(request.Name) || !validTransfer(request) {
		return Request{}, false
	}
	return request, true
}

// EncodeReply encodes reply into message, a long name through the name page
// of cache.
func EncodeReply(reply Reply, correlation uint64, message *ipc.Message, cache []byte) bool {
	if message == nil || correlation == 0 || !validOperation(reply.Operation) || reply.Status > ramfs.LastStatus ||
		reply.Kind > ramfs.KindDirectory || len(reply.Name) > ramfs.MaxName || !reply.Created.Valid() || !reply.Modified.Valid() ||
		reply.Attributes > (ramfs.LastAttribute*2-1) || !reply.AttributesAvailable && reply.Attributes != 0 || !reply.Filesystem.Valid() ||
		reply.Operation == OperationFilesystemInfo && len(reply.Name) > ramfs.MaxVolumeName || reply.Status == ramfs.StatusOK && reply.Operation == OperationFilesystemInfo && reply.Filesystem.Type == 0 {
		return false
	}
	inline := len(reply.Name) <= maxReplyName
	if !inline && !placeName(cache, reply.Name) {
		return false
	}
	clearMessage(message)
	message.ProtocolID, message.Version, message.Kind, message.Correlation = ProtocolID, Version, ipc.KindReply, correlation
	message.PayloadSize = replyHeader
	payload := message.Payload[:]
	payload[0], payload[1], payload[2] = byte(reply.Operation), byte(reply.Status), byte(reply.Kind)
	binary.LittleEndian.PutUint16(payload[3:], uint16(len(reply.Name)))
	binary.LittleEndian.PutUint64(payload[8:], uint64(reply.Node))
	binary.LittleEndian.PutUint64(payload[16:], reply.Value)
	binary.LittleEndian.PutUint32(payload[24:], reply.Cursor)
	payload[28] = byte(reply.Attributes)
	if reply.Created.Available {
		payload[29] |= 1
	}
	if reply.Modified.Available {
		payload[29] |= 2
	}
	if reply.AttributesAvailable {
		payload[29] |= 4
	}
	binary.LittleEndian.PutUint64(payload[32:], uint64(reply.Created.Seconds))
	binary.LittleEndian.PutUint64(payload[40:], uint64(reply.Modified.Seconds))
	binary.LittleEndian.PutUint32(payload[48:], reply.Created.Nanoseconds)
	binary.LittleEndian.PutUint32(payload[52:], reply.Modified.Nanoseconds)
	binary.LittleEndian.PutUint64(payload[56:], reply.Filesystem.TotalBytes)
	binary.LittleEndian.PutUint64(payload[64:], reply.Filesystem.FreeBytes)
	binary.LittleEndian.PutUint64(payload[72:], reply.Filesystem.AvailableBytes)
	payload[80] = byte(reply.Filesystem.Type)
	if reply.Filesystem.ReadOnly {
		payload[81] |= 1
	}
	if reply.Filesystem.CapacityAvailable {
		payload[81] |= 2
	}
	if inline {
		message.PayloadSize += uint16(copy(payload[replyHeader:], reply.Name))
	} else {
		payload[5] = flagNameInCache
	}
	return true
}

// DecodeReply validates a reply. Its name aliases the message or a copy in
// name, as DecodeRequest's does.
func DecodeReply(message *ipc.Message, cache []byte, name *[ramfs.MaxName]byte) (Reply, bool) {
	if message == nil || message.ProtocolID != ProtocolID || message.Version != Version || message.Kind != ipc.KindReply ||
		message.Correlation == 0 || message.HandleCount != 0 || message.PayloadSize < replyHeader || int(message.PayloadSize) > ipc.MaxPayload {
		return Reply{}, false
	}
	payload := message.Payload[:]
	nameSize := int(binary.LittleEndian.Uint16(payload[3:]))
	cached := payload[5] == flagNameInCache
	if payload[5]&^flagNameInCache != 0 || !zero(payload[6:8]) || payload[29]&^byte(7) != 0 || !zero(payload[30:32]) ||
		payload[81]&^byte(3) != 0 || !zero(payload[82:88]) || nameSize > ramfs.MaxName {
		return Reply{}, false
	}
	reply := Reply{
		Operation:           Operation(payload[0]),
		Status:              ramfs.Status(payload[1]),
		Kind:                ramfs.Kind(payload[2]),
		Node:                ramfs.NodeID(binary.LittleEndian.Uint64(payload[8:])),
		Value:               binary.LittleEndian.Uint64(payload[16:]),
		Cursor:              binary.LittleEndian.Uint32(payload[24:]),
		Attributes:          ramfs.Attributes(payload[28]),
		AttributesAvailable: payload[29]&4 != 0,
		Created:             ramfs.Timestamp{Seconds: int64(binary.LittleEndian.Uint64(payload[32:])), Nanoseconds: binary.LittleEndian.Uint32(payload[48:]), Available: payload[29]&1 != 0},
		Modified:            ramfs.Timestamp{Seconds: int64(binary.LittleEndian.Uint64(payload[40:])), Nanoseconds: binary.LittleEndian.Uint32(payload[52:]), Available: payload[29]&2 != 0},
		Filesystem:          ramfs.FilesystemInfo{Type: ramfs.FilesystemType(payload[80]), TotalBytes: binary.LittleEndian.Uint64(payload[56:]), FreeBytes: binary.LittleEndian.Uint64(payload[64:]), AvailableBytes: binary.LittleEndian.Uint64(payload[72:]), ReadOnly: payload[81]&1 != 0, CapacityAvailable: payload[81]&2 != 0},
	}
	switch {
	case cached:
		if int(message.PayloadSize) != replyHeader || nameSize <= maxReplyName {
			return Reply{}, false
		}
		copied, ok := takeName(cache, nameSize, name)
		if !ok {
			return Reply{}, false
		}
		reply.Name = copied
	case replyHeader+nameSize != int(message.PayloadSize):
		return Reply{}, false
	default:
		reply.Name = payload[replyHeader : replyHeader+nameSize]
	}
	if reply.Operation == OperationFilesystemInfo && !reply.Filesystem.SetVolumeName(reply.Name) {
		return Reply{}, false
	}
	if !validOperation(reply.Operation) || reply.Status > ramfs.LastStatus || reply.Kind > ramfs.KindDirectory ||
		!reply.Created.Valid() || !reply.Modified.Valid() || reply.Attributes > (ramfs.LastAttribute*2-1) || !reply.AttributesAvailable && reply.Attributes != 0 || !reply.Filesystem.Valid() ||
		reply.Status == ramfs.StatusOK && reply.Operation == OperationFilesystemInfo && reply.Filesystem.Type == 0 {
		return Reply{}, false
	}
	return reply, true
}

// placeName writes a name to the name page.
func placeName(cache []byte, name []byte) bool {
	if len(cache) != CacheSize {
		return false
	}
	copy(cache[NamePage*PageSize:], name)
	return true
}

// takeName copies size bytes of the name page into name.
func takeName(cache []byte, size int, name *[ramfs.MaxName]byte) ([]byte, bool) {
	if len(cache) != CacheSize || name == nil || size > len(name) {
		return nil, false
	}
	return name[:copy(name[:size], cache[NamePage*PageSize:])], true
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
