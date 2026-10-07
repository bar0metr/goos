// Package udpapi defines GUDP1, the protocol between a program that uses UDP
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
// RecvFrom at a time.
package udpapi

import (
	"encoding/binary"
	"goos/kernel/ipc"
)

const (
	ProtocolID  = uint64(0x474e455455445031)
	Version     = uint16(1)
	RequestSize = 16
	ReplySize   = 24

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
	requestReserved = 2
	requestEndpoint = 4
	requestAddress  = 8
	requestPort     = 12
	requestLength   = 14

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
	replyTail      = 22

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
	// OpClose closes Endpoint. A RecvFrom waiting on it is answered first,
	// with ResultClosed.
	OpClose
	lastOpcode = OpClose
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
	// ResultClosed answers a RecvFrom whose endpoint was closed while it
	// waited.
	ResultClosed
	lastResult = ResultClosed
)

// Request is one decoded request body. Which fields an opcode uses is fixed;
// every other field is zero on the wire and in the struct:
//
//	Open:     nothing
//	Bind:     Endpoint, Address, Port (both may be zero)
//	Connect:  Endpoint, Address and Port (both nonzero)
//	Send:     Endpoint, Slot, Length
//	SendTo:   Endpoint, Slot, Length, Address and Port (both nonzero)
//	RecvFrom: Endpoint
//	Release:  Slot
//	Close:    Endpoint
type Request struct {
	Opcode   Opcode
	Slot     uint8
	Endpoint uint32
	Address  [addressSize]byte
	Port     uint16
	Length   uint16
}

// Reply is one decoded reply body:
//
//	Open, OK:     Endpoint (nonzero)
//	Bind, OK:     Endpoint, Address (may be zero), Port
//	Connect, OK:  Endpoint, Address, Port, Local, LocalPort
//	Send, SendTo: Endpoint, Slot (the TX slot returned), for every result
//	RecvFrom, OK: Endpoint, Slot, Address, Port, Length, Local
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
}

var noAddress [addressSize]byte

func validRequest(r *Request) bool {
	none := r.Address == noAddress && r.Port == 0
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
	case OpRecvFrom, OpClose:
		return r.Endpoint != 0 && r.Slot == 0 && r.Length == 0 && none
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
	binary.LittleEndian.PutUint32(payload[requestEndpoint:], r.Endpoint)
	copy(payload[requestAddress:], r.Address[:])
	binary.LittleEndian.PutUint16(payload[requestPort:], r.Port)
	binary.LittleEndian.PutUint16(payload[requestLength:], r.Length)
	return true
}

// DecodeRequest fills out only from a well-formed request and returns its
// correlation, zero for Release.
func DecodeRequest(in *ipc.Message, out *Request) (uint64, bool) {
	if in.ProtocolID != ProtocolID || in.Version != Version || in.PayloadSize != RequestSize || in.HandleCount != 0 {
		return 0, false
	}
	payload := in.Payload[:RequestSize]
	if !zero(payload[requestReserved:requestEndpoint]) {
		return 0, false
	}
	r := Request{
		Opcode:   Opcode(payload[requestOpcode]),
		Slot:     payload[requestSlot],
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
	ok := r.Result == ResultOK
	switch {
	case r.Opcode == OpSend || r.Opcode == OpSendTo:
		return r.Endpoint != 0 && r.Slot < TXSlots && !addressed && r.Length == 0 && !local
	case !ok:
		return (r.Endpoint != 0 || r.Opcode == OpOpen) && r.Slot == 0 && !addressed && r.Length == 0 && !local &&
			(r.Result != ResultClosed || r.Opcode == OpRecvFrom)
	}
	if r.Endpoint == 0 {
		return false
	}
	switch r.Opcode {
	case OpOpen, OpClose:
		return r.Slot == 0 && !addressed && r.Length == 0 && !local
	case OpBind:
		return r.Slot == 0 && r.Port != 0 && r.Length == 0 && !local
	case OpConnect:
		return r.Slot == 0 && r.Address != noAddress && r.Port != 0 && r.Length == 0 && r.Local != noAddress && r.LocalPort != 0
	case OpRecvFrom:
		return r.Slot < RXSlots && r.Address != noAddress && r.Length <= MaxPayload && r.Local != noAddress && r.LocalPort == 0
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
	if payload[replyReserved] != 0 || !zero(payload[replyTail:ReplySize]) {
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
