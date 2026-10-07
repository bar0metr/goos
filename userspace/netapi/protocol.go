// Package netapi defines GNETAPI1, the protocol between network commands and
// the network service, and GNETCTL1, PID 1's control protocol to the service.
package netapi

import (
	"encoding/binary"
	"goos/kernel/ipc"
)

const (
	ProtocolID = uint64(0x474e455441504931)
	// Version 2 is the 152-byte reply with the MTU, the fallback flag, the
	// conflict counters and the loopback count. A version-1 message is not
	// this protocol and fails decoding.
	Version          = uint16(2)
	RequestSize      = 16
	ReplySize        = 152
	MinEchoTimeoutMS = 100
	MaxEchoTimeoutMS = 5000
	MaxPrefix        = 32
)

const (
	requestOpcode      = 0
	requestDestination = 4
	requestSequence    = 8
	requestTimeout     = 10
	requestReserved    = 12

	replyOpcode    = 0
	replyResult    = 1
	replyState     = 2
	replyMode      = 3
	replyLocal     = 4
	replyGateway   = 8
	replyResponder = 12
	replyRTT       = 16
	replyMAC       = 24
	replyPrefix    = 30
	replyLink      = 31
	replyLease     = 32
	replySequence  = 40
	replyField     = 42
	replyRX        = 44
	replyTX        = 52
	replyDrop      = 60
	replyMalformed = 68
	replyMTU       = 76
	replyFallback  = 78
	replyFlags     = 79
	replyCounters  = 80
	replyRun       = 144
	replyReserved  = 145

	// flagRateLimited is the only flag: the next address claim waits for
	// the conflict rate limit.
	flagRateLimited = 1
	counterCount    = 8

	addressSize = 4
	macSize     = 6
)

type Opcode uint8

const (
	OpStatus Opcode = iota + 1
	OpEcho
	OpReload
	lastOpcode = OpReload
)

type Result uint8

const (
	ResultOK Result = iota
	ResultDown
	ResultNoAddress
	ResultNoRoute
	ResultNeighborUnreachable
	ResultTimeout
	ResultBusy
	ResultReconfigured
	ResultPermissionDenied
	ResultInvalidArgument
	ResultDriverFault
	ResultConfigInvalid
	ResultRemoteUnreachable
	ResultTimeExceeded
	lastResult = ResultTimeExceeded
)

type State uint8

const (
	StateDown State = iota
	StateConfiguring
	StateReady
	StateFailed
	lastState = StateFailed
)

type Mode uint8

const (
	ModeDHCP Mode = iota
	ModeStatic
	lastMode = ModeStatic
)

type Field uint8

const (
	FieldNone Field = iota
	FieldMode
	FieldAddress
	FieldPrefix
	FieldGateway
	FieldTTL
	FieldProbeCount
	FieldProbeWait
	FieldProbeMin
	FieldProbeMax
	FieldAnnounceCount
	FieldAnnounceInterval
	FieldDefendInterval
	FieldMaxConflicts
	FieldConflictRateLimit
	FieldARPCacheTimeout
	FieldARPResolveInterval
	FieldARPResolveAttempts
	FieldEchoReplyRate
	FieldDHCPInitialRetry
	FieldDHCPMaxRetry
	FieldDHCPConflictRetry
	lastField = FieldDHCPConflictRetry
)

type Link uint8

const (
	LinkDown Link = iota
	LinkUp
	lastLink = LinkUp
)

type Request struct {
	Opcode      Opcode
	Destination [addressSize]byte
	Sequence    uint16
	TimeoutMS   uint16
}

// Reply is one decoded reply body. Which fields an opcode may set is fixed;
// every other field is zero on the wire and in the struct:
//
//	Status, OK:          State, Mode, Local, Gateway, MAC, Prefix, Link,
//	                     Lease, RX, TX, Drop, Malformed, MTU, Fallback,
//	                     RateLimited, the conflict counters, Loopback,
//	                     ConflictRun
//	Echo, OK:            Responder (nonzero), RTT, Sequence
//	Echo, RemoteUnreachable or TimeExceeded:
//	                     Responder (nonzero, the router that answered), Sequence
//	Echo, other results: Sequence
//	Reload, ConfigInvalid: Field (may be FieldNone)
//	any other result:    nothing beyond Opcode and Result
type Reply struct {
	Opcode    Opcode
	Result    Result
	State     State
	Mode      Mode
	Local     [addressSize]byte
	Gateway   [addressSize]byte
	Responder [addressSize]byte
	RTT       uint64
	MAC       [macSize]byte
	Prefix    uint8
	Link      Link
	Lease     uint64
	Sequence  uint16
	Field     Field
	RX        uint64
	TX        uint64
	Drop      uint64
	Malformed uint64
	// MTU is the link's IPv4 MTU, zero without a link.
	MTU uint16
	// Fallback reports that the service runs on built-in defaults because the
	// stored network settings could not be read or were invalid at startup.
	// It clears once a reload applies the stored settings.
	Fallback bool
	// RateLimited reports that the next address claim waits for the
	// conflict rate limit; ConflictRun counts conflicts since an address
	// last held.
	RateLimited bool
	ConflictRun uint8
	// The address-conflict counters, then echoes answered on lo.
	Probes          uint64
	ProbeConflicts  uint64
	Announcements   uint64
	ActiveConflicts uint64
	Defenses        uint64
	Relinquished    uint64
	RateLimits      uint64
	Loopback        uint64
}

// counters lists the reply's eight counters in wire order.
func (r *Reply) counters() [counterCount]*uint64 {
	return [counterCount]*uint64{&r.Probes, &r.ProbeConflicts, &r.Announcements, &r.ActiveConflicts,
		&r.Defenses, &r.Relinquished, &r.RateLimits, &r.Loopback}
}

func EncodeRequest(r *Request, correlation uint64, out *ipc.Message) bool {
	if correlation == 0 || !validRequest(r) {
		return false
	}
	prepare(out, ProtocolID, Version, ipc.KindRequest, correlation, RequestSize)
	payload := out.Payload[:RequestSize]
	payload[requestOpcode] = byte(r.Opcode)
	copy(payload[requestDestination:requestDestination+addressSize], r.Destination[:])
	binary.LittleEndian.PutUint16(payload[requestSequence:], r.Sequence)
	binary.LittleEndian.PutUint16(payload[requestTimeout:], r.TimeoutMS)
	return true
}

// DecodeRequest validates a request and fills out, untouched on failure. The
// service still checks that an Echo destination is a unicast address and
// that the session may use the opcode.
func DecodeRequest(in *ipc.Message, out *Request) (correlation uint64, ok bool) {
	if !requestEnvelope(in) || in.PayloadSize != RequestSize || in.HandleCount != 0 {
		return 0, false
	}
	payload := in.Payload[:RequestSize]
	if !zero(payload[requestOpcode+1:requestDestination]) || !zero(payload[requestReserved:RequestSize]) {
		return 0, false
	}
	r := Request{
		Opcode:    Opcode(payload[requestOpcode]),
		Sequence:  binary.LittleEndian.Uint16(payload[requestSequence:]),
		TimeoutMS: binary.LittleEndian.Uint16(payload[requestTimeout:]),
	}
	copy(r.Destination[:], payload[requestDestination:requestDestination+addressSize])
	if !validRequest(&r) {
		return 0, false
	}
	*out = r
	return in.Correlation, true
}

// RequestOpcode identifies a malformed request the service can still
// answer: a GNETAPI1 request with a nonzero correlation and a known opcode
// byte, whatever its size, other bytes or attachments.
func RequestOpcode(in *ipc.Message) (Opcode, uint64, bool) {
	if !requestEnvelope(in) || in.PayloadSize == 0 {
		return 0, 0, false
	}
	opcode := Opcode(in.Payload[requestOpcode])
	if opcode < OpStatus || opcode > lastOpcode {
		return 0, 0, false
	}
	return opcode, in.Correlation, true
}

func requestEnvelope(in *ipc.Message) bool {
	return in.ProtocolID == ProtocolID && in.Version == Version && in.Kind == ipc.KindRequest && in.Correlation != 0
}

func validRequest(r *Request) bool {
	switch r.Opcode {
	case OpStatus, OpReload:
		return r.Destination == [addressSize]byte{} && r.Sequence == 0 && r.TimeoutMS == 0
	case OpEcho:
		return r.Destination != [addressSize]byte{} && r.TimeoutMS >= MinEchoTimeoutMS && r.TimeoutMS <= MaxEchoTimeoutMS
	}
	return false
}

// EncodeReply fails when r sets a field its opcode and result do not use;
// see Reply. Counters are written as given: the service saturates them with
// SaturatingAdd rather than letting them wrap.
func EncodeReply(r *Reply, correlation uint64, out *ipc.Message) bool {
	if correlation == 0 || !validReply(r) {
		return false
	}
	prepare(out, ProtocolID, Version, ipc.KindReply, correlation, ReplySize)
	payload := out.Payload[:ReplySize]
	payload[replyOpcode] = byte(r.Opcode)
	payload[replyResult] = byte(r.Result)
	payload[replyState] = byte(r.State)
	payload[replyMode] = byte(r.Mode)
	copy(payload[replyLocal:replyLocal+addressSize], r.Local[:])
	copy(payload[replyGateway:replyGateway+addressSize], r.Gateway[:])
	copy(payload[replyResponder:replyResponder+addressSize], r.Responder[:])
	binary.LittleEndian.PutUint64(payload[replyRTT:], r.RTT)
	copy(payload[replyMAC:replyMAC+macSize], r.MAC[:])
	payload[replyPrefix] = r.Prefix
	payload[replyLink] = byte(r.Link)
	binary.LittleEndian.PutUint64(payload[replyLease:], r.Lease)
	binary.LittleEndian.PutUint16(payload[replySequence:], r.Sequence)
	binary.LittleEndian.PutUint16(payload[replyField:], uint16(r.Field))
	binary.LittleEndian.PutUint64(payload[replyRX:], r.RX)
	binary.LittleEndian.PutUint64(payload[replyTX:], r.TX)
	binary.LittleEndian.PutUint64(payload[replyDrop:], r.Drop)
	binary.LittleEndian.PutUint64(payload[replyMalformed:], r.Malformed)
	binary.LittleEndian.PutUint16(payload[replyMTU:], r.MTU)
	if r.Fallback {
		payload[replyFallback] = 1
	}
	if r.RateLimited {
		payload[replyFlags] = flagRateLimited
	}
	for index, counter := range r.counters() {
		binary.LittleEndian.PutUint64(payload[replyCounters+8*index:], *counter)
	}
	payload[replyRun] = r.ConflictRun
	return true
}

// DecodeReply accepts only the reply to the request the caller sent: the
// given correlation and opcode. out is untouched on failure.
func DecodeReply(in *ipc.Message, correlation uint64, opcode Opcode, out *Reply) bool {
	if in.ProtocolID != ProtocolID || in.Version != Version || in.Kind != ipc.KindReply || correlation == 0 ||
		in.Correlation != correlation || in.PayloadSize != ReplySize || in.HandleCount != 0 {
		return false
	}
	payload := in.Payload[:ReplySize]
	field := binary.LittleEndian.Uint16(payload[replyField:])
	if Opcode(payload[replyOpcode]) != opcode || !zero(payload[replyReserved:ReplySize]) || field > uint16(lastField) ||
		payload[replyFallback] > 1 || payload[replyFlags]&^flagRateLimited != 0 {
		return false
	}
	r := Reply{
		Opcode:    opcode,
		Result:    Result(payload[replyResult]),
		State:     State(payload[replyState]),
		Mode:      Mode(payload[replyMode]),
		RTT:       binary.LittleEndian.Uint64(payload[replyRTT:]),
		Prefix:    payload[replyPrefix],
		Link:      Link(payload[replyLink]),
		Lease:     binary.LittleEndian.Uint64(payload[replyLease:]),
		Sequence:  binary.LittleEndian.Uint16(payload[replySequence:]),
		Field:     Field(field),
		RX:        binary.LittleEndian.Uint64(payload[replyRX:]),
		TX:        binary.LittleEndian.Uint64(payload[replyTX:]),
		Drop:      binary.LittleEndian.Uint64(payload[replyDrop:]),
		Malformed: binary.LittleEndian.Uint64(payload[replyMalformed:]),
		MTU:       binary.LittleEndian.Uint16(payload[replyMTU:]),
		Fallback:  payload[replyFallback] == 1,

		RateLimited: payload[replyFlags] == flagRateLimited,
		ConflictRun: payload[replyRun],
	}
	for index, counter := range r.counters() {
		*counter = binary.LittleEndian.Uint64(payload[replyCounters+8*index:])
	}
	copy(r.Local[:], payload[replyLocal:replyLocal+addressSize])
	copy(r.Gateway[:], payload[replyGateway:replyGateway+addressSize])
	copy(r.Responder[:], payload[replyResponder:replyResponder+addressSize])
	copy(r.MAC[:], payload[replyMAC:replyMAC+macSize])
	if !validReply(&r) {
		return false
	}
	*out = r
	return true
}

func validReply(r *Reply) bool {
	if r.Result > lastResult || r.State > lastState || r.Mode > lastMode || r.Field > lastField ||
		r.Link > lastLink || r.Prefix > MaxPrefix {
		return false
	}
	noAddress := [addressSize]byte{}
	snapshot := r.State == StateDown && r.Mode == ModeDHCP && r.Local == noAddress && r.Gateway == noAddress &&
		r.MAC == [macSize]byte{} && r.Prefix == 0 && r.Link == LinkDown && r.Lease == 0 &&
		r.RX == 0 && r.TX == 0 && r.Drop == 0 && r.Malformed == 0 && r.MTU == 0 && !r.Fallback &&
		!r.RateLimited && r.ConflictRun == 0 && r.Probes == 0 && r.ProbeConflicts == 0 && r.Announcements == 0 &&
		r.ActiveConflicts == 0 && r.Defenses == 0 && r.Relinquished == 0 && r.RateLimits == 0 && r.Loopback == 0
	echo := r.Responder == noAddress && r.RTT == 0 && r.Sequence == 0
	switch r.Opcode {
	case OpStatus:
		if r.Result == ResultOK {
			return echo && r.Field == FieldNone
		}
		return snapshot && echo && r.Field == FieldNone
	case OpEcho:
		if !snapshot || r.Field != FieldNone {
			return false
		}
		switch r.Result {
		case ResultOK:
			return r.Responder != noAddress
		case ResultRemoteUnreachable, ResultTimeExceeded:
			return r.Responder != noAddress && r.RTT == 0
		}
		return r.Responder == noAddress && r.RTT == 0
	case OpReload:
		return snapshot && echo && (r.Result == ResultConfigInvalid || r.Field == FieldNone)
	}
	return false
}

// SaturatingAdd is how the service advances its counters: replies carry
// them as-is, so a counter must stop at the maximum instead of wrapping.
func SaturatingAdd(value, delta uint64) uint64 {
	if value+delta < value {
		return ^uint64(0)
	}
	return value + delta
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
// The whole payload is cleared so stale bytes never reach the peer.
func prepare(out *ipc.Message, protocol uint64, version uint16, kind ipc.Kind, correlation uint64, size uint16) {
	for index := range out.Payload[:] {
		out.Payload[index] = 0
	}
	for index := range out.Handles[:] {
		out.Handles[index] = 0
	}
	out.ProtocolID, out.Version, out.Kind = protocol, version, kind
	out.Correlation, out.PayloadSize, out.HandleCount = correlation, size, 0
}
