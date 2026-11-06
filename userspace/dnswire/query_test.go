package dnswire

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// query builds a client query: flags, one question unless noQuestion, and
// the given additional records.
func query(id, flags uint16, name string, qtype, class uint16, extra ...rr) []byte {
	out := make([]byte, HeaderSize)
	binary.BigEndian.PutUint16(out[0:], id)
	binary.BigEndian.PutUint16(out[2:], flags)
	binary.BigEndian.PutUint16(out[4:], 1)
	binary.BigEndian.PutUint16(out[10:], uint16(len(extra)))
	out = append(out, wire(name)...)
	out = binary.BigEndian.AppendUint16(out, qtype)
	out = binary.BigEndian.AppendUint16(out, class)
	for _, record := range extra {
		out = append(out, wire(record.name)...)
		out = binary.BigEndian.AppendUint16(out, record.kind)
		out = binary.BigEndian.AppendUint16(out, record.class)
		out = binary.BigEndian.AppendUint32(out, record.ttl)
		out = binary.BigEndian.AppendUint16(out, uint16(len(record.data)))
		out = append(out, record.data...)
	}
	return out
}

func TestParseQueryKeepsTheQuestionAsSent(t *testing.T) {
	payload := query(0x1234, FlagRD|FlagCD, "WwW.Example.TEST", TypeA, ClassIN, opt(4096, 0x0000_8000))
	var out Query
	if result := ParseQuery(payload, &out); result != QueryOK {
		t.Fatalf("result %d", result)
	}
	if out.ID != 0x1234 || out.Type != TypeA || out.Class != ClassIN || !out.Question || !out.HasOPT ||
		out.EDNSPayload != 4096 || out.EDNSVersion != 0 || !out.Name.Equal(named(t, "www.example.test")) {
		t.Fatalf("%+v", out)
	}
	var reply [MaxReply]byte
	length := EncodeReply(reply[:], &out, RcodeNoError, 300, [][4]byte{{192, 0, 2, 1}, {192, 0, 2, 2}})
	// The question comes back byte for byte, case included.
	if !bytes.Equal(reply[HeaderSize:HeaderSize+len(wire("WwW.Example.TEST"))], wire("WwW.Example.TEST")) {
		t.Fatalf("question % x", reply[HeaderSize:length])
	}
	var response Response
	var scratch Name
	if result := ParseResponse(reply[:length], length, 0x1234, &out.Name, TypeA, &scratch, &response); result != ParseOK {
		t.Fatalf("reply does not parse: %d", result)
	}
	if response.Flags&(FlagQR|FlagRD|FlagRA|FlagCD) != FlagQR|FlagRD|FlagRA|FlagCD || response.Rcode != RcodeNoError ||
		response.Answers != 2 || !response.HasOPT || response.EDNSPayload != EDNSPayload || response.EDNSDO {
		t.Fatalf("%+v", response)
	}
	var owner Name
	var item Record
	next, _ := ReadRecord(reply[:length], response.AnswerAt, &owner, &item)
	if item.Type != TypeA || item.TTL != 300 || !owner.Equal(&out.Name) || !bytes.Equal(reply[item.Data:item.Data+4], []byte{192, 0, 2, 1}) {
		t.Fatalf("first answer %+v", item)
	}
	ReadRecord(reply[:length], next, &owner, &item)
	if !bytes.Equal(reply[item.Data:item.Data+4], []byte{192, 0, 2, 2}) {
		t.Fatalf("second answer %+v", item)
	}
}

func TestParseQueryRefusals(t *testing.T) {
	plain := query(7, FlagRD, "a.test", TypeA, ClassIN)
	cases := []struct {
		name    string
		payload []byte
		want    QueryResult
	}{
		{"short", plain[:HeaderSize-1], QueryIgnore},
		{"response", func() []byte { p := bytes.Clone(plain); p[2] |= 0x80; return p }(), QueryIgnore},
		{"opcode", func() []byte { p := bytes.Clone(plain); p[2] |= 0x10; return p }(), QueryNotImp},
		{"no question", func() []byte { p := bytes.Clone(plain[:HeaderSize]); return p }(), QueryFormErr},
		{"two questions", func() []byte { p := bytes.Clone(plain); p[5] = 2; return p }(), QueryFormErr},
		{"answer", func() []byte { p := bytes.Clone(plain); p[7] = 1; return p }(), QueryFormErr},
		{"authority", func() []byte { p := bytes.Clone(plain); p[9] = 1; return p }(), QueryFormErr},
		{"cut question", plain[:len(plain)-1], QueryFormErr},
		{"compressed question", func() []byte {
			p := bytes.Clone(plain[:HeaderSize])
			return append(p, 1, 'a', 0xc0, 0, 0, 1, 0, 1)
		}(), QueryFormErr},
		{"missing additional", func() []byte { p := bytes.Clone(plain); p[11] = 1; return p }(), QueryFormErr},
		{"two OPT", query(7, 0, "a.test", TypeA, ClassIN, opt(1232, 0), opt(1232, 0)), QueryFormErr},
		{"OPT not at root", query(7, 0, "a.test", TypeA, ClassIN, rr{name: "x", kind: TypeOPT, class: 1232}), QueryFormErr},
	}
	for _, test := range cases {
		var out Query
		if got := ParseQuery(test.payload, &out); got != test.want {
			t.Errorf("%s: result %d, want %d", test.name, got, test.want)
		}
	}
	// Other additional records and trailing bytes are ignored.
	other := query(7, 0, "a.test", TypeA, ClassIN, rr{name: "key", kind: 250, class: 255, data: []byte{1, 2}})
	var out Query
	if got := ParseQuery(append(other, 0, 0, 0), &out); got != QueryOK || out.HasOPT {
		t.Fatalf("result %d %+v", got, out)
	}
}

func TestEncodeReplyForErrorsAndOtherTypes(t *testing.T) {
	var out Query
	ParseQuery(query(9, FlagRD, "a.test", TypeAAAA, ClassIN), &out)
	var reply [MaxReply]byte
	// No A records go into an AAAA reply, or into a reply that is not
	// NOERROR.
	for _, test := range []struct {
		qtype uint16
		rcode uint16
	}{{TypeAAAA, RcodeNoError}, {TypeA, RcodeNXDomain}, {TypeA, RcodeServFail}} {
		out.Type = test.qtype
		length := EncodeReply(reply[:], &out, test.rcode, 60, [][4]byte{{1, 2, 3, 4}})
		if length != HeaderSize+len(wire("a.test"))+QuestionFixed || binary.BigEndian.Uint16(reply[6:]) != 0 ||
			binary.BigEndian.Uint16(reply[2:])&RcodeMask != test.rcode || binary.BigEndian.Uint16(reply[10:]) != 0 {
			t.Fatalf("type %d rcode %d: % x", test.qtype, test.rcode, reply[:length])
		}
	}
	// A query whose question was not read gets a bare header.
	var broken Query
	ParseQuery(query(10, FlagRD|0x2800, "a.test", TypeA, ClassIN), &broken)
	length := EncodeReply(reply[:], &broken, RcodeNotImp, 0, nil)
	if length != HeaderSize || binary.BigEndian.Uint16(reply[0:]) != 10 ||
		binary.BigEndian.Uint16(reply[2:]) != FlagQR|FlagRA|FlagRD|0x2800|RcodeNotImp || binary.BigEndian.Uint16(reply[4:]) != 0 {
		t.Fatalf("% x", reply[:length])
	}
	// An extended code travels in OPT.
	var versioned Query
	if ParseQuery(query(11, 0, "a.test", TypeA, ClassIN, opt(1232, 1<<16)), &versioned) != QueryOK || versioned.EDNSVersion != 1 {
		t.Fatalf("%+v", versioned)
	}
	length = EncodeReply(reply[:], &versioned, RcodeBadVers, 0, nil)
	var response Response
	var scratch Name
	if result := ParseResponse(reply[:length], length, 11, &versioned.Name, TypeA, &scratch, &response); result != ParseOK ||
		response.Rcode != RcodeBadVers || response.EDNSVersion != 0 {
		t.Fatalf("%d %+v", result, response)
	}
}

func TestEncodeReplyLongestFitsClassicUDP(t *testing.T) {
	long := ""
	for len(long) < 240 {
		long += "abcdefghijklmnopqrstuvwxyz0123456789abcdefghijklmnopqrstu."
	}
	long = long[:249]
	var out Query
	if ParseQuery(query(1, 0, long, TypeA, ClassIN, opt(1232, 0)), &out) != QueryOK {
		t.Fatal("long query refused")
	}
	addresses := make([][4]byte, MaxReplyAddresses+3)
	var reply [MaxReply]byte
	length := EncodeReply(reply[:], &out, RcodeNoError, 1, addresses)
	if binary.BigEndian.Uint16(reply[6:]) != MaxReplyAddresses || length > MaxReply || MaxReply > 512 {
		t.Fatalf("length %d of %d", length, MaxReply)
	}
}
