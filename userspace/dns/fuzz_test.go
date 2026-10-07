package dns

import (
	"encoding/binary"
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
		var out wireName
		end, ok := decodeName(message, int(offset), &out)
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
			if length > 63 {
				t.Fatalf("label %d", length)
			}
			position += 1 + length
		}
		if position+1 != int(out.size) {
			t.Fatalf("labels end at %d, size %d", position, out.size)
		}
	})
}

// FuzzParseResponse checks the parser alone, then feeds the same bytes to a
// resolver with the ID patched in, so the interpretation and cache paths see
// messages that pass the ID check.
func FuzzParseResponse(f *testing.F) {
	question := "host.test"
	seeds := []reply{
		{question: question, answers: []rr{aRecord(question, 60, addr(1))}},
		{question: question, answers: []rr{cnameRecord(question, "b.test", 60), aRecord("b.test", 60, addr(2))}},
		{question: question, answers: []rr{cnameRecord(question, "b.test", 60)}},
		{question: question, flags: rcodeNXDomain, authority: []rr{soaRecord("test", 60, 30)}},
		{question: question, authority: []rr{soaRecord("test", 60, 30)}, opt: true},
		{question: question, flags: rcodeFormErr, noQuestion: true},
		{question: question, flags: flagTC},
		{question: question, flags: rcodeServFail},
	}
	for index, seed := range seeds {
		f.Add(byte(index), seed.bytes())
	}
	compressed := reply{question: question}.bytes()
	binary.BigEndian.PutUint16(compressed[6:], 1)
	compressed = append(compressed, 0xc0, 12)
	compressed = append(appendRecordHeader(compressed, typeA, 4), 1, 2, 3, 4)
	f.Add(byte(0), compressed)
	f.Fuzz(func(t *testing.T, mode byte, payload []byte) {
		var scratch wireName
		var asked wireName
		copy(asked.data[:], wire(question))
		asked.size = uint8(len(wire(question)))
		var out response
		parseResponse(payload, 0, &asked, &scratch, &out)

		h := newHarness(t, serverA, serverB)
		h.pending(0, question)
		for step := 0; step < 4; step++ {
			sent, ok := h.r.NextQuery(h.buf[:])
			if !ok {
				break
			}
			data := append([]byte(nil), payload...)
			if len(data) >= 2 && mode&1 == 0 {
				copy(data[:2], h.buf[:2])
			}
			h.r.Receive(sent.Transaction, sent.Server, data, h.now)
			if deadline, ok := h.r.Deadline(); ok && mode&2 != 0 {
				h.now = deadline
				h.r.Advance(h.now)
			}
		}
		h.r.Advance(h.now + 100*second)
		h.r.Completion()
		h.r.Lookup(1, name(t, "b.test"), h.now)
		h.r.Stats()
	})
}
