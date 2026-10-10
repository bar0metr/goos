package vfs

import (
	"encoding/binary"
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/ramfs"
)

func AddSessionWithAuthorityRequest(server handle.ID, authority Authority, correlation uint64) (ipc.Message, bool) {
	message, ok := AddSessionRequest(server, correlation)
	if !ok || !authority.Valid() {
		return ipc.Message{}, false
	}
	message.PayloadSize = 2 + MaxScopes
	if authority.Confined {
		message.Payload[1] = 1
	}
	for slot, rights := range authority.Grants {
		message.Payload[2+slot] = byte(rights)
	}
	return message, true
}

func DecodeAddSessionAuthority(message ipc.Message) (handle.ID, Authority, bool) {
	var authority Authority
	if message.ProtocolID != ControlProtocolID || message.Version != ControlVersion || message.Kind != ipc.KindRequest ||
		message.Correlation == 0 || message.HandleCount != 1 || message.Handles[0] == 0 || message.Payload[0] != operationAddSession {
		return 0, authority, false
	}
	if message.PayloadSize == 1 {
		return message.Handles[0], authority, true
	}
	if message.PayloadSize != 2+MaxScopes || message.Payload[1] > 1 {
		return 0, authority, false
	}
	authority.Confined = message.Payload[1] == 1
	for slot := range authority.Grants {
		authority.Grants[slot] = Rights(message.Payload[2+slot])
	}
	return message.Handles[0], authority, authority.Valid()
}

const scopeControlHeader = 6

// ScopeControl carries one PID 1 scope installation. A long path travels in
// the sole attached read-only memory object and must be copied before use.
type ScopeControl struct {
	Slot     uint8
	Subtree  bool
	Denied   Rights
	Path     []byte
	PathSize uint16
	Attached bool
}

func validScopePath(path []byte) bool {
	if len(path) == 0 || len(path) > MaxPath || path[0] != '/' {
		return false
	}
	component := 0
	for _, value := range path {
		if value == 0 {
			return false
		}
		if value == '/' {
			component = 0
		} else {
			component++
			if component > ramfs.MaxName {
				return false
			}
		}
	}
	return true
}

// InstallScopeRequest uses object only when path does not fit inline. The
// caller puts path at offset zero in that memory object.
func InstallScopeRequest(slot uint8, path []byte, subtree bool, denied Rights, object handle.ID, correlation uint64) (ipc.Message, bool) {
	if slot >= MaxScopes || !validScopePath(path) || denied & ^(Read|Write) != 0 || correlation == 0 {
		return ipc.Message{}, false
	}
	attached := len(path) > ipc.MaxPayload-scopeControlHeader
	if attached != (object != 0) {
		return ipc.Message{}, false
	}
	message := ipc.Message{ProtocolID: ControlProtocolID, Version: ControlVersion, Kind: ipc.KindRequest, Correlation: correlation}
	message.Payload[0], message.Payload[1], message.Payload[3] = operationInstallScope, slot, byte(denied)
	if subtree {
		message.Payload[2] = 1
	}
	binary.LittleEndian.PutUint16(message.Payload[4:6], uint16(len(path)))
	message.PayloadSize = scopeControlHeader
	if attached {
		message.Handles[0], message.HandleCount = object, 1
	} else {
		copy(message.Payload[scopeControlHeader:], path)
		message.PayloadSize += uint16(len(path))
	}
	return message, true
}

func DecodeInstallScope(message *ipc.Message) (ScopeControl, bool) {
	var request ScopeControl
	if message == nil || message.ProtocolID != ControlProtocolID || message.Version != ControlVersion || message.Kind != ipc.KindRequest ||
		message.Correlation == 0 || message.Payload[0] != operationInstallScope || message.PayloadSize < scopeControlHeader || message.PayloadSize > ipc.MaxPayload ||
		message.Payload[1] >= MaxScopes || message.Payload[2] > 1 || Rights(message.Payload[3]) & ^(Read|Write) != 0 {
		return request, false
	}
	request.Slot, request.Subtree, request.Denied = message.Payload[1], message.Payload[2] == 1, Rights(message.Payload[3])
	request.PathSize = binary.LittleEndian.Uint16(message.Payload[4:6])
	if request.PathSize == 0 || request.PathSize > MaxPath {
		return ScopeControl{}, false
	}
	request.Attached = int(request.PathSize) > ipc.MaxPayload-scopeControlHeader
	if request.Attached {
		if message.PayloadSize != scopeControlHeader || message.HandleCount != 1 || message.Handles[0] == 0 {
			return ScopeControl{}, false
		}
	} else {
		if message.HandleCount != 0 || int(message.PayloadSize) != scopeControlHeader+int(request.PathSize) {
			return ScopeControl{}, false
		}
		request.Path = message.Payload[scopeControlHeader:message.PayloadSize]
		if !validScopePath(request.Path) {
			return ScopeControl{}, false
		}
	}
	return request, true
}

func (request *ScopeControl) TakePath(memory []byte, target *[MaxPath]byte) bool {
	if request == nil || !request.Attached || target == nil || len(memory) < int(request.PathSize) || request.PathSize == 0 || request.PathSize > MaxPath {
		return false
	}
	copy(target[:], memory[:request.PathSize])
	request.Path = target[:request.PathSize]
	return validScopePath(request.Path)
}
