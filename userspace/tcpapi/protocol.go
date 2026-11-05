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
// Version 3 (Layout3) has eight TX and eight RX slots of 16 KiB each and a
// 28-byte request whose last four bytes are a Write's stream offset: the
// count of bytes the endpoint had accepted from the client, mod 2^32, when
// the client sent it. A client may send several NoWait Writes of one
// endpoint back to back. netd takes bytes from a Write only when its offset
// equals the endpoint's accepted count, so a Write sent after one that took
// a prefix answers ResultWouldBlock with nothing taken, and the client
// resends from its first unaccepted byte. A Write whose offset lies behind
// the count answers ResultInvalidArgument and takes nothing. An endpoint
// holds up to QueuedReads3 parked Reads; netd answers them in the order
// they arrived, each with an RX slot of its own. A NoWait Read behind
// parked ones answers ResultWouldBlock. Version 2 keeps one parked Read and
// no offset.
//
// PID 1 picks a session's version when it creates the session; netd decodes
// that session's messages with its Layout and fails a message of another
// version.
//
// Request body, 24 bytes (28 in version 3):
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
//	24..27 offset   version 3 Write: stream offset of the slot's first byte
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

	Version3     = uint16(3)
	RequestSize3 = 28
	SlotSize3    = 16384
	TXSlots3     = 8
	RXSlots3     = 8
	// QueuedReads3 is the most parked Reads one version 3 endpoint holds.
	QueuedReads3 = 4
	// PipelinedWrites3 is how many NoWait Writes of one endpoint a version
	// 3 client should have in flight at most: 64 KiB, half the TX slots.
	// netd answers each Write before it takes the next message, so it does
	// not count them; the session's TX slots are the hard bound.
	PipelinedWrites3 = 4
)

// Layout is one protocol version and the slot pools it implies. A session
// speaks exactly one Layout; Slots bitmasks bound either count to 8.
type Layout struct {
	Version     uint16
	RequestSize uint16
	SlotSize    uint32
	TXSlots     uint8
	RXSlots     uint8
	QueuedReads uint8
}

var (
	Layout2 = Layout{Version: Version, RequestSize: RequestSize, SlotSize: SlotSize, TXSlots: TXSlots, RXSlots: RXSlots, QueuedReads: 1}
	Layout3 = Layout{Version: Version3, RequestSize: RequestSize3, SlotSize: SlotSize3, TXSlots: TXSlots3, RXSlots: RXSlots3,
		QueuedReads: QueuedReads3}
)

// LayoutFor returns the Layout of a protocol version netd serves.
func LayoutFor(version uint16) (Layout, bool) {
	switch version {
	case Version:
		return Layout2, true
	case Version3:
		return Layout3, true
	}
	return Layout{}, false
}

func (l Layout) TXPool() uint64           { return uint64(l.TXSlots) * uint64(l.SlotSize) }
func (l Layout) RXPool() uint64           { return uint64(l.RXSlots) * uint64(l.SlotSize) }
func (l Layout) TXPoolPages() uint64      { return l.TXPool() / pageSize }
func (l Layout) RXPoolPages() uint64      { return l.RXPool() / pageSize }
func (l Layout) Offset(slot uint8) uint64 { return uint64(slot) * uint64(l.SlotSize) }

// StreamOffsets reports whether Writes carry a stream offset.
func (l Layout) StreamOffsets() bool { return l.RequestSize > RequestSize }

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
	// Offset is a version 3 Write's stream offset; zero otherwise.
	Offset uint32
	// Release, in version 3, names RX slots the client returns before
	// netd handles the request, as one Release each would; zero otherwise.
	Release uint8
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

func validRequest(l *Layout, r *Request) bool {
	none := r.Address == noAddress && r.Port == 0
	if r.Offset != 0 && (r.Opcode != OpWrite || !l.StreamOffsets()) {
		return false
	}
	if r.Release != 0 && (!l.StreamOffsets() || r.Opcode == OpRelease || uint32(r.Release)>>l.RXSlots != 0) {
		return false
	}
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
		return r.Endpoint != 0 && r.Slot < l.TXSlots && uint32(r.Length) <= l.SlotSize && r.Backlog == 0 && none
	case OpRead:
		return r.Endpoint != 0 && r.Slot == 0 && r.Length > 0 && uint32(r.Length) <= l.SlotSize && r.Backlog == 0 && none
	case OpRelease:
		return r.Endpoint == 0 && r.Slot < l.RXSlots && r.Length == 0 && r.Backlog == 0 && none
	}
	return false
}

// EncodeRequest, DecodeRequest, EncodeReply and DecodeReply speak version 2.
func EncodeRequest(r *Request, correlation uint64, out *ipc.Message) bool {
	return Layout2.EncodeRequest(r, correlation, out)
}

func DecodeRequest(in *ipc.Message, out *Request) (uint64, bool) {
	return Layout2.DecodeRequest(in, out)
}

func EncodeReply(r *Reply, correlation uint64, out *ipc.Message) bool {
	return Layout2.EncodeReply(r, correlation, out)
}

func DecodeReply(in *ipc.Message, out *Reply) (uint64, bool) { return Layout2.DecodeReply(in, out) }

func (l Layout) EncodeRequest(r *Request, correlation uint64, out *ipc.Message) bool {
	if !l.known() || !validRequest(&l, r) || (r.Opcode == OpRelease) != (correlation == 0) {
		return false
	}
	kind := ipc.KindRequest
	if r.Opcode == OpRelease {
		kind = ipc.KindNotification
	}
	prepare(out, l.Version, kind, correlation, l.RequestSize)
	out.Payload[0], out.Payload[1], out.Payload[2] = byte(r.Opcode), r.Slot, r.Release
	binary.LittleEndian.PutUint32(out.Payload[4:8], r.Endpoint)
	copy(out.Payload[8:12], r.Address[:])
	binary.LittleEndian.PutUint16(out.Payload[12:14], r.Port)
	binary.LittleEndian.PutUint16(out.Payload[14:16], r.Length)
	out.Payload[16], out.Payload[17] = r.Backlog, r.Flags
	binary.LittleEndian.PutUint16(out.Payload[18:20], r.Mask)
	out.Payload[20] = r.Option
	if l.StreamOffsets() {
		binary.LittleEndian.PutUint32(out.Payload[24:28], r.Offset)
	}
	return true
}

func (l Layout) DecodeRequest(in *ipc.Message, out *Request) (uint64, bool) {
	if !l.known() || in.ProtocolID != ProtocolID || in.Version != l.Version || in.PayloadSize != l.RequestSize || in.HandleCount != 0 ||
		!zero(in.Payload[3:4]) || !zero(in.Payload[21:24]) {
		return 0, false
	}
	r := Request{Opcode: Opcode(in.Payload[0]), Slot: in.Payload[1], Endpoint: binary.LittleEndian.Uint32(in.Payload[4:8]),
		Port: binary.LittleEndian.Uint16(in.Payload[12:14]), Length: binary.LittleEndian.Uint16(in.Payload[14:16]),
		Backlog: in.Payload[16], Flags: in.Payload[17], Mask: binary.LittleEndian.Uint16(in.Payload[18:20]), Option: in.Payload[20],
		Release: in.Payload[2]}
	copy(r.Address[:], in.Payload[8:12])
	if l.StreamOffsets() {
		r.Offset = binary.LittleEndian.Uint32(in.Payload[24:28])
	}
	if !validRequest(&l, &r) {
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
func validReply(l *Layout, r *Reply) bool {
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
		return r.Slot < l.TXSlots && uint32(r.Length) <= l.SlotSize && !addressed && !readiness
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
		return r.Slot < l.RXSlots && r.Length > 0 && uint32(r.Length) <= l.SlotSize && !addressed && !readiness
	case OpReady, OpWaitReady:
		return r.Slot == 0 && r.Length == 0 && !addressed && r.Mask&^ReadyAll == 0
	case OpError:
		return r.Slot == 0 && r.Length == 0 && !addressed && !readiness && r.Error <= lastResult
	case OpOption:
		return r.Slot == 0 && r.Length == 0 && !addressed && r.Mask == 0 && r.Room == 0
	}
	return false
}

func (l Layout) EncodeReply(r *Reply, correlation uint64, out *ipc.Message) bool {
	if !l.known() || correlation == 0 || !validReply(&l, r) {
		return false
	}
	prepare(out, l.Version, ipc.KindReply, correlation, ReplySize)
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

func (l Layout) DecodeReply(in *ipc.Message, out *Reply) (uint64, bool) {
	if !l.known() || in.ProtocolID != ProtocolID || in.Version != l.Version || in.Kind != ipc.KindReply || in.Correlation == 0 ||
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
	if !validReply(&l, &r) {
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

// known reports whether l is one of the layouts LayoutFor returns, so a
// zero or hand-built Layout encodes and decodes nothing.
func (l Layout) known() bool {
	known, ok := LayoutFor(l.Version)
	return ok && known == l
}

func prepare(out *ipc.Message, version uint16, kind ipc.Kind, correlation uint64, size uint16) {
	for index := range out.Payload[:] {
		out.Payload[index] = 0
	}
	for index := range out.Handles[:] {
		out.Handles[index] = 0
	}
	out.ProtocolID, out.Version, out.Kind = ProtocolID, version, kind
	out.Correlation, out.PayloadSize, out.HandleCount = correlation, size, 0
}
