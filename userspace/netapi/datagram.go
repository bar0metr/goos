package netapi

import (
	"encoding/binary"
	"goos/kernel/ipc"
)

// GNETDGRAM1 is the resolver transport between the resolver service and the
// network service (ADR 0194). It carries DNS datagrams to UDP port 53 and
// nothing else, and only the resolver holds an end of it.
//
// Datagram bodies live in two shared pools of DatagramSlots slots. The query
// pool belongs to the resolver, which writes it; the network service maps it
// read-only. The reply pool is the reverse. A slot changes owner only by a
// message: Send hands a query slot to the service and Accepted hands it
// back; Result hands a reply slot to the resolver and Release hands it back.
// Each side refuses a message about a slot it does not expect, and a new
// binding starts with every slot back with its pool's writer.
const (
	DatagramProtocolID = uint64(0x474e455444475231)
	DatagramVersion    = uint16(1)
	DatagramSize       = 48

	DatagramSlots     = 8
	DatagramSlotSize  = 1280
	DatagramPoolPages = 3
	// DatagramMaxPayload is the largest datagram either way.
	DatagramMaxPayload = 1232
	// DatagramPort is the one destination port the transport allows.
	DatagramPort = 53
	// MaxDatagramTimeoutMS bounds how long the service listens for a reply.
	MaxDatagramTimeoutMS = 30000
	MaxServers           = 4

	datagramOpcode     = 0
	datagramResult     = 1
	datagramSlot       = 2
	datagramCount      = 3
	datagramAddress    = 4
	datagramLength     = 8
	datagramPort       = 10
	datagramTimeout    = 12
	datagramReserved   = 14
	datagramTag        = 16
	datagramGeneration = 24
	datagramServers    = 32
)

type DatagramOpcode uint8

const (
	// DatagramSend asks the service to send the query in Slot, Length bytes,
	// to Address port 53 and listen TimeoutMS for one reply. Tag is the
	// resolver's nonzero name for the exchange.
	DatagramSend DatagramOpcode = iota + 1
	// DatagramAccepted answers a Send and returns its query slot. A Result
	// other than OK ends the exchange there.
	DatagramAccepted
	// DatagramResult ends an accepted exchange. On OK, Slot is a reply slot
	// now owned by the resolver, holding Length bytes from Address and Port.
	DatagramResult
	// DatagramRelease returns a reply slot to the service.
	DatagramRelease
	// DatagramServers reports the DNS servers of the current DHCP lease, up
	// to MaxServers, under a generation that grows with every change.
	DatagramServers
	lastDatagramOpcode = DatagramServers
)

type Datagram struct {
	Opcode     DatagramOpcode
	Result     Result
	Slot       uint8
	Count      uint8
	Address    [addressSize]byte
	Length     uint16
	Port       uint16
	TimeoutMS  uint16
	Tag        uint64
	Generation uint64
	Servers    [MaxServers][addressSize]byte
}

// EncodeDatagram fails when d sets a field its opcode does not use.
func EncodeDatagram(d *Datagram, out *ipc.Message) bool {
	if !validDatagram(d) {
		return false
	}
	prepare(out, DatagramProtocolID, DatagramVersion, ipc.KindNotification, 1, DatagramSize)
	payload := out.Payload[:DatagramSize]
	payload[datagramOpcode] = byte(d.Opcode)
	payload[datagramResult] = byte(d.Result)
	payload[datagramSlot] = d.Slot
	payload[datagramCount] = d.Count
	copy(payload[datagramAddress:], d.Address[:])
	binary.LittleEndian.PutUint16(payload[datagramLength:], d.Length)
	binary.LittleEndian.PutUint16(payload[datagramPort:], d.Port)
	binary.LittleEndian.PutUint16(payload[datagramTimeout:], d.TimeoutMS)
	binary.LittleEndian.PutUint64(payload[datagramTag:], d.Tag)
	binary.LittleEndian.PutUint64(payload[datagramGeneration:], d.Generation)
	for index := range d.Servers {
		copy(payload[datagramServers+4*index:], d.Servers[index][:])
	}
	return true
}

// DecodeDatagram fills out only from a well-formed message.
func DecodeDatagram(in *ipc.Message, out *Datagram) bool {
	if in.ProtocolID != DatagramProtocolID || in.Version != DatagramVersion || in.Kind != ipc.KindNotification ||
		in.Correlation != 1 || in.PayloadSize != DatagramSize || in.HandleCount != 0 {
		return false
	}
	payload := in.Payload[:DatagramSize]
	if !zero(payload[datagramReserved:datagramTag]) {
		return false
	}
	d := Datagram{
		Opcode:     DatagramOpcode(payload[datagramOpcode]),
		Result:     Result(payload[datagramResult]),
		Slot:       payload[datagramSlot],
		Count:      payload[datagramCount],
		Length:     binary.LittleEndian.Uint16(payload[datagramLength:]),
		Port:       binary.LittleEndian.Uint16(payload[datagramPort:]),
		TimeoutMS:  binary.LittleEndian.Uint16(payload[datagramTimeout:]),
		Tag:        binary.LittleEndian.Uint64(payload[datagramTag:]),
		Generation: binary.LittleEndian.Uint64(payload[datagramGeneration:]),
	}
	copy(d.Address[:], payload[datagramAddress:])
	for index := range d.Servers {
		copy(d.Servers[index][:], payload[datagramServers+4*index:])
	}
	if !validDatagram(&d) {
		return false
	}
	*out = d
	return true
}

func validDatagram(d *Datagram) bool {
	noAddress := [addressSize]byte{}
	noServers := d.Servers == [MaxServers][addressSize]byte{}
	if d.Result > lastResult || d.Slot >= DatagramSlots && d.Opcode != DatagramServers {
		return false
	}
	switch d.Opcode {
	case DatagramSend:
		return d.Result == ResultOK && d.Count == 0 && d.Address != noAddress && d.Length != 0 &&
			d.Length <= DatagramMaxPayload && d.Port == DatagramPort && d.TimeoutMS != 0 &&
			d.TimeoutMS <= MaxDatagramTimeoutMS && d.Tag != 0 && d.Generation == 0 && noServers
	case DatagramAccepted:
		return d.Count == 0 && d.Address == noAddress && d.Length == 0 && d.Port == 0 && d.TimeoutMS == 0 &&
			d.Tag != 0 && d.Generation == 0 && noServers
	case DatagramResult:
		if d.Count != 0 || d.TimeoutMS != 0 || d.Tag == 0 || d.Generation != 0 || !noServers {
			return false
		}
		if d.Result == ResultOK {
			return d.Address != noAddress && d.Length != 0 && d.Length <= DatagramMaxPayload && d.Port == DatagramPort
		}
		return d.Slot == 0 && d.Address == noAddress && d.Length == 0 && d.Port == 0
	case DatagramRelease:
		return d.Result == ResultOK && d.Count == 0 && d.Address == noAddress && d.Length == 0 && d.Port == 0 &&
			d.TimeoutMS == 0 && d.Tag == 0 && d.Generation == 0 && noServers
	case DatagramServers:
		if d.Result != ResultOK || d.Slot != 0 || d.Count > MaxServers || d.Address != noAddress || d.Length != 0 ||
			d.Port != 0 || d.TimeoutMS != 0 || d.Tag != 0 || d.Generation == 0 {
			return false
		}
		for index := int(d.Count); index < MaxServers; index++ {
			if d.Servers[index] != noAddress {
				return false
			}
		}
		return true
	}
	return false
}

// DatagramSlotOffset is where slot starts in a pool.
func DatagramSlotOffset(slot uint8) (uint64, bool) {
	if slot >= DatagramSlots {
		return 0, false
	}
	return uint64(slot) * DatagramSlotSize, true
}
