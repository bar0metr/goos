package config

import (
	"encoding/binary"
	"goos/kernel/ipc"
	"goos/userspace/hostname"
)

// GHST is the hosts protocol. It travels on the same session endpoint as
// GCF1, told apart by its protocol ID, and is checked against the session's
// HostsRead and HostsWrite rights rather than its scope: the table is not
// part of the key namespace.
//
// A request is an 8-byte header and a name:
//
//	[0] operation  [1] name size  [2] cursor (List)  [3] zero
//	[4:8] address (Set)
//
// A reply is an 8-byte header and either the record's name or the table's
// generation, never both:
//
//	[0] operation  [1] status  [2] name size  [3] next cursor (List)
//	[4:8] address
//	[8:]  name, when the name size is not zero; else generation, uint64 LE
//
// Only a List reply that returns a record carries a name. Every other reply,
// including the one that ends a listing, carries the generation. A header and
// a MaxHostName name fill one payload exactly, which is where that limit
// comes from.
const (
	HostsProtocolID      = uint64(0x47485354)
	HostsProtocolVersion = uint16(1)
	hostsHeader          = 8
	hostsGenerationSize  = 8
)

type HostsOperation uint8

const (
	HostsList HostsOperation = iota + 1
	HostsGet
	HostsSet
	HostsRemove
)

// HostsRequest names a host as text; the service normalizes it. Address is
// used by Set and Cursor by List.
type HostsRequest struct {
	Operation HostsOperation
	Name      []byte
	Address   [4]byte
	Cursor    uint8
}

// HostsReply carries a record for Get and List. Name is set only by a List
// that returned a record, and Generation only when Name is empty.
type HostsReply struct {
	Operation  HostsOperation
	Status     Status
	Generation uint64
	Address    [4]byte
	Name       []byte
	Next       uint8
}

func validHostsOperation(operation HostsOperation) bool {
	return operation >= HostsList && operation <= HostsRemove
}

// hostsRequestValid is the shape every request must have: a List names
// nothing, everything else names one host, and only Set carries an address.
func hostsRequestValid(request *HostsRequest) bool {
	if !validHostsOperation(request.Operation) || len(request.Name) > MaxHostName {
		return false
	}
	if request.Operation == HostsList {
		return len(request.Name) == 0 && request.Address == [4]byte{}
	}
	return len(request.Name) != 0 && request.Cursor == 0 &&
		(request.Operation == HostsSet || request.Address == [4]byte{})
}

func EncodeHostsRequest(request HostsRequest, correlation uint64) (ipc.Message, bool) {
	if correlation == 0 || !hostsRequestValid(&request) {
		return ipc.Message{}, false
	}
	message := ipc.Message{ProtocolID: HostsProtocolID, Version: HostsProtocolVersion, Kind: ipc.KindRequest,
		Correlation: correlation, PayloadSize: uint16(hostsHeader + len(request.Name))}
	message.Payload[0] = byte(request.Operation)
	message.Payload[1] = byte(len(request.Name))
	message.Payload[2] = request.Cursor
	copy(message.Payload[4:8], request.Address[:])
	copy(message.Payload[hostsHeader:], request.Name)
	return message, true
}

// DecodeHostsRequest checks the request's shape. The returned name is a slice
// of the message payload, and its syntax is checked when the request is
// served, so a bad name is answered rather than dropped.
func DecodeHostsRequest(message *ipc.Message) (HostsRequest, bool) {
	if message.ProtocolID != HostsProtocolID || message.Version != HostsProtocolVersion || message.Kind != ipc.KindRequest ||
		message.Correlation == 0 || message.HandleCount != 0 || message.PayloadSize < hostsHeader ||
		message.Payload[3] != 0 {
		return HostsRequest{}, false
	}
	size := int(message.Payload[1])
	if size > MaxHostName || hostsHeader+size != int(message.PayloadSize) {
		return HostsRequest{}, false
	}
	request := HostsRequest{
		Operation: HostsOperation(message.Payload[0]),
		Name:      message.Payload[hostsHeader : hostsHeader+size],
		Cursor:    message.Payload[2],
	}
	copy(request.Address[:], message.Payload[4:8])
	if !hostsRequestValid(&request) {
		return HostsRequest{}, false
	}
	return request, true
}

// hostsReplyValid is the shape every reply must have: a name exactly when a
// List returned a record, an address only on success, a cursor only from List.
func hostsReplyValid(reply *HostsReply) bool {
	if !validHostsOperation(reply.Operation) || reply.Status > StatusReadOnly || len(reply.Name) > MaxHostName {
		return false
	}
	record := reply.Operation == HostsList && reply.Status == StatusOK
	if record != (len(reply.Name) != 0) || record && reply.Generation != 0 {
		return false
	}
	if reply.Status != StatusOK && reply.Address != [4]byte{} {
		return false
	}
	return reply.Operation == HostsList || reply.Next == 0
}

func EncodeHostsReply(reply HostsReply, correlation uint64) (ipc.Message, bool) {
	if correlation == 0 || !hostsReplyValid(&reply) {
		return ipc.Message{}, false
	}
	tail := len(reply.Name)
	if tail == 0 {
		tail = hostsGenerationSize
	}
	message := ipc.Message{ProtocolID: HostsProtocolID, Version: HostsProtocolVersion, Kind: ipc.KindReply,
		Correlation: correlation, PayloadSize: uint16(hostsHeader + tail)}
	message.Payload[0] = byte(reply.Operation)
	message.Payload[1] = byte(reply.Status)
	message.Payload[2] = byte(len(reply.Name))
	message.Payload[3] = reply.Next
	copy(message.Payload[4:8], reply.Address[:])
	if len(reply.Name) != 0 {
		copy(message.Payload[hostsHeader:], reply.Name)
	} else {
		binary.LittleEndian.PutUint64(message.Payload[hostsHeader:], reply.Generation)
	}
	return message, true
}

// DecodeHostsReply returns a reply whose name is a slice of the message
// payload; the caller copies it before the message is reused.
func DecodeHostsReply(message *ipc.Message) (HostsReply, bool) {
	if message.ProtocolID != HostsProtocolID || message.Version != HostsProtocolVersion || message.Kind != ipc.KindReply ||
		message.Correlation == 0 || message.HandleCount != 0 || message.PayloadSize < hostsHeader {
		return HostsReply{}, false
	}
	size := int(message.Payload[2])
	tail := size
	if tail == 0 {
		tail = hostsGenerationSize
	}
	if size > MaxHostName || hostsHeader+tail != int(message.PayloadSize) {
		return HostsReply{}, false
	}
	reply := HostsReply{
		Operation: HostsOperation(message.Payload[0]),
		Status:    Status(message.Payload[1]),
		Next:      message.Payload[3],
	}
	copy(reply.Address[:], message.Payload[4:8])
	if size != 0 {
		reply.Name = message.Payload[hostsHeader : hostsHeader+size]
	} else {
		reply.Generation = binary.LittleEndian.Uint64(message.Payload[hostsHeader:])
	}
	if !hostsReplyValid(&reply) {
		return HostsReply{}, false
	}
	return reply, true
}

// Serve applies one hosts request on behalf of a session holding rights.
// Reading needs HostsRead and changing needs HostsWrite; the session's scope
// does not apply. A name that is not a host name, or is longer than
// MaxHostName, is StatusInvalidArgument; localhost and the names under it
// are StatusReadOnly to Set and Remove.
func (hosts *Hosts) Serve(rights Rights, request HostsRequest) HostsReply {
	reply := HostsReply{Operation: request.Operation}
	need := HostsWrite
	if request.Operation == HostsList || request.Operation == HostsGet {
		need = HostsRead
	}
	switch {
	case !validHostsOperation(request.Operation):
		reply.Status = StatusInvalidArgument
	case rights&need == 0:
		reply.Status = StatusPermissionDenied
	case request.Operation == HostsList:
		name, address, next, status := hosts.At(int(request.Cursor))
		reply.Status, reply.Next = status, uint8(next)
		if status == StatusOK {
			reply.Name, reply.Address = name, address
			return reply
		}
	default:
		name, ok := hostname.Parse(request.Name)
		if !ok || name.Size > MaxHostName {
			reply.Status = StatusInvalidArgument
			break
		}
		switch request.Operation {
		case HostsGet:
			reply.Address, reply.Status = hosts.Lookup(&name)
		case HostsSet:
			reply.Status = hosts.Set(&name, request.Address)
		case HostsRemove:
			reply.Status = hosts.Remove(&name)
		}
	}
	reply.Generation = hosts.Generation
	return reply
}
