// Package tcpapi defines GTCP, the typed channel protocol between native
// applications and netd. TX slots move to netd with Write and return in its
// reply. Read lends an RX slot to the client; Release returns it.
//
// Version 2 adds a request flags byte, readiness, pending errors and option
// reads. Without FlagNoWait, Accept, Connect, Read and Write park in netd
// until they can finish, as in version 1. With it they answer at once:
// Read and Accept with ResultWouldBlock, Write with the prefix that fit or
// ResultWouldBlock when nothing fit, Connect with ResultInProgress once the
// handshake has started. OpReady reports an endpoint's readiness mask now;
// OpWaitReady parks until the mask differs from the one the request names.
// Neither consumes a byte.
//
// Request body, 24 bytes:
//
//	0      opcode
//	1      slot     Write: TX slot
//	2..3   reserved
//	4..7   endpoint
//	8..11  address  Bind, Connect
//	12..13 port     Bind, Connect
//	14..15 length   Read: most bytes wanted; Write: bytes in the slot
//	16     backlog  Listen
//	17     flags    Flag* bits the opcode allows
//	18..19 mask     WaitReady: the mask the client last saw
//	20     option   Option: Option* selector
//	21..23 reserved
//
// Reply body, 32 bytes:
//
//	0      opcode
//	1      result
//	2      slot     Write: the TX slot returned; Read OK: the RX slot lent
//	3      error    Error OK: the pending error taken, ResultOK for none
//	4..7   endpoint the request's, or the opened or accepted one
//	8..11  address  Bind, Connect, Accept: remote or bound address
//	12..13 port
//	14..15 length   Read, Write: bytes moved
//	16..19 local    Connect, Accept: local address
//	20..21 local port
//	22..23 mask     Ready, WaitReady OK: Ready* bits
//	24..27 value    Ready, WaitReady OK: readable bytes; Option OK: value
//	28..31 room     Ready, WaitReady OK: send buffer room
package tcpapi

import (
	"encoding/binary"
	"goos/kernel/ipc"
)

const (
	ProtocolID  = uint64(0x474e455454435031)
	Version     = uint16(2)
	RequestSize = 24
	ReplySize   = 32
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
	// OpReady answers at once with the endpoint's mask, readable bytes and
	// send buffer room.
	OpReady
	// OpWaitReady answers when the endpoint's mask differs from Mask, at
	// once if it already does. An endpoint holds one; OpClose answers it
	// with ResultClosed first.
	OpWaitReady
	// OpError returns the pending error in Error and clears it.
	OpError
	// OpOption reads the effective value Option selects.
	OpOption
	lastOpcode = OpOption
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
	// ResultWouldBlock answers a NoWait Accept, Read or Write that cannot
	// move anything now.
	ResultWouldBlock
	// ResultInProgress answers a NoWait Connect whose handshake started.
	ResultInProgress
	lastResult = ResultInProgress
)

// Flags select request behavior. Each opcode allows only its own.
const (
	// FlagNoWait: Accept, Connect, Read and Write answer at once.
	FlagNoWait uint8 = 1 << iota
	// FlagPeek: Read copies bytes and leaves them buffered.
	FlagPeek
	// FlagReuseAddr: Bind with SO_REUSEADDR.
	FlagReuseAddr
)

// Ready bits of a mask. A client derives poll events from them.
const (
	ReadyAcceptable    uint16 = 1 << iota // a listener has a child to accept
	ReadyConnected                        // the handshake completed; stays set
	ReadyConnectFailed                    // an active open failed; see the pending error
	ReadyReadable                         // bytes are buffered
	ReadyReadEOF                          // the peer's FIN follows the buffered bytes
	ReadyWritable                         // the send buffer takes at least one byte
	ReadyWriteClosed                      // no byte can be written again
	ReadyError                            // a pending error waits for OpError
	ReadyHangUp                           // the connection ended, or both directions closed
	ReadyConnecting                       // an active open is in progress
	ReadyListening                        // the endpoint listens
	ReadyAll           = ReadyListening<<1 - 1
)

// Option selectors.
const (
	OptionSendMSS uint8 = iota + 1
	OptionSendBuffer
	OptionReceiveBuffer
	OptionTTL
	lastOption = OptionTTL
)

type Request struct {
	Opcode   Opcode
	Slot     uint8
	Endpoint uint32
	Address  [4]byte
	Port     uint16
	Length   uint16
	Backlog  uint8
	Flags    uint8
	Mask     uint16
	Option   uint8
}

type Reply struct {
	Opcode    Opcode
	Result    Result
	Slot      uint8
	Error     Result
	Endpoint  uint32
	Address   [4]byte
	Port      uint16
	Length    uint16
	Local     [4]byte
	LocalPort uint16
	Mask      uint16
	Value     uint32
	Room      uint32
}

var noAddress [4]byte

func allowedFlags(opcode Opcode) uint8 {
	switch opcode {
	case OpAccept, OpConnect, OpWrite:
		return FlagNoWait
	case OpRead:
		return FlagNoWait | FlagPeek
	case OpBind:
		return FlagReuseAddr
	}
	return 0
}

func validRequest(r *Request) bool {
	none := r.Address == noAddress && r.Port == 0
	if r.Flags&^allowedFlags(r.Opcode) != 0 || (r.Mask != 0 && r.Opcode != OpWaitReady) || (r.Option != 0) != (r.Opcode == OpOption) {
		return false
	}
	switch r.Opcode {
	case OpOpen:
		return r.Endpoint == 0 && r.Slot == 0 && r.Length == 0 && r.Backlog == 0 && none
	case OpBind:
		return r.Endpoint != 0 && r.Slot == 0 && r.Length == 0 && r.Backlog == 0
	case OpListen:
		return r.Endpoint != 0 && r.Backlog != 0 && r.Slot == 0 && r.Length == 0 && none
	case OpAccept, OpShutdownWrite, OpClose, OpReady, OpError:
		return r.Endpoint != 0 && r.Slot == 0 && r.Length == 0 && r.Backlog == 0 && none
	case OpWaitReady:
		return r.Endpoint != 0 && r.Slot == 0 && r.Length == 0 && r.Backlog == 0 && none && r.Mask&^ReadyAll == 0
	case OpOption:
		return r.Endpoint != 0 && r.Slot == 0 && r.Length == 0 && r.Backlog == 0 && none && r.Option <= lastOption
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
	out.Payload[16], out.Payload[17] = r.Backlog, r.Flags
	binary.LittleEndian.PutUint16(out.Payload[18:20], r.Mask)
	out.Payload[20] = r.Option
	return true
}

func DecodeRequest(in *ipc.Message, out *Request) (uint64, bool) {
	if in.ProtocolID != ProtocolID || in.Version != Version || in.PayloadSize != RequestSize || in.HandleCount != 0 ||
		!zero(in.Payload[2:4]) || !zero(in.Payload[21:RequestSize]) {
		return 0, false
	}
	r := Request{Opcode: Opcode(in.Payload[0]), Slot: in.Payload[1], Endpoint: binary.LittleEndian.Uint32(in.Payload[4:8]),
		Port: binary.LittleEndian.Uint16(in.Payload[12:14]), Length: binary.LittleEndian.Uint16(in.Payload[14:16]),
		Backlog: in.Payload[16], Flags: in.Payload[17], Mask: binary.LittleEndian.Uint16(in.Payload[18:20]), Option: in.Payload[20]}
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
	addressed := r.Address != noAddress || r.Port != 0 || r.Local != noAddress || r.LocalPort != 0
	readiness := r.Mask != 0 || r.Value != 0 || r.Room != 0
	if r.Error != ResultOK && (r.Opcode != OpError || r.Result != ResultOK) {
		return false
	}
	if readiness && r.Result != ResultOK {
		return false
	}
	switch {
	case r.Result == ResultWouldBlock && r.Opcode != OpAccept && r.Opcode != OpRead && r.Opcode != OpWrite,
		r.Result == ResultInProgress && r.Opcode != OpConnect,
		r.Result == ResultEOF && r.Opcode != OpRead:
		return false
	}
	if r.Opcode == OpWrite {
		return r.Slot < TXSlots && r.Length <= SlotSize && !addressed && !readiness
	}
	if r.Result == ResultInProgress {
		return r.Slot == 0 && r.Length == 0 && r.Address != noAddress && r.Port != 0 && r.Local != noAddress && r.LocalPort != 0
	}
	if r.Result != ResultOK {
		return r.Slot == 0 && r.Length == 0 && !addressed
	}
	switch r.Opcode {
	case OpOpen, OpListen, OpShutdownWrite, OpClose:
		return r.Slot == 0 && r.Length == 0 && !addressed && !readiness
	case OpBind:
		return r.Slot == 0 && r.Port != 0 && r.Length == 0 && r.Local == noAddress && r.LocalPort == 0 && !readiness
	case OpAccept, OpConnect:
		return r.Slot == 0 && r.Address != noAddress && r.Port != 0 && r.Length == 0 && r.Local != noAddress && r.LocalPort != 0 && !readiness
	case OpRead:
		return r.Slot < RXSlots && r.Length > 0 && r.Length <= SlotSize && !addressed && !readiness
	case OpReady, OpWaitReady:
		return r.Slot == 0 && r.Length == 0 && !addressed && r.Mask&^ReadyAll == 0
	case OpError:
		return r.Slot == 0 && r.Length == 0 && !addressed && !readiness && r.Error <= lastResult
	case OpOption:
		return r.Slot == 0 && r.Length == 0 && !addressed && r.Mask == 0 && r.Room == 0
	}
	return false
}

func EncodeReply(r *Reply, correlation uint64, out *ipc.Message) bool {
	if correlation == 0 || !validReply(r) {
		return false
	}
	prepare(out, ipc.KindReply, correlation, ReplySize)
	out.Payload[0], out.Payload[1], out.Payload[2], out.Payload[3] = byte(r.Opcode), byte(r.Result), r.Slot, byte(r.Error)
	binary.LittleEndian.PutUint32(out.Payload[4:8], r.Endpoint)
	copy(out.Payload[8:12], r.Address[:])
	binary.LittleEndian.PutUint16(out.Payload[12:14], r.Port)
	binary.LittleEndian.PutUint16(out.Payload[14:16], r.Length)
	copy(out.Payload[16:20], r.Local[:])
	binary.LittleEndian.PutUint16(out.Payload[20:22], r.LocalPort)
	binary.LittleEndian.PutUint16(out.Payload[22:24], r.Mask)
	binary.LittleEndian.PutUint32(out.Payload[24:28], r.Value)
	binary.LittleEndian.PutUint32(out.Payload[28:32], r.Room)
	return true
}

func DecodeReply(in *ipc.Message, out *Reply) (uint64, bool) {
	if in.ProtocolID != ProtocolID || in.Version != Version || in.Kind != ipc.KindReply || in.Correlation == 0 ||
		in.PayloadSize != ReplySize || in.HandleCount != 0 {
		return 0, false
	}
	r := Reply{Opcode: Opcode(in.Payload[0]), Result: Result(in.Payload[1]), Slot: in.Payload[2], Error: Result(in.Payload[3]),
		Endpoint: binary.LittleEndian.Uint32(in.Payload[4:8]), Port: binary.LittleEndian.Uint16(in.Payload[12:14]),
		Length: binary.LittleEndian.Uint16(in.Payload[14:16]), LocalPort: binary.LittleEndian.Uint16(in.Payload[20:22]),
		Mask: binary.LittleEndian.Uint16(in.Payload[22:24]), Value: binary.LittleEndian.Uint32(in.Payload[24:28]),
		Room: binary.LittleEndian.Uint32(in.Payload[28:32])}
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
	case ResultWouldBlock:
		return "would block"
	case ResultInProgress:
		return "in progress"
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
