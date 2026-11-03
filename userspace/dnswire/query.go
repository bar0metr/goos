package dnswire

import "encoding/binary"

// The server side of the format: what the resolver's local stub reads from
// a client and writes back. A query is checked as strictly as a response,
// and a reply carries only what the stub can vouch for: the client's ID and
// question, A records for the queried name, and an OPT record when the
// query had one.

const (
	RcodeNotImp  = 4
	RcodeBadVers = 16

	// MaxReplyAddresses bounds the A records one reply carries.
	MaxReplyAddresses = 8
	// answerSize is one A record with its owner compressed to the question.
	answerSize = 2 + RecordFixed + 4
	// MaxReply is the longest reply EncodeReply writes, 410 bytes. It is
	// below the 512 bytes a client without EDNS(0) takes over UDP, so a
	// reply never needs TC=1.
	MaxReply = HeaderSize + MaxName + QuestionFixed + MaxReplyAddresses*answerSize + OPTSize

	// questionOffset is where the question starts; a reply's answers point
	// back to it.
	questionOffset = HeaderSize
)

// QueryResult is what ParseQuery made of a message.
type QueryResult uint8

const (
	QueryOK QueryResult = iota
	// QueryIgnore is a message that gets no reply at all: shorter than a
	// header, or a response. Answering a response could start a loop.
	QueryIgnore
	// QueryFormErr is a query whose header was read and whose body breaks
	// the protocol. Question tells whether the question was read.
	QueryFormErr
	// QueryNotImp is a query with an opcode other than QUERY.
	QueryNotImp
)

// Query is one parsed query. Name is the question folded to lower case for
// lookups; the reply repeats the question exactly as the client sent it.
type Query struct {
	ID    uint16
	Flags uint16
	Name  Name
	Type  uint16
	Class uint16
	// Question reports that the question was read; sent and sentSize hold
	// it in wire form as received.
	Question bool
	sent     [MaxName]byte
	sentSize uint8

	HasOPT      bool
	EDNSPayload uint16
	EDNSVersion uint8
}

// ParseQuery validates a query of at most 65535 bytes. A query has QR clear,
// opcode QUERY, exactly one question, no answer or authority records,
// and at most one OPT record, in the additional section and owned by the
// root. The question name may not use compression: in a message with
// nothing before it there is nothing to point at. Additional records other
// than OPT are walked and ignored. Bytes after the counted records are
// ignored, as ParseResponse does.
func ParseQuery(message []byte, out *Query) QueryResult {
	out.ID, out.Flags, out.Type, out.Class = 0, 0, 0, 0
	out.Question, out.sentSize = false, 0
	out.HasOPT, out.EDNSPayload, out.EDNSVersion = false, 0, 0
	if len(message) < HeaderSize || len(message) > 0xffff {
		return QueryIgnore
	}
	out.ID = binary.BigEndian.Uint16(message[0:2])
	out.Flags = binary.BigEndian.Uint16(message[2:4])
	if out.Flags&FlagQR != 0 {
		return QueryIgnore
	}
	if out.Flags&OpcodeMask != 0 {
		return QueryNotImp
	}
	questions := binary.BigEndian.Uint16(message[4:6])
	answers := binary.BigEndian.Uint16(message[6:8])
	authorities := binary.BigEndian.Uint16(message[8:10])
	additionals := int(binary.BigEndian.Uint16(message[10:12]))
	if questions != 1 || answers != 0 || authorities != 0 {
		return QueryFormErr
	}
	next, ok := DecodeName(message, questionOffset, &out.Name)
	if !ok || next-questionOffset != int(out.Name.size) || next+QuestionFixed > len(message) {
		return QueryFormErr
	}
	out.sentSize = uint8(copy(out.sent[:], message[questionOffset:next]))
	out.Type = binary.BigEndian.Uint16(message[next:])
	out.Class = binary.BigEndian.Uint16(message[next+2:])
	out.Question = true
	position := next + QuestionFixed
	if additionals*MinRecordSize > len(message)-position {
		return QueryFormErr
	}
	var owner Name
	var item Record
	for index := 0; index < additionals; index++ {
		if position, ok = ReadRecord(message, position, &owner, &item); !ok {
			return QueryFormErr
		}
		if item.Type != TypeOPT {
			continue
		}
		if out.HasOPT || owner.size != 1 {
			return QueryFormErr
		}
		out.HasOPT = true
		out.EDNSPayload = item.Class
		out.EDNSVersion = uint8(item.TTL >> 16)
	}
	return QueryOK
}

// EncodeReply writes the reply to query with rcode, which may be an extended
// code when the query had OPT, and returns its length. A reply to a query
// whose question was read repeats it; a NOERROR reply to an A question
// carries one record per address, at most MaxReplyAddresses, each with ttl.
// RD and CD are copied from the query and RA is set. With OPT in the query
// the reply carries OPT advertising EDNSPayload, version 0. dst must hold
// MaxReply bytes.
func EncodeReply(dst []byte, query *Query, rcode uint16, ttl uint32, addresses [][4]byte) int {
	if !query.HasOPT {
		rcode &= RcodeMask
	}
	flags := FlagQR | FlagRA | query.Flags&(OpcodeMask|FlagRD|FlagCD) | rcode&RcodeMask
	binary.BigEndian.PutUint16(dst[0:2], query.ID)
	binary.BigEndian.PutUint16(dst[2:4], flags)
	binary.BigEndian.PutUint16(dst[4:6], 0)
	binary.BigEndian.PutUint16(dst[6:8], 0)
	binary.BigEndian.PutUint16(dst[8:10], 0)
	binary.BigEndian.PutUint16(dst[10:12], 0)
	length := HeaderSize
	if query.Question {
		binary.BigEndian.PutUint16(dst[4:6], 1)
		length += copy(dst[length:], query.sent[:query.sentSize])
		binary.BigEndian.PutUint16(dst[length:], query.Type)
		binary.BigEndian.PutUint16(dst[length+2:], query.Class)
		length += QuestionFixed
		if rcode == RcodeNoError && query.Type == TypeA && query.Class == ClassIN {
			count := min(len(addresses), MaxReplyAddresses)
			binary.BigEndian.PutUint16(dst[6:8], uint16(count))
			for index := 0; index < count; index++ {
				binary.BigEndian.PutUint16(dst[length:], uint16(pointerMask)<<8|questionOffset)
				binary.BigEndian.PutUint16(dst[length+2:], TypeA)
				binary.BigEndian.PutUint16(dst[length+4:], ClassIN)
				binary.BigEndian.PutUint32(dst[length+6:], ttl&^ttlSignBit)
				binary.BigEndian.PutUint16(dst[length+10:], 4)
				copy(dst[length+12:length+16], addresses[index][:])
				length += answerSize
			}
		}
	}
	if !query.HasOPT {
		return length
	}
	binary.BigEndian.PutUint16(dst[10:12], 1)
	dst[length] = 0
	binary.BigEndian.PutUint16(dst[length+1:], TypeOPT)
	binary.BigEndian.PutUint16(dst[length+3:], EDNSPayload)
	binary.BigEndian.PutUint32(dst[length+5:], uint32(rcode>>4)<<24)
	binary.BigEndian.PutUint16(dst[length+9:], 0)
	return length + OPTSize
}
