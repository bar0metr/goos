package netapi

import (
	"encoding/binary"
	"goos/kernel/handle"
	"goos/kernel/ipc"
)

const (
	ControlProtocolID = uint64(0x474e455443544c31)
	// Version 2 replaced the resolver transport with UDP sessions.
	ControlVersion = uint16(2)
	ControlSize    = 16

	controlOpcode     = 0
	controlClass      = 1
	controlReserved   = 2
	controlGeneration = 8

	// AddSessionHandles counts the attached client channel endpoint.
	AddSessionHandles = 1
	// SetDeviceHandles are the driver channel, the RX pool and the TX pool,
	// in that order. The codec checks the count; the service checks each
	// handle's type and rights through the kernel.
	SetDeviceHandles = 3
	// AddUDPSessionHandles are the session channel, the TX pool and the RX
	// pool, in that order (GUDP1, package udpapi).
	AddUDPSessionHandles = 3
	// SetNetworkHandles are a UDP session's three handles followed by a
	// network session channel.
	SetNetworkHandles = 4
)

type ControlOpcode uint8

const (
	ControlAddSession ControlOpcode = iota + 1
	ControlSetDevice
	// ControlAddUDPSession asks the network service to serve a UDP session
	// for a program. Rights is RightsUDP.
	ControlAddUDPSession
	// ControlSetNetwork gives a service that uses the network, the resolver
	// service, its own UDP session and a network session, under a generation
	// that grows with every new pair. The network service never receives it.
	ControlSetNetwork
)

type RightsClass uint8

const (
	RightsPingStatus RightsClass = iota + 1
	RightsAdminStatus
	// RightsResolveStatus is a resolver session that may resolve names and
	// read the resolver's status. The network service takes it as a session
	// that may read the status and the DNS servers and nothing more.
	RightsResolveStatus
	// RightsUDP is a UDP session, the authority to use UDP (ADR 0195).
	RightsUDP
)

type ControlResult uint8

const (
	ControlOK ControlResult = iota
	ControlInvalidArgument
	ControlBusy
	ControlDriverFault
	lastControlResult = ControlDriverFault
)

type ControlRequest struct {
	Opcode     ControlOpcode
	Rights     RightsClass
	Generation uint64
}

type ControlReply struct {
	Opcode     ControlOpcode
	Result     ControlResult
	Generation uint64
}

func controlHandles(opcode ControlOpcode) (int, bool) {
	switch opcode {
	case ControlAddSession:
		return AddSessionHandles, true
	case ControlSetDevice:
		return SetDeviceHandles, true
	case ControlAddUDPSession:
		return AddUDPSessionHandles, true
	case ControlSetNetwork:
		return SetNetworkHandles, true
	}
	return 0, false
}

func validControlRequest(r *ControlRequest) bool {
	switch r.Opcode {
	case ControlAddSession:
		return r.Generation == 0 && r.Rights >= RightsPingStatus && r.Rights <= RightsResolveStatus
	case ControlAddUDPSession:
		return r.Generation == 0 && r.Rights == RightsUDP
	case ControlSetDevice, ControlSetNetwork:
		return r.Generation != 0 && r.Rights == 0
	}
	return false
}

// EncodeControlRequest attaches exactly the handles the opcode requires.
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
	prepare(out, ControlProtocolID, ControlVersion, ipc.KindRequest, correlation, ControlSize)
	out.HandleCount = uint8(count)
	copy(out.Handles[:], attachments)
	out.Payload[controlOpcode] = byte(r.Opcode)
	out.Payload[controlClass] = byte(r.Rights)
	binary.LittleEndian.PutUint64(out.Payload[controlGeneration:], r.Generation)
	return true
}

// DecodeControlRequest leaves the attachments in in.Handles. On failure the
// receiver still closes whatever was attached.
func DecodeControlRequest(in *ipc.Message, out *ControlRequest) (correlation uint64, ok bool) {
	if !controlRequestEnvelope(in) || in.PayloadSize != ControlSize {
		return 0, false
	}
	payload := in.Payload[:ControlSize]
	r := ControlRequest{
		Opcode:     ControlOpcode(payload[controlOpcode]),
		Rights:     RightsClass(payload[controlClass]),
		Generation: binary.LittleEndian.Uint64(payload[controlGeneration:]),
	}
	count, known := controlHandles(r.Opcode)
	if !known || int(in.HandleCount) != count || !zero(payload[controlReserved:controlGeneration]) || !validControlRequest(&r) {
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

// ControlRequestOpcode identifies a malformed control request that still
// deserves a reply.
func ControlRequestOpcode(in *ipc.Message) (ControlOpcode, uint64, bool) {
	if !controlRequestEnvelope(in) || in.PayloadSize == 0 {
		return 0, 0, false
	}
	opcode := ControlOpcode(in.Payload[controlOpcode])
	if _, known := controlHandles(opcode); !known {
		return 0, 0, false
	}
	return opcode, in.Correlation, true
}

func controlRequestEnvelope(in *ipc.Message) bool {
	return in.ProtocolID == ControlProtocolID && in.Version == ControlVersion && in.Kind == ipc.KindRequest &&
		in.Correlation != 0 && int(in.HandleCount) <= len(in.Handles)
}

func EncodeControlReply(r *ControlReply, correlation uint64, out *ipc.Message) bool {
	if _, known := controlHandles(r.Opcode); !known || correlation == 0 || r.Result > lastControlResult {
		return false
	}
	prepare(out, ControlProtocolID, ControlVersion, ipc.KindReply, correlation, ControlSize)
	out.Payload[controlOpcode] = byte(r.Opcode)
	out.Payload[controlClass] = byte(r.Result)
	binary.LittleEndian.PutUint64(out.Payload[controlGeneration:], r.Generation)
	return true
}

// DecodeControlReply accepts only the reply to the caller's request. The
// caller compares Generation with the one it sent.
func DecodeControlReply(in *ipc.Message, correlation uint64, opcode ControlOpcode, out *ControlReply) bool {
	if in.ProtocolID != ControlProtocolID || in.Version != ControlVersion || in.Kind != ipc.KindReply ||
		correlation == 0 || in.Correlation != correlation || in.PayloadSize != ControlSize || in.HandleCount != 0 {
		return false
	}
	payload := in.Payload[:ControlSize]
	result := ControlResult(payload[controlClass])
	if ControlOpcode(payload[controlOpcode]) != opcode || result > lastControlResult ||
		!zero(payload[controlReserved:controlGeneration]) {
		return false
	}
	if _, known := controlHandles(opcode); !known {
		return false
	}
	out.Opcode, out.Result, out.Generation = opcode, result, binary.LittleEndian.Uint64(payload[controlGeneration:])
	return true
}
