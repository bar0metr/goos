package main

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/netip"
)

// maxFrame bounds one framed packet. QEMU's stream backend accepts up to
// NET_BUFSIZE (4 + 68 KiB); Ethernet frames on this segment stay far below.
const maxFrame = 69632

// minFrame is the shortest Ethernet frame without its FCS. Short frames the
// peer builds are padded to it, as a NIC would.
const minFrame = 60

const (
	etherTypeIPv4 = 0x0800
	etherTypeARP  = 0x0806
)

// readFrame reads one frame in QEMU stream framing: a 4-byte big-endian
// length followed by that many bytes of Ethernet frame.
func readFrame(r io.Reader) ([]byte, error) {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, err
	}
	length := binary.BigEndian.Uint32(header[:])
	if length > maxFrame {
		return nil, fmt.Errorf("frame length %d above %d", length, maxFrame)
	}
	frame := make([]byte, length)
	if _, err := io.ReadFull(r, frame); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return nil, err
	}
	return frame, nil
}

// writeFrame writes one frame in QEMU stream framing with a single Write, so
// the length and the frame never interleave with another writer's output.
func writeFrame(w io.Writer, frame []byte) error {
	if len(frame) > maxFrame {
		return fmt.Errorf("frame length %d above %d", len(frame), maxFrame)
	}
	buffer := make([]byte, 4+len(frame))
	binary.BigEndian.PutUint32(buffer, uint32(len(frame)))
	copy(buffer[4:], frame)
	_, err := w.Write(buffer)
	return err
}

type mac [6]byte

var broadcastMAC = mac{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}

func parseMAC(text string) (mac, error) {
	hw, err := net.ParseMAC(text)
	if err != nil || len(hw) != 6 {
		return mac{}, fmt.Errorf("invalid MAC address %q", text)
	}
	var m mac
	copy(m[:], hw)
	return m, nil
}

func (m mac) String() string { return net.HardwareAddr(m[:]).String() }

type ipv4 [4]byte

func parseIPv4Addr(text string) (ipv4, error) {
	addr, err := netip.ParseAddr(text)
	if err != nil || !addr.Is4() {
		return ipv4{}, fmt.Errorf("invalid IPv4 address %q", text)
	}
	return addr.As4(), nil
}

func (a ipv4) String() string { return netip.AddrFrom4(a).String() }

var broadcastIPv4 = ipv4{255, 255, 255, 255}

// etherFrame builds an Ethernet II frame and pads it to the minimum length.
func etherFrame(dst, src mac, etherType uint16, payload []byte) []byte {
	size := 14 + len(payload)
	if size < minFrame {
		size = minFrame
	}
	frame := make([]byte, size)
	copy(frame[0:6], dst[:])
	copy(frame[6:12], src[:])
	binary.BigEndian.PutUint16(frame[12:14], etherType)
	copy(frame[14:], payload)
	return frame
}

// checksum returns the Internet checksum of data with an initial partial sum.
func checksum(data []byte, sum uint32) uint16 {
	for i := 0; i+1 < len(data); i += 2 {
		sum += uint32(data[i])<<8 | uint32(data[i+1])
	}
	if len(data)%2 == 1 {
		sum += uint32(data[len(data)-1]) << 8
	}
	for sum>>16 != 0 {
		sum = sum&0xffff + sum>>16
	}
	return ^uint16(sum)
}

type ipPacket struct {
	src, dst ipv4
	protocol byte
	payload  []byte
}

// parseIPv4 accepts an unfragmented IPv4 packet with a valid header
// checksum. Bytes past the total length (Ethernet padding) are dropped.
func parseIPv4(data []byte) (ipPacket, bool) {
	if len(data) < 20 || data[0]>>4 != 4 {
		return ipPacket{}, false
	}
	headerLength := int(data[0]&0x0f) * 4
	total := int(binary.BigEndian.Uint16(data[2:4]))
	if headerLength < 20 || total < headerLength || total > len(data) {
		return ipPacket{}, false
	}
	if checksum(data[:headerLength], 0) != 0 {
		return ipPacket{}, false
	}
	if binary.BigEndian.Uint16(data[6:8])&0x3fff != 0 {
		return ipPacket{}, false
	}
	packet := ipPacket{protocol: data[9], payload: data[headerLength:total]}
	copy(packet.src[:], data[12:16])
	copy(packet.dst[:], data[16:20])
	return packet, true
}

// buildIPv4 builds an IPv4 packet with a 20-byte header, TTL 64 and the
// don't-fragment bit clear.
func buildIPv4(src, dst ipv4, protocol byte, id uint16, payload []byte) []byte {
	packet := make([]byte, 20+len(payload))
	packet[0] = 0x45
	binary.BigEndian.PutUint16(packet[2:4], uint16(len(packet)))
	binary.BigEndian.PutUint16(packet[4:6], id)
	packet[8] = 64
	packet[9] = protocol
	copy(packet[12:16], src[:])
	copy(packet[16:20], dst[:])
	binary.BigEndian.PutUint16(packet[10:12], checksum(packet[:20], 0))
	copy(packet[20:], payload)
	return packet
}

type udpDatagram struct {
	srcPort, dstPort uint16
	payload          []byte
}

// parseUDP reads a UDP header. The checksum is not verified: a guest that
// leaves it to an offload engine would otherwise lose every datagram.
func parseUDP(data []byte) (udpDatagram, bool) {
	if len(data) < 8 {
		return udpDatagram{}, false
	}
	length := int(binary.BigEndian.Uint16(data[4:6]))
	if length < 8 || length > len(data) {
		return udpDatagram{}, false
	}
	return udpDatagram{
		srcPort: binary.BigEndian.Uint16(data[0:2]),
		dstPort: binary.BigEndian.Uint16(data[2:4]),
		payload: data[8:length],
	}, true
}

// buildUDP builds a UDP datagram with its checksum over the IPv4 pseudo
// header.
func buildUDP(src, dst ipv4, srcPort, dstPort uint16, payload []byte) []byte {
	datagram := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint16(datagram[0:2], srcPort)
	binary.BigEndian.PutUint16(datagram[2:4], dstPort)
	binary.BigEndian.PutUint16(datagram[4:6], uint16(len(datagram)))
	copy(datagram[8:], payload)
	sum := uint32(src[0])<<8 | uint32(src[1])
	sum += uint32(src[2])<<8 | uint32(src[3])
	sum += uint32(dst[0])<<8 | uint32(dst[1])
	sum += uint32(dst[2])<<8 | uint32(dst[3])
	sum += 17 + uint32(len(datagram))
	value := checksum(datagram, sum)
	if value == 0 {
		value = 0xffff
	}
	binary.BigEndian.PutUint16(datagram[6:8], value)
	return datagram
}
