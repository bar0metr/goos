package vfs

import (
	"encoding/binary"

	"goos/kernel/handle"
	"goos/kernel/ipc"
)

const (
	ControlProtocolID = uint64(0x4756465343544c31)
	// Version 9 adds the attach generation and fence to SetFilesystem and
	// the notice that a filesystem service must be replaced.
	ControlVersion      = uint16(9)
	operationAddSession = byte(1)
	operationSync       = byte(2)
	// operationSetFilesystem hands the service the client end of a channel to
	// the persistent filesystem service, at its first start and after every
	// restart (ADR 0096).
	operationSetFilesystem = byte(3)
	// operationSession hands a client the endpoint of a new session after the
	// service was restarted and its old session died with it.
	operationSession = byte(4)
	// operationUnmount is accepted only on the control endpoint held by PID 1.
	operationUnmount = byte(5)
	// operationPublish mounts the installed system and applications of the
	// persistent volume at /system and /apps; operationWithdraw removes them
	// again (ADR 0175).
	operationPublish      = byte(6)
	operationWithdraw     = byte(7)
	operationInstallScope = byte(8)
	// operationFilesystemLost is the VFS telling PID 1 that the filesystem
	// service it attached last left an exchange unanswered.
	operationFilesystemLost = byte(9)
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

// FilesystemAttach is what PID 1 hands the VFS with a filesystem service.
// Generation names this attachment; a later replacement notice carries it.
// Fenced states that every earlier filesystem service instance, the only
// other processes that mapped the file cache, has exited as the kernel
// reported, so none can still write the cache.
type FilesystemAttach struct {
	Client     handle.ID
	Generation uint64
	Fenced     bool
}

// setFilesystemFenced is the one flag a SetFilesystem request may carry.
const setFilesystemFenced = byte(1)

// setFilesystemPayload is the operation, the flags and the generation.
const setFilesystemPayload = 10

func SetFilesystemRequest(attach FilesystemAttach, correlation uint64) (ipc.Message, bool) {
	if attach.Client == 0 || attach.Generation == 0 || correlation == 0 {
		return ipc.Message{}, false
	}
	message := ipc.Message{ProtocolID: ControlProtocolID, Version: ControlVersion, Kind: ipc.KindRequest,
		Correlation: correlation, PayloadSize: setFilesystemPayload,
		Handles: [ipc.MaxAttachedHandles]handle.ID{attach.Client}, HandleCount: 1}
	message.Payload[0] = operationSetFilesystem
	if attach.Fenced {
		message.Payload[1] = setFilesystemFenced
	}
	binary.LittleEndian.PutUint64(message.Payload[2:], attach.Generation)
	return message, true
}

func DecodeSetFilesystem(message ipc.Message) (FilesystemAttach, bool) {
	if message.ProtocolID != ControlProtocolID || message.Version != ControlVersion || message.Kind != ipc.KindRequest || message.Correlation == 0 ||
		message.PayloadSize != setFilesystemPayload || message.Payload[0] != operationSetFilesystem || message.HandleCount != 1 ||
		message.Payload[1]&^setFilesystemFenced != 0 {
		return FilesystemAttach{}, false
	}
	attach := FilesystemAttach{Client: message.Handles[0], Generation: binary.LittleEndian.Uint64(message.Payload[2:]),
		Fenced: message.Payload[1] == setFilesystemFenced}
	return attach, attach.Client != 0 && attach.Generation != 0
}

// FilesystemLostNotice tells PID 1 that the filesystem service of the named
// attachment left an accepted exchange unanswered. The VFS has detached it
// and keeps the file cache quarantined until a fenced attach. It is a
// notification: the kernel accepts a reply only for a pending request.
func FilesystemLostNotice(generation uint64) (ipc.Message, bool) {
	if generation == 0 {
		return ipc.Message{}, false
	}
	message := ipc.Message{ProtocolID: ControlProtocolID, Version: ControlVersion, Kind: ipc.KindNotification, PayloadSize: 9}
	message.Payload[0] = operationFilesystemLost
	binary.LittleEndian.PutUint64(message.Payload[1:], generation)
	return message, true
}

// DecodeFilesystemLostNotice returns the attachment generation a notice names.
func DecodeFilesystemLostNotice(message *ipc.Message) (uint64, bool) {
	if message.ProtocolID != ControlProtocolID || message.Version != ControlVersion || message.Kind != ipc.KindNotification ||
		message.Correlation != 0 || message.PayloadSize != 9 || message.Payload[0] != operationFilesystemLost || message.HandleCount != 0 {
		return 0, false
	}
	generation := binary.LittleEndian.Uint64(message.Payload[1:])
	return generation, generation != 0
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

// UnmountRequest asks the VFS to flush and detach persistent storage.
func UnmountRequest(correlation uint64) (ipc.Message, bool) {
	if correlation == 0 {
		return ipc.Message{}, false
	}
	message := ipc.Message{ProtocolID: ControlProtocolID, Version: ControlVersion, Kind: ipc.KindRequest, Correlation: correlation, PayloadSize: 1}
	message.Payload[0] = operationUnmount
	return message, true
}

func IsUnmount(message ipc.Message) bool {
	return message.ProtocolID == ControlProtocolID && message.Version == ControlVersion && message.Kind == ipc.KindRequest &&
		message.Correlation != 0 && message.PayloadSize == 1 && message.Payload[0] == operationUnmount && message.HandleCount == 0
}

// PublishRequest asks the VFS to mount the protected system directory of the
// persistent volume at /system and its applications at /apps.
func PublishRequest(correlation uint64) (ipc.Message, bool) {
	return plainRequest(operationPublish, correlation)
}

func IsPublish(message ipc.Message) bool { return isPlainRequest(message, operationPublish) }

// WithdrawRequest asks the VFS to remove /system and /apps.
func WithdrawRequest(correlation uint64) (ipc.Message, bool) {
	return plainRequest(operationWithdraw, correlation)
}

func IsWithdraw(message ipc.Message) bool { return isPlainRequest(message, operationWithdraw) }

func plainRequest(operation byte, correlation uint64) (ipc.Message, bool) {
	if correlation == 0 {
		return ipc.Message{}, false
	}
	message := ipc.Message{ProtocolID: ControlProtocolID, Version: ControlVersion, Kind: ipc.KindRequest, Correlation: correlation, PayloadSize: 1}
	message.Payload[0] = operation
	return message, true
}

func isPlainRequest(message ipc.Message, operation byte) bool {
	return message.ProtocolID == ControlProtocolID && message.Version == ControlVersion && message.Kind == ipc.KindRequest &&
		message.Correlation != 0 && message.PayloadSize == 1 && message.Payload[0] == operation && message.HandleCount == 0
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
	if correlation == 0 || status > lastStatus {
		return ipc.Message{}, false
	}
	message := ipc.Message{ProtocolID: ControlProtocolID, Version: ControlVersion, Kind: ipc.KindReply, Correlation: correlation, PayloadSize: 1}
	message.Payload[0] = byte(status)
	return message, true
}

func DecodeAddSessionReply(message ipc.Message) (Status, bool) {
	if message.ProtocolID != ControlProtocolID || message.Version != ControlVersion || message.Kind != ipc.KindReply || message.Correlation == 0 || message.PayloadSize != 1 || message.HandleCount != 0 || Status(message.Payload[0]) > lastStatus {
		return 0, false
	}
	return Status(message.Payload[0]), true
}
