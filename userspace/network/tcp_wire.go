package network

import "encoding/binary"

const (
	tcpHeaderSize = 20
	tcpFlagFIN    = 0x01
	tcpFlagSYN    = 0x02
	tcpFlagRST    = 0x04
	tcpFlagPSH    = 0x08
	tcpFlagACK    = 0x10
	tcpOptionEOL  = 0
	tcpOptionNOP  = 1
	tcpOptionMSS  = 2
	tcpDefaultMSS = 536
)

type tcpStatus uint8

const (
	tcpOK tcpStatus = iota
	tcpMalformed
	tcpBadChecksum
)

type tcpSegment struct {
	sourcePort      uint16
	destinationPort uint16
	sequence        uint32
	acknowledgment  uint32
	flags           uint8
	window          uint16
	mss             uint16
	payload         []byte
}

// parseTCP validates the complete header and options before a caller looks up
// a connection. Unknown well-formed options are skipped.
func parseTCP(packet ipv4Packet) (tcpSegment, tcpStatus) {
	var segment tcpSegment
	data := packet.payload
	if len(data) < tcpHeaderSize {
		return segment, tcpMalformed
	}
	headerLength := int(data[12]>>4) * 4
	if headerLength < tcpHeaderSize || headerLength > len(data) || data[12]&1 != 0 || data[13]&0xc0 != 0 {
		return segment, tcpMalformed
	}
	segment.sourcePort = binary.BigEndian.Uint16(data[0:2])
	segment.destinationPort = binary.BigEndian.Uint16(data[2:4])
	if segment.sourcePort == 0 || segment.destinationPort == 0 {
		return tcpSegment{}, tcpMalformed
	}
	length := len(data)
	sum := pseudoHeaderSum(packet.source, packet.destination, protocolTCP, length)
	if checksumFold(checksumAdd(sum, data)) != 0 {
		return tcpSegment{}, tcpBadChecksum
	}
	segment.sequence = binary.BigEndian.Uint32(data[4:8])
	segment.acknowledgment = binary.BigEndian.Uint32(data[8:12])
	segment.flags = data[13] & 0x3f
	segment.window = binary.BigEndian.Uint16(data[14:16])
	segment.mss = tcpDefaultMSS
	for offset := tcpHeaderSize; offset < headerLength; {
		kind := data[offset]
		switch kind {
		case tcpOptionEOL:
			offset = headerLength
			continue
		case tcpOptionNOP:
			offset++
			continue
		}
		if offset+2 > headerLength {
			return tcpSegment{}, tcpMalformed
		}
		optionLength := int(data[offset+1])
		if optionLength < 2 || offset+optionLength > headerLength {
			return tcpSegment{}, tcpMalformed
		}
		if kind == tcpOptionMSS {
			if optionLength != 4 {
				return tcpSegment{}, tcpMalformed
			}
			mss := binary.BigEndian.Uint16(data[offset+2 : offset+4])
			if mss == 0 {
				return tcpSegment{}, tcpMalformed
			}
			segment.mss = mss
		}
		offset += optionLength
	}
	segment.payload = data[headerLength:]
	return segment, tcpOK
}

// putTCP writes a header, an MSS option on SYN, and payload. It returns the
// segment length. The caller must provide enough space.
func putTCP(out []byte, source, destination IPv4, segment tcpSegment) int {
	headerLength := tcpHeaderSize
	if segment.flags&tcpFlagSYN != 0 {
		headerLength += 4
	}
	length := headerLength + len(segment.payload)
	for index := 0; index < headerLength; index++ {
		out[index] = 0
	}
	binary.BigEndian.PutUint16(out[0:2], segment.sourcePort)
	binary.BigEndian.PutUint16(out[2:4], segment.destinationPort)
	binary.BigEndian.PutUint32(out[4:8], segment.sequence)
	binary.BigEndian.PutUint32(out[8:12], segment.acknowledgment)
	out[12] = byte(headerLength/4) << 4
	out[13] = segment.flags
	binary.BigEndian.PutUint16(out[14:16], segment.window)
	if headerLength > tcpHeaderSize {
		out[20], out[21] = tcpOptionMSS, 4
		binary.BigEndian.PutUint16(out[22:24], segment.mss)
	}
	copy(out[headerLength:length], segment.payload)
	sum := pseudoHeaderSum(source, destination, protocolTCP, length)
	binary.BigEndian.PutUint16(out[16:18], checksumFold(checksumAdd(sum, out[:length])))
	return length
}

func seqLT(a, b uint32) bool { return int32(a-b) < 0 }
func seqLE(a, b uint32) bool { return int32(a-b) <= 0 }
func seqGT(a, b uint32) bool { return int32(a-b) > 0 }
func seqGE(a, b uint32) bool { return int32(a-b) >= 0 }

func seqBetween(value, first, last uint32) bool {
	return seqGE(value, first) && seqLE(value, last)
}
