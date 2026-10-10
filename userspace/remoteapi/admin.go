package remoteapi

import (
	"encoding/binary"
	"goos/kernel/handle"
	"goos/kernel/ipc"
)

const AdminProtocolID = uint64(0x475241444d494e31) // GRADMIN1
const AdminVersion = uint16(2)
const MaxStatus = ipc.MaxPayload - 16
const MaxStatusParts = 4

type AdminOperation uint8

const (
	AdminStatus AdminOperation = iota + 1
	AdminReload
	AdminAddSession
	AdminCounts
)

type AdminResult uint8

const (
	AdminOK AdminResult = iota
	AdminInvalid
	AdminUnavailable
)

func EncodeAdminRequest(op AdminOperation, corr uint64, out *ipc.Message) bool {
	if corr == 0 || op < AdminStatus || op > AdminReload {
		return false
	}
	*out = ipc.Message{ProtocolID: AdminProtocolID, Version: AdminVersion, Kind: ipc.KindRequest, Correlation: corr, PayloadSize: 8}
	out.Payload[0] = byte(op)
	return true
}
func DecodeAdminRequest(in *ipc.Message) (AdminOperation, bool) {
	if in.ProtocolID != AdminProtocolID || in.Version != AdminVersion || in.Kind != ipc.KindRequest || in.Correlation == 0 || in.HandleCount != 0 || in.PayloadSize != 8 || !zeros(in.Payload[1:8]) {
		return 0, false
	}
	op := AdminOperation(in.Payload[0])
	return op, op >= AdminStatus && op <= AdminReload
}
func EncodeAdminReply(op AdminOperation, result AdminResult, text []byte, corr uint64, out *ipc.Message) bool {
	return EncodeAdminPart(op, result, text, false, corr, out)
}
func EncodeAdminPart(op AdminOperation, result AdminResult, text []byte, more bool, corr uint64, out *ipc.Message) bool {
	if op < AdminStatus || op > AdminReload || result > AdminUnavailable || len(text) > MaxStatus || corr == 0 {
		return false
	}
	*out = ipc.Message{ProtocolID: AdminProtocolID, Version: AdminVersion, Kind: ipc.KindReply, Correlation: corr, PayloadSize: uint16(16 + len(text))}
	out.Payload[0], out.Payload[1] = byte(op), byte(result)
	binary.LittleEndian.PutUint16(out.Payload[2:4], uint16(len(text)))
	binary.LittleEndian.PutUint64(out.Payload[8:16], corr)
	copy(out.Payload[16:], text)
	if more {
		// IPC consumes a pending correlation on the first reply. Only the
		// final fragment replies; notifications retain the request in payload.
		out.Kind, out.Correlation = ipc.KindNotification, 0
		out.Payload[4] = 1
	}
	return true
}
func DecodeAdminReply(in *ipc.Message, op AdminOperation, corr uint64) (AdminResult, []byte, bool) {
	result, text, more, ok := DecodeAdminPart(in, op, corr)
	return result, text, ok && !more
}
func DecodeAdminPart(in *ipc.Message, op AdminOperation, corr uint64) (AdminResult, []byte, bool, bool) {
	if in.ProtocolID != AdminProtocolID || in.Version != AdminVersion || corr == 0 || in.HandleCount != 0 || in.PayloadSize < 16 || in.PayloadSize > 16+MaxStatus || in.Payload[0] != byte(op) || in.Payload[1] > byte(AdminUnavailable) || in.Payload[4] > 1 || !zeros(in.Payload[5:8]) || int(in.PayloadSize) != 16+int(binary.LittleEndian.Uint16(in.Payload[2:4])) || binary.LittleEndian.Uint64(in.Payload[8:16]) != corr {
		return AdminUnavailable, nil, false, false
	}
	more := in.Payload[4] == 1
	if more && (in.Kind != ipc.KindNotification || in.Correlation != 0) || !more && (in.Kind != ipc.KindReply || in.Correlation != corr) {
		return AdminUnavailable, nil, false, false
	}
	return AdminResult(in.Payload[1]), in.Payload[16:in.PayloadSize], more, true
}
func EncodeAdminNotice(server handle.ID, out *ipc.Message) bool {
	if server == 0 {
		return false
	}
	*out = ipc.Message{ProtocolID: AdminProtocolID, Version: AdminVersion, Kind: ipc.KindNotification, PayloadSize: 8, HandleCount: 1}
	out.Payload[0] = byte(AdminAddSession)
	out.Handles[0] = server
	return true
}
func DecodeAdminNotice(in *ipc.Message) (handle.ID, bool) {
	if in.ProtocolID != AdminProtocolID || in.Version != AdminVersion || in.Kind != ipc.KindNotification || in.Correlation != 0 || in.PayloadSize != 8 || in.HandleCount != 1 || in.Handles[0] == 0 || in.Payload[0] != byte(AdminAddSession) || !zeros(in.Payload[1:8]) {
		return 0, false
	}
	return in.Handles[0], true
}
func EncodeCounts(pending, authenticated uint8, out *ipc.Message) bool {
	if pending > 2 || authenticated > 4 {
		return false
	}
	*out = ipc.Message{ProtocolID: AdminProtocolID, Version: AdminVersion, Kind: ipc.KindNotification, PayloadSize: 8}
	out.Payload[0], out.Payload[1], out.Payload[2] = byte(AdminCounts), pending, authenticated
	return true
}
func DecodeCounts(in *ipc.Message) (int, int, bool) {
	if in.ProtocolID != AdminProtocolID || in.Version != AdminVersion || in.Kind != ipc.KindNotification || in.Correlation != 0 || in.PayloadSize != 8 || in.HandleCount != 0 || in.Payload[0] != byte(AdminCounts) || in.Payload[1] > 2 || in.Payload[2] > 4 || !zeros(in.Payload[3:8]) {
		return 0, 0, false
	}
	return int(in.Payload[1]), int(in.Payload[2]), true
}
func AdminSessionNotice(server handle.ID) (ipc.Message, bool) {
	var out ipc.Message
	ok := EncodeAdminNotice(server, &out)
	return out, ok
}
func AdminCountsNotice(pending, authenticated uint8) (ipc.Message, bool) {
	var out ipc.Message
	ok := EncodeCounts(pending, authenticated, &out)
	return out, ok
}
func zeros(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return false
		}
	}
	return true
}
