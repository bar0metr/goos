package vfs

import (
	"encoding/binary"
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/ramfs"
)

// The flags of an AddSession request, in payload byte 1.
const (
	addSessionConfined = 1
	addSessionIdentity = 2
)

// sessionIdentitySize is the fixed inline size of a SessionIdentity:
// AccountID (uint64), UID (uint32), PrimaryGID (uint32), Epoch ([16]byte).
const sessionIdentitySize = 8 + 4 + 4 + 16

func AddSessionWithAuthorityRequest(server handle.ID, authority Authority, correlation uint64) (ipc.Message, bool) {
	return addSessionRequest(server, authority, SessionIdentity{}, correlation)
}

// AddIdentitySessionRequest hands the VFS a new session with authority and a
// trusted principal. It is the PID 1 control path an account-aware session
// uses; ordinary requests cannot install or override an identity (ADR 0234).
func AddIdentitySessionRequest(server handle.ID, authority Authority, identity SessionIdentity, correlation uint64) (ipc.Message, bool) {
	return addSessionRequest(server, authority, identity, correlation)
}

func addSessionRequest(server handle.ID, authority Authority, identity SessionIdentity, correlation uint64) (ipc.Message, bool) {
	if server == 0 || correlation == 0 || !authority.Valid() || !identity.Valid() {
		return ipc.Message{}, false
	}
	message := ipc.Message{ProtocolID: ControlProtocolID, Version: ControlVersion, Kind: ipc.KindRequest,
		Correlation: correlation, Handles: [ipc.MaxAttachedHandles]handle.ID{server}, HandleCount: 1}
	message.Payload[0] = operationAddSession
	if authority.Confined {
		message.Payload[1] |= addSessionConfined
	}
	for slot, rights := range authority.Grants {
		message.Payload[2+slot] = byte(rights)
	}
	offset := 2 + MaxScopes
	message.Payload[offset] = authority.RootSize
	offset++
	offset += copy(message.Payload[offset:], authority.Root[:authority.RootSize])
	message.PayloadSize = uint16(offset)
	if identity.AccountID != 0 {
		message.Payload[1] |= addSessionIdentity
		binary.LittleEndian.PutUint64(message.Payload[offset:], identity.AccountID)
		binary.LittleEndian.PutUint32(message.Payload[offset+8:], identity.UID)
		binary.LittleEndian.PutUint32(message.Payload[offset+12:], identity.PrimaryGID)
		copy(message.Payload[offset+16:offset+sessionIdentitySize], identity.Epoch[:])
		message.PayloadSize = uint16(offset + sessionIdentitySize)
	}
	return message, true
}

func DecodeAddSessionAuthority(message ipc.Message) (handle.ID, Authority, SessionIdentity, bool) {
	var authority Authority
	var identity SessionIdentity
	if message.ProtocolID != ControlProtocolID || message.Version != ControlVersion || message.Kind != ipc.KindRequest ||
		message.Correlation == 0 || message.HandleCount != 1 || message.Handles[0] == 0 || message.Payload[0] != operationAddSession {
		return 0, authority, identity, false
	}
	if message.PayloadSize == 1 {
		return message.Handles[0], authority, identity, message.Payload[1] == 0
	}
	if message.PayloadSize < 2+MaxScopes+1 || message.Payload[1]&^(addSessionConfined|addSessionIdentity) != 0 {
		return 0, authority, identity, false
	}
	authority.Confined = message.Payload[1]&addSessionConfined != 0
	for slot := range authority.Grants {
		authority.Grants[slot] = Rights(message.Payload[2+slot])
	}
	offset := 2 + MaxScopes
	rootSize := int(message.Payload[offset])
	offset++
	if rootSize > MaxRootPath || offset+rootSize > int(message.PayloadSize) {
		return 0, authority, identity, false
	}
	authority.RootSize = uint8(copy(authority.Root[:], message.Payload[offset:offset+rootSize]))
	offset += rootSize
	if !authority.Valid() {
		return 0, authority, identity, false
	}
	if message.Payload[1]&addSessionIdentity != 0 {
		if offset+sessionIdentitySize != int(message.PayloadSize) {
			return 0, authority, identity, false
		}
		identity.AccountID = binary.LittleEndian.Uint64(message.Payload[offset:])
		identity.UID = binary.LittleEndian.Uint32(message.Payload[offset+8:])
		identity.PrimaryGID = binary.LittleEndian.Uint32(message.Payload[offset+12:])
		copy(identity.Epoch[:], message.Payload[offset+16:offset+sessionIdentitySize])
		if !identity.Valid() {
			return 0, authority, identity, false
		}
	} else if offset != int(message.PayloadSize) {
		return 0, authority, identity, false
	}
	return message.Handles[0], authority, identity, true
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
