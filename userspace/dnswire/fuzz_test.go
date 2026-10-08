package dnswire

import (
	"testing"
)

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
