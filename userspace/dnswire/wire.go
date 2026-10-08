// Package dnswire is the DNS message format shared by the resolver core and
// the dnsq command: names, query encoding, response validation, record
// iteration and presentation text. It knows nothing of transports, caches or
// transactions. Every parser here is bounded: labels of at most 63 bytes,
// names of at most 255, compression pointers that only point backwards and
// record lengths checked against the message. Nothing allocates.
package dnswire

import (
	"encoding/binary"

	"goos/userspace/hostname"
)

const (
	HeaderSize = 12
	// MaxName is the longest name in wire form, root label included.
	MaxName  = 255
	MaxLabel = 63
	// MaxPointers bounds the compression pointers followed in one name. Each
	// pointer must also land before the previous one, so a loop cannot form.
	MaxPointers = 64
	// MinRecordSize is a root owner name and the fixed record fields.
	MinRecordSize = 11
	QuestionFixed = 4
	RecordFixed   = 10
	SOAFixed      = 20
	OPTSize       = 11
	// MaxQuery is the longest query EncodeQuery writes.
	MaxQuery = HeaderSize + MaxName + QuestionFixed + OPTSize
	// EDNSPayload is the UDP payload size a query advertises in EDNS(0).
	EDNSPayload = 1232

	TypeA     = 1
	TypeNS    = 2
	TypeCNAME = 5
	TypeSOA   = 6
	TypePTR   = 12
	TypeMX    = 15
	TypeTXT   = 16
	TypeAAAA  = 28
	TypeOPT   = 41
	ClassIN   = 1

	FlagQR     = 0x8000
	FlagAA     = 0x0400
	FlagTC     = 0x0200
	FlagRD     = 0x0100
	FlagRA     = 0x0080
	FlagAD     = 0x0020
	FlagCD     = 0x0010
	OpcodeMask = 0x7800
	RcodeMask  = 0x000f

	RcodeNoError  = 0
	RcodeFormErr  = 1
	RcodeServFail = 2
	RcodeNXDomain = 3
	RcodeRefused  = 5

	ttlSignBit  = 0x80000000
	pointerMask = 0xc0
	optDO       = 0x8000
)

// Name is a name in wire form with ASCII letters folded to lower case, so
// two spellings of one name compare equal byte for byte. A set name always
// ends with the root label.
type Name struct {
	data [MaxName]byte
	size uint8
}

// Bytes returns the name in wire form.
func (name *Name) Bytes() []byte { return name.data[:name.size] }

func (name *Name) Equal(other *Name) bool {
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

// Within reports whether name is zone or a name under it.
func (name *Name) Within(zone *Name) bool {
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

func (name *Name) CopyFrom(other *Name) {
	name.size = other.size
	copy(name.data[:other.size], other.data[:other.size])
}

// SetHost encodes a normalized host name. A name of at most 253 bytes always
// fits in 255 wire bytes.
func (name *Name) SetHost(host *hostname.Name) {
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

// Host converts the name to a normalized host name. It fails for the root,
// for labels holding a dot or any byte outside host-name syntax, and for
// names longer than hostname.MaxText.
func (name *Name) Host(out *hostname.Name) bool {
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

// SetText encodes a dotted name typed by a person. It is wider than host-name
// syntax so that names like _sip._tcp.example. and in-addr.arpa names work:
// any printable ASCII except the dot and backslash is taken as is, which
// keeps the text form unambiguous without an escape syntax. A trailing dot is
// optional and "." alone is the root. Empty labels, labels over 63 bytes and
// names over 255 wire bytes fail.
func (name *Name) SetText(text []byte) bool {
	name.size = 0
	if len(text) == 1 && text[0] == '.' {
		name.data[0], name.size = 0, 1
		return true
	}
	if len(text) != 0 && text[len(text)-1] == '.' {
		text = text[:len(text)-1]
	}
	if len(text) == 0 || len(text)+2 > MaxName {
		return false
	}
	out, start := 0, 0
	for index := 0; index <= len(text); index++ {
		if index < len(text) {
			character := text[index]
			if character != '.' {
				if character <= ' ' || character > '~' || character == '\\' {
					return false
				}
				continue
			}
		}
		length := index - start
		if length == 0 || length > MaxLabel {
			return false
		}
		name.data[out] = byte(length)
		for offset := 0; offset < length; offset++ {
			character := text[start+offset]
			if character >= 'A' && character <= 'Z' {
				character += 'a' - 'A'
			}
			name.data[out+1+offset] = character
		}
		out += 1 + length
		start = index + 1
	}
	name.data[out] = 0
	name.size = uint8(out + 1)
	return true
}

// DecodeName reads the name at offset into out and returns the offset after
// it in the original position. Compression pointers must point before the
// previous pointer target (or the name's start), labels are at most 63 bytes,
// the expanded name is at most 255 bytes, and the extended label types are
// refused.
func DecodeName(message []byte, offset int, out *Name) (int, bool) {
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
			if target >= limit || pointers > MaxPointers {
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
		if position+1+length > len(message) || size+1+length+1 > MaxName {
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

// EncodeQuery writes a recursive query of class IN for name and qtype and
// returns its length. With edns it carries an OPT record advertising
// EDNSPayload, version 0, DO clear. dst must hold MaxQuery bytes.
func EncodeQuery(dst []byte, id uint16, name *Name, qtype uint16, edns bool) int {
	binary.BigEndian.PutUint16(dst[0:2], id)
	binary.BigEndian.PutUint16(dst[2:4], FlagRD)
	binary.BigEndian.PutUint16(dst[4:6], 1)
	binary.BigEndian.PutUint16(dst[6:8], 0)
	binary.BigEndian.PutUint16(dst[8:10], 0)
	binary.BigEndian.PutUint16(dst[10:12], 0)
	length := HeaderSize + copy(dst[HeaderSize:], name.data[:name.size])
	binary.BigEndian.PutUint16(dst[length:], qtype)
	binary.BigEndian.PutUint16(dst[length+2:], ClassIN)
	length += QuestionFixed
	if !edns {
		return length
	}
	binary.BigEndian.PutUint16(dst[10:12], 1)
	// Root owner, type OPT, the payload size in the class field, then a zero
	// extended rcode, version and flags (DO clear) and no options.
	dst[length] = 0
	binary.BigEndian.PutUint16(dst[length+1:], TypeOPT)
	binary.BigEndian.PutUint16(dst[length+3:], EDNSPayload)
	binary.BigEndian.PutUint32(dst[length+5:], 0)
	binary.BigEndian.PutUint16(dst[length+9:], 0)
	return length + OPTSize
}
