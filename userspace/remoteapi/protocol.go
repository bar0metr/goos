// Package remoteapi defines PID 1's remote terminal session control protocol.
package remoteapi

import (
	"encoding/binary"
	"goos/kernel/handle"
	"goos/kernel/ipc"
)

const (
	ProtocolID    = uint64(0x4752454d4f544531) // GREMOTE1
	Version       = uint16(1)
	MaxSessions   = 4
	MaxHandshakes = 2
	MaxWorkers    = MaxSessions + MaxHandshakes
	MaxTerm       = 64
	requestSize   = 24
	replySize     = 24
)

type Operation uint8

const (
	OperationOpen Operation = iota + 1
	OperationClose
	OperationGeometry
	OperationAuthenticate
	OperationTerminalEnd
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
	geometry.TermSize = uint8(copy(geometry.Term[:], term))
	return geometry, geometry.Valid()
}

func (geometry *Geometry) Valid() bool {
	if geometry.Columns < 1 || geometry.Columns > 1000 || geometry.Rows < 1 || geometry.Rows > 1000 || geometry.TermSize == 0 || int(geometry.TermSize) > len(geometry.Term) {
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
	Operation Operation
	Session   uint64
	Geometry  Geometry
}

func validRequest(request *Request) bool {
	switch request.Operation {
	case OperationAuthenticate:
		return request.Session == 0 && request.Geometry == (Geometry{})
	case OperationOpen:
		return request.Session == 0 && request.Geometry.Valid()
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
	*out = ipc.Message{ProtocolID: ProtocolID, Version: Version, Kind: ipc.KindRequest, Correlation: correlation, PayloadSize: requestSize + uint16(request.Geometry.TermSize)}
	out.Payload[0] = byte(request.Operation)
	out.Payload[1] = request.Geometry.TermSize
	binary.LittleEndian.PutUint64(out.Payload[8:16], request.Session)
	binary.LittleEndian.PutUint16(out.Payload[16:18], request.Geometry.Columns)
	binary.LittleEndian.PutUint16(out.Payload[18:20], request.Geometry.Rows)
	copy(out.Payload[requestSize:], request.Geometry.Term[:request.Geometry.TermSize])
	return true
}

func DecodeRequest(in *ipc.Message, out *Request) bool {
	if in.ProtocolID != ProtocolID || in.Version != Version || in.Kind != ipc.KindRequest || in.Correlation == 0 || in.HandleCount != 0 || in.PayloadSize < requestSize || in.PayloadSize > requestSize+MaxTerm {
		return false
	}
	var request Request
	request.Operation = Operation(in.Payload[0])
	request.Geometry.TermSize = in.Payload[1]
	if int(request.Geometry.TermSize) > MaxTerm || in.PayloadSize != requestSize+uint16(request.Geometry.TermSize) || !zero(in.Payload[2:8]) || !zero(in.Payload[20:requestSize]) {
		return false
	}
	request.Session = binary.LittleEndian.Uint64(in.Payload[8:16])
	request.Geometry.Columns = binary.LittleEndian.Uint16(in.Payload[16:18])
	request.Geometry.Rows = binary.LittleEndian.Uint16(in.Payload[18:20])
	copy(request.Geometry.Term[:], in.Payload[requestSize:in.PayloadSize])
	if !validRequest(&request) {
		return false
	}
	*out = request
	return true
}

// A successful Open transfers the GTTY server endpoint and stdout reader.
type Reply struct {
	Operation Operation
	Result    Result
	Session   uint64
}

func replyHandles(reply *Reply) int {
	if reply.Operation == OperationOpen && reply.Result == ResultOK {
		return 2
	}
	return 0
}

func validReply(reply *Reply) bool {
	if reply.Operation < OperationOpen || reply.Operation > OperationAuthenticate || reply.Result > ResultPermissionDenied {
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
	binary.LittleEndian.PutUint64(out.Payload[8:16], reply.Session)
	copy(out.Handles[:], attachments)
	return true
}

func DecodeReply(in *ipc.Message, correlation uint64, operation Operation, out *Reply) bool {
	if in.ProtocolID != ProtocolID || in.Version != Version || in.Kind != ipc.KindReply || correlation == 0 || in.Correlation != correlation || in.PayloadSize != replySize || !zero(in.Payload[2:8]) || !zero(in.Payload[16:replySize]) {
		return false
	}
	reply := Reply{Operation: Operation(in.Payload[0]), Result: Result(in.Payload[1]), Session: binary.LittleEndian.Uint64(in.Payload[8:16])}
	if reply.Operation != operation || !validReply(&reply) || int(in.HandleCount) != replyHandles(&reply) {
		return false
	}
	for index := 0; index < int(in.HandleCount); index++ {
		if in.Handles[index] == 0 || index != 0 && in.Handles[index] == in.Handles[0] {
			return false
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
