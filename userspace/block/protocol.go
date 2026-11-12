// Package block defines the bounded service protocol for byte-addressable block
// storage. Hardware drivers remain behind the service Backend boundary.
package block

import (
	"encoding/binary"
	"goos/kernel/clock"
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/userspace/partition"
)

const (
	ProtocolID = uint64(0x47424c4b30303100)
	// Version 2 adds the bulk operations (ADR 0107).
	Version    = uint16(2)
	headerSize = 24
	// RequestDeadline bounds one storage exchange. A stalled peer must surface
	// an I/O error rather than block the caller indefinitely.
	RequestDeadline = 5 * clock.TicksPerSecond
	MaxData         = ipc.MaxPayload - headerSize
	// MaxBulk bounds one bulk transfer.
	MaxBulk = 1 << 20
)

type Operation uint8

const (
	OperationInfo Operation = iota + 1
	OperationRead
	OperationWrite
	OperationFlush
	// OperationReadBulk reads Length bytes into the shared-memory object
	// attached to the request, at byte Buffer of it; OperationWriteBulk
	// writes them from it. The data never travels in the messages
	// (ADR 0107).
	OperationReadBulk
	OperationWriteBulk
	lastOperation = OperationWriteBulk
)

// Valid reports whether an operation is one this protocol defines.
func (operation Operation) Valid() bool {
	return operation >= OperationInfo && operation <= lastOperation
}

// Bulk reports whether an operation moves its data through an attached
// object.
func (operation Operation) Bulk() bool {
	return operation == OperationReadBulk || operation == OperationWriteBulk
}

type Status uint8

const (
	StatusOK Status = iota
	StatusInvalidArgument
	StatusOutOfRange
	StatusReadOnly
	StatusIOError
	// StatusStale is a control-plane refusal: a resolved partition lacks the
	// pinned identity or no longer matches it.
	StatusStale
)

type Request struct {
	Operation Operation
	Offset    uint64
	Length    uint32
	Data      []byte
	// Buffer and Object are the offset in, and the handle of, the object a
	// bulk operation moves its data through.
	Buffer uint32
	Object handle.ID
}

type Reply struct {
	Operation Operation
	Status    Status
	Capacity  uint64
	BlockSize uint32
	// Writable says, in an Info reply, whether the client's slice accepts
	// writes. A filesystem service mounts read-write only when it does.
	Writable bool
	Data     []byte
}

func EncodeRequest(request Request, correlation uint64) (ipc.Message, bool) {
	var message ipc.Message
	ok := EncodeRequestInto(request, correlation, &message)
	return message, ok
}

func EncodeRequestInto(request Request, correlation uint64, message *ipc.Message) bool {
	if correlation == 0 || request.Operation < OperationInfo || request.Operation > lastOperation || !validRequest(request) {
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
	if request.Operation.Bulk() {
		binary.LittleEndian.PutUint32(message.Payload[4:8], request.Buffer)
		message.HandleCount, message.Handles[0] = 1, request.Object
	}
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
	if message.ProtocolID != ProtocolID || message.Version != Version || message.Kind != ipc.KindRequest || message.Correlation == 0 || message.PayloadSize < headerSize {
		return Request{}, false
	}
	bulk := Operation(message.Payload[0]).Bulk()
	if !zero(message.Payload[1:4]) || !bulk && !zero(message.Payload[4:8]) || message.Payload[22] != 0 || message.Payload[23] != 0 {
		return Request{}, false
	}
	if bulk != (message.HandleCount == 1) || bulk && message.Handles[0] == 0 || !bulk && message.HandleCount != 0 {
		return Request{}, false
	}
	dataSize := int(binary.LittleEndian.Uint16(message.Payload[20:22]))
	if dataSize > MaxData || headerSize+dataSize != int(message.PayloadSize) {
		return Request{}, false
	}
	request := Request{Operation: Operation(message.Payload[0]), Offset: binary.LittleEndian.Uint64(message.Payload[8:16]), Length: binary.LittleEndian.Uint32(message.Payload[16:20]), Data: message.Payload[headerSize : headerSize+dataSize]}
	if bulk {
		request.Buffer, request.Object = binary.LittleEndian.Uint32(message.Payload[4:8]), message.Handles[0]
	}
	if request.Operation < OperationInfo || request.Operation > lastOperation || !validRequest(request) {
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
	if correlation == 0 || reply.Operation < OperationInfo || reply.Operation > lastOperation || reply.Status > StatusIOError || len(reply.Data) > MaxData || reply.Operation != OperationRead && len(reply.Data) != 0 || reply.Status != StatusOK && len(reply.Data) != 0 {
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
	if reply.Writable {
		if reply.Operation != OperationInfo {
			return false
		}
		message.Payload[2] = replyWritable
	}
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
	if message.ProtocolID != ProtocolID || message.Version != Version || message.Kind != ipc.KindReply || message.Correlation == 0 || message.HandleCount != 0 || message.PayloadSize < headerSize || !zero(message.Payload[3:8]) ||
		message.Payload[2]&^replyWritable != 0 || message.Payload[2] != 0 && Operation(message.Payload[0]) != OperationInfo || message.Payload[22] != 0 || message.Payload[23] != 0 {
		return Reply{}, false
	}
	dataSize := int(binary.LittleEndian.Uint16(message.Payload[20:22]))
	reply := Reply{Operation: Operation(message.Payload[0]), Status: Status(message.Payload[1]), Capacity: binary.LittleEndian.Uint64(message.Payload[8:16]), BlockSize: binary.LittleEndian.Uint32(message.Payload[16:20]), Writable: message.Payload[2] != 0, Data: message.Payload[headerSize : headerSize+dataSize]}
	if reply.Operation < OperationInfo || reply.Operation > lastOperation || reply.Status > StatusIOError || dataSize > MaxData || headerSize+dataSize != int(message.PayloadSize) || reply.Operation != OperationRead && dataSize != 0 || reply.Status != StatusOK && dataSize != 0 {
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
	case OperationReadBulk, OperationWriteBulk:
		end := uint64(request.Buffer) + uint64(request.Length)
		return request.Length != 0 && request.Length <= MaxBulk && end <= MaxBulk*16 && len(request.Data) == 0 && request.Object != 0
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

// The control protocol replaces the endpoint this service talks to a driver
// over. A driver that has crashed leaves its peer holding a channel whose other
// side is gone; PID 1 restarts the driver, creates a fresh pair, and hands this
// service the new client side. PID 1 holds the only control endpoint, so no
// other process can redirect where storage requests go.
const (
	ControlProtocolID = uint64(0x47424c4b43)
	// Version 3 adds the volume selector to SetFilesystem (ADR 0115);
	// version 4 adds the volume slot and writable flag; version 5 pins the
	// resolved partition identity across additional-volume restarts (ADR 0222).
	ControlVersion     = uint16(5)
	operationSetDriver = byte(1)
	// operationSetFilesystem replaces the endpoint the filesystem service
	// reaches this service over, after that service restarts (ADR 0096).
	operationSetFilesystem = byte(2)
)

// The selector travels in a fixed layout after the operation byte: kind,
// index, GUID, name length and name. Fields the kind does not use must be
// zero, so a malformed request cannot be read two ways.
const (
	selectorKind     = 1
	selectorIndex    = 2
	selectorGUID     = 6
	selectorNameSize = 22
	selectorName     = 23
	selectorSize     = selectorName + partition.MaxName
	volumeSlot       = selectorSize
	volumeFlags      = selectorSize + 1
	identityScheme   = selectorSize + 2
	identityDiskGUID = identityScheme + 1
	identityPartGUID = identityDiskGUID + 16
	identityStart    = identityPartGUID + 16
	identityLength   = identityStart + 8
	identityIndex    = identityLength + 8
	identityMBRSig   = identityIndex + 4
	identityMBRType  = identityMBRSig + 4
	identityEnd      = identityMBRType + 1
	volumePayload    = identityEnd
	volumeReplySize  = 1 + identityEnd - identityScheme
	flagWritable     = 1
	// replyWritable marks an Info reply for a writable slice.
	replyWritable = 1
)

// MaxVolumeSlots is the number of filesystem endpoints the block service
// keeps, one per volume instance: slot 0 is the persistent volume, the rest
// hold additional volumes (ADR 0222).
const MaxVolumeSlots = 5

// PersistentSlot is the slot of the persistent volume.
const PersistentSlot = 0

// VolumeGrant is what a SetFilesystem request carries: the endpoint a
// filesystem service reaches this service over, the slot it fills, the
// volume it may reach, and whether it may write.
type VolumeGrant struct {
	Server   handle.ID
	Slot     uint8
	Selector partition.Selector
	// Identity is zero on the first attach. A replacement supplies the
	// identity returned for the first selection and must resolve to it again.
	Identity partition.Identity
	Writable bool
}

// SetFilesystemRequest hands the block service the writable endpoint of the
// persistent volume's filesystem service (ADR 0096, ADR 0115).
func SetFilesystemRequest(server handle.ID, selector partition.Selector, correlation uint64) (ipc.Message, bool) {
	return SetVolumeRequest(VolumeGrant{Server: server, Slot: PersistentSlot, Selector: selector, Writable: true}, correlation)
}

// SetVolumeRequest hands the block service the endpoint of one volume slot,
// replacing and closing the endpoint the slot had.
func SetVolumeRequest(grant VolumeGrant, correlation uint64) (ipc.Message, bool) {
	if grant.Server == 0 || correlation == 0 || grant.Slot >= MaxVolumeSlots || !validSelector(&grant.Selector) || !validIdentity(grant.Identity) {
		return ipc.Message{}, false
	}
	selector := &grant.Selector
	message := ipc.Message{ProtocolID: ControlProtocolID, Version: ControlVersion, Kind: ipc.KindRequest,
		Correlation: correlation, PayloadSize: volumePayload,
		Handles: [ipc.MaxAttachedHandles]handle.ID{grant.Server}, HandleCount: 1}
	message.Payload[0] = operationSetFilesystem
	message.Payload[selectorKind] = byte(selector.Kind)
	binary.LittleEndian.PutUint32(message.Payload[selectorIndex:], selector.Index)
	copy(message.Payload[selectorGUID:selectorNameSize], selector.GUID[:])
	message.Payload[selectorNameSize] = selector.NameSize
	copy(message.Payload[selectorName:], selector.Name[:selector.NameSize])
	message.Payload[volumeSlot] = grant.Slot
	if grant.Writable {
		message.Payload[volumeFlags] = flagWritable
	}
	encodeIdentity(message.Payload[identityScheme:identityEnd], grant.Identity)
	return message, true
}

func DecodeSetFilesystem(message ipc.Message) (VolumeGrant, bool) {
	var grant VolumeGrant
	selector := &grant.Selector
	if message.ProtocolID != ControlProtocolID || message.Version != ControlVersion || message.Kind != ipc.KindRequest ||
		message.Correlation == 0 || message.PayloadSize != volumePayload || message.Payload[0] != operationSetFilesystem ||
		message.HandleCount != 1 || message.Handles[0] == 0 {
		return grant, false
	}
	selector.Kind = partition.SelectorKind(message.Payload[selectorKind])
	selector.Index = binary.LittleEndian.Uint32(message.Payload[selectorIndex:])
	copy(selector.GUID[:], message.Payload[selectorGUID:selectorNameSize])
	selector.NameSize = message.Payload[selectorNameSize]
	if int(selector.NameSize) > partition.MaxName {
		return grant, false
	}
	copy(selector.Name[:], message.Payload[selectorName:selectorName+int(selector.NameSize)])
	for _, value := range message.Payload[selectorName+int(selector.NameSize) : selectorSize] {
		if value != 0 {
			return grant, false
		}
	}
	grant.Slot = message.Payload[volumeSlot]
	flags := message.Payload[volumeFlags]
	grant.Identity = decodeIdentity(message.Payload[identityScheme:identityEnd])
	if !validSelector(selector) || !validIdentity(grant.Identity) || grant.Slot >= MaxVolumeSlots || flags&^flagWritable != 0 {
		return grant, false
	}
	grant.Server, grant.Writable = message.Handles[0], flags&flagWritable != 0
	return grant, true
}

func validIdentity(identity partition.Identity) bool {
	return identity == (partition.Identity{}) || identity.Valid()
}

func encodeIdentity(data []byte, identity partition.Identity) {
	data[0] = byte(identity.Scheme)
	copy(data[1:17], identity.DiskGUID[:])
	copy(data[17:33], identity.PartitionGUID[:])
	binary.LittleEndian.PutUint64(data[33:41], identity.Start)
	binary.LittleEndian.PutUint64(data[41:49], identity.Length)
	binary.LittleEndian.PutUint32(data[49:53], identity.Index)
	binary.LittleEndian.PutUint32(data[53:57], identity.MBRSignature)
	data[57] = identity.MBRType
}

func decodeIdentity(data []byte) partition.Identity {
	identity := partition.Identity{Scheme: partition.Scheme(data[0])}
	copy(identity.DiskGUID[:], data[1:17])
	copy(identity.PartitionGUID[:], data[17:33])
	identity.Start = binary.LittleEndian.Uint64(data[33:41])
	identity.Length = binary.LittleEndian.Uint64(data[41:49])
	identity.Index = binary.LittleEndian.Uint32(data[49:53])
	identity.MBRSignature = binary.LittleEndian.Uint32(data[53:57])
	identity.MBRType = data[57]
	return identity
}

// validSelector accepts a selector whose kind is defined and whose unused
// fields are zero.
func validSelector(selector *partition.Selector) bool {
	index, guid, name := selector.Index != 0, selector.GUID != partition.GUID{}, selector.NameSize != 0
	switch selector.Kind {
	case partition.SelectAuto, partition.SelectWhole:
		return !index && !guid && !name
	case partition.SelectIndex:
		return index && !guid && !name
	case partition.SelectGUID:
		return !index && guid && !name
	case partition.SelectName:
		return !index && !guid && name && int(selector.NameSize) <= partition.MaxName
	}
	return false
}

func SetDriverRequest(driver handle.ID, correlation uint64) (ipc.Message, bool) {
	if driver == 0 || correlation == 0 {
		return ipc.Message{}, false
	}
	return ipc.Message{ProtocolID: ControlProtocolID, Version: ControlVersion, Kind: ipc.KindRequest,
		Correlation: correlation, Payload: [ipc.MaxPayload]byte{operationSetDriver}, PayloadSize: 1,
		Handles: [ipc.MaxAttachedHandles]handle.ID{driver}, HandleCount: 1}, true
}

func DecodeSetDriver(message ipc.Message) (handle.ID, bool) {
	if message.ProtocolID != ControlProtocolID || message.Version != ControlVersion || message.Kind != ipc.KindRequest ||
		message.Correlation == 0 || message.PayloadSize != 1 || message.Payload[0] != operationSetDriver ||
		message.HandleCount != 1 || message.Handles[0] == 0 {
		return 0, false
	}
	return message.Handles[0], true
}

func SetDriverReply(status Status, correlation uint64) (ipc.Message, bool) {
	if correlation == 0 || status > StatusStale {
		return ipc.Message{}, false
	}
	message := ipc.Message{ProtocolID: ControlProtocolID, Version: ControlVersion, Kind: ipc.KindReply, Correlation: correlation, PayloadSize: 1}
	message.Payload[0] = byte(status)
	return message, true
}

func DecodeSetDriverReply(message ipc.Message, correlation uint64) (Status, bool) {
	if message.ProtocolID != ControlProtocolID || message.Version != ControlVersion || message.Kind != ipc.KindReply ||
		correlation == 0 || message.Correlation != correlation || message.PayloadSize != 1 || message.HandleCount != 0 || Status(message.Payload[0]) > StatusStale {
		return 0, false
	}
	return Status(message.Payload[0]), true
}

// SetVolumeReply returns both the selection result and the physical partition
// identity that PID 1 must present when reusing this slot.
func SetVolumeReply(status Status, identity partition.Identity, correlation uint64) (ipc.Message, bool) {
	if correlation == 0 || status > StatusStale || status == StatusOK && !identity.Valid() || status != StatusOK && identity != (partition.Identity{}) {
		return ipc.Message{}, false
	}
	message := ipc.Message{ProtocolID: ControlProtocolID, Version: ControlVersion, Kind: ipc.KindReply,
		Correlation: correlation, PayloadSize: volumeReplySize}
	message.Payload[0] = byte(status)
	encodeIdentity(message.Payload[1:volumeReplySize], identity)
	return message, true
}

// DecodeSetVolumeReply rejects delayed replies from an earlier use of the
// shared control channel by requiring the exact expected correlation.
func DecodeSetVolumeReply(message ipc.Message, correlation uint64) (Status, partition.Identity, bool) {
	if message.ProtocolID != ControlProtocolID || message.Version != ControlVersion || message.Kind != ipc.KindReply ||
		correlation == 0 || message.Correlation != correlation || message.PayloadSize != volumeReplySize || message.HandleCount != 0 {
		return 0, partition.Identity{}, false
	}
	status := Status(message.Payload[0])
	identity := decodeIdentity(message.Payload[1:volumeReplySize])
	if status > StatusStale || status == StatusOK && !identity.Valid() || status != StatusOK && identity != (partition.Identity{}) {
		return 0, partition.Identity{}, false
	}
	return status, identity, true
}
