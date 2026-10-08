package dns

import (
	"encoding/binary"

	"goos/userspace/hostname"
)

const (
	headerSize = 12
	// maxWireName is the longest name in wire form, root label included.
	maxWireName = 255
	// maxPointers bounds the compression pointers followed in one name. Each
	// pointer must also land before the previous one, so a loop cannot form.
	maxPointers = 64
	// minRecordSize is a root owner name and the fixed record fields.
	minRecordSize = 11
	questionFixed = 4
	recordFixed   = 10
	soaFixed      = 20
	optRecordSize = 11
	maxQuerySize  = headerSize + maxWireName + questionFixed + optRecordSize
	typeA         = 1
	typeCNAME     = 5
	typeSOA       = 6
	typeOPT       = 41
	classIN       = 1
	flagQR        = 0x8000
	flagTC        = 0x0200
	flagRD        = 0x0100
	opcodeMask    = 0x7800
	rcodeMask     = 0x000f
	rcodeNoError  = 0
	rcodeFormErr  = 1
	rcodeServFail = 2
	rcodeNXDomain = 3
	rcodeRefused  = 5
	ttlSignBit    = 0x80000000
	pointerMask   = 0xc0
)

// wireName is a name in wire form with ASCII letters folded to lower case,
// so two spellings of one name compare equal byte for byte. It always ends
// with the root label.
type wireName struct {
	data [maxWireName]byte
	size uint8
}

func (name *wireName) equal(other *wireName) bool {
	if name.size != other.size {
		return false
	}
	for index := 0; index < int(name.size); index++ {
		if name.data[index] != other.data[index] {
			return false
		}
	}
	return true
}

// within reports whether name is zone or a name under it.
func (name *wireName) within(zone *wireName) bool {
	size, want := int(name.size), int(zone.size)
	position := 0
	for size-position >= want {
		if size-position == want {
			for index := 0; index < want; index++ {
				if name.data[position+index] != zone.data[index] {
					return false
				}
			}
			return true
		}
		length := int(name.data[position])
		if length == 0 {
			return false
		}
		position += 1 + length
	}
	return false
}

func (name *wireName) copyFrom(other *wireName) {
	name.size = other.size
	copy(name.data[:other.size], other.data[:other.size])
}

// setHost encodes a normalized host name. A name of at most 253 bytes always
// fits in 255 wire bytes.
func (name *wireName) setHost(host *hostname.Name) {
	text := host.Bytes()
	out := 0
	start := 0
	for index := 0; index <= len(text); index++ {
		if index < len(text) && text[index] != '.' {
			continue
		}
		name.data[out] = byte(index - start)
		out++
		out += copy(name.data[out:], text[start:index])
		start = index + 1
	}
	name.data[out] = 0
	name.size = uint8(out + 1)
}

// host converts the name to a normalized host name. It fails for the root,
// for labels holding a dot or any byte outside host-name syntax, and for
// names longer than hostname.MaxText.
func (name *wireName) host(out *hostname.Name) bool {
	size := int(name.size)
	if size < 2 || size-2 > hostname.MaxText {
		return false
	}
	position := 0
	text := 0
	for {
		length := int(name.data[position])
		if length == 0 {
			break
		}
		if text != 0 {
			out.Text[text] = '.'
			text++
		}
		for index := 1; index <= length; index++ {
			character := name.data[position+index]
			if character == '.' {
				return false
			}
			out.Text[text] = character
			text++
		}
		position += 1 + length
	}
	parsed, ok := hostname.Parse(out.Text[:text])
	if !ok {
		return false
	}
	*out = parsed
	return true
}

// decodeName reads the name at offset into out and returns the offset after
// it in the original position. Compression pointers must point before the
// previous pointer target (or the name's start), labels are at most 63 bytes,
// the expanded name is at most 255 bytes, and the extended label types are
// refused.
func decodeName(message []byte, offset int, out *wireName) (int, bool) {
	out.size = 0
	position := offset
	limit := offset
	end := -1
	pointers := 0
	size := 0
	for {
		if position < 0 || position >= len(message) {
			return 0, false
		}
		length := int(message[position])
		switch length & pointerMask {
		case pointerMask:
			if position+1 >= len(message) {
				return 0, false
			}
			target := (length&^pointerMask)<<8 | int(message[position+1])
			pointers++
			if target >= limit || pointers > maxPointers {
				return 0, false
			}
			if end < 0 {
				end = position + 2
			}
			position, limit = target, target
			continue
		case 0:
		default:
			return 0, false
		}
		if length == 0 {
			out.data[size] = 0
			out.size = uint8(size + 1)
			if end < 0 {
				end = position + 1
			}
			return end, true
		}
		// The label and at least the root label must still fit.
		if position+1+length > len(message) || size+1+length+1 > maxWireName {
			return 0, false
		}
		out.data[size] = byte(length)
		for index := 1; index <= length; index++ {
			character := message[position+index]
			if character >= 'A' && character <= 'Z' {
				character += 'a' - 'A'
			}
			out.data[size+index] = character
		}
		size += 1 + length
		position += 1 + length
	}
}

// encodeQuery writes a recursive A query for name and returns its length.
// dst must hold maxQuerySize bytes.
func encodeQuery(dst []byte, id uint16, name *wireName, edns bool) int {
	binary.BigEndian.PutUint16(dst[0:2], id)
	binary.BigEndian.PutUint16(dst[2:4], flagRD)
	binary.BigEndian.PutUint16(dst[4:6], 1)
	binary.BigEndian.PutUint16(dst[6:8], 0)
	binary.BigEndian.PutUint16(dst[8:10], 0)
	binary.BigEndian.PutUint16(dst[10:12], 0)
	length := headerSize + copy(dst[headerSize:], name.data[:name.size])
	binary.BigEndian.PutUint16(dst[length:], typeA)
	binary.BigEndian.PutUint16(dst[length+2:], classIN)
	length += questionFixed
	if !edns {
		return length
	}
	binary.BigEndian.PutUint16(dst[10:12], 1)
	// Root owner, type OPT, the payload size in the class field, then a zero
	// extended rcode, version and flags (DO clear) and no options.
	dst[length] = 0
	binary.BigEndian.PutUint16(dst[length+1:], typeOPT)
	binary.BigEndian.PutUint16(dst[length+3:], DatagramSize)
	binary.BigEndian.PutUint32(dst[length+5:], 0)
	binary.BigEndian.PutUint16(dst[length+9:], 0)
	return length + optRecordSize
}

type parseResult uint8

const (
	parseOK parseResult = iota
	// parseMismatched is a message that does not answer the outstanding
	// query: another ID or question. The query keeps waiting.
	parseMismatched
	// parseMalformed is a message for the outstanding query that breaks
	// the protocol. Its server counts as unusable.
	parseMalformed
)

// response is what a validated message holds. Record sections are kept as
// offsets and counts and walked again when needed.
type response struct {
	// rcode includes the extended bits from OPT.
	rcode       uint16
	truncated   bool
	hasOPT      bool
	answerAt    int
	answers     int
	authorityAt int
	authorities int
}

// record is one resource record's fixed fields and where its data lies.
type record struct {
	kind   uint16
	class  uint16
	ttl    uint32
	data   int
	length int
}

// readRecord decodes the owner name into owner and the fixed fields into
// out, and returns the offset after the record.
func readRecord(message []byte, offset int, owner *wireName, out *record) (int, bool) {
	position, ok := decodeName(message, offset, owner)
	if !ok || position+recordFixed > len(message) {
		return 0, false
	}
	out.kind = binary.BigEndian.Uint16(message[position:])
	out.class = binary.BigEndian.Uint16(message[position+2:])
	out.ttl = binary.BigEndian.Uint32(message[position+4:])
	out.length = int(binary.BigEndian.Uint16(message[position+8:]))
	out.data = position + recordFixed
	if out.length > len(message)-out.data {
		return 0, false
	}
	return out.data + out.length, true
}

// recordTTL applies RFC 2181: a TTL with the top bit set means zero.
func recordTTL(ttl uint32) uint32 {
	if ttl&ttlSignBit != 0 {
		return 0
	}
	return ttl
}

// parseResponse validates a whole message of at most limit bytes against
// the outstanding query before anything in it is used. The ID and question
// are checked first, so a stray datagram is a mismatch, not a reason to drop
// the server. A truncated message is reported without reading its records,
// which may be cut short, but only once its question matched: TC=1 alone
// never starts a TCP fallback. Bytes after the counted records are ignored:
// some servers pad replies and nothing there is read.
//
// One concession: a FORMERR without a question section is accepted when the
// ID matches, since servers that reject EDNS(0) often send exactly that.
func parseResponse(message []byte, limit int, id uint16, question *wireName, scratch *wireName, out *response) parseResult {
	*out = response{}
	if len(message) < headerSize || len(message) > limit {
		return parseMalformed
	}
	if binary.BigEndian.Uint16(message[0:2]) != id {
		return parseMismatched
	}
	flags := binary.BigEndian.Uint16(message[2:4])
	if flags&flagQR == 0 || flags&opcodeMask != 0 {
		return parseMalformed
	}
	out.rcode = flags & rcodeMask
	questions := int(binary.BigEndian.Uint16(message[4:6]))
	answers := int(binary.BigEndian.Uint16(message[6:8]))
	authorities := int(binary.BigEndian.Uint16(message[8:10]))
	additionals := int(binary.BigEndian.Uint16(message[10:12]))
	position := headerSize
	switch {
	case questions == 0 && out.rcode == rcodeFormErr:
		if flags&flagTC != 0 {
			return parseMalformed
		}
	case questions != 1:
		return parseMismatched
	default:
		next, ok := decodeName(message, position, scratch)
		if !ok || next+questionFixed > len(message) {
			return parseMalformed
		}
		if !scratch.equal(question) || binary.BigEndian.Uint16(message[next:]) != typeA ||
			binary.BigEndian.Uint16(message[next+2:]) != classIN {
			return parseMismatched
		}
		position = next + questionFixed
	}
	if flags&flagTC != 0 {
		out.truncated = true
		return parseOK
	}
	if (answers+authorities+additionals)*minRecordSize > len(message)-position {
		return parseMalformed
	}
	out.answerAt, out.answers = position, answers
	var item record
	for index := 0; index < answers+authorities+additionals; index++ {
		if index == answers {
			out.authorityAt, out.authorities = position, authorities
		}
		next, ok := readRecord(message, position, scratch, &item)
		if !ok || !validRecord(message, &item, scratch, index >= answers+authorities) {
			return parseMalformed
		}
		if item.kind == typeOPT {
			if out.hasOPT || scratch.size != 1 {
				return parseMalformed
			}
			out.hasOPT = true
			out.rcode |= uint16(item.ttl>>24) << 4
		}
		position = next
	}
	if authorities == 0 {
		out.authorityAt = position
	}
	return parseOK
}

// validRecord checks the data of the record types the resolver reads. Other
// types are skipped; readRecord already checked that their data fits.
func validRecord(message []byte, item *record, scratch *wireName, additional bool) bool {
	if item.kind == typeOPT {
		return additional
	}
	if item.class != classIN {
		return true
	}
	end := item.data + item.length
	switch item.kind {
	case typeA:
		return item.length == 4
	case typeCNAME:
		next, ok := decodeName(message, item.data, scratch)
		return ok && next == end
	case typeSOA:
		next, ok := decodeName(message, item.data, scratch)
		if !ok {
			return false
		}
		next, ok = decodeName(message, next, scratch)
		return ok && next+soaFixed == end
	}
	return true
}
