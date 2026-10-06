// Package netdev defines the GNETDRV1 protocol between the virtio-net driver
// and the network service, the slot ownership both sides keep for the shared
// frame pools, and the driver's virtqueue bookkeeping.
package netdev

import (
	"encoding/binary"
	"goos/kernel/ipc"
)

const (
	ProtocolID  = uint64(0x474e455444525631)
	Version     = uint16(1)
	PayloadSize = 32
	Slots       = 4
	SlotSize    = 2048
	PageSize    = 4096
	PoolPages   = 2
	PoolSize    = Slots * SlotSize
	MinFrame    = 14
	MaxFrame    = 1514
	MTU         = 1500
)

// A pool must fill its pages exactly; either subtraction underflows and
// fails to compile otherwise.
const _ = uint(PoolSize-PoolPages*PageSize) + uint(PoolPages*PageSize-PoolSize)

const (
	offsetOpcode     = 0
	offsetGeneration = 8
	offsetSlot       = 16
	offsetLength     = 18
	offsetStatus     = 20
	offsetMTU        = 22
	offsetMAC        = 24
	offsetLink       = 30
	offsetReserved   = 31
	macSize          = 6
	multicastBit     = 0x01
)

type Opcode uint8

const (
	OpReady Opcode = iota + 1
	OpRxReady
	OpRxRelease
	OpTx
	OpTxDone
	OpFault
)

type Result uint16

const (
	ResultOK Result = iota
	ResultBusy
	ResultInvalidFrame
	ResultDeviceFault
	lastResult = ResultDeviceFault
)

type Link uint8

const (
	LinkDown Link = iota
	LinkUp
)

type FaultReason uint16

const (
	FaultDevice FaultReason = iota + 1
	FaultProtocol
	FaultGeneration
	FaultResources
	MaxFaultReason = FaultResources
)

// Message is one decoded GNETDRV1 body. Result is used only by TxDone and
// Reason only by Fault; they share the wire status field.
type Message struct {
	Opcode     Opcode
	Generation uint64
	Slot       uint16
	Length     uint16
	Result     Result
	Reason     FaultReason
	MTU        uint16
	MAC        [macSize]byte
	Link       Link
}

// envelope returns the GIPC kind an opcode travels as and whether it carries
// a correlation. Tx and TxDone pair through the channel's request tracking,
// and that tracking rejects a duplicate or orphan completion.
func envelope(opcode Opcode) (kind ipc.Kind, correlated bool, ok bool) {
	switch opcode {
	case OpReady, OpRxReady, OpRxRelease, OpFault:
		return ipc.KindNotification, false, true
	case OpTx:
		return ipc.KindRequest, true, true
	case OpTxDone:
		return ipc.KindReply, true, true
	}
	return 0, false, false
}

// Encode writes m into out. It fails on any field the opcode does not allow,
// and on a correlation that is zero for Tx/TxDone or nonzero otherwise.
func Encode(m *Message, correlation uint64, out *ipc.Message) bool {
	kind, correlated, ok := envelope(m.Opcode)
	if !ok || correlated != (correlation != 0) || !valid(m) {
		return false
	}
	prepare(out, ProtocolID, Version, kind, correlation, PayloadSize)
	payload := out.Payload[:PayloadSize]
	payload[offsetOpcode] = byte(m.Opcode)
	binary.LittleEndian.PutUint64(payload[offsetGeneration:], m.Generation)
	binary.LittleEndian.PutUint16(payload[offsetSlot:], m.Slot)
	binary.LittleEndian.PutUint16(payload[offsetLength:], m.Length)
	binary.LittleEndian.PutUint16(payload[offsetStatus:], uint16(m.Result)|uint16(m.Reason))
	binary.LittleEndian.PutUint16(payload[offsetMTU:], m.MTU)
	copy(payload[offsetMAC:offsetMAC+macSize], m.MAC[:])
	payload[offsetLink] = byte(m.Link)
	return true
}

// Decode validates in and fills out, returning the correlation. out is
// untouched on failure. The caller still compares Generation with its
// current binding and Slot with its Pool.
func Decode(in *ipc.Message, out *Message) (correlation uint64, ok bool) {
	if in.ProtocolID != ProtocolID || in.Version != Version || in.PayloadSize != PayloadSize || in.HandleCount != 0 {
		return 0, false
	}
	payload := in.Payload[:PayloadSize]
	if !zero(payload[offsetOpcode+1:offsetGeneration]) || payload[offsetReserved] != 0 {
		return 0, false
	}
	m := Message{
		Opcode:     Opcode(payload[offsetOpcode]),
		Generation: binary.LittleEndian.Uint64(payload[offsetGeneration:]),
		Slot:       binary.LittleEndian.Uint16(payload[offsetSlot:]),
		Length:     binary.LittleEndian.Uint16(payload[offsetLength:]),
		MTU:        binary.LittleEndian.Uint16(payload[offsetMTU:]),
		Link:       Link(payload[offsetLink]),
	}
	copy(m.MAC[:], payload[offsetMAC:offsetMAC+macSize])
	status := binary.LittleEndian.Uint16(payload[offsetStatus:])
	switch m.Opcode {
	case OpTxDone:
		m.Result = Result(status)
	case OpFault:
		m.Reason = FaultReason(status)
	default:
		if status != 0 {
			return 0, false
		}
	}
	kind, correlated, known := envelope(m.Opcode)
	if !known || in.Kind != kind || correlated != (in.Correlation != 0) || !valid(&m) {
		return 0, false
	}
	*out = m
	return in.Correlation, true
}

func valid(m *Message) bool {
	if m.Generation == 0 {
		return false
	}
	noDevice := m.MTU == 0 && m.MAC == [macSize]byte{} && m.Link == LinkDown
	noStatus := m.Result == ResultOK && m.Reason == 0
	switch m.Opcode {
	case OpReady:
		return m.Slot == 0 && m.Length == 0 && noStatus && m.MTU == MTU && ValidMAC(m.MAC) && m.Link <= LinkUp
	case OpRxReady, OpTx:
		return m.Slot < Slots && m.Length >= MinFrame && m.Length <= MaxFrame && noStatus && noDevice
	case OpRxRelease:
		return m.Slot < Slots && m.Length == 0 && noStatus && noDevice
	case OpTxDone:
		return m.Slot < Slots && m.Length == 0 && m.Reason == 0 && m.Result <= lastResult && noDevice
	case OpFault:
		return m.Slot == 0 && m.Length == 0 && m.Result == ResultOK && m.Reason >= FaultDevice &&
			m.Reason <= MaxFaultReason && noDevice
	}
	return false
}

// ValidMAC accepts a unicast station address: not all zero and with the
// group bit clear, which also rules out broadcast.
func ValidMAC(mac [macSize]byte) bool {
	return mac != [macSize]byte{} && mac[0]&multicastBit == 0
}

// SlotOffset returns the byte offset of slot within either pool.
func SlotOffset(slot uint16) (uint64, bool) {
	if slot >= Slots {
		return 0, false
	}
	return uint64(slot) * SlotSize, true
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
