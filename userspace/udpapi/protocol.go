// Package udpapi defines GUDP, the protocol between a program that uses UDP
// and the network service (ADR 0195).
//
// PID 1 gives a program a UDP session: one channel and two shared pools. The
// program writes outgoing payloads into TX slots and the service only reads
// them; the service writes received payloads into RX slots and the program
// only reads them. A slot changes owner only by message: Send and SendTo hand
// a TX slot to the service and their reply hands it back; a RecvFrom reply
// with result OK hands an RX slot to the program and Release hands it back.
// The service closes a session that releases a slot it does not hold or
// names a slot or endpoint that is not its own.
//
// Every operation except Release is a request answered by exactly one reply
// with the request's correlation. A RecvFrom waits in the service until a
// datagram arrives or its endpoint closes; each endpoint takes one waiting
// RecvFrom at a time. With FlagNoWait it answers ResultWouldBlock instead
// of waiting.
//
// Version 2 adds the flags byte, OpReady, OpWaitReady, OpOption, Peek on
// RecvFrom and ResultMessageSize for a datagram larger than its path
// carries; a send is never truncated. A RecvFrom reply's Length is always
// the whole datagram: the slot holds up to MaxPayload bytes, the most any
// datagram can have.
//
// Request body, 24 bytes:
//
//	0      opcode
//	1      slot
//	2      flags    Flag* bits the opcode allows
//	3      option   Option: Option* selector
//	4..7   endpoint
//	8..11  address  Bind, Connect, SendTo; Option MaxPayload: destination
//	12..13 port
//	14..15 length
//	16..17 mask     WaitReady: the mask the client last saw
//	18..23 reserved
//
// Reply body, 32 bytes:
//
//	0      opcode
//	1      result
//	2      slot
//	3      reserved
//	4..7   endpoint
//	8..11  address
//	12..13 port
//	14..15 length
//	16..19 local
//	20..21 local port
//	22..23 mask     Ready, WaitReady OK: Ready* bits
//	24..27 value    Ready, WaitReady OK: next datagram length; Option OK: value
//	28..31 count    Ready, WaitReady OK: datagrams queued
package udpapi

import (
	"encoding/binary"
	"goos/kernel/ipc"
)

const (
	ProtocolID  = uint64(0x474e455455445031)
	Version     = uint16(2)
	RequestSize = 24
	ReplySize   = 32

	// MaxPayload is the largest datagram either way and the slot size. The
	// service's max-payload setting may lower the limit it applies, never
	// raise it.
	MaxPayload = 4096
	SlotSize   = MaxPayload
	TXSlots    = 4
	RXSlots    = 8
	pageSize   = 4096
	// TXPoolPages and RXPoolPages size the two pools.
	TXPoolPages = TXSlots * SlotSize / pageSize
	RXPoolPages = RXSlots * SlotSize / pageSize

	requestOpcode   = 0
	requestSlot     = 1
	requestFlags    = 2
	requestOption   = 3
	requestEndpoint = 4
	requestAddress  = 8
	requestPort     = 12
	requestLength   = 14
	requestMask     = 16
	requestReserved = 18

	replyOpcode    = 0
	replyResult    = 1
	replySlot      = 2
	replyReserved  = 3
	replyEndpoint  = 4
	replyAddress   = 8
	replyPort      = 12
	replyLength    = 14
	replyLocal     = 16
	replyLocalPort = 20
	replyMask      = 22
	replyValue     = 24
	replyCount     = 28

	addressSize = 4
)

type Opcode uint8

const (
	// OpOpen creates an endpoint. The reply's Endpoint names it.
	OpOpen Opcode = iota + 1
	// OpBind binds Endpoint to Address (zero for the wildcard) and Port
	// (zero for an ephemeral one). The reply carries the address and port
	// bound.
	OpBind
	// OpConnect fixes Endpoint's peer at Address and Port. The reply also
	// carries the source address the peer is reached from now and the local
	// port, in Local and LocalPort. An endpoint bound to the wildcard stays
	// on it and picks its source at each send.
	OpConnect
	// OpSend sends Length bytes of TX Slot to a connected endpoint's peer.
	OpSend
	// OpSendTo sends Length bytes of TX Slot to Address and Port.
	OpSendTo
	// OpRecvFrom takes the oldest datagram queued on a bound endpoint. On
	// OK the reply hands over RX Slot holding Length bytes from Address and
	// Port, sent to Local.
	OpRecvFrom
	// OpRelease returns RX Slot to the service. It is a notification and
	// gets no reply.
	OpRelease
	// OpClose closes Endpoint. A RecvFrom or WaitReady waiting on it is
	// answered first, with ResultClosed.
	OpClose
	// OpReady answers at once with Endpoint's mask, the length of the next
	// datagram and the number queued.
	OpReady
	// OpWaitReady answers when Endpoint's mask differs from Mask, at once
	// if it already does. An endpoint holds one.
	OpWaitReady
	// OpOption reads the effective value Option selects.
	OpOption
	lastOpcode = OpOption
)

// Result is a GUDP1 result code.
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
	ResultDriverFault
	// ResultClosed answers a RecvFrom or WaitReady whose endpoint was
	// closed while it waited.
	ResultClosed
	// ResultWouldBlock answers a NoWait RecvFrom with nothing queued.
	ResultWouldBlock
	// ResultMessageSize answers a Send or SendTo larger than its path
	// carries: max-payload, and the link MTU less 28 on the wire.
	ResultMessageSize
	lastResult = ResultMessageSize
)

// Flags select request behavior. Each opcode allows only its own.
const (
	// FlagNoWait: RecvFrom answers at once.
	FlagNoWait uint8 = 1 << iota
	// FlagPeek: RecvFrom copies the datagram and leaves it queued.
	FlagPeek
	// FlagReuseAddr: Bind with SO_REUSEADDR.
	FlagReuseAddr
)

// Ready bits of a mask.
const (
	ReadyReadable uint16 = 1 << iota // a datagram is queued, perhaps empty
	ReadyWritable                    // a send is not refused for lack of endpoint resources
	ReadyAll      = ReadyWritable<<1 - 1
)

// Option selectors. OptionMaxPayload names a destination in Address and
// reads the largest datagram the path to it carries.
const (
	OptionTTL uint8 = iota + 1
	OptionMaxPayload
	lastOption = OptionMaxPayload
)

// Request is one decoded request body. Which fields an opcode uses is fixed;
// every other field is zero on the wire and in the struct:
//
//	Open:     nothing
//	Bind:     Endpoint, Address, Port (both may be zero)
//	Connect:  Endpoint, Address and Port (both nonzero)
//	Send:     Endpoint, Slot, Length
//	SendTo:   Endpoint, Slot, Length, Address and Port (both nonzero)
//	RecvFrom: Endpoint, Flags NoWait and Peek
//	Release:  Slot
//	Close:    Endpoint
//	Ready:    Endpoint
//	WaitReady: Endpoint, Mask
//	Option:   Endpoint, Option, Address for OptionMaxPayload
type Request struct {
	Opcode   Opcode
	Slot     uint8
	Flags    uint8
	Option   uint8
	Endpoint uint32
	Address  [addressSize]byte
	Port     uint16
	Length   uint16
	Mask     uint16
}

// Reply is one decoded reply body:
//
//	Open, OK:     Endpoint (nonzero)
//	Bind, OK:     Endpoint, Address (may be zero), Port
//	Connect, OK:  Endpoint, Address, Port, Local, LocalPort
//	Send, SendTo: Endpoint, Slot (the TX slot returned), for every result
//	RecvFrom, OK: Endpoint, Slot, Address, Port, Length, Local
//	Ready, WaitReady, OK: Endpoint, Mask, Value, Count
//	Option, OK:   Endpoint, Value
//	any other:    Endpoint, as the request named it, and nothing else
type Reply struct {
	Opcode    Opcode
	Result    Result
	Slot      uint8
	Endpoint  uint32
	Address   [addressSize]byte
	Port      uint16
	Length    uint16
	Local     [addressSize]byte
	LocalPort uint16
	Mask      uint16
	Value     uint32
	Count     uint32
}

var noAddress [addressSize]byte

func allowedFlags(opcode Opcode) uint8 {
	switch opcode {
	case OpRecvFrom:
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
		return r.Endpoint == 0 && r.Slot == 0 && r.Length == 0 && none
	case OpBind:
		return r.Endpoint != 0 && r.Slot == 0 && r.Length == 0
	case OpConnect:
		return r.Endpoint != 0 && r.Slot == 0 && r.Length == 0 && r.Address != noAddress && r.Port != 0
	case OpSend:
		return r.Endpoint != 0 && r.Slot < TXSlots && r.Length <= MaxPayload && none
	case OpSendTo:
		return r.Endpoint != 0 && r.Slot < TXSlots && r.Length <= MaxPayload && r.Address != noAddress && r.Port != 0
	case OpRecvFrom, OpClose, OpReady:
		return r.Endpoint != 0 && r.Slot == 0 && r.Length == 0 && none
	case OpWaitReady:
		return r.Endpoint != 0 && r.Slot == 0 && r.Length == 0 && none && r.Mask&^ReadyAll == 0
	case OpOption:
		if r.Option == OptionMaxPayload {
			return r.Endpoint != 0 && r.Slot == 0 && r.Length == 0 && r.Address != noAddress && r.Port == 0
		}
		return r.Endpoint != 0 && r.Slot == 0 && r.Length == 0 && none && r.Option <= lastOption
	case OpRelease:
		return r.Endpoint == 0 && r.Slot < RXSlots && r.Length == 0 && none
	}
	return false
}

// EncodeRequest fails when r sets a field its opcode does not use. Release
// is a notification and takes correlation 0; every other request takes a
// nonzero one.
func EncodeRequest(r *Request, correlation uint64, out *ipc.Message) bool {
	if !validRequest(r) || (r.Opcode == OpRelease) != (correlation == 0) {
		return false
	}
	kind := ipc.KindRequest
	if r.Opcode == OpRelease {
		kind = ipc.KindNotification
	}
	prepare(out, kind, correlation, RequestSize)
	payload := out.Payload[:RequestSize]
	payload[requestOpcode] = byte(r.Opcode)
	payload[requestSlot] = r.Slot
	payload[requestFlags] = r.Flags
	payload[requestOption] = r.Option
	binary.LittleEndian.PutUint32(payload[requestEndpoint:], r.Endpoint)
	copy(payload[requestAddress:], r.Address[:])
	binary.LittleEndian.PutUint16(payload[requestPort:], r.Port)
	binary.LittleEndian.PutUint16(payload[requestLength:], r.Length)
	binary.LittleEndian.PutUint16(payload[requestMask:], r.Mask)
	return true
}

// DecodeRequest fills out only from a well-formed request and returns its
// correlation, zero for Release.
func DecodeRequest(in *ipc.Message, out *Request) (uint64, bool) {
	if in.ProtocolID != ProtocolID || in.Version != Version || in.PayloadSize != RequestSize || in.HandleCount != 0 {
		return 0, false
	}
	payload := in.Payload[:RequestSize]
	if !zero(payload[requestReserved:RequestSize]) {
		return 0, false
	}
	r := Request{
		Opcode:   Opcode(payload[requestOpcode]),
		Slot:     payload[requestSlot],
		Flags:    payload[requestFlags],
		Option:   payload[requestOption],
		Mask:     binary.LittleEndian.Uint16(payload[requestMask:]),
		Endpoint: binary.LittleEndian.Uint32(payload[requestEndpoint:]),
		Port:     binary.LittleEndian.Uint16(payload[requestPort:]),
		Length:   binary.LittleEndian.Uint16(payload[requestLength:]),
	}
	copy(r.Address[:], payload[requestAddress:])
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

func validReply(r *Reply) bool {
	if r.Result > lastResult || r.Opcode < OpOpen || r.Opcode > lastOpcode || r.Opcode == OpRelease {
		return false
	}
	addressed := r.Address != noAddress || r.Port != 0
	local := r.Local != noAddress || r.LocalPort != 0
	readiness := r.Mask != 0 || r.Value != 0 || r.Count != 0
	ok := r.Result == ResultOK
	switch {
	case readiness && (!ok || (r.Opcode != OpReady && r.Opcode != OpWaitReady && r.Opcode != OpOption)),
		r.Result == ResultWouldBlock && r.Opcode != OpRecvFrom,
		r.Result == ResultMessageSize && r.Opcode != OpSend && r.Opcode != OpSendTo:
		return false
	case r.Opcode == OpSend || r.Opcode == OpSendTo:
		return r.Endpoint != 0 && r.Slot < TXSlots && !addressed && r.Length == 0 && !local
	case !ok:
		return (r.Endpoint != 0 || r.Opcode == OpOpen) && r.Slot == 0 && !addressed && r.Length == 0 && !local &&
			(r.Result != ResultClosed || r.Opcode == OpRecvFrom || r.Opcode == OpWaitReady)
	}
	if r.Endpoint == 0 {
		return false
	}
	switch r.Opcode {
	case OpOpen, OpClose:
		return r.Slot == 0 && !addressed && r.Length == 0 && !local && !readiness
	case OpReady, OpWaitReady:
		return r.Slot == 0 && !addressed && r.Length == 0 && !local && r.Mask&^ReadyAll == 0
	case OpOption:
		return r.Slot == 0 && !addressed && r.Length == 0 && !local && r.Mask == 0 && r.Count == 0
	case OpBind:
		return r.Slot == 0 && r.Port != 0 && r.Length == 0 && !local && !readiness
	case OpConnect:
		return r.Slot == 0 && r.Address != noAddress && r.Port != 0 && r.Length == 0 && r.Local != noAddress && r.LocalPort != 0 && !readiness
	case OpRecvFrom:
		return r.Slot < RXSlots && r.Address != noAddress && r.Length <= MaxPayload && r.Local != noAddress && r.LocalPort == 0 && !readiness
	}
	return false
}

// EncodeReply fails when r sets a field its opcode and result do not use;
// see Reply.
func EncodeReply(r *Reply, correlation uint64, out *ipc.Message) bool {
	if correlation == 0 || !validReply(r) {
		return false
	}
	prepare(out, ipc.KindReply, correlation, ReplySize)
	payload := out.Payload[:ReplySize]
	payload[replyOpcode] = byte(r.Opcode)
	payload[replyResult] = byte(r.Result)
	payload[replySlot] = r.Slot
	binary.LittleEndian.PutUint32(payload[replyEndpoint:], r.Endpoint)
	copy(payload[replyAddress:], r.Address[:])
	binary.LittleEndian.PutUint16(payload[replyPort:], r.Port)
	binary.LittleEndian.PutUint16(payload[replyLength:], r.Length)
	copy(payload[replyLocal:], r.Local[:])
	binary.LittleEndian.PutUint16(payload[replyLocalPort:], r.LocalPort)
	binary.LittleEndian.PutUint16(payload[replyMask:], r.Mask)
	binary.LittleEndian.PutUint32(payload[replyValue:], r.Value)
	binary.LittleEndian.PutUint32(payload[replyCount:], r.Count)
	return true
}

// DecodeReply fills out only from a well-formed reply and returns its
// correlation. The caller matches the correlation to a request it sent.
func DecodeReply(in *ipc.Message, out *Reply) (uint64, bool) {
	if in.ProtocolID != ProtocolID || in.Version != Version || in.Kind != ipc.KindReply || in.Correlation == 0 ||
		in.PayloadSize != ReplySize || in.HandleCount != 0 {
		return 0, false
	}
	payload := in.Payload[:ReplySize]
	if payload[replyReserved] != 0 {
		return 0, false
	}
	r := Reply{
		Opcode:    Opcode(payload[replyOpcode]),
		Result:    Result(payload[replyResult]),
		Slot:      payload[replySlot],
		Endpoint:  binary.LittleEndian.Uint32(payload[replyEndpoint:]),
		Port:      binary.LittleEndian.Uint16(payload[replyPort:]),
		Length:    binary.LittleEndian.Uint16(payload[replyLength:]),
		LocalPort: binary.LittleEndian.Uint16(payload[replyLocalPort:]),
		Mask:      binary.LittleEndian.Uint16(payload[replyMask:]),
		Value:     binary.LittleEndian.Uint32(payload[replyValue:]),
		Count:     binary.LittleEndian.Uint32(payload[replyCount:]),
	}
	copy(r.Address[:], payload[replyAddress:])
	copy(r.Local[:], payload[replyLocal:])
	if !validReply(&r) {
		return 0, false
	}
	*out = r
	return in.Correlation, true
}

// Text returns short lowercase text for a result, or "" for an unknown one.
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
	case ResultDriverFault:
		return "driver fault"
	case ResultClosed:
		return "closed"
	case ResultWouldBlock:
		return "would block"
	case ResultMessageSize:
		return "message too long"
	}
	return ""
}

func zero(bytes []byte) bool {
	for _, value := range bytes {
		if value != 0 {
			return false
		}
	}
	return true
}

// prepare fills out field by field: a composite literal of an ipc.Message
// exceeds TinyGo's stack allocation limit and would leak heap on every call.
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
