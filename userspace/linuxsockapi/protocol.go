// Package linuxsockapi defines GLNXSOCK2, the protocol between the kernel's
// Linux compatibility layer and linuxsockd (ADR 0210).
//
// One session serves one Linux family: a channel and one shared pool. The
// channel carries the control plane: the kernel sends requests and
// linuxsockd answers each with one reply carrying the request's
// correlation. No request waits in the service for long: an operation that
// cannot complete now answers WouldBlock or InProgress, and the kernel
// sleeps on readiness instead.
//
// Payload moves through the pool, not through the channel. The pool holds
// a readiness page, a ring page, eight TX slots of 16 KiB and eight RX
// slots of 16 KiB. The kernel copies a send into TX slots it owns and
// commits each slot on the submit ring; linuxsockd drains the slot into
// netd and hands it back on the complete ring. linuxsockd reads ahead from
// netd into RX slots it owns and publishes each slot to the kernel on the
// complete ring; the kernel copies from it straight into the Linux buffer
// and hands it back on the submit ring once a read consumed all of it.
// Every slot carries a generation that the side handing it back repeats.
//
// The channel only rings the doorbells: Doorbell tells linuxsockd that the
// submit ring has entries, ReadyChanged tells the kernel that the readiness
// page or the complete ring changed. Each side sends one only when the
// other has cleared the matching token, so a full queue never hides work.
package linuxsockapi

import (
	"encoding/binary"

	"goos/kernel/ipc"
)

const (
	ProtocolID  = uint64(0x474c4e58534f4332) // "GLNXSOC2"
	Version     = uint16(2)
	RequestSize = 48
	ReplySize   = 48

	// MaxSockets bounds the sockets of one family, TCP and UDP together.
	MaxSockets = 32

	SlotSize = 16384
	TXSlots  = 8
	RXSlots  = 8
	pageSize = 4096
	// SlotPages is the pages of one slot.
	SlotPages = SlotSize / pageSize
	// The pool: the readiness page, the ring page, the TX slots, then the
	// RX slots.
	ReadinessPage = 0
	RingPage      = 1
	TXFirstPage   = 2
	RXFirstPage   = TXFirstPage + TXSlots*SlotPages
	PoolPages     = RXFirstPage + RXSlots*SlotPages
	PoolBytes     = PoolPages * pageSize

	// MaxDatagram is the largest datagram either way, the GUDP limit.
	MaxDatagram = 4096
	// SocketTXSlots bounds the TX slots one socket holds committed, so one
	// stream leaves room for the family's others: 64 KiB, one whole Linux
	// write of the largest size the kernel moves in one call.
	SocketTXSlots = 4
	// SocketRXSlots bounds the RX slots linuxsockd fills ahead for one
	// socket, and RXReserve the slots it always leaves free for a read the
	// kernel asks for, so sockets with unread data cannot starve another.
	SocketRXSlots = 4
	RXReserve     = 2
)

// TXOffset and RXOffset locate a slot in the pool.
func TXOffset(slot uint8) uint64 { return uint64(TXFirstPage)*pageSize + uint64(slot)*SlotSize }
func RXOffset(slot uint8) uint64 { return uint64(RXFirstPage)*pageSize + uint64(slot)*SlotSize }

// TXPage and RXPage are the pool page holding page index of a slot.
func TXPage(slot uint8, index int) uint32 { return uint32(TXFirstPage + int(slot)*SlotPages + index) }
func RXPage(slot uint8, index int) uint32 { return uint32(RXFirstPage + int(slot)*SlotPages + index) }

type Opcode uint8

const (
	// OpOpen creates a socket of Kind. The reply's Socket names it.
	OpOpen Opcode = iota + 1
	// OpBind binds Socket to Address (zero for INADDR_ANY) and Port (zero
	// for an ephemeral one). FlagReuseAddr applies SO_REUSEADDR to this
	// bind. The reply carries the bound address and port in Local.
	OpBind
	// OpListen makes a bound or unbound stream socket a listener with
	// Length as the requested backlog. The service clamps it.
	OpListen
	// OpAccept takes one established child of a listener. The reply's
	// Value is the child's socket, Address and Port its peer, Local the
	// local end. WouldBlock when none is queued.
	OpAccept
	// OpConnect starts a connection to Address and Port, or fixes a
	// datagram socket's peer. A stream connect that has not finished
	// answers InProgress; its outcome arrives through readiness and
	// GetPendingError. The reply carries Local for a completed connect.
	OpConnect
	// OpRead asks for the socket's next data when the kernel holds none.
	// The service answers OK once it published an RX slot for the socket
	// on the complete ring, OK with FlagEOF at the end of a stream with
	// nothing left, WouldBlock when nothing is there, or the error.
	OpRead
	// OpWrite asks whether a connected socket takes data: OK means its
	// readiness entry says so now, otherwise the result is the error a
	// send reports. The kernel asks only when the entry does not let it
	// commit TX slots.
	OpWrite
	// OpSendTo asks the same for a datagram to Address and Port. An
	// unbound socket is bound to an ephemeral port first, as Linux does.
	OpSendTo
	// OpShutdown shuts down Socket in direction Length (Linux SHUT_RD,
	// SHUT_WR, or SHUT_RDWR).
	OpShutdown
	// OpGetSockName and OpGetPeerName answer with Local, or Address and
	// Port.
	OpGetSockName
	OpGetPeerName
	// OpGetOption and OpSetOption read and write the transport option
	// Level and Name. Value carries the option's value either way; the
	// reply's Value is the effective value after a set.
	OpGetOption
	OpSetOption
	// OpGetPendingError returns and clears the socket's pending error in
	// the reply's Value, as a Result.
	OpGetPendingError
	// OpQueryReady answers with the socket's readiness: Value holds the
	// Ready mask, Extra the bytes a read would return now (for a datagram
	// socket, the next datagram's length).
	OpQueryReady
	// OpClose closes Socket. The kernel sends it once the last Linux
	// description is gone.
	OpClose
	// OpCancel abandons the request whose correlation is Value. Every
	// request completes at once, so the service needs to do nothing but
	// answer; the opcode exists so an abandoned exchange has a defined end.
	OpCancel
	// OpDoorbell is a kernel notification: the submit ring has entries.
	OpDoorbell
	// OpReadyChanged is a service notification: the readiness page or the
	// complete ring changed. It carries nothing.
	OpReadyChanged
	lastOpcode = OpReadyChanged
)

// Kind of an Open.
const (
	KindStream   = uint32(1)
	KindDatagram = uint32(2)
)

// Request flags.
const (
	FlagReuseAddr = uint8(1 << 1) // Bind: SO_REUSEADDR is set
)

// Reply flags.
const (
	FlagEOF = uint8(1 << 0) // Read: the stream ended and nothing is left
)

// Result is a GLNXSOCK2 result. The kernel turns it into a Linux errno with
// the call's context.
type Result uint8

const (
	ResultOK Result = iota
	ResultWouldBlock
	ResultInProgress
	ResultAlready
	ResultIsConnected
	ResultNotConnected
	ResultInvalidArgument
	ResultNotSupported
	ResultNoOption
	ResultFamilyLimit
	ResultServiceLimit
	ResultNoBuffers
	ResultDown
	ResultNoAddress
	ResultNoRoute
	ResultHostUnreachable
	ResultReconfigured
	ResultAddressInUse
	ResultRefused
	ResultTimeout
	ResultReset
	ResultClosed
	ResultMessageSize
	ResultDestinationRequired
	ResultServiceLost
	ResultStale
	lastResult = ResultStale
)

// Ready is the readiness mask of one socket.
type Ready uint32

const (
	// ReadyIn: a read returns data, EOF, or an error without blocking; a
	// listener has a child to accept.
	ReadyIn Ready = 1 << iota
	// ReadyOut: a write takes at least one byte without blocking, or a
	// connect has finished either way.
	ReadyOut
	// ReadyError: a pending error waits in GetPendingError.
	ReadyError
	// ReadyHangup: both directions are closed or the transport is gone.
	ReadyHangup
	// ReadyReadHangup: the peer closed its write side.
	ReadyReadHangup
	allReady = ReadyIn | ReadyOut | ReadyError | ReadyHangup | ReadyReadHangup
)

// Socket identities: the session generation in the high 32 bits, a socket
// generation in bits 8 to 31, and the family table index in the low byte.
// The kernel uses only the index, to find the readiness entry; it never
// builds an identity itself.
func SocketIndex(socket uint64) (int, bool) {
	index := int(socket & 0xff)
	return index, socket != 0 && index < MaxSockets
}

func MakeSocket(session uint32, generation uint32, index int) uint64 {
	return uint64(session)<<32 | uint64(generation&0xffffff)<<8 | uint64(index&0xff)
}

// SessionOf is the session generation a socket identity belongs to.
func SessionOf(socket uint64) uint32 { return uint32(socket >> 32) }

// Request is one decoded request. Fields an opcode does not use are zero on
// the wire and in the struct:
//
//	Open:            Length (the Kind)
//	Bind:            Socket, Flags (ReuseAddr), Address, Port
//	Listen:          Socket, Length (backlog)
//	Accept:          Socket
//	Connect:         Socket, Address, Port
//	Read, Write:     Socket
//	SendTo:          Socket, Address, Port
//	Shutdown:        Socket, Length (0, 1, or 2)
//	GetSockName, GetPeerName, GetPendingError, QueryReady, Close: Socket
//	GetOption:       Socket, Level, Name
//	SetOption:       Socket, Level, Name, Value
//	Cancel:          Value (the abandoned correlation)
//	Doorbell, ReadyChanged: nothing
type Request struct {
	Opcode  Opcode
	Flags   uint8
	Slot    uint8
	Length  uint32
	Socket  uint64
	Address [4]byte
	Port    uint16
	Level   uint32
	Name    uint32
	Value   uint64
}

// Reply is one decoded reply. Socket is the request's socket, or the new
// socket of an Open; a failed Open names none.
//
//	Open:            Socket
//	Bind:            Local, LocalPort
//	Accept:          Value (child), Address, Port, Local, LocalPort
//	Connect:         Local, LocalPort on OK
//	Read:            Flags (EOF)
//	GetSockName:     Local, LocalPort
//	GetPeerName:     Address, Port
//	GetOption, SetOption, GetPendingError: Value
//	QueryReady:      Value (Ready), Extra (readable bytes)
type Reply struct {
	Opcode    Opcode
	Result    Result
	Slot      uint8
	Flags     uint8
	Length    uint32
	Socket    uint64
	Address   [4]byte
	Port      uint16
	Local     [4]byte
	LocalPort uint16
	Value     uint64
	Extra     uint64
}

// Wire layout, shared by requests and replies where the field is the same.
const (
	offOpcode  = 0
	offSecond  = 1 // request Flags, reply Result
	offSlot    = 2
	offThird   = 3 // request reserved, reply Flags
	offLength  = 4
	offSocket  = 8
	offAddress = 16
	offPort    = 20
	offGap     = 22 // two reserved bytes
	offLevel   = 24 // request Level, reply Local
	offName    = 28 // request Name, reply LocalPort and two reserved bytes
	offValue   = 32
	offExtra   = 40 // request reserved, reply Extra
)

type field uint16

const (
	fSocket field = 1 << iota
	fFlags
	fSlot
	fLength
	fAddress
	fLevel
	fValue
	fLocal
	fExtra
)

func requestFields(op Opcode) (field, uint8, bool) {
	switch op {
	case OpOpen:
		return fLength, 0, true
	case OpBind:
		return fSocket | fFlags | fAddress, FlagReuseAddr, true
	case OpListen, OpShutdown:
		return fSocket | fLength, 0, true
	case OpAccept, OpGetSockName, OpGetPeerName, OpGetPendingError, OpQueryReady, OpClose:
		return fSocket, 0, true
	case OpConnect, OpSendTo:
		return fSocket | fAddress, 0, true
	case OpRead, OpWrite:
		return fSocket, 0, true
	case OpGetOption:
		return fSocket | fLevel, 0, true
	case OpSetOption:
		return fSocket | fLevel | fValue, 0, true
	case OpCancel:
		return fValue, 0, true
	case OpDoorbell, OpReadyChanged:
		return 0, 0, true
	}
	return 0, 0, false
}

func notification(op Opcode) bool { return op == OpDoorbell || op == OpReadyChanged }

var noAddress [4]byte

func validRequest(r *Request) bool {
	used, flags, known := requestFields(r.Opcode)
	if !known || r.Flags&^flags != 0 {
		return false
	}
	if used&fSocket == 0 && r.Socket != 0 || used&fFlags == 0 && r.Flags != 0 || used&fSlot == 0 && r.Slot != 0 ||
		used&fLength == 0 && r.Length != 0 || used&fAddress == 0 && (r.Address != noAddress || r.Port != 0) ||
		used&fLevel == 0 && (r.Level != 0 || r.Name != 0) || used&fValue == 0 && r.Value != 0 {
		return false
	}
	if used&fSocket != 0 {
		if _, ok := SocketIndex(r.Socket); !ok {
			return false
		}
	}
	switch r.Opcode {
	case OpOpen:
		return r.Length == KindStream || r.Length == KindDatagram
	case OpConnect, OpSendTo:
		return r.Port != 0
	case OpShutdown:
		return r.Length <= 2
	case OpCancel:
		return r.Value != 0
	}
	return true
}

// EncodeRequest fails for a request with a field its opcode does not use.
// Notifications take correlation 0, requests a nonzero one. The kernel sends
// requests and Doorbell; the service sends ReadyChanged with EncodeRequest
// too, since it has no reply.
func EncodeRequest(r *Request, correlation uint64, out *ipc.Message) bool {
	if !validRequest(r) || notification(r.Opcode) != (correlation == 0) {
		return false
	}
	kind := ipc.KindRequest
	if notification(r.Opcode) {
		kind = ipc.KindNotification
	}
	prepare(out, kind, correlation, RequestSize)
	p := out.Payload[:RequestSize]
	p[offOpcode], p[offSecond], p[offSlot] = byte(r.Opcode), r.Flags, r.Slot
	binary.LittleEndian.PutUint32(p[offLength:], r.Length)
	binary.LittleEndian.PutUint64(p[offSocket:], r.Socket)
	copy(p[offAddress:], r.Address[:])
	binary.LittleEndian.PutUint16(p[offPort:], r.Port)
	binary.LittleEndian.PutUint32(p[offLevel:], r.Level)
	binary.LittleEndian.PutUint32(p[offName:], r.Name)
	binary.LittleEndian.PutUint64(p[offValue:], r.Value)
	return true
}

// DecodeRequest fills out only from a well-formed request or notification
// and returns its correlation, zero for a notification.
func DecodeRequest(in *ipc.Message, out *Request) (uint64, bool) {
	if in.ProtocolID != ProtocolID || in.Version != Version || in.PayloadSize != RequestSize || in.HandleCount != 0 {
		return 0, false
	}
	p := in.Payload[:RequestSize]
	if p[offThird] != 0 || !zero(p[offGap:offLevel]) || !zero(p[offExtra:RequestSize]) {
		return 0, false
	}
	r := Request{Opcode: Opcode(p[offOpcode]), Flags: p[offSecond], Slot: p[offSlot],
		Length: binary.LittleEndian.Uint32(p[offLength:]), Socket: binary.LittleEndian.Uint64(p[offSocket:]),
		Port: binary.LittleEndian.Uint16(p[offPort:]), Level: binary.LittleEndian.Uint32(p[offLevel:]),
		Name: binary.LittleEndian.Uint32(p[offName:]), Value: binary.LittleEndian.Uint64(p[offValue:])}
	copy(r.Address[:], p[offAddress:])
	if !validRequest(&r) {
		return 0, false
	}
	if notification(r.Opcode) {
		if in.Kind != ipc.KindNotification || in.Correlation != 0 {
			return 0, false
		}
	} else if in.Kind != ipc.KindRequest || in.Correlation == 0 {
		return 0, false
	}
	*out = r
	return in.Correlation, true
}

func replyFields(r *Reply) (field, uint8) {
	ok := r.Result == ResultOK
	switch r.Opcode {
	case OpRead:
		if ok {
			return fSocket | fFlags, FlagEOF
		}
	case OpOpen:
		if ok {
			return fSocket, 0
		}
		return 0, 0
	case OpBind, OpGetSockName:
		if ok {
			return fSocket | fLocal, 0
		}
	case OpAccept:
		if ok {
			return fSocket | fValue | fAddress | fLocal, 0
		}
	case OpConnect:
		if ok {
			return fSocket | fAddress | fLocal, 0
		}
	case OpGetPeerName:
		if ok {
			return fSocket | fAddress, 0
		}
	case OpGetOption, OpSetOption, OpGetPendingError:
		if ok {
			return fSocket | fValue, 0
		}
	case OpQueryReady:
		if ok {
			return fSocket | fValue | fExtra, 0
		}
	case OpCancel:
		return 0, 0
	}
	return fSocket, 0
}

func validReply(r *Reply) bool {
	if r.Opcode < OpOpen || r.Opcode > lastOpcode || notification(r.Opcode) || r.Result > lastResult {
		return false
	}
	used, flags := replyFields(r)
	if r.Flags&^flags != 0 ||
		used&fSlot == 0 && r.Slot != 0 || used&fLength == 0 && r.Length != 0 ||
		used&fAddress == 0 && (r.Address != noAddress || r.Port != 0) ||
		used&fLocal == 0 && (r.Local != noAddress || r.LocalPort != 0) ||
		used&fValue == 0 && r.Value != 0 || used&fExtra == 0 && r.Extra != 0 {
		return false
	}
	if used&fSocket == 0 {
		if r.Socket != 0 {
			return false
		}
	} else if _, ok := SocketIndex(r.Socket); !ok {
		return false
	}
	switch r.Opcode {
	case OpAccept:
		if r.Result == ResultOK {
			_, ok := SocketIndex(r.Value)
			return ok
		}
	case OpQueryReady:
		if r.Result == ResultOK {
			return Ready(r.Value)&^allReady == 0 && r.Extra <= 0xffffffff
		}
	case OpGetPendingError:
		if r.Result == ResultOK {
			return r.Value <= uint64(lastResult)
		}
	}
	return true
}

// EncodeReply fails for a reply with a field its opcode and result do not
// use.
func EncodeReply(r *Reply, correlation uint64, out *ipc.Message) bool {
	if correlation == 0 || !validReply(r) {
		return false
	}
	prepare(out, ipc.KindReply, correlation, ReplySize)
	p := out.Payload[:ReplySize]
	p[offOpcode], p[offSecond], p[offSlot], p[offThird] = byte(r.Opcode), byte(r.Result), r.Slot, r.Flags
	binary.LittleEndian.PutUint32(p[offLength:], r.Length)
	binary.LittleEndian.PutUint64(p[offSocket:], r.Socket)
	copy(p[offAddress:], r.Address[:])
	binary.LittleEndian.PutUint16(p[offPort:], r.Port)
	copy(p[offLevel:], r.Local[:])
	binary.LittleEndian.PutUint16(p[offName:], r.LocalPort)
	binary.LittleEndian.PutUint64(p[offValue:], r.Value)
	binary.LittleEndian.PutUint64(p[offExtra:], r.Extra)
	return true
}

// DecodeReply fills out only from a well-formed reply and returns its
// correlation. The caller still matches the correlation and opcode with the
// request it sent.
func DecodeReply(in *ipc.Message, out *Reply) (uint64, bool) {
	if in.ProtocolID != ProtocolID || in.Version != Version || in.Kind != ipc.KindReply || in.Correlation == 0 ||
		in.PayloadSize != ReplySize || in.HandleCount != 0 {
		return 0, false
	}
	p := in.Payload[:ReplySize]
	if !zero(p[offGap:offLevel]) || !zero(p[offName+2:offValue]) {
		return 0, false
	}
	r := Reply{Opcode: Opcode(p[offOpcode]), Result: Result(p[offSecond]), Slot: p[offSlot], Flags: p[offThird],
		Length: binary.LittleEndian.Uint32(p[offLength:]), Socket: binary.LittleEndian.Uint64(p[offSocket:]),
		Port: binary.LittleEndian.Uint16(p[offPort:]), LocalPort: binary.LittleEndian.Uint16(p[offName:]),
		Value: binary.LittleEndian.Uint64(p[offValue:]), Extra: binary.LittleEndian.Uint64(p[offExtra:])}
	copy(r.Address[:], p[offAddress:])
	copy(r.Local[:], p[offLevel:])
	if !validReply(&r) {
		return 0, false
	}
	*out = r
	return in.Correlation, true
}

func prepare(out *ipc.Message, kind ipc.Kind, correlation uint64, size uint16) {
	*out = ipc.Message{ProtocolID: ProtocolID, Version: Version, Kind: kind, Correlation: correlation, PayloadSize: size}
}

func zero(bytes []byte) bool {
	for _, b := range bytes {
		if b != 0 {
			return false
		}
	}
	return true
}
