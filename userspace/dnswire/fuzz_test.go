package dnswire

import (
	"testing"
)

// FuzzParseQuery parses a client query and writes the reply the resolver's
// stub would send. A reply to a query whose question was read must parse as
// the response to that question, with the client's ID, and fit MaxReply.
func FuzzParseQuery(f *testing.F) {
	f.Add(query(1, FlagRD, "a.test", TypeA, ClassIN))
	f.Add(query(2, FlagRD, "Mixed.Case.test", TypeAAAA, ClassIN, opt(1232, 0)))
	f.Add(query(3, 0, "a.test", TypeMX, 3, opt(512, 1<<16), rr{name: "k", kind: 250, class: 255}))
	f.Add([]byte{0, 1, 0x28, 0, 0, 1, 0, 0, 0, 0, 0, 0})
	f.Fuzz(func(t *testing.T, payload []byte) {
		var out Query
		result := ParseQuery(payload, &out)
		if result == QueryIgnore {
			return
		}
		if result != QueryOK && result != QueryFormErr && result != QueryNotImp {
			t.Fatalf("result %d", result)
		}
		var reply [MaxReply]byte
		addresses := [][4]byte{{1, 2, 3, 4}, {5, 6, 7, 8}}
		for _, rcode := range []uint16{RcodeNoError, RcodeServFail, RcodeBadVers} {
			length := EncodeReply(reply[:], &out, rcode, 60, addresses)
			if length < HeaderSize || length > MaxReply {
				t.Fatalf("reply length %d", length)
			}
			if result != QueryOK || out.Class != ClassIN {
				continue
			}
			var response Response
			var scratch Name
			if got := ParseResponse(reply[:length], length, out.ID, &out.Name, out.Type, &scratch, &response); got != ParseOK {
				t.Fatalf("reply to % x does not parse: %d", payload, got)
			}
		}
	})
}

func FuzzDecodeName(f *testing.F) {
	f.Add(wire("www.example.com"), uint16(0))
	f.Add(append(wire("example.com"), 3, 'w', 'w', 'w', 0xc0, 0), uint16(13))
	f.Add([]byte{0xc0, 0}, uint16(0))
	f.Add([]byte{1, 'a', 0xc0, 0}, uint16(0))
	f.Add([]byte{0x40, 0}, uint16(0))
	f.Add([]byte{63}, uint16(0))
	f.Fuzz(func(t *testing.T, message []byte, offset uint16) {
		var out Name
		end, ok := DecodeName(message, int(offset), &out)
		if !ok {
			return
		}
		if end <= int(offset) || end > len(message) || out.size == 0 || out.data[out.size-1] != 0 {
			t.Fatalf("end %d size %d", end, out.size)
		}
		// The result is a well-formed wire name.
		position := 0
		for out.data[position] != 0 {
			length := int(out.data[position])
			if length > MaxLabel {
				t.Fatalf("label %d", length)
			}
			position += 1 + length
		}
		if position+1 != int(out.size) {
			t.Fatalf("labels end at %d, size %d", position, out.size)
		}
	})
}

// FuzzParseResponse validates a message, then checks and formats every
// record the way dnsq does. Formatting must never panic on a record that
// passed ValidRData, and its output must hold printable ASCII only.
func FuzzParseResponse(f *testing.F) {
	seeds := []message{
		{id: testID, name: "a.test", answers: []rr{{name: "a.test", kind: TypeA, data: []byte{1, 2, 3, 4}}}},
		{id: testID, name: "a.test", answers: []rr{{name: "a.test", kind: TypeTXT, data: []byte{2, 0x1b, 'x'}}}},
		{id: testID, name: "a.test", answers: []rr{{name: "a.test", kind: TypeMX, data: append(u16(5), 0xc0, 12)}}},
		{id: testID, name: "a.test", answers: []rr{{name: "a.test", kind: TypeAAAA, data: make([]byte, 16)}}},
		{id: testID, name: "a.test", authority: []rr{{name: "test", kind: TypeSOA,
			data: append(append(wire("ns"), wire("h")...), make([]byte, SOAFixed)...)}}, extra: []rr{opt(1232, 0)}},
		{id: testID, flags: FlagTC, name: "a.test"},
		{id: testID, flags: RcodeFormErr, noQuestion: true},
	}
	for _, seed := range seeds {
		f.Add(seed.bytes())
	}
	f.Fuzz(func(t *testing.T, payload []byte) {
		var question, scratch, owner Name
		question.SetText([]byte("a.test"))
		var out Response
		if ParseResponse(payload, len(payload), testID, &question, TypeA, &scratch, &out) != ParseOK || out.Truncated {
			return
		}
		position := out.AnswerAt
		var item Record
		for index := 0; index < out.Answers+out.Authorities+out.Additionals; index++ {
			next, ok := ReadRecord(payload, position, &owner, &item)
			if !ok {
				t.Fatalf("record %d unreadable after ParseOK", index)
			}
			position = next
			if !ValidRData(payload, &item, &scratch) {
				continue
			}
			var text text
			FormatName(&text, &owner)
			FormatRData(&text, payload, &item, &scratch)
			for _, character := range []byte(text.String()) {
				if character < ' ' || character > '~' {
					t.Fatalf("raw byte %#x in %q", character, text.String())
				}
			}
		}
	})
}
