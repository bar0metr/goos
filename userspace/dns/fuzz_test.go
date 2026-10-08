package dns

import (
	"encoding/binary"
	"testing"

	"goos/userspace/dnswire"
)

// FuzzParseResponse checks the parser alone, then feeds the same bytes to a
// resolver with the ID patched in, so the interpretation and cache paths see
// messages that pass the ID check.
func FuzzParseResponse(f *testing.F) {
	question := "host.test"
	seeds := []reply{
		{question: question, answers: []rr{aRecord(question, 60, addr(1))}},
		{question: question, answers: []rr{cnameRecord(question, "b.test", 60), aRecord("b.test", 60, addr(2))}},
		{question: question, answers: []rr{cnameRecord(question, "b.test", 60)}},
		{question: question, flags: dnswire.RcodeNXDomain, authority: []rr{soaRecord("test", 60, 30)}},
		{question: question, authority: []rr{soaRecord("test", 60, 30)}, opt: true},
		{question: question, flags: dnswire.RcodeFormErr, noQuestion: true},
		{question: question, flags: dnswire.FlagTC},
		{question: question, flags: dnswire.RcodeServFail},
	}
	for index, seed := range seeds {
		f.Add(byte(index), seed.bytes())
	}
	compressed := reply{question: question}.bytes()
	binary.BigEndian.PutUint16(compressed[6:], 1)
	compressed = append(compressed, 0xc0, 12)
	compressed = append(appendRecordHeader(compressed, dnswire.TypeA, 4), 1, 2, 3, 4)
	f.Add(byte(0), compressed)
	f.Fuzz(func(t *testing.T, mode byte, payload []byte) {
		var scratch, asked dnswire.Name
		asked.SetText([]byte(question))
		var out dnswire.Response
		dnswire.ParseResponse(payload, MaxStreamMessage, 0, &asked, dnswire.TypeA, &scratch, &out)

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
			if stream, ok := h.r.NextStream(h.buf[:]); ok {
				if len(data) >= 2 && mode&1 == 0 {
					copy(data[:2], h.buf[:2])
				}
				h.r.ReceiveStream(stream.Transaction, stream.Serial, data, h.now)
			}
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
