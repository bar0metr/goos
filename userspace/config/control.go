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
	ControlProtocolID = uint64(0x4743464331)
	// Version 2 adds the session notice a client of a restarted service is
	// given.
	ControlVersion      = uint16(2)
	operationAddSession = byte(1)
	operationSession    = byte(2)
)

// SessionNotice hands a client of a restarted configuration service the
// client end of a new session, limited as PID 1 decided. It is a
// notification over a channel the client already holds with PID 1.
func SessionNotice(client handle.ID) (ipc.Message, bool) {
	if client == 0 {
		return ipc.Message{}, false
	}
	return ipc.Message{ProtocolID: ControlProtocolID, Version: ControlVersion, Kind: ipc.KindNotification,
		Payload: [ipc.MaxPayload]byte{operationSession}, PayloadSize: 1,
		Handles: [ipc.MaxAttachedHandles]handle.ID{client}, HandleCount: 1}, true
}

// DecodeSessionNotice returns the session endpoint a notice carries, as the
// receiver's handle.
func DecodeSessionNotice(message *ipc.Message) (handle.ID, bool) {
	if message.ProtocolID != ControlProtocolID || message.Version != ControlVersion || message.Kind != ipc.KindNotification ||
		message.Correlation != 0 || message.PayloadSize != 1 || message.Payload[0] != operationSession ||
		message.HandleCount != 1 || message.Handles[0] == 0 {
		return 0, false
	}
	return message.Handles[0], true
}

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
