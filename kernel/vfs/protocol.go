package vfs

import (
	"encoding/binary"
	"goos/internal/limits"
	"goos/kernel/ipc"
	"goos/kernel/ramfs"
)

const (
	ProtocolID = uint64(0x47564631)
	// Version 3 adds the no-space and corrupt statuses (ADR 0102); version 4
	// adds the bulk operations (ADR 0107); version 5 carries paths of up to
	// PathMax bytes and names of up to NameMax, the long ones in an attached
	// object, and gives a rename's target a field of its own (ADR 0110).
	ProtocolVersion = uint16(5)
	protocolHeader  = 32
	// MaxProtocolData is the most write data one inline request carries.
	MaxProtocolData = ipc.MaxPayload - protocolHeader
	replyHeader     = 32
	MaxReplyData    = ipc.MaxPayload - replyHeader
	// MaxInlineName is the longest name a reply carries in the message.
	MaxInlineName = ipc.MaxPayload - replyHeader

	// flagPathsAttached marks a request whose path and target are in the
	// attached object, one after the other, instead of in the message.
	flagPathsAttached = 1
	// flagNameReturn lets a directory listing return a name too long for the
	// message in the attached object, at its start.
	flagNameReturn = 2
	// flagNameAttached marks a reply whose name is in the attached object.
	flagNameAttached = 1
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
	// OperationReadInto reads Size bytes from the file's position into the
	// memory object attached to the request, at byte Offset of it; the
	// reply's Size is how many it read (ADR 0107). OperationWriteFrom writes
	// them from the object. OperationFillExecutable reads the file from its
	// position into the attached executable object until the object seals,
	// Size bytes, so a launcher never handles the program's bytes itself.
	OperationReadInto
	OperationWriteFrom
	OperationFillExecutable
	lastOperation = OperationFillExecutable
)

// Bulk reports whether an operation moves its data through an attached object
// and so carries exactly one handle.
func (operation Operation) Bulk() bool {
	return operation == OperationReadInto || operation == OperationWriteFrom || operation == OperationFillExecutable
}

// Request is one client call. Path names what it acts on, Target where a
// rename moves it, and Data is a write's bytes.
//
// A path or target too long for the message travels in an attached memory
// object instead (PathsAttached); the receiver copies it out of the object
// with TakePaths before using it. NameReturn attaches an object a listing may
// write a long name to.
type Request struct {
	Operation     Operation
	Rights        Rights
	Handle        Handle
	Offset        uint64
	Size          uint32
	Path          []byte
	Target        []byte
	Data          []byte
	PathsAttached bool
	NameReturn    bool
	pathSize      int
	targetSize    int
}

// Reply answers a request. A listed name too long for the message is in the
// object the request attached, and AttachedName is its length.
type Reply struct {
	Operation    Operation
	Status       Status
	Kind         ramfs.Kind
	Handle       Handle
	Size         uint64
	Cursor       uint32
	Name         []byte
	Data         []byte
	AttachedName uint8
}

// FitsInline reports whether a request's path, target, and data fit in one
// message.
func FitsInline(request Request) bool {
	return protocolHeader+len(request.Path)+len(request.Target)+len(request.Data) <= ipc.MaxPayload
}

// AttachedPaths returns the lengths of the path and target a request carries
// in its attached object.
func (request Request) AttachedPaths() (int, int) { return request.pathSize, request.targetSize }

// TakePaths copies the path and target a request carries in object into
// path and target, which the request then names. The object is shared with
// the client, so the copy is what is checked and used.
func (request *Request) TakePaths(object []byte, path, target *[limits.PathMax]byte) bool {
	if !request.PathsAttached || request.pathSize+request.targetSize > len(object) {
		return false
	}
	request.Path = path[:copy(path[:request.pathSize], object)]
	request.Target = target[:copy(target[:request.targetSize], object[request.pathSize:])]
	return true
}

func EncodeReply(reply Reply, correlation uint64) (ipc.Message, bool) {
	nameSize := len(reply.Name)
	if reply.AttachedName != 0 {
		nameSize = int(reply.AttachedName)
	}
	if reply.Operation < OperationOpen || reply.Operation > lastOperation || correlation == 0 ||
		reply.Status > lastStatus || len(reply.Name) > MaxInlineName || nameSize > ramfs.MaxName || len(reply.Data) > MaxReplyData ||
		replyHeader+len(reply.Name)+len(reply.Data) > ipc.MaxPayload || reply.AttachedName != 0 && (len(reply.Name) != 0 || int(reply.AttachedName) <= MaxInlineName) {
		return ipc.Message{}, false
	}
	message := ipc.Message{ProtocolID: ProtocolID, Version: ProtocolVersion, Kind: ipc.KindReply, Correlation: correlation,
		PayloadSize: uint16(replyHeader + len(reply.Name) + len(reply.Data))}
	message.Payload[0] = byte(reply.Operation)
	message.Payload[1] = byte(reply.Status)
	message.Payload[2] = byte(reply.Kind)
	message.Payload[3] = byte(nameSize)
	binary.LittleEndian.PutUint16(message.Payload[4:], uint16(len(reply.Data)))
	if reply.AttachedName != 0 {
		message.Payload[6] = flagNameAttached
	}
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
	attached := message.Payload[6] == flagNameAttached
	inlineName := nameSize
	if attached {
		inlineName = 0
	}
	if operation < OperationOpen || operation > lastOperation || status > lastStatus ||
		kind > ramfs.KindDirectory || inlineName > MaxInlineName || dataSize > MaxReplyData ||
		message.Payload[6]&^flagNameAttached != 0 || message.Payload[7] != 0 || binary.LittleEndian.Uint32(message.Payload[28:]) != 0 ||
		attached && nameSize <= MaxInlineName || replyHeader+inlineName+dataSize != int(message.PayloadSize) {
		return Reply{}, false
	}
	reply := Reply{Operation: operation, Status: status, Kind: kind,
		Handle: Handle(binary.LittleEndian.Uint64(message.Payload[8:])),
		Size:   binary.LittleEndian.Uint64(message.Payload[16:]), Cursor: binary.LittleEndian.Uint32(message.Payload[24:]),
		Name: message.Payload[replyHeader : replyHeader+inlineName],
		Data: message.Payload[replyHeader+inlineName : replyHeader+inlineName+dataSize]}
	if attached {
		reply.AttachedName = uint8(nameSize)
	}
	return reply, true
}

// EncodeRequest encodes a request. One whose paths do not fit the message
// must be marked PathsAttached, and the caller attaches the object holding
// them.
func EncodeRequest(request Request, correlation uint64) (ipc.Message, bool) {
	if request.Operation < OperationOpen || request.Operation > lastOperation || correlation == 0 ||
		len(request.Path) > limits.PathMax || len(request.Target) > limits.PathMax || len(request.Data) > MaxProtocolData ||
		request.NameReturn && request.Operation != OperationReadDir || request.PathsAttached && request.Operation.Bulk() {
		return ipc.Message{}, false
	}
	inline := len(request.Path) + len(request.Target)
	flags := uint32(0)
	if request.PathsAttached {
		inline, flags = 0, flagPathsAttached
	}
	if request.NameReturn {
		flags |= flagNameReturn
	}
	if protocolHeader+inline+len(request.Data) > ipc.MaxPayload {
		return ipc.Message{}, false
	}
	message := ipc.Message{ProtocolID: ProtocolID, Version: ProtocolVersion, Kind: ipc.KindRequest, Correlation: correlation, PayloadSize: uint16(protocolHeader + inline + len(request.Data))}
	message.Payload[0] = byte(request.Operation)
	message.Payload[1] = byte(request.Rights)
	binary.LittleEndian.PutUint16(message.Payload[2:], uint16(len(request.Path)))
	binary.LittleEndian.PutUint16(message.Payload[4:], uint16(len(request.Data)))
	binary.LittleEndian.PutUint16(message.Payload[6:], uint16(len(request.Target)))
	binary.LittleEndian.PutUint64(message.Payload[8:], uint64(request.Handle))
	binary.LittleEndian.PutUint64(message.Payload[16:], request.Offset)
	binary.LittleEndian.PutUint32(message.Payload[24:], request.Size)
	binary.LittleEndian.PutUint32(message.Payload[28:], flags)
	offset := protocolHeader
	if !request.PathsAttached {
		offset += copy(message.Payload[offset:], request.Path)
		offset += copy(message.Payload[offset:], request.Target)
	}
	copy(message.Payload[offset:], request.Data)
	return message, true
}

// DecodeRequest validates a request. Its path, target, and data alias the
// message; a request whose paths are attached has neither until TakePaths.
func DecodeRequest(message ipc.Message) (Request, bool) {
	if message.ProtocolID != ProtocolID || message.Version != ProtocolVersion || message.Kind != ipc.KindRequest || message.Correlation == 0 || message.PayloadSize < protocolHeader {
		return Request{}, false
	}
	pathSize := int(binary.LittleEndian.Uint16(message.Payload[2:]))
	dataSize := int(binary.LittleEndian.Uint16(message.Payload[4:]))
	targetSize := int(binary.LittleEndian.Uint16(message.Payload[6:]))
	flags := binary.LittleEndian.Uint32(message.Payload[28:])
	attached := flags&flagPathsAttached != 0
	inline := pathSize + targetSize
	if attached {
		inline = 0
	}
	if flags&^(flagPathsAttached|flagNameReturn) != 0 || pathSize > limits.PathMax || targetSize > limits.PathMax || dataSize > MaxProtocolData ||
		protocolHeader+inline+dataSize != int(message.PayloadSize) || attached && protocolHeader+pathSize+targetSize+dataSize <= ipc.MaxPayload {
		return Request{}, false
	}
	operation := Operation(message.Payload[0])
	rights := Rights(message.Payload[1])
	if operation < OperationOpen || operation > lastOperation || rights&^(Read|Write) != 0 || flags&flagNameReturn != 0 && operation != OperationReadDir || attached && operation.Bulk() {
		return Request{}, false
	}
	request := Request{
		Operation:     operation,
		Rights:        rights,
		Handle:        Handle(binary.LittleEndian.Uint64(message.Payload[8:])),
		Offset:        binary.LittleEndian.Uint64(message.Payload[16:]),
		Size:          binary.LittleEndian.Uint32(message.Payload[24:]),
		PathsAttached: attached,
		NameReturn:    flags&flagNameReturn != 0,
		pathSize:      pathSize,
		targetSize:    targetSize,
		Data:          message.Payload[protocolHeader+inline : protocolHeader+inline+dataSize],
	}
	if !attached {
		request.Path = message.Payload[protocolHeader : protocolHeader+pathSize]
		request.Target = message.Payload[protocolHeader+pathSize : protocolHeader+inline]
	}
	return request, true
}

// Attaches reports whether a request carries exactly one handle: a bulk
// operation's object, or the object holding its paths or receiving a name.
func (request Request) Attaches() bool {
	return request.Operation.Bulk() || request.PathsAttached || request.NameReturn
}
