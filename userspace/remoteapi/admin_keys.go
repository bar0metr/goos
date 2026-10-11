package remoteapi

import (
	"encoding/binary"
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/userspace/remotepolicy"
)

func validPublicAdmin(op AdminOperation) bool {
	return op == AdminStatus || op == AdminReload || op == AdminKeys || op == AdminKeyAdd || op == AdminKeyRemove
}

// Key arguments travel in ordered fragments, not an unbounded file upload.
// Notifications retain a request correlation in their payload; only the last
// fragment opens an IPC request/reply correlation.
func EncodeKeyRequestPart(op AdminOperation, argument []byte, offset int, corr uint64, out *ipc.Message) bool {
	if corr == 0 || op != AdminKeyAdd && op != AdminKeyRemove || len(argument) == 0 || len(argument) > remotepolicy.MaxAuthorizedKeyLine || offset < 0 || offset >= len(argument) {
		return false
	}
	n := min(len(argument)-offset, MaxStatus)
	more := offset+n < len(argument)
	*out = ipc.Message{ProtocolID: AdminProtocolID, Version: AdminVersion, Kind: ipc.KindRequest, Correlation: corr, PayloadSize: uint16(16 + n)}
	out.Payload[0] = byte(op)
	if more {
		out.Payload[1] = 1
		out.Kind = ipc.KindNotification
		out.Correlation = 0
	}
	binary.LittleEndian.PutUint16(out.Payload[2:4], uint16(len(argument)))
	binary.LittleEndian.PutUint16(out.Payload[4:6], uint16(offset))
	binary.LittleEndian.PutUint64(out.Payload[8:16], corr)
	copy(out.Payload[16:], argument[offset:offset+n])
	return true
}

type KeyRequestBuffer struct {
	data        [remotepolicy.MaxAuthorizedKeyLine]byte
	used, total int
	correlation uint64
	operation   AdminOperation
}

func (b *KeyRequestBuffer) Pending() bool { return b.used != 0 }
func (b *KeyRequestBuffer) Reset() {
	clear(b.data[:])
	b.used, b.total, b.correlation, b.operation = 0, 0, 0, 0
}

// Accept fails closed on duplicate, reordered, interleaved or oversized parts.
// The returned argument is borrowed until the buffer is reset.
func (b *KeyRequestBuffer) Accept(in *ipc.Message) (AdminOperation, []byte, uint64, bool, bool) {
	fail := func() (AdminOperation, []byte, uint64, bool, bool) { b.Reset(); return 0, nil, 0, false, false }
	if in.ProtocolID != AdminProtocolID || in.Version != AdminVersion || in.HandleCount != 0 || in.PayloadSize <= 16 || in.PayloadSize > ipc.MaxPayload || !zeros(in.Payload[6:8]) || in.Payload[1] > 1 {
		return fail()
	}
	op := AdminOperation(in.Payload[0])
	if op != AdminKeyAdd && op != AdminKeyRemove {
		return fail()
	}
	total, offset := int(binary.LittleEndian.Uint16(in.Payload[2:4])), int(binary.LittleEndian.Uint16(in.Payload[4:6]))
	corr := binary.LittleEndian.Uint64(in.Payload[8:16])
	more := in.Payload[1] == 1
	n := int(in.PayloadSize) - 16
	if corr == 0 || total == 0 || total > len(b.data) || offset+n > total || more != (offset+n < total) || n != min(total-offset, MaxStatus) {
		return fail()
	}
	if more && (in.Kind != ipc.KindNotification || in.Correlation != 0) || !more && (in.Kind != ipc.KindRequest || in.Correlation != corr) {
		return fail()
	}
	if b.used == 0 {
		if offset != 0 {
			return fail()
		}
		b.total, b.correlation, b.operation = total, corr, op
	}
	if b.used != offset || b.total != total || b.correlation != corr || b.operation != op {
		return fail()
	}
	copy(b.data[offset:], in.Payload[16:in.PayloadSize])
	b.used += n
	return op, b.data[:b.used], corr, !more, true
}

const KeyControlProtocolID = uint64(0x47524b4559435431) // GRKEYCT1
const KeyControlVersion = uint16(1)

type KeyControlOperation uint8

const (
	KeyBegin KeyControlOperation = iota + 1
	KeyCheck
	KeyEnd
)

type KeyControlReply struct {
	Operation       KeyControlOperation
	Result          Result
	Token, Revision uint64
	Path            []byte
}

func EncodeKeyControlRequest(op KeyControlOperation, token, corr uint64, out *ipc.Message) bool {
	if corr == 0 || op < KeyBegin || op > KeyEnd || (op == KeyBegin) != (token == 0) {
		return false
	}
	*out = ipc.Message{ProtocolID: KeyControlProtocolID, Version: KeyControlVersion, Kind: ipc.KindRequest, Correlation: corr, PayloadSize: 16}
	out.Payload[0] = byte(op)
	binary.LittleEndian.PutUint64(out.Payload[8:16], token)
	return true
}
func DecodeKeyControlRequest(in *ipc.Message) (KeyControlOperation, uint64, bool) {
	if in.ProtocolID != KeyControlProtocolID || in.Version != KeyControlVersion || in.Kind != ipc.KindRequest || in.PayloadSize != 16 || in.HandleCount != 0 || !zeros(in.Payload[1:8]) {
		return 0, 0, false
	}
	op, token := KeyControlOperation(in.Payload[0]), binary.LittleEndian.Uint64(in.Payload[8:16])
	var canonical ipc.Message
	return op, token, EncodeKeyControlRequest(op, token, in.Correlation, &canonical)
}
func EncodeKeyControlReply(reply KeyControlReply, files handle.ID, corr uint64, out *ipc.Message) bool {
	if corr == 0 || reply.Operation < KeyBegin || reply.Operation > KeyEnd || reply.Result > ResultPermissionDenied {
		return false
	}
	success := reply.Operation == KeyBegin && reply.Result == ResultOK
	if success {
		if files == 0 || reply.Token != corr || reply.Revision == 0 || !ValidAuthorityPath(reply.Path) {
			return false
		}
	} else if files != 0 || len(reply.Path) != 0 || reply.Revision != 0 || reply.Token != 0 {
		return false
	}
	*out = ipc.Message{ProtocolID: KeyControlProtocolID, Version: KeyControlVersion, Kind: ipc.KindReply, Correlation: corr, PayloadSize: uint16(24 + len(reply.Path))}
	out.Payload[0], out.Payload[1], out.Payload[2] = byte(reply.Operation), byte(reply.Result), byte(len(reply.Path))
	binary.LittleEndian.PutUint64(out.Payload[8:16], reply.Token)
	binary.LittleEndian.PutUint64(out.Payload[16:24], reply.Revision)
	copy(out.Payload[24:], reply.Path)
	if success {
		out.HandleCount = 1
		out.Handles[0] = files
	}
	return true
}
func DecodeKeyControlReply(in *ipc.Message, op KeyControlOperation, corr uint64) (KeyControlReply, bool) {
	if in.ProtocolID != KeyControlProtocolID || in.Version != KeyControlVersion || in.Kind != ipc.KindReply || in.Correlation != corr || in.PayloadSize < 24 || in.PayloadSize > 88 || !zeros(in.Payload[3:8]) || int(in.PayloadSize) != 24+int(in.Payload[2]) || in.Payload[0] != byte(op) || in.HandleCount > 1 {
		return KeyControlReply{}, false
	}
	reply := KeyControlReply{Operation: op, Result: Result(in.Payload[1]), Token: binary.LittleEndian.Uint64(in.Payload[8:16]), Revision: binary.LittleEndian.Uint64(in.Payload[16:24]), Path: in.Payload[24:in.PayloadSize]}
	files := handle.ID(0)
	if in.HandleCount == 1 {
		files = in.Handles[0]
	}
	var canonical ipc.Message
	ok := EncodeKeyControlReply(reply, files, corr, &canonical) && canonical.HandleCount == in.HandleCount
	return reply, ok
}
