package config

import (
	"goos/kernel/handle"
	"goos/kernel/ipc"
)

// The control protocol is how sessions come into being. PID 1 holds the only
// client endpoint, so it is the only process that can create a session, and the
// scope and rights travel in the request: a client receives an endpoint that is
// already limited and has no way to ask for more.
const (
	ControlProtocolID   = uint64(0x4743464331)
	ControlVersion      = uint16(1)
	operationAddSession = byte(1)
)

// AddSessionRequest asks the service to serve a new session on the attached
// server endpoint, confined to scope and limited to rights.
func AddSessionRequest(server handle.ID, scope Scope, rights Rights, correlation uint64) (ipc.Message, bool) {
	if server == 0 || correlation == 0 || rights == 0 || rights & ^(Read|Write) != 0 || int(scope.size) > MaxKey {
		return ipc.Message{}, false
	}
	message := ipc.Message{ProtocolID: ControlProtocolID, Version: ControlVersion, Kind: ipc.KindRequest,
		Correlation: correlation, PayloadSize: uint16(3 + int(scope.size)),
		Handles: [ipc.MaxAttachedHandles]handle.ID{server}, HandleCount: 1}
	message.Payload[0] = operationAddSession
	message.Payload[1] = byte(rights)
	message.Payload[2] = scope.size
	copy(message.Payload[3:], scope.prefix[:scope.size])
	return message, true
}

func DecodeAddSession(message ipc.Message) (handle.ID, Scope, Rights, bool) {
	if message.ProtocolID != ControlProtocolID || message.Version != ControlVersion || message.Kind != ipc.KindRequest ||
		message.Correlation == 0 || message.PayloadSize < 3 || message.Payload[0] != operationAddSession ||
		message.HandleCount != 1 || message.Handles[0] == 0 {
		return 0, Scope{}, 0, false
	}
	rights := Rights(message.Payload[1])
	size := int(message.Payload[2])
	if rights == 0 || rights & ^(Read|Write) != 0 || size > MaxKey || 3+size != int(message.PayloadSize) {
		return 0, Scope{}, 0, false
	}
	var scope Scope
	if size != 0 {
		if !validScope(string(message.Payload[3 : 3+size])) {
			return 0, Scope{}, 0, false
		}
		scope.size = uint8(copy(scope.prefix[:], message.Payload[3:3+size]))
	}
	return message.Handles[0], scope, rights, true
}

func AddSessionReply(status Status, correlation uint64) (ipc.Message, bool) {
	if correlation == 0 || status > StatusReadOnly {
		return ipc.Message{}, false
	}
	message := ipc.Message{ProtocolID: ControlProtocolID, Version: ControlVersion, Kind: ipc.KindReply, Correlation: correlation, PayloadSize: 1}
	message.Payload[0] = byte(status)
	return message, true
}

func DecodeAddSessionReply(message ipc.Message) (Status, bool) {
	if message.ProtocolID != ControlProtocolID || message.Version != ControlVersion || message.Kind != ipc.KindReply ||
		message.Correlation == 0 || message.PayloadSize != 1 || message.HandleCount != 0 || Status(message.Payload[0]) > StatusReadOnly {
		return 0, false
	}
	return Status(message.Payload[0]), true
}
