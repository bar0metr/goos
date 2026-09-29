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
	// object, and gives a rename's target a field of its own (ADR 0110);
	// version 6 adds the timeout, peer-died, and unsupported statuses; version
	// 7 widens returned-name lengths for the 1024-byte native name limit;
	// version 8 adds immutable executable preparation and page reads; version
	// 9 adds fixed-width timestamps, native attributes and mount identity;
	// version 10 adds filesystem capacity and bounded mount enumeration;
	// version 11 adds whole-file replacement (ADR 0129).
	ProtocolVersion = uint16(11)
	protocolHeader  = 40
	// MaxProtocolData is the most write data one inline request carries.
	MaxProtocolData = ipc.MaxPayload - protocolHeader
	replyHeader     = 104
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
	// OperationPrepareExecutable pins an open file against mutation and reads
	// its first Size bytes into an attached header object.
	// OperationReadExecutable reads Size bytes at Offset without changing the
	// open file's position and uses the VFS/kernel transport slot.
	OperationPrepareExecutable
	OperationReadExecutable
	OperationFilesystemInfo
	OperationReadMount
	// OperationReplace moves the regular file Path onto Target, removing the
	// file Target named in the same step, or renames it when there was none.
	OperationReplace
	lastOperation = OperationReplace
)

// Bulk reports whether an operation moves its data through an attached object
// and so carries exactly one handle.
func (operation Operation) Bulk() bool {
	return operation == OperationReadInto || operation == OperationWriteFrom || operation == OperationFillExecutable || operation == OperationPrepareExecutable
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
	MountID       MountID
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
	AttachedName uint16
	Created      ramfs.Timestamp
	Modified     ramfs.Timestamp
	Attributes   ramfs.Attributes
	// AttributesAvailable distinguishes a filesystem with no attribute model
	// from one whose native attribute set is empty.
	AttributesAvailable bool
	MountID             MountID
	Filesystem          ramfs.FilesystemInfo
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
	maxName := ramfs.MaxName
	if reply.Operation == OperationReadMount {
		maxName = limits.PathMax
	} else if reply.Operation == OperationFilesystemInfo {
		maxName = ramfs.MaxVolumeName
	}
	if reply.Operation < OperationOpen || reply.Operation > lastOperation || correlation == 0 ||
		reply.Status > lastStatus || len(reply.Name) > MaxInlineName || nameSize > maxName || len(reply.Data) > MaxReplyData ||
		replyHeader+len(reply.Name)+len(reply.Data) > ipc.MaxPayload || reply.AttachedName != 0 && (len(reply.Name) != 0 || int(reply.AttachedName) <= MaxInlineName) ||
		!reply.Created.Valid() || !reply.Modified.Valid() || reply.Attributes > (ramfs.LastAttribute*2-1) || !reply.AttributesAvailable && reply.Attributes != 0 || !reply.Filesystem.Valid() ||
		reply.Status == StatusOK && (reply.Operation == OperationFilesystemInfo || reply.Operation == OperationReadMount) && reply.Filesystem.Type == 0 {
		return ipc.Message{}, false
	}
	message := ipc.Message{ProtocolID: ProtocolID, Version: ProtocolVersion, Kind: ipc.KindReply, Correlation: correlation,
		PayloadSize: uint16(replyHeader + len(reply.Name) + len(reply.Data))}
	message.Payload[0] = byte(reply.Operation)
	message.Payload[1] = byte(reply.Status)
	message.Payload[2] = byte(reply.Kind)
	binary.LittleEndian.PutUint16(message.Payload[3:], uint16(nameSize))
	binary.LittleEndian.PutUint16(message.Payload[5:], uint16(len(reply.Data)))
	if reply.AttachedName != 0 {
		message.Payload[7] = flagNameAttached
	}
	binary.LittleEndian.PutUint64(message.Payload[8:], uint64(reply.Handle))
	binary.LittleEndian.PutUint64(message.Payload[16:], reply.Size)
	binary.LittleEndian.PutUint32(message.Payload[24:], reply.Cursor)
	binary.LittleEndian.PutUint32(message.Payload[28:], 0)
	binary.LittleEndian.PutUint64(message.Payload[32:], uint64(reply.Created.Seconds))
	binary.LittleEndian.PutUint64(message.Payload[40:], uint64(reply.Modified.Seconds))
	binary.LittleEndian.PutUint32(message.Payload[48:], reply.Created.Nanoseconds)
	binary.LittleEndian.PutUint32(message.Payload[52:], reply.Modified.Nanoseconds)
	message.Payload[56] = byte(reply.Attributes)
	if reply.Created.Available {
		message.Payload[57] |= 1
	}
	if reply.Modified.Available {
		message.Payload[57] |= 2
	}
	if reply.AttributesAvailable {
		message.Payload[57] |= 4
	}
	binary.LittleEndian.PutUint64(message.Payload[64:], uint64(reply.MountID))
	binary.LittleEndian.PutUint64(message.Payload[72:], reply.Filesystem.TotalBytes)
	binary.LittleEndian.PutUint64(message.Payload[80:], reply.Filesystem.FreeBytes)
	binary.LittleEndian.PutUint64(message.Payload[88:], reply.Filesystem.AvailableBytes)
	message.Payload[96] = byte(reply.Filesystem.Type)
	if reply.Filesystem.ReadOnly {
		message.Payload[97] |= 1
	}
	if reply.Filesystem.CapacityAvailable {
		message.Payload[97] |= 2
	}
	copy(message.Payload[replyHeader:], reply.Name)
	copy(message.Payload[replyHeader+len(reply.Name):], reply.Data)
	return message, true
}

func DecodeReply(message ipc.Message) (Reply, bool) {
	return DecodeReplyFrom(&message)
}

// DecodeReplyFrom validates a message without copying it. Kernel fault paths
// use the pointer form so reply slices refer to fixed storage rather than a
// heap-escaped parameter copy.
func DecodeReplyFrom(message *ipc.Message) (Reply, bool) {
	if message == nil {
		return Reply{}, false
	}
	if message.ProtocolID != ProtocolID || message.Version != ProtocolVersion || message.Kind != ipc.KindReply ||
		message.Correlation == 0 || message.PayloadSize < replyHeader {
		return Reply{}, false
	}
	operation := Operation(message.Payload[0])
	status := Status(message.Payload[1])
	kind := ramfs.Kind(message.Payload[2])
	nameSize := int(binary.LittleEndian.Uint16(message.Payload[3:]))
	dataSize := int(binary.LittleEndian.Uint16(message.Payload[5:]))
	attached := message.Payload[7] == flagNameAttached
	inlineName := nameSize
	if attached {
		inlineName = 0
	}
	maxName := ramfs.MaxName
	if operation == OperationReadMount {
		maxName = limits.PathMax
	} else if operation == OperationFilesystemInfo {
		maxName = ramfs.MaxVolumeName
	}
	if operation < OperationOpen || operation > lastOperation || status > lastStatus ||
		kind > ramfs.KindDirectory || inlineName > MaxInlineName || dataSize > MaxReplyData ||
		nameSize > maxName || message.Payload[7]&^flagNameAttached != 0 || message.Payload[57]&^byte(7) != 0 || binary.LittleEndian.Uint16(message.Payload[58:]) != 0 || binary.LittleEndian.Uint32(message.Payload[60:]) != 0 ||
		message.Payload[97]&^byte(3) != 0 || binary.LittleEndian.Uint16(message.Payload[98:]) != 0 || binary.LittleEndian.Uint32(message.Payload[100:]) != 0 || attached && nameSize <= MaxInlineName || replyHeader+inlineName+dataSize != int(message.PayloadSize) {
		return Reply{}, false
	}
	metadata := message.Payload[57]
	reply := Reply{Operation: operation, Status: status, Kind: kind,
		Handle: Handle(binary.LittleEndian.Uint64(message.Payload[8:])),
		Size:   binary.LittleEndian.Uint64(message.Payload[16:]), Cursor: binary.LittleEndian.Uint32(message.Payload[24:]),
		MountID:    MountID(binary.LittleEndian.Uint64(message.Payload[64:])),
		Created:    ramfs.Timestamp{Seconds: int64(binary.LittleEndian.Uint64(message.Payload[32:])), Nanoseconds: binary.LittleEndian.Uint32(message.Payload[48:]), Available: metadata&1 != 0},
		Modified:   ramfs.Timestamp{Seconds: int64(binary.LittleEndian.Uint64(message.Payload[40:])), Nanoseconds: binary.LittleEndian.Uint32(message.Payload[52:]), Available: metadata&2 != 0},
		Attributes: ramfs.Attributes(message.Payload[56]), AttributesAvailable: metadata&4 != 0,
		Filesystem: ramfs.FilesystemInfo{Type: ramfs.FilesystemType(message.Payload[96]), TotalBytes: binary.LittleEndian.Uint64(message.Payload[72:]), FreeBytes: binary.LittleEndian.Uint64(message.Payload[80:]), AvailableBytes: binary.LittleEndian.Uint64(message.Payload[88:]), ReadOnly: message.Payload[97]&1 != 0, CapacityAvailable: message.Payload[97]&2 != 0},
		Name:       message.Payload[replyHeader : replyHeader+inlineName],
		Data:       message.Payload[replyHeader+inlineName : replyHeader+inlineName+dataSize]}
	if attached {
		reply.AttachedName = uint16(nameSize)
	}
	if reply.Operation == OperationFilesystemInfo && !reply.Filesystem.SetVolumeName(reply.Name) {
		return Reply{}, false
	}
	if !reply.Created.Valid() || !reply.Modified.Valid() || reply.Attributes > (ramfs.LastAttribute*2-1) || !reply.AttributesAvailable && reply.Attributes != 0 || !reply.Filesystem.Valid() ||
		reply.Status == StatusOK && (reply.Operation == OperationFilesystemInfo || reply.Operation == OperationReadMount) && reply.Filesystem.Type == 0 {
		return Reply{}, false
	}
	return reply, true
}

// EncodeRequest encodes a request. One whose paths do not fit the message
// must be marked PathsAttached, and the caller attaches the object holding
// them.
func EncodeRequest(request Request, correlation uint64) (ipc.Message, bool) {
	if request.Operation < OperationOpen || request.Operation > lastOperation || correlation == 0 ||
		len(request.Path) > limits.PathMax || len(request.Target) > limits.PathMax || len(request.Data) > MaxProtocolData ||
		request.NameReturn && request.Operation != OperationReadDir && request.Operation != OperationReadMount || request.PathsAttached && request.Operation.Bulk() {
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
	binary.LittleEndian.PutUint64(message.Payload[32:], uint64(request.MountID))
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
	if operation < OperationOpen || operation > lastOperation || rights&^(Read|Write) != 0 || flags&flagNameReturn != 0 && operation != OperationReadDir && operation != OperationReadMount || attached && operation.Bulk() {
		return Request{}, false
	}
	request := Request{
		Operation:     operation,
		Rights:        rights,
		Handle:        Handle(binary.LittleEndian.Uint64(message.Payload[8:])),
		Offset:        binary.LittleEndian.Uint64(message.Payload[16:]),
		Size:          binary.LittleEndian.Uint32(message.Payload[24:]),
		MountID:       MountID(binary.LittleEndian.Uint64(message.Payload[32:])),
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
