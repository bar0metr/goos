package dnswire

import (
	"bytes"
	"encoding/binary"
	"testing"
)

const testID = 0x9906

func parse(t *testing.T, payload []byte, name string, qtype uint16) (Result, Response) {
	t.Helper()
	var scratch Name
	var out Response
	result := ParseResponse(payload, 65535, testID, named(t, name), qtype, &scratch, &out)
	return result, out
}

func TestParseResponseSections(t *testing.T) {
	payload := message{id: testID, flags: FlagAA, name: "a.test", qtype: TypeMX,
		answers:   []rr{{name: "a.test", kind: TypeMX, ttl: 60, data: append(u16(10), wire("mail.test")...)}},
		authority: []rr{{name: "test", kind: TypeNS, ttl: 60, data: wire("ns.test")}},
		extra:     []rr{{name: "mail.test", kind: TypeA, ttl: 60, data: []byte{192, 0, 2, 25}}, opt(1232, 0x0000_8000)},
	}.bytes()
	result, out := parse(t, payload, "A.Test.", TypeMX)
	if result != ParseOK || out.Truncated || out.Questions != 1 || out.Answers != 1 || out.Authorities != 1 ||
		out.Additionals != 2 || !out.HasOPT || out.EDNSPayload != 1232 || out.EDNSVersion != 0 || !out.EDNSDO ||
		out.Flags&FlagAA == 0 || out.Rcode != RcodeNoError {
		t.Fatalf("%v %+v", result, out)
	}
	// Walking each section from its offset lands on the next one.
	var owner Name
	var item Record
	next, _ := ReadRecord(payload, out.AnswerAt, &owner, &item)
	if next != out.AuthorityAt || item.Type != TypeMX || item.TTL != 60 {
		t.Fatalf("answer %+v next %d", item, next)
	}
	next, _ = ReadRecord(payload, out.AuthorityAt, &owner, &item)
	if next != out.AdditionalAt || item.Type != TypeNS {
		t.Fatalf("authority %+v", item)
	}
}

func TestParseResponseEmptySections(t *testing.T) {
	payload := message{id: testID, flags: RcodeNXDomain, name: "gone.test"}.bytes()
	result, out := parse(t, payload, "gone.test", TypeA)
	if result != ParseOK || out.Rcode != RcodeNXDomain || out.AnswerAt != len(payload) ||
		out.AuthorityAt != len(payload) || out.AdditionalAt != len(payload) {
		t.Fatalf("%v %+v", result, out)
	}
}

func TestParseResponseExtendedRcode(t *testing.T) {
	payload := message{id: testID, name: "a.test", extra: []rr{opt(512, 1<<24)}}.bytes()
	if result, out := parse(t, payload, "a.test", TypeA); result != ParseOK || out.Rcode != 16 {
		t.Fatalf("%v %+v", result, out)
	}
}

func TestParseResponseMismatch(t *testing.T) {
	good := message{id: testID, name: "a.test", qtype: TypeTXT}
	cases := map[string]func() []byte{
		"id":       func() []byte { m := good; m.id++; return m.bytes() },
		"name":     func() []byte { m := good; m.name = "b.test"; return m.bytes() },
		"type":     func() []byte { m := good; m.qtype = TypeA; return m.bytes() },
		"class":    func() []byte { p := good.bytes(); p[len(p)-1] = 3; return p },
		"two":      func() []byte { p := good.bytes(); p[5] = 2; return p },
		"nothing":  func() []byte { m := good; m.noQuestion = true; return m.bytes() },
		"servfail": func() []byte { m := good; m.noQuestion, m.flags = true, RcodeServFail; return m.bytes() },
	}
	for what, build := range cases {
		if result, _ := parse(t, build(), "a.test", TypeTXT); result != ParseMismatched {
			t.Errorf("%s: %v", what, result)
		}
	}
	// A FORMERR without a question is the one accepted exception.
	formerr := message{id: testID, flags: RcodeFormErr, noQuestion: true}.bytes()
	if result, out := parse(t, formerr, "a.test", TypeTXT); result != ParseOK || out.Questions != 0 {
		t.Fatalf("FORMERR: %v", result)
	}
}

func TestParseResponseMalformed(t *testing.T) {
	good := message{id: testID, name: "a.test", answers: []rr{{name: "a.test", kind: TypeA, ttl: 1, data: []byte{1, 2, 3, 4}}}}
	cases := map[string]func() []byte{
		"short":      func() []byte { return good.bytes()[:HeaderSize-1] },
		"query":      func() []byte { p := good.bytes(); p[2] &^= FlagQR >> 8; return p },
		"opcode":     func() []byte { p := good.bytes(); p[2] |= 0x10; return p },
		"cut record": func() []byte { p := good.bytes(); return p[:len(p)-1] },
		"rdlength":   func() []byte { p := good.bytes(); p[len(p)-5] = 9; return p },
		"count":      func() []byte { p := good.bytes(); p[7] = 2; return p },
		"bad A": func() []byte {
			m := good
			m.answers = []rr{{name: "a.test", kind: TypeA, data: []byte{1}}}
			return m.bytes()
		},
		"bad CNAME": func() []byte {
			m := good
			m.answers = []rr{{name: "a.test", kind: TypeCNAME, data: []byte{1, 'x'}}}
			return m.bytes()
		},
		"CNAME slack": func() []byte {
			m := good
			m.answers = []rr{{name: "a.test", kind: TypeCNAME, data: append(wire("x"), 0)}}
			return m.bytes()
		},
		"bad SOA": func() []byte {
			m := good
			m.authority = []rr{{name: "test", kind: TypeSOA, data: wire("ns")}}
			return m.bytes()
		},
		"OPT answer":   func() []byte { m := good; m.answers = []rr{opt(512, 0)}; return m.bytes() },
		"two OPT":      func() []byte { m := good; m.extra = []rr{opt(512, 0), opt(512, 0)}; return m.bytes() },
		"OPT owner":    func() []byte { m := good; o := opt(512, 0); o.name = "a"; m.extra = []rr{o}; return m.bytes() },
		"question cut": func() []byte { return good.bytes()[:HeaderSize+len(wire("a.test"))+2] },
		"TC FORMERR": func() []byte {
			return message{id: testID, flags: RcodeFormErr | FlagTC, noQuestion: true}.bytes()
		},
	}
	for what, build := range cases {
		if result, _ := parse(t, build(), "a.test", TypeA); result != ParseMalformed {
			t.Errorf("%s: %v", what, result)
		}
	}
	if result, _ := parse(t, good.bytes(), "a.test", TypeA); result != ParseOK {
		t.Fatal(result)
	}
	var scratch Name
	var out Response
	if ParseResponse(good.bytes(), 20, testID, named(t, "a.test"), TypeA, &scratch, &out) != ParseMalformed {
		t.Fatal("over limit")
	}
}

// Only the question is read from a truncated reply: its records may be cut.
func TestParseResponseTruncated(t *testing.T) {
	payload := message{id: testID, flags: FlagTC, name: "big.test"}.bytes()
	binary.BigEndian.PutUint16(payload[6:], 40)
	result, out := parse(t, payload, "big.test", TypeA)
	if result != ParseOK || !out.Truncated || out.Answers != 0 {
		t.Fatalf("%v %+v", result, out)
	}
}

// Records the resolver does not read are kept opaque by ParseResponse but
// checked by ValidRData.
func TestValidRData(t *testing.T) {
	good := map[uint16][]byte{
		TypeA:     {192, 0, 2, 1},
		TypeAAAA:  make([]byte, 16),
		TypeNS:    wire("ns.test"),
		TypeCNAME: wire("x.test"),
		TypePTR:   wire("host.test"),
		TypeMX:    append(u16(10), wire("mail.test")...),
		TypeTXT:   {5, 'h', 'e', 'l', 'l', 'o', 0},
		TypeSOA:   append(append(wire("ns.test"), wire("root.test")...), make([]byte, SOAFixed)...),
		65:        {1, 2, 3},
		99:        nil,
	}
	bad := map[string]rr{
		"A":         {kind: TypeA, data: []byte{1, 2, 3}},
		"AAAA":      {kind: TypeAAAA, data: make([]byte, 15)},
		"NS slack":  {kind: TypeNS, data: append(wire("ns"), 0)},
		"PTR cut":   {kind: TypePTR, data: []byte{3, 'a'}},
		"MX short":  {kind: TypeMX, data: []byte{0, 1}},
		"MX name":   {kind: TypeMX, data: []byte{0, 1, 5, 'a'}},
		"TXT empty": {kind: TypeTXT},
		"TXT cut":   {kind: TypeTXT, data: []byte{4, 'a'}},
		"SOA short": {kind: TypeSOA, data: append(append(wire("a"), wire("b")...), make([]byte, SOAFixed-1)...)},
	}
	var scratch Name
	for kind, data := range good {
		payload := message{id: testID, name: "a.test", answers: []rr{{name: "a.test", kind: kind, data: data}}}.bytes()
		result, out := parse(t, payload, "a.test", TypeA)
		var item Record
		ReadRecord(payload, out.AnswerAt, &scratch, &item)
		if result != ParseOK || !ValidRData(payload, &item, &scratch) {
			t.Errorf("type %d: %v", kind, result)
		}
	}
	for what, record := range bad {
		record.name = "a.test"
		payload := message{id: testID, name: "a.test", answers: []rr{record}}.bytes()
		var scratch Name
		var item Record
		ReadRecord(payload, HeaderSize+len(wire("a.test"))+QuestionFixed, &scratch, &item)
		if ValidRData(payload, &item, &scratch) {
			t.Errorf("%s accepted", what)
		}
	}
	// Another class is opaque.
	record := Record{Type: TypeA, Class: 3, Data: 0, Length: 1}
	if !ValidRData([]byte{1}, &record, &scratch) {
		t.Fatal("CH A refused")
	}
}

func format(t *testing.T, record rr) string {
	t.Helper()
	record.name = "a.test"
	payload := message{id: testID, name: "a.test", answers: []rr{record}}.bytes()
	var scratch Name
	var item Record
	ReadRecord(payload, HeaderSize+len(wire("a.test"))+QuestionFixed, &scratch, &item)
	if !ValidRData(payload, &item, &scratch) {
		t.Fatalf("invalid %+v", record)
	}
	var out text
	FormatRData(&out, payload, &item, &scratch)
	return out.String()
}

func TestFormatRData(t *testing.T) {
	soa := append(append(wire("ns.test"), wire("hostmaster.test")...),
		0, 0, 0, 7, 0, 0, 0x0e, 0x10, 0, 0, 0x02, 0x58, 0, 0x09, 0x3a, 0x80, 0, 0, 0, 60)
	cases := []struct {
		record rr
		want   string
	}{
		{rr{kind: TypeA, data: []byte{192, 0, 2, 80}}, "192.0.2.80"},
		{rr{kind: TypeA, data: []byte{0, 0, 0, 0}}, "0.0.0.0"},
		{rr{kind: TypeAAAA, data: []byte{0x20, 0x01, 0x0d, 0xb8, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1}}, "2001:db8::1"},
		{rr{kind: TypeAAAA, data: make([]byte, 16)}, "::"},
		{rr{kind: TypeAAAA, data: append(make([]byte, 15), 1)}, "::1"},
		{rr{kind: TypeAAAA, data: []byte{0x20, 0x01, 0x0d, 0xb8, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0}}, "2001:db8:0:1::"},
		{rr{kind: TypeAAAA, data: []byte{0x20, 0x01, 0x0d, 0xb8, 0, 0, 0, 1, 0, 1, 0, 1, 0, 1, 0, 1}}, "2001:db8:0:1:1:1:1:1"},
		{rr{kind: TypeAAAA, data: []byte{0x20, 0x01, 0, 0, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0xab, 0xcd}}, "2001:0:0:1::abcd"},
		{rr{kind: TypeAAAA, data: []byte{0xfe, 0x80, 0, 0, 0, 0, 0, 0, 0x0a, 0xbc, 0, 0, 0, 0, 0, 0}}, "fe80::abc:0:0:0"},
		{rr{kind: TypeCNAME, data: wire("Host.Example.Test")}, "host.example.test."},
		{rr{kind: TypeNS, data: wire("ns1.test")}, "ns1.test."},
		{rr{kind: TypePTR, data: wire("host.test")}, "host.test."},
		{rr{kind: TypeCNAME, data: wire(".")}, "."},
		{rr{kind: TypeMX, data: append(u16(10), wire("mail.test")...)}, "10 mail.test."},
		{rr{kind: TypeTXT, data: []byte{5, 'h', 'e', 'l', 'l', 'o'}}, `"hello"`},
		{rr{kind: TypeTXT, data: []byte{1, 'a', 0, 3, 'b', ' ', 'c'}}, `"a" "" "b c"`},
		{rr{kind: TypeSOA, data: soa}, "ns.test. hostmaster.test. 7 3600 600 604800 60"},
		{rr{kind: 65, data: []byte{0xde, 0xad, 0xbe, 0xef}}, `\# 4 deadbeef`},
		{rr{kind: 99}, `\# 0`},
		{rr{kind: TypeA, class: 3, data: []byte{1, 2}}, `\# 2 0102`},
	}
	for _, c := range cases {
		if got := format(t, c.record); got != c.want {
			t.Errorf("type %d: %q, want %q", c.record.kind, got, c.want)
		}
	}
	long := format(t, rr{kind: 65, data: bytes.Repeat([]byte{0xab}, 100)})
	if long != `\# 100 `+string(bytes.Repeat([]byte("ab"), MaxGenericData))+" ..." {
		t.Fatalf("%q", long)
	}
}

// Nothing a server puts in a TXT string or a name reaches the terminal as a
// control byte.
func TestFormatEscapes(t *testing.T) {
	txt := []byte{11, 0x1b, '[', '2', 'J', '"', '\\', '\n', 0x7f, 0x9b, 0xff, ' '}
	got := format(t, rr{kind: TypeTXT, data: txt})
	if want := `"\027[2J\"\\\010\127\155\255 "`; got != want {
		t.Fatalf("%q, want %q", got, want)
	}
	name := []byte{6, 'a', '.', 'b', 0x1b, ' ', '\\', 4, 't', 'e', 's', 't', 0}
	got = format(t, rr{kind: TypePTR, data: name})
	if want := `a\.b\027\032\\.test.`; got != want {
		t.Fatalf("%q, want %q", got, want)
	}
	for _, character := range []byte(got) {
		if character < ' ' || character > '~' {
			t.Fatalf("raw byte %#x", character)
		}
	}
}

func TestFormatNames(t *testing.T) {
	var out text
	FormatType(&out, TypeAAAA)
	out.Byte(' ')
	FormatType(&out, 65)
	out.Byte(' ')
	FormatClass(&out, ClassIN)
	out.Byte(' ')
	FormatClass(&out, 254)
	out.Byte(' ')
	FormatRcode(&out, RcodeNXDomain)
	out.Byte(' ')
	FormatRcode(&out, RcodeRefused)
	out.Byte(' ')
	FormatRcode(&out, 23)
	out.Byte(' ')
	FormatDecimal(&out, 0)
	out.Byte(' ')
	FormatDecimal(&out, 18446744073709551615)
	if want := "AAAA TYPE65 IN CLASS254 NXDOMAIN REFUSED RCODE23 0 18446744073709551615"; out.String() != want {
		t.Fatalf("%q", out.String())
	}
}
