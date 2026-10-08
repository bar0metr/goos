package dnswire

import "encoding/binary"

type Result uint8

const (
	ParseOK Result = iota
	// ParseMismatched is a message that does not answer the outstanding
	// query: another ID or question.
	ParseMismatched
	// ParseMalformed is a message for the outstanding query that breaks the
	// protocol.
	ParseMalformed
)

// Response is what a validated message holds. Record sections are kept as
// offsets and counts and walked again when needed.
type Response struct {
	Flags uint16
	// Rcode includes the extended bits from OPT.
	Rcode       uint16
	Truncated   bool
	Questions   int
	AnswerAt    int
	Answers     int
	AuthorityAt int
	Authorities int
	// AdditionalAt and Additionals count the OPT record too.
	AdditionalAt int
	Additionals  int
	HasOPT       bool
	EDNSPayload  uint16
	EDNSVersion  uint8
	EDNSDO       bool
}

// Record is one resource record's fixed fields and where its data lies.
type Record struct {
	Type   uint16
	Class  uint16
	TTL    uint32
	Data   int
	Length int
}

// ReadRecord decodes the owner name into owner and the fixed fields into
// out, and returns the offset after the record.
func ReadRecord(message []byte, offset int, owner *Name, out *Record) (int, bool) {
	position, ok := DecodeName(message, offset, owner)
	if !ok || position+RecordFixed > len(message) {
		return 0, false
	}
	out.Type = binary.BigEndian.Uint16(message[position:])
	out.Class = binary.BigEndian.Uint16(message[position+2:])
	out.TTL = binary.BigEndian.Uint32(message[position+4:])
	out.Length = int(binary.BigEndian.Uint16(message[position+8:]))
	out.Data = position + RecordFixed
	if out.Length > len(message)-out.Data {
		return 0, false
	}
	return out.Data + out.Length, true
}

// RecordTTL applies RFC 2181: a TTL with the top bit set means zero.
func RecordTTL(ttl uint32) uint32 {
	if ttl&ttlSignBit != 0 {
		return 0
	}
	return ttl
}

// ParseResponse validates a whole message of at most limit bytes against
// the outstanding query for question and qtype before anything in it is
// used. The ID and question are checked first, so a stray datagram is a
// mismatch, not a protocol error. A truncated message is reported without
// reading its records, which may be cut short, but only once its question
// matched. Bytes after the counted records are ignored: some servers pad
// replies and nothing there is read.
//
// Record data is checked for A, CNAME and SOA, the types the resolver reads;
// a caller that reads other types checks them with ValidRData.
//
// One concession: a FORMERR without a question section is accepted when the
// ID matches, since servers that reject EDNS(0) often send exactly that.
func ParseResponse(message []byte, limit int, id uint16, question *Name, qtype uint16, scratch *Name, out *Response) Result {
	*out = Response{}
	if len(message) < HeaderSize || len(message) > limit {
		return ParseMalformed
	}
	if binary.BigEndian.Uint16(message[0:2]) != id {
		return ParseMismatched
	}
	flags := binary.BigEndian.Uint16(message[2:4])
	if flags&FlagQR == 0 || flags&OpcodeMask != 0 {
		return ParseMalformed
	}
	out.Flags = flags
	out.Rcode = flags & RcodeMask
	questions := int(binary.BigEndian.Uint16(message[4:6]))
	answers := int(binary.BigEndian.Uint16(message[6:8]))
	authorities := int(binary.BigEndian.Uint16(message[8:10]))
	additionals := int(binary.BigEndian.Uint16(message[10:12]))
	position := HeaderSize
	switch {
	case questions == 0 && out.Rcode == RcodeFormErr:
		if flags&FlagTC != 0 {
			return ParseMalformed
		}
	case questions != 1:
		return ParseMismatched
	default:
		next, ok := DecodeName(message, position, scratch)
		if !ok || next+QuestionFixed > len(message) {
			return ParseMalformed
		}
		if !scratch.Equal(question) || binary.BigEndian.Uint16(message[next:]) != qtype ||
			binary.BigEndian.Uint16(message[next+2:]) != ClassIN {
			return ParseMismatched
		}
		position = next + QuestionFixed
	}
	out.Questions = questions
	if flags&FlagTC != 0 {
		out.Truncated = true
		return ParseOK
	}
	if (answers+authorities+additionals)*MinRecordSize > len(message)-position {
		return ParseMalformed
	}
	out.AnswerAt, out.Answers = position, answers
	out.AuthorityAt, out.Authorities = position, authorities
	out.AdditionalAt, out.Additionals = position, additionals
	var item Record
	for index := 0; index < answers+authorities+additionals; index++ {
		if index == answers {
			out.AuthorityAt = position
		}
		if index == answers+authorities {
			out.AdditionalAt = position
		}
		next, ok := ReadRecord(message, position, scratch, &item)
		if !ok || !validRecord(message, &item, scratch, index >= answers+authorities) {
			return ParseMalformed
		}
		if item.Type == TypeOPT {
			if out.HasOPT || scratch.size != 1 {
				return ParseMalformed
			}
			out.HasOPT = true
			out.Rcode |= uint16(item.TTL>>24) << 4
			out.EDNSPayload = item.Class
			out.EDNSVersion = uint8(item.TTL >> 16)
			out.EDNSDO = item.TTL&optDO != 0
		}
		position = next
	}
	if authorities == 0 {
		out.AuthorityAt = position
	}
	if additionals == 0 {
		out.AdditionalAt = position
	}
	return ParseOK
}

// validRecord checks an OPT record's place and the data of the types the
// resolver reads. Other types are left to ValidRData; ReadRecord already
// checked that their data fits.
func validRecord(message []byte, item *Record, scratch *Name, additional bool) bool {
	switch item.Type {
	case TypeOPT:
		return additional
	case TypeA, TypeCNAME, TypeSOA:
		return ValidRData(message, item, scratch)
	}
	return true
}

// ValidRData checks that an IN record of a type this package formats holds
// exactly the data its type defines. Records of other classes and types are
// opaque and always valid.
func ValidRData(message []byte, item *Record, scratch *Name) bool {
	if item.Class != ClassIN {
		return true
	}
	end := item.Data + item.Length
	switch item.Type {
	case TypeA:
		return item.Length == 4
	case TypeAAAA:
		return item.Length == 16
	case TypeNS, TypeCNAME, TypePTR:
		next, ok := DecodeName(message, item.Data, scratch)
		return ok && next == end
	case TypeMX:
		if item.Length < 3 {
			return false
		}
		next, ok := DecodeName(message, item.Data+2, scratch)
		return ok && next == end
	case TypeSOA:
		next, ok := DecodeName(message, item.Data, scratch)
		if !ok {
			return false
		}
		next, ok = DecodeName(message, next, scratch)
		return ok && next+SOAFixed == end
	case TypeTXT:
		if item.Length == 0 {
			return false
		}
		position := item.Data
		for position < end {
			position += 1 + int(message[position])
		}
		return position == end
	}
	return true
}
