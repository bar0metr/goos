// Package netapi defines GNETAPI1, the protocol between network commands and
// the network service, and GNETCTL1, PID 1's control protocol to the service.
package netapi

import (
	"encoding/binary"
	"goos/kernel/ipc"
)

const (
	ProtocolID = uint64(0x474e455441504931)
	// Version 4 adds a separate bounded TCP diagnostics reply, version 5
	// its GTCP message counters, version 6 OpTCPDump, version 7 wider
	// limits: TCP and UDP endpoints and the backlog in 16 bits, buffers in
	// 32, version 8 the TCP capacity block and a 256-byte reply. A message
	// of an older version is not this protocol and fails decoding.
	Version          = uint16(8)
	RequestSize      = 24
	ReplySize        = 256
	MaxServers       = 4
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
	requestGeneration  = 16

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
	replyUDP       = 152
	replyEndpoints = 208
	replyPortMin   = 210
	replyPortMax   = 212
	replyPayload   = 214
	replyQueue     = 216
	// replyReserved1 held max-endpoints while it fit a byte; it stays zero.
	replyReserved1 = 217
	replySessionEP = 218
	replyServerN   = 219
	replyMaxEP     = 220
	replyReserved2 = 222
	replyServerGen = 224
	replyServers   = 232
	replyStatusEnd = replyServers + addressSize*MaxServers

	// replyTCPSessions holds the session counts and limits, two bytes per
	// kind; the status reply's state, mode and address use these bytes.
	replyTCPSessions = 2

	replyTCPCounters    = 8
	replyTCPEndpoints   = 80
	replyTCPListeners   = 82
	replyTCPConnections = 84
	replyTCPStates      = 88
	// replyTCPSessionEP follows the state counts; the limits block holds
	// the wider limits.
	replyTCPSessionEP = 116
	replyTCPLimits    = 120
	replyTCPMessages  = 152
	// replyTCPCapacity is the TCPCapacity block: six 64-bit values, three
	// 32-bit and three 16-bit.
	replyTCPCapacity    = 184
	replyTCPCapacityEnd = replyTCPCapacity + 66

	// flagRateLimited is the only flag: the next address claim waits for
	// the conflict rate limit.
	flagRateLimited = 1
	counterCount    = 8
	udpCounterCount = 7
	tcpCounterCount = 9
	tcpStateCount   = 14
	// gtcpCounterCount: requests and replies of GTCP 2, then of GTCP 3.
	gtcpCounterCount = 4

	addressSize = 4
	macSize     = 6
)

type Opcode uint8

const (
	OpStatus Opcode = iota + 1
	OpEcho
	OpReload
	// OpServers asks for the DNS servers of the current DHCP lease. The
	// request carries the generation of the list the client has; the
	// service answers at once when its list is of another generation and
	// otherwise holds the request until the list changes. A session holds
	// one such request at a time.
	OpServers
	// OpTCPStatus returns transport counters, state counts, and active limits.
	OpTCPStatus
	// OpTCPDump has the service write every TCP endpoint's transport state
	// to its log, for a stuck connection's diagnosis. The reply carries
	// only the result.
	OpTCPDump
	lastOpcode = OpTCPDump
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
	FieldUDPEphemeralMin
	FieldUDPEphemeralMax
	FieldUDPReceiveQueue
	FieldUDPMaxEndpoints
	FieldUDPSessionEndpoints
	FieldUDPMaxPayload
	FieldTCPEphemeralMin
	FieldTCPEphemeralMax
	FieldTCPMaxEndpoints
	FieldTCPSessionEndpoints
	FieldTCPListenBacklog
	FieldTCPSendBuffer
	FieldTCPReceiveBuffer
	FieldTCPConnectTimeout
	FieldTCPRTOMin
	FieldTCPRTOMax
	FieldTCPTimeWait
	FieldTCPMemoryBudget
	FieldTCPOwnerMemoryBudget
	lastField = FieldTCPOwnerMemoryBudget
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
	// Generation is set only for Servers.
	Generation uint64
}

// Reply is one decoded reply body. Which fields an opcode may set is fixed;
// every other field is zero on the wire and in the struct:
//
//	Status, OK:          State, Mode, Local, Gateway, MAC, Prefix, Link,
//	                     Lease, RX, TX, Drop, Malformed, MTU, Fallback,
//	                     RateLimited, the conflict counters, Loopback,
//	                     ConflictRun, the UDP counters and limits
//	Echo, OK:            Responder (nonzero), RTT, Sequence
//	Echo, RemoteUnreachable or TimeExceeded:
//	                     Responder (nonzero, the router that answered), Sequence
//	Echo, other results: Sequence
//	Reload, ConfigInvalid: Field (may be FieldNone)
//	Servers, OK:         ServerCount, Servers (the first ServerCount),
//	                     Generation
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
	// The UDP counters, the endpoints open, and the UDP limits in force
	// (system.net.udp.*).
	UDPRX               uint64
	UDPTX               uint64
	UDPDropNoEndpoint   uint64
	UDPDropQueueFull    uint64
	UDPDropChecksum     uint64
	UDPDropTooLong      uint64
	UDPBindFail         uint64
	UDPEndpoints        uint16
	UDPEphemeralMin     uint16
	UDPEphemeralMax     uint16
	UDPMaxPayload       uint16
	UDPReceiveQueue     uint8
	UDPMaxEndpoints     uint16
	UDPSessionEndpoints uint8
	// ServerCount, Servers and Generation answer Servers.
	ServerCount uint8
	Generation  uint64
	Servers     [MaxServers][addressSize]byte

	TCPRX, TCPTX, TCPRetransmits, TCPRXReset, TCPTXReset           uint64
	TCPDropChecksum, TCPDropNoTCB, TCPDropWindow, TCPDropMalformed uint64
	TCPEndpoints, TCPListeners, TCPConnections                     uint16
	TCPStates                                                      [tcpStateCount]uint16
	TCPEphemeralMin, TCPEphemeralMax                               uint16
	TCPMaxEndpoints, TCPListenBacklog                              uint16
	TCPSessionEndpoints                                            uint8
	TCPSendBuffer, TCPReceiveBuffer                                uint32
	TCPConnectTimeoutMS, TCPRTOMinMS, TCPRTOMaxMS, TCPTimeWaitMS   uint32
	// GTCP2Requests and the rest count the client messages netd decoded and
	// the replies it sent, per GTCP version, over every session.
	GTCP2Requests, GTCP2Replies, GTCP3Requests, GTCP3Replies uint64
	// Capacity answers OpTCPStatus only.
	Capacity TCPCapacity
}

// TCPCapacity is how much of each TCP and session limit is in use. A
// lowered budget takes nothing back, so a use can exceed its budget until
// connections close.
type TCPCapacity struct {
	// MemoryBytes is the ring bytes charged against MemoryBudget, and per
	// session against OwnerMemoryBudget.
	MemoryBytes                     uint64
	MemoryBudget, OwnerMemoryBudget uint32
	// TimeWait counts tombstones against TimeWaitBudget, and per session
	// against OwnerTimeWaitBudget. TimeWaitHeld counts connections that
	// stayed endpoints because no tombstone was free.
	TimeWait, TimeWaitBudget, OwnerTimeWaitBudget uint16
	TimeWaitHeld                                  uint64
	// DropMemory counts handshakes and segments refused for want of
	// memory, DropBacklog SYNs a full listener dropped, MemoryCapped ring
	// growths the budgets cut short.
	DropMemory, DropBacklog, MemoryCapped uint64
	// LookupProbes counts index entries received segments looked at;
	// MaxSegmentProbes is the most one segment needed.
	LookupProbes     uint64
	MaxSegmentProbes uint32
	// The command, UDP and TCP sessions netd serves, and how many of each
	// it can serve.
	CommandSessions, CommandSessionLimit uint8
	UDPSessions, UDPSessionLimit         uint8
	TCPSessions, TCPSessionLimit         uint8
}

func (c *TCPCapacity) wide() [6]*uint64 {
	return [6]*uint64{&c.MemoryBytes, &c.DropMemory, &c.DropBacklog, &c.MemoryCapped, &c.LookupProbes, &c.TimeWaitHeld}
}

func (c *TCPCapacity) words() [3]*uint32 {
	return [3]*uint32{&c.MemoryBudget, &c.OwnerMemoryBudget, &c.MaxSegmentProbes}
}

func (c *TCPCapacity) halves() [3]*uint16 {
	return [3]*uint16{&c.TimeWait, &c.TimeWaitBudget, &c.OwnerTimeWaitBudget}
}

func (c *TCPCapacity) sessions() [6]*uint8 {
	return [6]*uint8{&c.CommandSessions, &c.CommandSessionLimit, &c.UDPSessions, &c.UDPSessionLimit, &c.TCPSessions, &c.TCPSessionLimit}
}

func (c *TCPCapacity) encode(payload []byte) {
	for index, value := range c.sessions() {
		payload[replyTCPSessions+index] = *value
	}
	offset := replyTCPCapacity
	for _, value := range c.wide() {
		binary.LittleEndian.PutUint64(payload[offset:], *value)
		offset += 8
	}
	for _, value := range c.words() {
		binary.LittleEndian.PutUint32(payload[offset:], *value)
		offset += 4
	}
	for _, value := range c.halves() {
		binary.LittleEndian.PutUint16(payload[offset:], *value)
		offset += 2
	}
}

func (c *TCPCapacity) decode(payload []byte) {
	for index, value := range c.sessions() {
		*value = payload[replyTCPSessions+index]
	}
	offset := replyTCPCapacity
	for _, value := range c.wide() {
		*value = binary.LittleEndian.Uint64(payload[offset:])
		offset += 8
	}
	for _, value := range c.words() {
		*value = binary.LittleEndian.Uint32(payload[offset:])
		offset += 4
	}
	for _, value := range c.halves() {
		*value = binary.LittleEndian.Uint16(payload[offset:])
		offset += 2
	}
}

func (c *TCPCapacity) valid() bool {
	return c.CommandSessions <= c.CommandSessionLimit && c.UDPSessions <= c.UDPSessionLimit && c.TCPSessions <= c.TCPSessionLimit
}

func (r *Reply) udpCounters() [udpCounterCount]*uint64 {
	return [udpCounterCount]*uint64{&r.UDPRX, &r.UDPTX, &r.UDPDropNoEndpoint, &r.UDPDropQueueFull,
		&r.UDPDropChecksum, &r.UDPDropTooLong, &r.UDPBindFail}
}

func (r *Reply) tcpCounters() [tcpCounterCount]*uint64 {
	return [tcpCounterCount]*uint64{&r.TCPRX, &r.TCPTX, &r.TCPRetransmits, &r.TCPRXReset, &r.TCPTXReset,
		&r.TCPDropChecksum, &r.TCPDropNoTCB, &r.TCPDropWindow, &r.TCPDropMalformed}
}

func (r *Reply) gtcpCounters() [gtcpCounterCount]*uint64 {
	return [gtcpCounterCount]*uint64{&r.GTCP2Requests, &r.GTCP2Replies, &r.GTCP3Requests, &r.GTCP3Replies}
}

func (r *Reply) tcpEmpty() bool {
	copy := *r
	copy.TCPRX, copy.TCPTX, copy.TCPRetransmits, copy.TCPRXReset, copy.TCPTXReset = 0, 0, 0, 0, 0
	copy.TCPDropChecksum, copy.TCPDropNoTCB, copy.TCPDropWindow, copy.TCPDropMalformed = 0, 0, 0, 0
	copy.TCPEndpoints, copy.TCPListeners, copy.TCPConnections = 0, 0, 0
	copy.TCPStates = [tcpStateCount]uint16{}
	copy.TCPEphemeralMin, copy.TCPEphemeralMax = 0, 0
	copy.TCPMaxEndpoints, copy.TCPSessionEndpoints, copy.TCPListenBacklog = 0, 0, 0
	copy.TCPSendBuffer, copy.TCPReceiveBuffer = 0, 0
	copy.TCPConnectTimeoutMS, copy.TCPRTOMinMS, copy.TCPRTOMaxMS, copy.TCPTimeWaitMS = 0, 0, 0, 0
	copy.GTCP2Requests, copy.GTCP2Replies, copy.GTCP3Requests, copy.GTCP3Replies = 0, 0, 0, 0
	copy.Capacity = TCPCapacity{}
	return copy == *r
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
	binary.LittleEndian.PutUint64(payload[requestGeneration:], r.Generation)
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
	if !zero(payload[requestOpcode+1:requestDestination]) || !zero(payload[requestReserved:requestGeneration]) {
		return 0, false
	}
	r := Request{
		Opcode:     Opcode(payload[requestOpcode]),
		Sequence:   binary.LittleEndian.Uint16(payload[requestSequence:]),
		TimeoutMS:  binary.LittleEndian.Uint16(payload[requestTimeout:]),
		Generation: binary.LittleEndian.Uint64(payload[requestGeneration:]),
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
	case OpStatus, OpReload, OpTCPStatus, OpTCPDump:
		return r.Destination == [addressSize]byte{} && r.Sequence == 0 && r.TimeoutMS == 0 && r.Generation == 0
	case OpEcho:
		return r.Destination != [addressSize]byte{} && r.TimeoutMS >= MinEchoTimeoutMS && r.TimeoutMS <= MaxEchoTimeoutMS &&
			r.Generation == 0
	case OpServers:
		return r.Destination == [addressSize]byte{} && r.Sequence == 0 && r.TimeoutMS == 0
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
	if r.Opcode == OpTCPStatus {
		for index, counter := range r.tcpCounters() {
			binary.LittleEndian.PutUint64(payload[replyTCPCounters+8*index:], *counter)
		}
		binary.LittleEndian.PutUint16(payload[replyTCPEndpoints:], r.TCPEndpoints)
		binary.LittleEndian.PutUint16(payload[replyTCPListeners:], r.TCPListeners)
		binary.LittleEndian.PutUint16(payload[replyTCPConnections:], r.TCPConnections)
		for index, count := range r.TCPStates {
			binary.LittleEndian.PutUint16(payload[replyTCPStates+2*index:], count)
		}
		binary.LittleEndian.PutUint16(payload[replyTCPLimits:], r.TCPEphemeralMin)
		binary.LittleEndian.PutUint16(payload[replyTCPLimits+2:], r.TCPEphemeralMax)
		binary.LittleEndian.PutUint16(payload[replyTCPLimits+4:], r.TCPMaxEndpoints)
		binary.LittleEndian.PutUint16(payload[replyTCPLimits+6:], r.TCPListenBacklog)
		payload[replyTCPSessionEP] = r.TCPSessionEndpoints
		binary.LittleEndian.PutUint32(payload[replyTCPLimits+8:], r.TCPSendBuffer)
		binary.LittleEndian.PutUint32(payload[replyTCPLimits+12:], r.TCPReceiveBuffer)
		binary.LittleEndian.PutUint32(payload[replyTCPLimits+16:], r.TCPConnectTimeoutMS)
		binary.LittleEndian.PutUint32(payload[replyTCPLimits+20:], r.TCPRTOMinMS)
		binary.LittleEndian.PutUint32(payload[replyTCPLimits+24:], r.TCPRTOMaxMS)
		binary.LittleEndian.PutUint32(payload[replyTCPLimits+28:], r.TCPTimeWaitMS)
		for index, counter := range r.gtcpCounters() {
			binary.LittleEndian.PutUint64(payload[replyTCPMessages+8*index:], *counter)
		}
		r.Capacity.encode(payload)
		return true
	}
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
	for index, counter := range r.udpCounters() {
		binary.LittleEndian.PutUint64(payload[replyUDP+8*index:], *counter)
	}
	binary.LittleEndian.PutUint16(payload[replyEndpoints:], r.UDPEndpoints)
	binary.LittleEndian.PutUint16(payload[replyPortMin:], r.UDPEphemeralMin)
	binary.LittleEndian.PutUint16(payload[replyPortMax:], r.UDPEphemeralMax)
	binary.LittleEndian.PutUint16(payload[replyPayload:], r.UDPMaxPayload)
	payload[replyQueue], payload[replySessionEP] = r.UDPReceiveQueue, r.UDPSessionEndpoints
	binary.LittleEndian.PutUint16(payload[replyMaxEP:], r.UDPMaxEndpoints)
	payload[replyServerN] = r.ServerCount
	binary.LittleEndian.PutUint64(payload[replyServerGen:], r.Generation)
	for index := range r.Servers {
		copy(payload[replyServers+addressSize*index:], r.Servers[index][:])
	}
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
	if Opcode(payload[replyOpcode]) == OpTCPStatus {
		if opcode != OpTCPStatus || !zero(payload[replyTCPSessions+6:replyTCPCounters]) || !zero(payload[replyTCPConnections+2:replyTCPStates]) ||
			!zero(payload[replyTCPStates+2*tcpStateCount:replyTCPSessionEP]) || !zero(payload[replyTCPSessionEP+1:replyTCPLimits]) ||
			!zero(payload[replyTCPCapacityEnd:ReplySize]) {
			return false
		}
		r := Reply{Opcode: OpTCPStatus, Result: Result(payload[replyResult]),
			TCPEndpoints:    binary.LittleEndian.Uint16(payload[replyTCPEndpoints:]),
			TCPListeners:    binary.LittleEndian.Uint16(payload[replyTCPListeners:]),
			TCPConnections:  binary.LittleEndian.Uint16(payload[replyTCPConnections:]),
			TCPEphemeralMin: binary.LittleEndian.Uint16(payload[replyTCPLimits:]),
			TCPEphemeralMax: binary.LittleEndian.Uint16(payload[replyTCPLimits+2:]),
			TCPMaxEndpoints: binary.LittleEndian.Uint16(payload[replyTCPLimits+4:]), TCPSessionEndpoints: payload[replyTCPSessionEP],
			TCPListenBacklog: binary.LittleEndian.Uint16(payload[replyTCPLimits+6:]),
			TCPSendBuffer:    binary.LittleEndian.Uint32(payload[replyTCPLimits+8:]), TCPReceiveBuffer: binary.LittleEndian.Uint32(payload[replyTCPLimits+12:]),
			TCPConnectTimeoutMS: binary.LittleEndian.Uint32(payload[replyTCPLimits+16:]), TCPRTOMinMS: binary.LittleEndian.Uint32(payload[replyTCPLimits+20:]),
			TCPRTOMaxMS: binary.LittleEndian.Uint32(payload[replyTCPLimits+24:]), TCPTimeWaitMS: binary.LittleEndian.Uint32(payload[replyTCPLimits+28:])}
		for index, counter := range r.tcpCounters() {
			*counter = binary.LittleEndian.Uint64(payload[replyTCPCounters+8*index:])
		}
		for index := range r.TCPStates {
			r.TCPStates[index] = binary.LittleEndian.Uint16(payload[replyTCPStates+2*index:])
		}
		for index, counter := range r.gtcpCounters() {
			*counter = binary.LittleEndian.Uint64(payload[replyTCPMessages+8*index:])
		}
		r.Capacity.decode(payload)
		if !validReply(&r) {
			return false
		}
		*out = r
		return true
	}
	field := binary.LittleEndian.Uint16(payload[replyField:])
	if Opcode(payload[replyOpcode]) != opcode || !zero(payload[replyReserved:replyUDP]) ||
		payload[replyReserved1] != 0 || !zero(payload[replyReserved2:replyServerGen]) || !zero(payload[replyStatusEnd:ReplySize]) ||
		field > uint16(lastField) ||
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

		UDPEndpoints:        binary.LittleEndian.Uint16(payload[replyEndpoints:]),
		UDPEphemeralMin:     binary.LittleEndian.Uint16(payload[replyPortMin:]),
		UDPEphemeralMax:     binary.LittleEndian.Uint16(payload[replyPortMax:]),
		UDPMaxPayload:       binary.LittleEndian.Uint16(payload[replyPayload:]),
		UDPReceiveQueue:     payload[replyQueue],
		UDPMaxEndpoints:     binary.LittleEndian.Uint16(payload[replyMaxEP:]),
		UDPSessionEndpoints: payload[replySessionEP],
		ServerCount:         payload[replyServerN],
		Generation:          binary.LittleEndian.Uint64(payload[replyServerGen:]),
	}
	for index, counter := range r.counters() {
		*counter = binary.LittleEndian.Uint64(payload[replyCounters+8*index:])
	}
	for index, counter := range r.udpCounters() {
		*counter = binary.LittleEndian.Uint64(payload[replyUDP+8*index:])
	}
	for index := range r.Servers {
		copy(r.Servers[index][:], payload[replyServers+addressSize*index:])
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
	if r.Opcode == OpTCPStatus {
		base := *r
		base.TCPRX, base.TCPTX, base.TCPRetransmits, base.TCPRXReset, base.TCPTXReset = 0, 0, 0, 0, 0
		base.TCPDropChecksum, base.TCPDropNoTCB, base.TCPDropWindow, base.TCPDropMalformed = 0, 0, 0, 0
		base.TCPEndpoints, base.TCPListeners, base.TCPConnections = 0, 0, 0
		base.TCPStates = [tcpStateCount]uint16{}
		base.TCPEphemeralMin, base.TCPEphemeralMax = 0, 0
		base.TCPMaxEndpoints, base.TCPSessionEndpoints, base.TCPListenBacklog = 0, 0, 0
		base.TCPSendBuffer, base.TCPReceiveBuffer = 0, 0
		base.TCPConnectTimeoutMS, base.TCPRTOMinMS, base.TCPRTOMaxMS, base.TCPTimeWaitMS = 0, 0, 0, 0
		base.GTCP2Requests, base.GTCP2Replies, base.GTCP3Requests, base.GTCP3Replies = 0, 0, 0, 0
		base.Capacity = TCPCapacity{}
		if base != (Reply{Opcode: OpTCPStatus, Result: r.Result}) {
			return false
		}
		if r.Result != ResultOK {
			return *r == base
		}
		return r.TCPListeners <= r.TCPEndpoints && r.TCPConnections <= r.TCPEndpoints &&
			uint16(r.TCPSessionEndpoints) <= r.TCPMaxEndpoints && r.TCPListenBacklog != 0 && r.Capacity.valid()
	}
	noAddress := [addressSize]byte{}
	snapshot := r.State == StateDown && r.Mode == ModeDHCP && r.Local == noAddress && r.Gateway == noAddress &&
		r.MAC == [macSize]byte{} && r.Prefix == 0 && r.Link == LinkDown && r.Lease == 0 &&
		r.RX == 0 && r.TX == 0 && r.Drop == 0 && r.Malformed == 0 && r.MTU == 0 && !r.Fallback &&
		!r.RateLimited && r.ConflictRun == 0 && r.Probes == 0 && r.ProbeConflicts == 0 && r.Announcements == 0 &&
		r.ActiveConflicts == 0 && r.Defenses == 0 && r.Relinquished == 0 && r.RateLimits == 0 && r.Loopback == 0 &&
		r.UDPRX == 0 && r.UDPTX == 0 && r.UDPDropNoEndpoint == 0 && r.UDPDropQueueFull == 0 && r.UDPDropChecksum == 0 &&
		r.UDPDropTooLong == 0 && r.UDPBindFail == 0 && r.UDPEndpoints == 0 && r.UDPEphemeralMin == 0 &&
		r.UDPEphemeralMax == 0 && r.UDPMaxPayload == 0 && r.UDPReceiveQueue == 0 && r.UDPMaxEndpoints == 0 &&
		r.UDPSessionEndpoints == 0 && r.tcpEmpty()
	echo := r.Responder == noAddress && r.RTT == 0 && r.Sequence == 0
	for index := range r.Servers {
		if index >= int(r.ServerCount) && r.Servers[index] != noAddress {
			return false
		}
	}
	noServers := r.ServerCount == 0 && r.Generation == 0
	switch r.Opcode {
	case OpServers:
		if r.Result == ResultOK {
			return snapshot && echo && r.Field == FieldNone && r.ServerCount <= MaxServers
		}
		return snapshot && echo && r.Field == FieldNone && noServers
	}
	if !noServers {
		return false
	}
	switch r.Opcode {
	case OpStatus:
		if r.Result == ResultOK {
			return echo && r.Field == FieldNone && r.tcpEmpty()
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
	case OpTCPDump:
		return snapshot && echo && r.Field == FieldNone
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
