package vfs

import (
	"goos/kernel/handle"
	"goos/kernel/ipc"
)

const (
	ControlProtocolID = uint64(0x4756465343544c31)
	// Version 5 adds the session notice a client of a restarted service is
	// given.
	ControlVersion      = uint16(5)
	operationAddSession = byte(1)
	operationSync       = byte(2)
	// operationSetFilesystem hands the service the client end of a channel to
	// the persistent filesystem service, at its first start and after every
	// restart (ADR 0096).
	operationSetFilesystem = byte(3)
	// operationSession hands a client the endpoint of a new session after the
	// service was restarted and its old session died with it.
	operationSession = byte(4)
)

// SessionNotice hands a client of a restarted VFS the client end of a new
// session. It is a notification: PID 1 sends it over a channel the client
// already holds and waits for nothing.
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

func SetFilesystemRequest(client handle.ID, correlation uint64) (ipc.Message, bool) {
	if client == 0 || correlation == 0 {
		return ipc.Message{}, false
	}
	return ipc.Message{ProtocolID: ControlProtocolID, Version: ControlVersion, Kind: ipc.KindRequest,
		Correlation: correlation, Payload: [ipc.MaxPayload]byte{operationSetFilesystem}, PayloadSize: 1,
		Handles: [ipc.MaxAttachedHandles]handle.ID{client}, HandleCount: 1}, true
}

func DecodeSetFilesystem(message ipc.Message) (handle.ID, bool) {
	if message.ProtocolID != ControlProtocolID || message.Version != ControlVersion || message.Kind != ipc.KindRequest || message.Correlation == 0 ||
		message.PayloadSize != 1 || message.Payload[0] != operationSetFilesystem || message.HandleCount != 1 {
		return 0, false
	}
	return message.Handles[0], message.Handles[0] != 0
}

// SyncRequest asks the VFS service to make every completed write durable.
func SyncRequest(correlation uint64) (ipc.Message, bool) {
	if correlation == 0 {
		return ipc.Message{}, false
	}
	message := ipc.Message{ProtocolID: ControlProtocolID, Version: ControlVersion, Kind: ipc.KindRequest, Correlation: correlation, PayloadSize: 1}
	message.Payload[0] = operationSync
	return message, true
}

func IsSync(message ipc.Message) bool {
	return message.ProtocolID == ControlProtocolID && message.Version == ControlVersion && message.Kind == ipc.KindRequest &&
		message.Correlation != 0 && message.PayloadSize == 1 && message.Payload[0] == operationSync && message.HandleCount == 0
}

func AddSessionRequest(server handle.ID, correlation uint64) (ipc.Message, bool) {
	if server == 0 || correlation == 0 {
		return ipc.Message{}, false
	}
	return ipc.Message{ProtocolID: ControlProtocolID, Version: ControlVersion, Kind: ipc.KindRequest,
		Correlation: correlation, Payload: [ipc.MaxPayload]byte{operationAddSession}, PayloadSize: 1,
		Handles: [ipc.MaxAttachedHandles]handle.ID{server}, HandleCount: 1}, true
}

func DecodeAddSession(message ipc.Message) (handle.ID, bool) {
	if message.ProtocolID != ControlProtocolID || message.Version != ControlVersion || message.Kind != ipc.KindRequest || message.Correlation == 0 || message.PayloadSize != 1 || message.Payload[0] != operationAddSession || message.HandleCount != 1 {
		return 0, false
	}
	return message.Handles[0], message.Handles[0] != 0
}

func AddSessionReply(status Status, correlation uint64) (ipc.Message, bool) {
	if correlation == 0 || status > StatusBackendError {
		return ipc.Message{}, false
	}
	message := ipc.Message{ProtocolID: ControlProtocolID, Version: ControlVersion, Kind: ipc.KindReply, Correlation: correlation, PayloadSize: 1}
	message.Payload[0] = byte(status)
	return message, true
}

func DecodeAddSessionReply(message ipc.Message) (Status, bool) {
	if message.ProtocolID != ControlProtocolID || message.Version != ControlVersion || message.Kind != ipc.KindReply || message.Correlation == 0 || message.PayloadSize != 1 || message.HandleCount != 0 || Status(message.Payload[0]) > StatusBackendError {
		return 0, false
	}
	return Status(message.Payload[0]), true
}
