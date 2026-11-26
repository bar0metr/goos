// Package remoteapi defines PID 1's remote terminal session control protocol.
package remoteapi

import (
	"encoding/binary"
	"goos/kernel/handle"
	"goos/kernel/ipc"
)

const (
	ProtocolID = uint64(0x4752454d4f544531) // GREMOTE1
	// Version 3 carried terminal/exec/SFTP session control. Version 4 adds
	// account-aware authorization: OperationAuthenticate and the new
	// OperationAuthorizeKey carry the requested username and the presented
	// 32-byte Ed25519 public key so PID 1 can revalidate against the identity
	// database at admission. Version 5 adds the bounded one-use admission
	// token PID 1 issues to the handshake authorization and consumes once at
	// signed authentication. A message of any other version fails decoding.
	Version       = uint16(5)
	MaxSessions   = 4
	MaxHandshakes = 2
	MaxWorkers    = MaxSessions + MaxHandshakes
	MaxTerm       = 64
	MaxCommand    = 4096
	requestSize   = 32
	replySize     = 24
)

type ExecutionMode uint8

const (
	ModeTerminal ExecutionMode = iota
	ModeExec
	ModeSFTP
)

type Operation uint8

const (
	OperationOpen Operation = iota + 1
	OperationClose
	OperationGeometry
	OperationAuthenticate
	OperationTerminalEnd
	OperationAuthorizeKey
)

type Result uint8

const (
	ResultOK Result = iota
	ResultInvalidArgument
	ResultLimit
	ResultNotFound
	ResultUnavailable
	ResultPermissionDenied
)

// Geometry comes from a validated SSH pty-req or window-change.
type Geometry struct {
	Sequence uint64
	Columns  uint16
	Rows     uint16
	Term     [MaxTerm]byte
	TermSize uint8
}

func MakeGeometry(columns, rows uint32, term []byte) (Geometry, bool) {
	var geometry Geometry
	if columns < 1 || columns > 1000 || rows < 1 || rows > 1000 || len(term) == 0 || len(term) > MaxTerm {
		return geometry, false
	}
	geometry.Columns, geometry.Rows = uint16(columns), uint16(rows)
	geometry.Sequence = 1
	geometry.TermSize = uint8(copy(geometry.Term[:], term))
	return geometry, geometry.Valid()
}

func (geometry *Geometry) Valid() bool {
	if geometry.Sequence == 0 || geometry.Columns < 1 || geometry.Columns > 1000 || geometry.Rows < 1 || geometry.Rows > 1000 || geometry.TermSize == 0 || int(geometry.TermSize) > len(geometry.Term) {
		return false
	}
	for _, value := range geometry.Term[:geometry.TermSize] {
		if value < 0x20 || value > 0x7e {
			return false
		}
	}
	return true
}

// Request carries no executable, launch grants, or network endpoint.
type Request struct {
	Operation   Operation
	Session     uint64
	Geometry    Geometry
	Mode        ExecutionMode
	CommandSize uint16
	Command     handle.ID
	// Username, UserSize and Key carry the requested username and the
	// presented 32-byte Ed25519 public key for OperationAuthenticate and
	// OperationAuthorizeKey. They are empty for every other operation, and the
	// worker-supplied values never choose grants: PID 1 revalidates them
	// against the identity database at admission.
	Username [MaxUsername]byte
	UserSize uint8
	Key      [32]byte
	// Token is the one-use admission token PID 1 issued to this worker's
	// handshake authorization. OperationAuthorizeKey sends zero; the signed
	// OperationAuthenticate must present the exact outstanding token, which PID 1
	// consumes once.
	Token uint64
}

func validRequest(request *Request) bool {
	if request.Mode > ModeSFTP {
		return false
	}
	if request.Operation == OperationAuthenticate || request.Operation == OperationAuthorizeKey {
		if request.Operation == OperationAuthorizeKey && request.Token != 0 {
			return false
		}
		return request.Mode == ModeTerminal && request.Session == 0 && request.Geometry == (Geometry{}) &&
			request.CommandSize == 0 && request.Command == 0 &&
			request.UserSize >= 1 && int(request.UserSize) <= MaxUsername &&
			ValidUsername(request.Username[:request.UserSize]) && zero(request.Username[request.UserSize:])
	}
	if request.UserSize != 0 || request.Username != [MaxUsername]byte{} || request.Key != [32]byte{} {
		return false
	}
	if request.Operation != OperationOpen && (request.Mode != ModeTerminal || request.CommandSize != 0 || request.Command != 0) {
		return false
	}
	switch request.Operation {
	case OperationOpen:
		if request.Mode == ModeExec {
			return request.Session == 0 && request.Geometry == (Geometry{}) && request.Command != 0 && request.CommandSize > 0 && int(request.CommandSize) <= MaxCommand
		}
		if request.Mode == ModeSFTP {
			return request.Session == 0 && request.Geometry == (Geometry{}) && request.Command == 0 && request.CommandSize == 0
		}
		return request.Session == 0 && request.Geometry.Valid() && request.Command == 0 && request.CommandSize == 0
	case OperationClose:
		return request.Session != 0 && request.Geometry == (Geometry{})
	case OperationGeometry:
		return request.Session != 0 && request.Geometry.Valid()
	}
	return false
}

func EncodeRequest(request *Request, correlation uint64, out *ipc.Message) bool {
	if correlation == 0 || !validRequest(request) {
		return false
	}
	size := requestSize + uint16(request.Geometry.TermSize)
	if request.Operation == OperationAuthenticate || request.Operation == OperationAuthorizeKey {
		size = requestSize + uint16(request.UserSize) + 32
	}
	*out = ipc.Message{ProtocolID: ProtocolID, Version: Version, Kind: ipc.KindRequest, Correlation: correlation, PayloadSize: size}
	out.Payload[0] = byte(request.Operation)
	out.Payload[1] = request.Geometry.TermSize
	if request.Operation == OperationAuthenticate || request.Operation == OperationAuthorizeKey {
		out.Payload[1] = request.UserSize
	}
	out.Payload[2] = byte(request.Mode)
	binary.LittleEndian.PutUint16(out.Payload[4:6], request.CommandSize)
	if request.Mode == ModeExec {
		out.HandleCount = 1
		out.Handles[0] = request.Command
	}
	binary.LittleEndian.PutUint64(out.Payload[8:16], request.Session)
	binary.LittleEndian.PutUint16(out.Payload[16:18], request.Geometry.Columns)
	binary.LittleEndian.PutUint16(out.Payload[18:20], request.Geometry.Rows)
	binary.LittleEndian.PutUint64(out.Payload[24:32], request.Geometry.Sequence)
	if request.Operation == OperationAuthenticate || request.Operation == OperationAuthorizeKey {
		binary.LittleEndian.PutUint64(out.Payload[24:32], request.Token)
		copy(out.Payload[requestSize:], request.Username[:request.UserSize])
		copy(out.Payload[requestSize+request.UserSize:], request.Key[:])
	} else {
		copy(out.Payload[requestSize:], request.Geometry.Term[:request.Geometry.TermSize])
	}
	return true
}

func DecodeRequest(in *ipc.Message, out *Request) bool {
	if in.ProtocolID != ProtocolID || in.Version != Version || in.Kind != ipc.KindRequest || in.Correlation == 0 || in.HandleCount > 1 || in.PayloadSize < requestSize || in.PayloadSize > requestSize+MaxTerm {
		return false
	}
	var request Request
	request.Operation = Operation(in.Payload[0])
	request.Mode = ExecutionMode(in.Payload[2])
	request.CommandSize = binary.LittleEndian.Uint16(in.Payload[4:6])
	if in.HandleCount == 1 {
		request.Command = in.Handles[0]
	}
	auth := request.Operation == OperationAuthenticate || request.Operation == OperationAuthorizeKey
	if !auth {
		request.Geometry.TermSize = in.Payload[1]
		if int(request.Geometry.TermSize) > MaxTerm || in.PayloadSize != requestSize+uint16(request.Geometry.TermSize) {
			return false
		}
	} else {
		request.UserSize = in.Payload[1]
		if request.UserSize == 0 || int(request.UserSize) > MaxUsername || in.PayloadSize != requestSize+uint16(request.UserSize)+32 {
			return false
		}
	}
	if in.Payload[3] != 0 || !zero(in.Payload[6:8]) || !zero(in.Payload[20:24]) {
		return false
	}
	request.Session = binary.LittleEndian.Uint64(in.Payload[8:16])
	request.Geometry.Columns = binary.LittleEndian.Uint16(in.Payload[16:18])
	request.Geometry.Rows = binary.LittleEndian.Uint16(in.Payload[18:20])
	request.Geometry.Sequence = binary.LittleEndian.Uint64(in.Payload[24:32])
	if auth {
		request.Token = binary.LittleEndian.Uint64(in.Payload[24:32])
		copy(request.Username[:], in.Payload[requestSize:requestSize+int(request.UserSize)])
		copy(request.Key[:], in.Payload[requestSize+int(request.UserSize):requestSize+int(request.UserSize)+32])
		request.Geometry = Geometry{}
	} else {
		copy(request.Geometry.Term[:], in.Payload[requestSize:in.PayloadSize])
	}
	if !validRequest(&request) || (request.Mode == ModeExec) != (in.HandleCount == 1) {
		return false
	}
	*out = request
	return true
}

// Open transfers GTTY and stdout for a terminal, stdin writer, stdout reader
// and stderr reader for exec, or stdin writer and stdout reader for SFTP. Exec
// carries a bounded command object; no mode permits selecting an executable or
// launch authority.
type Reply struct {
	Operation Operation
	Result    Result
	Session   uint64
	Mode      ExecutionMode
	// Token is the one-use admission token OperationAuthorizeKey returns on an
	// allowed probe. Every other reply and every denial carries zero so a probe
	// that is not proven cannot mint an admission.
	Token uint64
}

func replyHandles(reply *Reply) int {
	if reply.Operation == OperationOpen && reply.Result == ResultOK {
		if reply.Mode == ModeExec {
			return 3
		}
		return 2 // A terminal's GTTY and stdout, or SFTP's two streams.
	}
	return 0
}

func validReply(reply *Reply) bool {
	if reply.Mode > ModeSFTP || reply.Operation != OperationOpen && reply.Mode != ModeTerminal {
		return false
	}
	if reply.Operation < OperationOpen || reply.Operation > OperationAuthorizeKey || reply.Result > ResultPermissionDenied {
		return false
	}
	if reply.Operation == OperationAuthorizeKey {
		if reply.Result == ResultPermissionDenied {
			return reply.Session == 0 && reply.Token == 0
		}
		return reply.Session == 0 && reply.Result == ResultOK
	}
	if reply.Token != 0 {
		return false
	}
	return reply.Result != ResultOK || reply.Session != 0
}

func EncodeReply(reply *Reply, attachments []handle.ID, correlation uint64, out *ipc.Message) bool {
	if correlation == 0 || !validReply(reply) || len(attachments) != replyHandles(reply) {
		return false
	}
	for index, attachment := range attachments {
		if attachment == 0 {
			return false
		}
		for _, previous := range attachments[:index] {
			if previous == attachment {
				return false
			}
		}
	}
	*out = ipc.Message{ProtocolID: ProtocolID, Version: Version, Kind: ipc.KindReply, Correlation: correlation, PayloadSize: replySize, HandleCount: uint8(len(attachments))}
	out.Payload[0], out.Payload[1] = byte(reply.Operation), byte(reply.Result)
	out.Payload[2] = byte(reply.Mode)
	binary.LittleEndian.PutUint64(out.Payload[8:16], reply.Session)
	binary.LittleEndian.PutUint64(out.Payload[16:24], reply.Token)
	copy(out.Handles[:], attachments)
	return true
}

func DecodeReply(in *ipc.Message, correlation uint64, operation Operation, out *Reply) bool {
	if in.ProtocolID != ProtocolID || in.Version != Version || in.Kind != ipc.KindReply || correlation == 0 || in.Correlation != correlation || in.PayloadSize != replySize || !zero(in.Payload[3:8]) {
		return false
	}
	reply := Reply{Operation: Operation(in.Payload[0]), Result: Result(in.Payload[1]), Mode: ExecutionMode(in.Payload[2]),
		Session: binary.LittleEndian.Uint64(in.Payload[8:16]), Token: binary.LittleEndian.Uint64(in.Payload[16:24])}
	if reply.Operation != operation || !validReply(&reply) || int(in.HandleCount) != replyHandles(&reply) {
		return false
	}
	for index := 0; index < int(in.HandleCount); index++ {
		if in.Handles[index] == 0 {
			return false
		}
		for previous := 0; previous < index; previous++ {
			if in.Handles[index] == in.Handles[previous] {
				return false
			}
		}
	}
	*out = reply
	return true
}

func zero(bytes []byte) bool {
	for _, value := range bytes {
		if value != 0 {
			return false
		}
	}
	return true
}

// TerminalEndNotice carries the reaped shell's exit status. It transfers no
// capability and does not authorize another session to close or reopen.
func TerminalEndNotice(session, code uint64) (ipc.Message, bool) {
	if session == 0 {
		return ipc.Message{}, false
	}
	message := ipc.Message{ProtocolID: ProtocolID, Version: Version, Kind: ipc.KindNotification, PayloadSize: 24}
	message.Payload[0] = byte(OperationTerminalEnd)
	binary.LittleEndian.PutUint64(message.Payload[8:16], session)
	binary.LittleEndian.PutUint64(message.Payload[16:24], code)
	return message, true
}
func DecodeTerminalEndNotice(message *ipc.Message) (uint64, uint64, bool) {
	if message.ProtocolID != ProtocolID || message.Version != Version || message.Kind != ipc.KindNotification || message.Correlation != 0 || message.HandleCount != 0 || message.PayloadSize != 24 || message.Payload[0] != byte(OperationTerminalEnd) || !zero(message.Payload[1:8]) {
		return 0, 0, false
	}
	session := binary.LittleEndian.Uint64(message.Payload[8:16])
	if session == 0 {
		return 0, 0, false
	}
	return session, binary.LittleEndian.Uint64(message.Payload[16:24]), true
}
