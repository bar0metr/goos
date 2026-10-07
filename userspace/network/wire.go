package network

import "encoding/binary"

const (
	MTU = 1500
	// HeaderSize is the untagged Ethernet II header.
	HeaderSize = 14
	MaxFrame   = HeaderSize + MTU
	// minFrame is the shortest Ethernet frame without its frame check
	// sequence. Shorter outgoing frames are zero-padded to it.
	minFrame = 60

	etherTypeIPv4 = 0x0800
	etherTypeARP  = 0x0806

	arpSize           = 28
	arpHardwareEther  = 1
	arpRequest        = 1
	arpReply          = 2
	arpHardwareLength = 6
	arpProtocolLength = 4

	ipv4HeaderSize   = 20
	ipv4Version      = 4
	ipv4MinIHL       = 5
	ipv4FlagMore     = 0x2000
	ipv4OffsetMask   = 0x1fff
	protocolICMP     = 1
	protocolUDP      = 17
	icmpHeaderSize   = 8
	icmpEchoReply    = 0
	icmpUnreachable  = 3
	icmpEchoRequest  = 8
	icmpTimeExceeded = 11
	// icmpQuotedEcho is the part of the original datagram an ICMP error must
	// quote for us to match it: the IPv4 header plus the echo header.
	icmpQuotedEcho = ipv4HeaderSize + icmpHeaderSize

	udpHeaderSize = 8
	portServer    = 67
	portClient    = 68
)

type parseStatus uint8

const (
	parseOK parseStatus = iota
	// parseMalformed marks bytes that break the protocol's own rules.
	parseMalformed
	// parseUnsupported marks well-formed input this stack declines, such as
	// IPv4 options or fragments.
	parseUnsupported
)

type ethernetFrame struct {
	destination MAC
	source      MAC
	etherType   uint16
	payload     []byte
}

func parseEthernet(frame []byte) (ethernetFrame, bool) {
	var parsed ethernetFrame
	if len(frame) < HeaderSize || len(frame) > MaxFrame {
		return parsed, false
	}
	copy(parsed.destination[:], frame[0:6])
	copy(parsed.source[:], frame[6:12])
	parsed.etherType = binary.BigEndian.Uint16(frame[12:14])
	parsed.payload = frame[HeaderSize:]
	return parsed, true
}

func putEthernet(frame []byte, destination, source MAC, etherType uint16) {
	copy(frame[0:6], destination[:])
	copy(frame[6:12], source[:])
	binary.BigEndian.PutUint16(frame[12:14], etherType)
}

type arpPacket struct {
	operation uint16
	senderMAC MAC
	senderIP  IPv4
	targetMAC MAC
	targetIP  IPv4
}

// parseARP reads an Ethernet/IPv4 request or reply. Trailing bytes are the
// Ethernet minimum-size padding and are ignored.
func parseARP(payload []byte) (arpPacket, parseStatus) {
	var packet arpPacket
	if len(payload) < arpSize {
		return packet, parseMalformed
	}
	if binary.BigEndian.Uint16(payload[0:2]) != arpHardwareEther ||
		binary.BigEndian.Uint16(payload[2:4]) != etherTypeIPv4 ||
		payload[4] != arpHardwareLength || payload[5] != arpProtocolLength {
		return packet, parseUnsupported
	}
	packet.operation = binary.BigEndian.Uint16(payload[6:8])
	if packet.operation != arpRequest && packet.operation != arpReply {
		return packet, parseUnsupported
	}
	copy(packet.senderMAC[:], payload[8:14])
	copy(packet.senderIP[:], payload[14:18])
	copy(packet.targetMAC[:], payload[18:24])
	copy(packet.targetIP[:], payload[24:28])
	return packet, parseOK
}

func putARP(payload []byte, packet arpPacket) {
	binary.BigEndian.PutUint16(payload[0:2], arpHardwareEther)
	binary.BigEndian.PutUint16(payload[2:4], etherTypeIPv4)
	payload[4], payload[5] = arpHardwareLength, arpProtocolLength
	binary.BigEndian.PutUint16(payload[6:8], packet.operation)
	copy(payload[8:14], packet.senderMAC[:])
	copy(payload[14:18], packet.senderIP[:])
	copy(payload[18:24], packet.targetMAC[:])
	copy(payload[24:28], packet.targetIP[:])
}

// checksumAdd sums data as big-endian 16-bit words. A 1500-byte packet adds at
// most 750 words of 0xffff, far below the uint32 limit, so folding once at the
// end is enough.
func checksumAdd(sum uint32, data []byte) uint32 {
	index := 0
	for ; index+1 < len(data); index += 2 {
		sum += uint32(data[index])<<8 | uint32(data[index+1])
	}
	if index < len(data) {
		sum += uint32(data[index]) << 8
	}
	return sum
}

func checksumFold(sum uint32) uint16 {
	for sum>>16 != 0 {
		sum = sum&0xffff + sum>>16
	}
	return ^uint16(sum)
}

func checksum(data []byte) uint16 { return checksumFold(checksumAdd(0, data)) }

func pseudoHeaderSum(source, destination IPv4, protocol uint8, length int) uint32 {
	sum := checksumAdd(0, source[:])
	sum = checksumAdd(sum, destination[:])
	return sum + uint32(protocol) + uint32(length)
}

type ipv4Packet struct {
	source      IPv4
	destination IPv4
	protocol    uint8
	payload     []byte
}

// parseIPv4 trims the packet to its total length, which drops Ethernet
// padding, and refuses anything the stack cannot process whole.
func parseIPv4(data []byte) (ipv4Packet, parseStatus) {
	var packet ipv4Packet
	if len(data) < ipv4HeaderSize {
		return packet, parseMalformed
	}
	if data[0]>>4 != ipv4Version || data[0]&0x0f < ipv4MinIHL {
		return packet, parseMalformed
	}
	headerLength := int(data[0]&0x0f) * 4
	total := int(binary.BigEndian.Uint16(data[2:4]))
	if total < headerLength || total > len(data) {
		return packet, parseMalformed
	}
	if checksum(data[:headerLength]) != 0 {
		return packet, parseMalformed
	}
	if headerLength != ipv4HeaderSize {
		return packet, parseUnsupported
	}
	fragment := binary.BigEndian.Uint16(data[6:8])
	if fragment&ipv4FlagMore != 0 || fragment&ipv4OffsetMask != 0 {
		return packet, parseUnsupported
	}
	if data[8] == 0 {
		return packet, parseUnsupported
	}
	packet.protocol = data[9]
	copy(packet.source[:], data[12:16])
	copy(packet.destination[:], data[16:20])
	packet.payload = data[ipv4HeaderSize:total]
	return packet, parseOK
}

func putIPv4Header(header []byte, ttl uint8, source, destination IPv4, protocol uint8, identification uint16, payloadLength int) {
	header[0] = ipv4Version<<4 | ipv4MinIHL
	header[1] = 0
	binary.BigEndian.PutUint16(header[2:4], uint16(ipv4HeaderSize+payloadLength))
	binary.BigEndian.PutUint16(header[4:6], identification)
	binary.BigEndian.PutUint16(header[6:8], 0)
	header[8] = ttl
	header[9] = protocol
	header[10], header[11] = 0, 0
	copy(header[12:16], source[:])
	copy(header[16:20], destination[:])
	binary.BigEndian.PutUint16(header[10:12], checksum(header[:ipv4HeaderSize]))
}

type icmpMessage struct {
	kind       uint8
	code       uint8
	identifier uint16
	sequence   uint16
	// body follows the eight-byte header: echo data, or the quoted datagram
	// of an error.
	body []byte
}

func parseICMP(data []byte) (icmpMessage, bool) {
	var message icmpMessage
	if len(data) < icmpHeaderSize || checksum(data) != 0 {
		return message, false
	}
	message.kind, message.code = data[0], data[1]
	message.identifier = binary.BigEndian.Uint16(data[4:6])
	message.sequence = binary.BigEndian.Uint16(data[6:8])
	message.body = data[icmpHeaderSize:]
	return message, true
}

// quotedEcho is the part of our own echo request that an ICMP error quotes.
type quotedEcho struct {
	source      IPv4
	destination IPv4
	identifier  uint16
	sequence    uint16
}

// parseQuotedEcho accepts only a quoted IPv4 header without options that
// carried an ICMP echo request, since that is the only datagram we send that
// can draw an error we report.
func parseQuotedEcho(body []byte) (quotedEcho, bool) {
	var quoted quotedEcho
	if len(body) < icmpQuotedEcho {
		return quoted, false
	}
	if body[0] != ipv4Version<<4|ipv4MinIHL || body[9] != protocolICMP {
		return quoted, false
	}
	echo := body[ipv4HeaderSize:]
	if echo[0] != icmpEchoRequest || echo[1] != 0 {
		return quoted, false
	}
	copy(quoted.source[:], body[12:16])
	copy(quoted.destination[:], body[16:20])
	quoted.identifier = binary.BigEndian.Uint16(echo[4:6])
	quoted.sequence = binary.BigEndian.Uint16(echo[6:8])
	return quoted, true
}

func putICMPEcho(message []byte, kind uint8, identifier, sequence uint16, payload []byte) int {
	message[0], message[1] = kind, 0
	message[2], message[3] = 0, 0
	binary.BigEndian.PutUint16(message[4:6], identifier)
	binary.BigEndian.PutUint16(message[6:8], sequence)
	length := icmpHeaderSize + copy(message[icmpHeaderSize:], payload)
	binary.BigEndian.PutUint16(message[2:4], checksum(message[:length]))
	return length
}

type udpDatagram struct {
	sourcePort      uint16
	destinationPort uint16
	payload         []byte
}

// parseUDP checks the UDP length against the IPv4 payload and verifies a
// nonzero checksum over the pseudo-header. Zero is the sender's no-checksum
// form.
func parseUDP(packet ipv4Packet) (udpDatagram, bool) {
	var datagram udpDatagram
	data := packet.payload
	if len(data) < udpHeaderSize {
		return datagram, false
	}
	length := int(binary.BigEndian.Uint16(data[4:6]))
	if length < udpHeaderSize || length > len(data) {
		return datagram, false
	}
	data = data[:length]
	if binary.BigEndian.Uint16(data[6:8]) != 0 {
		sum := pseudoHeaderSum(packet.source, packet.destination, protocolUDP, length)
		if checksumFold(checksumAdd(sum, data)) != 0 {
			return datagram, false
		}
	}
	datagram.sourcePort = binary.BigEndian.Uint16(data[0:2])
	datagram.destinationPort = binary.BigEndian.Uint16(data[2:4])
	datagram.payload = data[udpHeaderSize:]
	return datagram, true
}

// putUDP writes the UDP header for a payload already in place after it and
// always sends a checksum.
func putUDP(datagram []byte, source, destination IPv4, sourcePort, destinationPort uint16, payloadLength int) int {
	length := udpHeaderSize + payloadLength
	binary.BigEndian.PutUint16(datagram[0:2], sourcePort)
	binary.BigEndian.PutUint16(datagram[2:4], destinationPort)
	binary.BigEndian.PutUint16(datagram[4:6], uint16(length))
	datagram[6], datagram[7] = 0, 0
	sum := checksumFold(checksumAdd(pseudoHeaderSum(source, destination, protocolUDP, length), datagram[:length]))
	if sum == 0 {
		sum = 0xffff
	}
	binary.BigEndian.PutUint16(datagram[6:8], sum)
	return length
}
