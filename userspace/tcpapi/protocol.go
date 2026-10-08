// Package tcpapi defines GTCP1, the typed channel protocol between native
// applications and netd. TX slots move to netd with Write and return in its
// reply. Read lends an RX slot to the client; Release returns it.
package tcpapi

import (
	"encoding/binary"
	"goos/kernel/ipc"
)

const (
	ProtocolID  = uint64(0x474e455454435031)
	Version     = uint16(1)
	RequestSize = 24
	ReplySize   = 24
	SlotSize    = 4096
	TXSlots     = 4
	RXSlots     = 8
	pageSize    = 4096
	TXPoolPages = TXSlots * SlotSize / pageSize
	RXPoolPages = RXSlots * SlotSize / pageSize
)

type Opcode uint8

const (
	OpOpen Opcode = iota + 1
	OpBind
	OpListen
	OpAccept
	OpConnect
	OpWrite
	OpRead
	OpShutdownWrite
	OpClose
	OpRelease
	lastOpcode = OpRelease
)

type Result uint8

const (
	ResultOK Result = iota
	ResultInvalidArgument
	ResultBusy
	ResultDown
	ResultNoAddress
	ResultNoRoute
	ResultNeighborUnreachable
	ResultReconfigured
	ResultAddressInUse
	ResultConnectionRefused
	ResultTimeout
	ResultReset
	ResultDriverFault
	ResultClosed
	ResultEOF
	lastResult = ResultEOF
)

type Request struct {
	Opcode   Opcode
	Slot     uint8
	Endpoint uint32
	Address  [4]byte
	Port     uint16
	Length   uint16
	Backlog  uint8
}

type Reply struct {
	Opcode    Opcode
	Result    Result
	Slot      uint8
	Endpoint  uint32
	Address   [4]byte
	Port      uint16
	Length    uint16
	Local     [4]byte
	LocalPort uint16
}

var noAddress [4]byte

func validRequest(r *Request) bool {
	none := r.Address == noAddress && r.Port == 0
	switch r.Opcode {
	case OpOpen:
		return r.Endpoint == 0 && r.Slot == 0 && r.Length == 0 && r.Backlog == 0 && none
	case OpBind:
		return r.Endpoint != 0 && r.Slot == 0 && r.Length == 0 && r.Backlog == 0
	case OpListen:
		return r.Endpoint != 0 && r.Backlog != 0 && r.Slot == 0 && r.Length == 0 && none
	case OpAccept, OpShutdownWrite, OpClose:
		return r.Endpoint != 0 && r.Slot == 0 && r.Length == 0 && r.Backlog == 0 && none
	case OpConnect:
		return r.Endpoint != 0 && r.Address != noAddress && r.Port != 0 && r.Slot == 0 && r.Length == 0 && r.Backlog == 0
	case OpWrite:
		return r.Endpoint != 0 && r.Slot < TXSlots && r.Length <= SlotSize && r.Backlog == 0 && none
	case OpRead:
		return r.Endpoint != 0 && r.Slot == 0 && r.Length > 0 && r.Length <= SlotSize && r.Backlog == 0 && none
	case OpRelease:
		return r.Endpoint == 0 && r.Slot < RXSlots && r.Length == 0 && r.Backlog == 0 && none
	}
	return false
}

func EncodeRequest(r *Request, correlation uint64, out *ipc.Message) bool {
	if !validRequest(r) || (r.Opcode == OpRelease) != (correlation == 0) {
		return false
	}
	kind := ipc.KindRequest
	if r.Opcode == OpRelease {
		kind = ipc.KindNotification
	}
	prepare(out, kind, correlation, RequestSize)
	out.Payload[0], out.Payload[1] = byte(r.Opcode), r.Slot
	binary.LittleEndian.PutUint32(out.Payload[4:8], r.Endpoint)
	copy(out.Payload[8:12], r.Address[:])
	binary.LittleEndian.PutUint16(out.Payload[12:14], r.Port)
	binary.LittleEndian.PutUint16(out.Payload[14:16], r.Length)
	out.Payload[16] = r.Backlog
	return true
}

func DecodeRequest(in *ipc.Message, out *Request) (uint64, bool) {
	if in.ProtocolID != ProtocolID || in.Version != Version || in.PayloadSize != RequestSize || in.HandleCount != 0 ||
		!zero(in.Payload[2:4]) || !zero(in.Payload[17:RequestSize]) {
		return 0, false
	}
	r := Request{Opcode: Opcode(in.Payload[0]), Slot: in.Payload[1], Endpoint: binary.LittleEndian.Uint32(in.Payload[4:8]),
		Port: binary.LittleEndian.Uint16(in.Payload[12:14]), Length: binary.LittleEndian.Uint16(in.Payload[14:16]), Backlog: in.Payload[16]}
	copy(r.Address[:], in.Payload[8:12])
	if !validRequest(&r) {
		return 0, false
	}
	if r.Opcode == OpRelease {
		if in.Kind != ipc.KindNotification || in.Correlation != 0 {
			return 0, false
		}
	} else if in.Kind != ipc.KindRequest || in.Correlation == 0 {
		return 0, false
	}
	*out = r
	return in.Correlation, true
}

// validReply requires the request's endpoint, except that a failed Open has
// no endpoint to name.
func validReply(r *Reply) bool {
	if r.Opcode < OpOpen || r.Opcode > lastOpcode || r.Opcode == OpRelease || r.Result > lastResult {
		return false
	}
	if (r.Endpoint == 0) != (r.Opcode == OpOpen && r.Result != ResultOK) {
		return false
	}
	if r.Opcode == OpWrite {
		return r.Slot < TXSlots && r.Length <= SlotSize && r.Address == noAddress && r.Port == 0 && r.Local == noAddress && r.LocalPort == 0
	}
	if r.Result != ResultOK {
		return r.Slot == 0 && r.Address == noAddress && r.Port == 0 && r.Length == 0 && r.Local == noAddress && r.LocalPort == 0 &&
			(r.Result != ResultEOF || r.Opcode == OpRead)
	}
	switch r.Opcode {
	case OpOpen, OpListen, OpShutdownWrite, OpClose:
		return r.Slot == 0 && r.Address == noAddress && r.Port == 0 && r.Length == 0 && r.Local == noAddress && r.LocalPort == 0
	case OpBind:
		return r.Slot == 0 && r.Port != 0 && r.Length == 0 && r.Local == noAddress && r.LocalPort == 0
	case OpAccept, OpConnect:
		return r.Slot == 0 && r.Address != noAddress && r.Port != 0 && r.Length == 0 && r.Local != noAddress && r.LocalPort != 0
	case OpRead:
		return r.Slot < RXSlots && r.Length > 0 && r.Length <= SlotSize && r.Address == noAddress && r.Port == 0 && r.Local == noAddress && r.LocalPort == 0
	}
	return false
}

func EncodeReply(r *Reply, correlation uint64, out *ipc.Message) bool {
	if correlation == 0 || !validReply(r) {
		return false
	}
	prepare(out, ipc.KindReply, correlation, ReplySize)
	out.Payload[0], out.Payload[1], out.Payload[2] = byte(r.Opcode), byte(r.Result), r.Slot
	binary.LittleEndian.PutUint32(out.Payload[4:8], r.Endpoint)
	copy(out.Payload[8:12], r.Address[:])
	binary.LittleEndian.PutUint16(out.Payload[12:14], r.Port)
	binary.LittleEndian.PutUint16(out.Payload[14:16], r.Length)
	copy(out.Payload[16:20], r.Local[:])
	binary.LittleEndian.PutUint16(out.Payload[20:22], r.LocalPort)
	return true
}

func DecodeReply(in *ipc.Message, out *Reply) (uint64, bool) {
	if in.ProtocolID != ProtocolID || in.Version != Version || in.Kind != ipc.KindReply || in.Correlation == 0 ||
		in.PayloadSize != ReplySize || in.HandleCount != 0 || in.Payload[3] != 0 || !zero(in.Payload[22:ReplySize]) {
		return 0, false
	}
	r := Reply{Opcode: Opcode(in.Payload[0]), Result: Result(in.Payload[1]), Slot: in.Payload[2],
		Endpoint: binary.LittleEndian.Uint32(in.Payload[4:8]), Port: binary.LittleEndian.Uint16(in.Payload[12:14]),
		Length: binary.LittleEndian.Uint16(in.Payload[14:16]), LocalPort: binary.LittleEndian.Uint16(in.Payload[20:22])}
	copy(r.Address[:], in.Payload[8:12])
	copy(r.Local[:], in.Payload[16:20])
	if !validReply(&r) {
		return 0, false
	}
	*out = r
	return in.Correlation, true
}

func (r Result) Text() string {
	switch r {
	case ResultOK:
		return "ok"
	case ResultInvalidArgument:
		return "invalid argument"
	case ResultBusy:
		return "busy"
	case ResultDown:
		return "interface down"
	case ResultNoAddress:
		return "no address"
	case ResultNoRoute:
		return "no route"
	case ResultNeighborUnreachable:
		return "neighbor unreachable"
	case ResultReconfigured:
		return "reconfigured"
	case ResultAddressInUse:
		return "address in use"
	case ResultConnectionRefused:
		return "connection refused"
	case ResultTimeout:
		return "timeout"
	case ResultReset:
		return "reset"
	case ResultDriverFault:
		return "driver fault"
	case ResultClosed:
		return "closed"
	case ResultEOF:
		return "eof"
	}
	return ""
}

func zero(data []byte) bool {
	for _, value := range data {
		if value != 0 {
			return false
		}
	}
	return true
}

func prepare(out *ipc.Message, kind ipc.Kind, correlation uint64, size uint16) {
	for index := range out.Payload[:] {
		out.Payload[index] = 0
	}
	for index := range out.Handles[:] {
		out.Handles[index] = 0
	}
	out.ProtocolID, out.Version, out.Kind = ProtocolID, Version, kind
	out.Correlation, out.PayloadSize, out.HandleCount = correlation, size, 0
}
