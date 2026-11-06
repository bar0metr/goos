package linuxsockapi

import (
	"encoding/binary"

	"goos/kernel/handle"
	"goos/kernel/ipc"
)

// GLNXCTL2 is the control protocol between PID 1 and linuxsockd (ADR 0210).
// PID 1 holds the only client end. It hands the service its native TCP and
// UDP sessions, adds and removes Linux family sessions, and reads the
// service's counters and socket quotas. Every request is answered by one
// reply with its correlation; replies carry no handles.
const (
	ControlProtocolID  = uint64(0x474c4e5843544c32) // "GLNXCTL2"
	ControlVersion     = uint16(2)
	ControlRequestSize = 32
	ControlReplySize   = ctlFamilySockets + 2*StatusFamilies

	// StatusFamilies is the family table entries a Status reply reports.
	StatusFamilies = 16

	// AddFamilyHandles are the family channel end and the family pool.
	AddFamilyHandles = 2
	// SetTCPHandles and SetUDPHandles are a native session's channel, TX
	// pool and RX pool, in that order (GTCP, GUDP).
	SetTCPHandles = 3
	SetUDPHandles = 3

	// CounterCount is the number of counters in a Status reply.
	CounterCount = 14

	ctlOpcode        = 0
	ctlSecond        = 1 // reply Result
	ctlFamilies      = 2 // reply
	ctlBackends      = 3 // reply: bit 0 TCP up, bit 1 UDP up
	ctlSockets       = 4 // reply, 16 bits
	ctlSocketLimit   = 6 // reply, 16 bits
	ctlSession       = 4 // request
	ctlToken         = 8
	ctlGeneration    = 16
	ctlCounters      = 24 // reply
	ctlFamilyLimit   = ctlCounters + CounterCount*8
	ctlFamilyReserve = ctlFamilyLimit + 2
	ctlFamilySockets = ctlFamilyReserve + 2 // reply, 16 bits per family entry
)

// A Status reply fits one message.
var _ [ipc.MaxPayload - ControlReplySize]byte

type ControlOpcode uint8

const (
	// ControlAddFamily gives the service one Linux family session: its
	// channel end and pool, under Token (nonzero, chosen by PID 1) and
	// Session, the session generation socket identities carry (nonzero and
	// unused by any family the service holds).
	ControlAddFamily ControlOpcode = iota + 1
	// ControlRemoveFamily ends the family session named by Token: every
	// socket of it is closed and its handles released.
	ControlRemoveFamily
	// ControlSetTCP and ControlSetUDP hand over a native session under
	// Generation, which grows with every session PID 1 opens. The previous
	// session of the same protocol, if any, is dropped and every socket it
	// backed fails.
	ControlSetTCP
	ControlSetUDP
	// ControlStatus reads the service's table use and counters.
	ControlStatus
	lastControlOpcode = ControlStatus
)

type ControlResult uint8

const (
	ControlOK ControlResult = iota
	ControlInvalidArgument
	// ControlBusy: the family table is full.
	ControlBusy
	// ControlNotFound: no family holds the token.
	ControlNotFound
	// ControlStale: the generation is not newer than the one in use.
	ControlStale
	lastControlResult = ControlStale
)

// Backend bits of a Status reply.
const (
	BackendTCP = uint8(1 << 0)
	BackendUDP = uint8(1 << 1)
)

// Counters are the service's totals since it started. No counter carries
// payload.
type Counters struct {
	Opens, Closes, Connects, Accepts     uint64
	BytesIn, BytesOut                    uint64 // stream bytes to and from the kernel
	DatagramsIn, DatagramsOut            uint64
	WouldBlock                           uint64 // WouldBlock and InProgress answers
	NativeFailures                       uint64 // native results other than success
	SessionFailures                      uint64 // family sessions ended for a malformed message
	BackendLosses                        uint64 // sockets failed because their native session died
	Notifications, DeferredNotifications uint64 // ReadyChanged sent, and sends that found the queue full
}

func (c *Counters) fields() [CounterCount]*uint64 {
	return [CounterCount]*uint64{&c.Opens, &c.Closes, &c.Connects, &c.Accepts, &c.BytesIn, &c.BytesOut,
		&c.DatagramsIn, &c.DatagramsOut, &c.WouldBlock, &c.NativeFailures, &c.SessionFailures, &c.BackendLosses,
		&c.Notifications, &c.DeferredNotifications}
}

// ControlRequest is one decoded request:
//
//	AddFamily:    Session, Token, two handles
//	RemoveFamily: Token
//	SetTCP:       Generation, three handles
//	SetUDP:       Generation, three handles
//	Status:       nothing
type ControlRequest struct {
	Opcode     ControlOpcode
	Session    uint32
	Token      uint64
	Generation uint64
}

// ControlReply echoes the request's Token or Generation. A Status reply
// carries the table use, the live backends, the counters and the socket
// quotas; every other reply leaves them zero. Sockets and SocketLimit are
// the service's use and quota across families. FamilyLimit is each
// family's quota and FamilyReserve the sockets the service keeps back for
// each family. FamilySockets holds each family table entry's use.
type ControlReply struct {
	Opcode        ControlOpcode
	Result        ControlResult
	Families      uint8
	Backends      uint8
	Sockets       uint16
	SocketLimit   uint16
	FamilyLimit   uint16
	FamilyReserve uint16
	FamilySockets [StatusFamilies]uint16
	Token         uint64
	Generation    uint64
	Counters      Counters
}

func (r *ControlReply) quotasZero() bool {
	return r.Sockets == 0 && r.SocketLimit == 0 && r.FamilyLimit == 0 && r.FamilyReserve == 0 &&
		r.FamilySockets == [StatusFamilies]uint16{}
}

func controlHandles(op ControlOpcode) (int, bool) {
	switch op {
	case ControlAddFamily:
		return AddFamilyHandles, true
	case ControlSetTCP:
		return SetTCPHandles, true
	case ControlSetUDP:
		return SetUDPHandles, true
	case ControlRemoveFamily, ControlStatus:
		return 0, true
	}
	return 0, false
}

func validControlRequest(r *ControlRequest) bool {
	switch r.Opcode {
	case ControlAddFamily:
		return r.Session != 0 && r.Token != 0 && r.Generation == 0
	case ControlRemoveFamily:
		return r.Session == 0 && r.Token != 0 && r.Generation == 0
	case ControlSetTCP, ControlSetUDP:
		return r.Session == 0 && r.Token == 0 && r.Generation != 0
	case ControlStatus:
		return r.Session == 0 && r.Token == 0 && r.Generation == 0
	}
	return false
}

// EncodeControlRequest attaches exactly the handles the opcode takes.
func EncodeControlRequest(r *ControlRequest, attachments []handle.ID, correlation uint64, out *ipc.Message) bool {
	count, known := controlHandles(r.Opcode)
	if !known || correlation == 0 || len(attachments) != count || !validControlRequest(r) {
		return false
	}
	for _, attachment := range attachments {
		if attachment == 0 {
			return false
		}
	}
	prepareControl(out, ipc.KindRequest, correlation, ControlRequestSize)
	out.HandleCount = uint8(count)
	copy(out.Handles[:], attachments)
	p := out.Payload[:ControlRequestSize]
	p[ctlOpcode] = byte(r.Opcode)
	binary.LittleEndian.PutUint32(p[ctlSession:], r.Session)
	binary.LittleEndian.PutUint64(p[ctlToken:], r.Token)
	binary.LittleEndian.PutUint64(p[ctlGeneration:], r.Generation)
	return true
}

// DecodeControlRequest leaves the attachments in in.Handles. On failure the
// receiver still closes whatever was attached.
func DecodeControlRequest(in *ipc.Message, out *ControlRequest) (uint64, bool) {
	if in.ProtocolID != ControlProtocolID || in.Version != ControlVersion || in.Kind != ipc.KindRequest ||
		in.Correlation == 0 || in.PayloadSize != ControlRequestSize || int(in.HandleCount) > len(in.Handles) {
		return 0, false
	}
	p := in.Payload[:ControlRequestSize]
	if !zero(p[ctlOpcode+1:ctlSession]) || !zero(p[ctlCounters:ControlRequestSize]) {
		return 0, false
	}
	r := ControlRequest{Opcode: ControlOpcode(p[ctlOpcode]), Session: binary.LittleEndian.Uint32(p[ctlSession:]),
		Token: binary.LittleEndian.Uint64(p[ctlToken:]), Generation: binary.LittleEndian.Uint64(p[ctlGeneration:])}
	count, known := controlHandles(r.Opcode)
	if !known || int(in.HandleCount) != count || !validControlRequest(&r) {
		return 0, false
	}
	for _, attachment := range in.Handles[:count] {
		if attachment == 0 {
			return 0, false
		}
	}
	*out = r
	return in.Correlation, true
}

// ControlRequestOpcode identifies a malformed request that still deserves a
// reply.
func ControlRequestOpcode(in *ipc.Message) (ControlOpcode, uint64, bool) {
	if in.ProtocolID != ControlProtocolID || in.Version != ControlVersion || in.Kind != ipc.KindRequest ||
		in.Correlation == 0 || in.PayloadSize == 0 {
		return 0, 0, false
	}
	op := ControlOpcode(in.Payload[ctlOpcode])
	if _, known := controlHandles(op); !known {
		return 0, 0, false
	}
	return op, in.Correlation, true
}

func validControlReply(r *ControlReply) bool {
	if _, known := controlHandles(r.Opcode); !known || r.Result > lastControlResult || r.Backends&^(BackendTCP|BackendUDP) != 0 {
		return false
	}
	status := r.Opcode == ControlStatus && r.Result == ControlOK
	if !status {
		if r.Families != 0 || r.Backends != 0 || !r.quotasZero() || r.Counters != (Counters{}) {
			return false
		}
	}
	switch r.Opcode {
	case ControlAddFamily, ControlRemoveFamily:
		return r.Generation == 0
	case ControlSetTCP, ControlSetUDP:
		return r.Token == 0
	}
	return r.Token == 0 && r.Generation == 0
}

func EncodeControlReply(r *ControlReply, correlation uint64, out *ipc.Message) bool {
	if correlation == 0 || !validControlReply(r) {
		return false
	}
	prepareControl(out, ipc.KindReply, correlation, ControlReplySize)
	p := out.Payload[:ControlReplySize]
	p[ctlOpcode], p[ctlSecond], p[ctlFamilies], p[ctlBackends] = byte(r.Opcode), byte(r.Result), r.Families, r.Backends
	binary.LittleEndian.PutUint16(p[ctlSockets:], r.Sockets)
	binary.LittleEndian.PutUint16(p[ctlSocketLimit:], r.SocketLimit)
	binary.LittleEndian.PutUint64(p[ctlToken:], r.Token)
	binary.LittleEndian.PutUint64(p[ctlGeneration:], r.Generation)
	for index, counter := range r.Counters.fields() {
		binary.LittleEndian.PutUint64(p[ctlCounters+8*index:], *counter)
	}
	binary.LittleEndian.PutUint16(p[ctlFamilyLimit:], r.FamilyLimit)
	binary.LittleEndian.PutUint16(p[ctlFamilyReserve:], r.FamilyReserve)
	for index := 0; index < StatusFamilies; index++ {
		binary.LittleEndian.PutUint16(p[ctlFamilySockets+2*index:], r.FamilySockets[index])
	}
	return true
}

// DecodeControlReply accepts only the reply to the caller's request.
func DecodeControlReply(in *ipc.Message, correlation uint64, opcode ControlOpcode, out *ControlReply) bool {
	if in.ProtocolID != ControlProtocolID || in.Version != ControlVersion || in.Kind != ipc.KindReply ||
		correlation == 0 || in.Correlation != correlation || in.PayloadSize != ControlReplySize || in.HandleCount != 0 {
		return false
	}
	p := in.Payload[:ControlReplySize]
	if ControlOpcode(p[ctlOpcode]) != opcode {
		return false
	}
	r := ControlReply{Opcode: opcode, Result: ControlResult(p[ctlSecond]), Families: p[ctlFamilies],
		Backends: p[ctlBackends], Sockets: binary.LittleEndian.Uint16(p[ctlSockets:]),
		SocketLimit: binary.LittleEndian.Uint16(p[ctlSocketLimit:]), Token: binary.LittleEndian.Uint64(p[ctlToken:]),
		Generation: binary.LittleEndian.Uint64(p[ctlGeneration:]), FamilyLimit: binary.LittleEndian.Uint16(p[ctlFamilyLimit:]),
		FamilyReserve: binary.LittleEndian.Uint16(p[ctlFamilyReserve:])}
	for index, counter := range r.Counters.fields() {
		*counter = binary.LittleEndian.Uint64(p[ctlCounters+8*index:])
	}
	for index := 0; index < StatusFamilies; index++ {
		r.FamilySockets[index] = binary.LittleEndian.Uint16(p[ctlFamilySockets+2*index:])
	}
	if !validControlReply(&r) {
		return false
	}
	*out = r
	return true
}

func prepareControl(out *ipc.Message, kind ipc.Kind, correlation uint64, size uint16) {
	for index := range out.Payload[:] {
		out.Payload[index] = 0
	}
	for index := range out.Handles[:] {
		out.Handles[index] = 0
	}
	out.ProtocolID, out.Version, out.Kind = ControlProtocolID, ControlVersion, kind
	out.Correlation, out.PayloadSize, out.HandleCount = correlation, size, 0
}
