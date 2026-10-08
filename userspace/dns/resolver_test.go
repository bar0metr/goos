package dns

import (
	"encoding/binary"
	"testing"

	"goos/userspace/dnswire"
	"goos/userspace/network"
	"goos/userspace/udpapi"
)

func TestMinimalAnswer(t *testing.T) {
	h := newHarness(t, serverA)
	h.pending(0, "Example.COM")
	q := h.sent()
	if q.Server != serverA || q.name != "example.com" || q.flags != dnswire.FlagRD {
		t.Fatalf("query %+v", q)
	}
	if !q.opt || q.payload != DatagramSize {
		t.Fatalf("query without EDNS(0): %+v", q)
	}
	h.answer(q, reply{answers: []rr{aRecord("EXAMPLE.com", 300, addr(1))}})
	answer := h.completion(0)
	expectResult(t, answer, ResultOK, SourceUpstream)
	expectAddresses(t, answer, addr(1))
	if answer.TTL != 300 {
		t.Fatalf("TTL %d", answer.TTL)
	}
	h.noCompletion()
	h.noQuery()
	if _, ok := h.r.Deadline(); ok {
		t.Fatal("deadline left after the answer")
	}
	cached, done := h.lookup(1, "example.com")
	if !done {
		t.Fatal("cache miss")
	}
	expectResult(t, cached, ResultOK, SourceCache)
	expectAddresses(t, cached, addr(1))
	if stats := h.r.Stats(); stats.Queries != 1 || stats.PositiveHits != 1 || stats.Misses != 1 || stats.Positive != 1 {
		t.Fatalf("stats %+v", stats)
	}
}

func TestMultipleAddresses(t *testing.T) {
	h := newHarness(t, serverA)
	var records []rr
	for index := byte(1); index <= 10; index++ {
		records = append(records, aRecord("multi.test", 600-uint32(index), addr(index)))
	}
	// A record of another class and one for another owner are ignored.
	records = append([]rr{{name: "multi.test", kind: dnswire.TypeA, class: 3, ttl: 1, data: []byte{1, 2, 3, 4}},
		aRecord("other.test", 1, addr(99))}, records...)
	answer := h.resolve("multi.test", reply{answers: records})
	expectAddresses(t, answer, addr(1), addr(2), addr(3), addr(4), addr(5), addr(6), addr(7), addr(8))
	if answer.TTL != 592 {
		t.Fatalf("TTL %d, want the smallest of the eight used", answer.TTL)
	}
}

func TestCNAMEInOneResponse(t *testing.T) {
	h := newHarness(t, serverA)
	answer := h.resolve("www.example.com", reply{answers: []rr{
		cnameRecord("www.example.com", "Edge.CDN.net", 120),
		aRecord("edge.cdn.net", 600, addr(7)),
	}})
	expectResult(t, answer, ResultOK, SourceUpstream)
	expectAddresses(t, answer, addr(7))
	if answer.TTL != 120 {
		t.Fatalf("TTL %d, want the CNAME's", answer.TTL)
	}
	h.noQuery()
	canonical, done := h.lookup(0, "edge.cdn.net")
	if !done || canonical.TTL != 600 {
		t.Fatalf("canonical entry %+v %v", canonical, done)
	}
	expectAddresses(t, canonical, addr(7))
}

func TestCNAMENeedsSecondQuery(t *testing.T) {
	h := newHarness(t, serverA, serverB)
	h.pending(0, "alias.test")
	first := h.sent()
	h.answer(first, reply{answers: []rr{cnameRecord("alias.test", "target.test", 50)}})
	h.noCompletion()
	second := h.sent()
	if second.name != "target.test" || second.Server != serverA || second.id == first.id {
		t.Fatalf("second query %+v", second)
	}
	// An answer for the old question is a mismatch now.
	h.answer(second, reply{question: "alias.test", answers: []rr{aRecord("alias.test", 9, addr(9))}})
	h.noCompletion()
	h.answer(second, reply{answers: []rr{aRecord("target.test", 300, addr(3))}})
	answer := h.completion(0)
	expectAddresses(t, answer, addr(3))
	if answer.TTL != 50 {
		t.Fatalf("TTL %d", answer.TTL)
	}
	if stats := h.r.Stats(); stats.Mismatched != 1 || stats.Queries != 2 {
		t.Fatalf("stats %+v", stats)
	}
}

func TestCNAMELoop(t *testing.T) {
	h := newHarness(t, serverA)
	answer := h.resolve("a.test", reply{answers: []rr{
		cnameRecord("a.test", "b.test", 60),
		cnameRecord("b.test", "A.test", 60),
	}})
	expectResult(t, answer, ResultTemporaryFailure, SourceUpstream)
	h.noQuery()
	cached, done := h.lookup(0, "a.test")
	if !done {
		t.Fatal("loop failure not cached")
	}
	expectResult(t, cached, ResultTemporaryFailure, SourceCache)
}

func TestCNAMELoopAcrossQueries(t *testing.T) {
	h := newHarness(t, serverA)
	h.pending(0, "a.test")
	h.answer(h.sent(), reply{answers: []rr{cnameRecord("a.test", "b.test", 60)}})
	h.answer(h.sent(), reply{answers: []rr{cnameRecord("b.test", "a.test", 60)}})
	expectResult(t, h.completion(0), ResultTemporaryFailure, SourceUpstream)
	h.noQuery()
}

func TestCNAMEChainLimit(t *testing.T) {
	chain := func(links int) []rr {
		var records []rr
		names := []string{"n0.test", "n1.test", "n2.test", "n3.test", "n4.test", "n5.test", "n6.test", "n7.test",
			"n8.test", "n9.test"}
		for index := 0; index < links; index++ {
			records = append(records, cnameRecord(names[index], names[index+1], 60))
		}
		return append(records, aRecord(names[links], 60, addr(1)))
	}
	h := newHarness(t, serverA)
	expectResult(t, h.resolve("n0.test", reply{answers: chain(MaxCNAMEChain)}), ResultOK, SourceUpstream)
	h.r.Flush()
	expectResult(t, h.resolve("n0.test", reply{answers: chain(MaxCNAMEChain + 1)}), ResultTemporaryFailure,
		SourceUpstream)
	h.noQuery()
}

func TestNXDomain(t *testing.T) {
	cases := []struct {
		name         string
		soa          []rr
		maxNegative  uint32
		wantTTL      uint32
		wantCached   bool
		wantQuestion string
	}{
		{"soa ttl smaller", []rr{soaRecord("test", 100, 900)}, 3600, 100, true, ""},
		{"minimum smaller", []rr{soaRecord("test", 900, 200)}, 3600, 200, true, ""},
		{"capped", []rr{soaRecord("test", 9000, 9000)}, 30, 30, true, ""},
		{"no soa", nil, 3600, 0, false, ""},
		{"soa outside the name", []rr{soaRecord("other", 100, 100)}, 3600, 0, false, ""},
		{"soa for the root", []rr{soaRecord(".", 100, 100)}, 3600, 100, true, ""},
		{"zero ttl", []rr{soaRecord("test", 0, 100)}, 3600, 0, false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, serverA)
			policy := DefaultPolicy()
			policy.MaxNegative = c.maxNegative
			h.r.SetPolicy(policy, h.now)
			answer := h.resolve("missing.test", reply{flags: dnswire.RcodeNXDomain, authority: c.soa})
			expectResult(t, answer, ResultNotFound, SourceUpstream)
			if answer.TTL != c.wantTTL {
				t.Fatalf("TTL %d, want %d", answer.TTL, c.wantTTL)
			}
			cached, done := h.lookup(0, "missing.test")
			if done != c.wantCached {
				t.Fatalf("cached %v, want %v", done, c.wantCached)
			}
			if done {
				expectResult(t, cached, ResultNotFound, SourceCache)
				if h.r.Stats().NegativeHits != 1 {
					t.Fatal("negative hit not counted")
				}
			}
		})
	}
}

func TestNXDomainAfterCNAME(t *testing.T) {
	h := newHarness(t, serverA)
	answer := h.resolve("alias.test", reply{flags: dnswire.RcodeNXDomain,
		answers:   []rr{cnameRecord("alias.test", "gone.example", 40)},
		authority: []rr{soaRecord("example", 300, 300)}})
	expectResult(t, answer, ResultNotFound, SourceUpstream)
	if answer.TTL != 40 {
		t.Fatalf("TTL %d", answer.TTL)
	}
}

func TestNoData(t *testing.T) {
	h := newHarness(t, serverA)
	answer := h.resolve("v6only.test", reply{authority: []rr{soaRecord("test", 500, 60)}})
	expectResult(t, answer, ResultNoData, SourceUpstream)
	if answer.TTL != 60 {
		t.Fatalf("TTL %d", answer.TTL)
	}
	cached, done := h.lookup(0, "v6only.test")
	if !done {
		t.Fatal("NODATA not cached")
	}
	expectResult(t, cached, ResultNoData, SourceCache)

	// Without an SOA the answer is passed on but not cached.
	answer = h.resolve("bare.test", reply{})
	expectResult(t, answer, ResultNoData, SourceUpstream)
	if answer.TTL != 0 {
		t.Fatalf("TTL %d", answer.TTL)
	}
	if _, done := h.lookup(0, "bare.test"); done {
		t.Fatal("NODATA without SOA cached")
	}
}

func TestServerErrorsMoveOn(t *testing.T) {
	for _, rcode := range []uint16{dnswire.RcodeServFail, dnswire.RcodeRefused, 4} {
		h := newHarness(t, serverA, serverB)
		h.pending(0, "host.test")
		first := h.sent()
		h.answer(first, reply{flags: rcode})
		second := h.sent()
		if second.Server != serverB {
			t.Fatalf("rcode %d: second query to %v", rcode, second.Server)
		}
		h.answer(second, reply{flags: rcode})
		expectResult(t, h.completion(0), ResultTemporaryFailure, SourceUpstream)
		h.noQuery()
		if stats := h.r.Stats(); stats.ServerFailures != 2 {
			t.Fatalf("stats %+v", stats)
		}
	}
}

func TestServerErrorThenAnswer(t *testing.T) {
	h := newHarness(t, serverA, serverB)
	h.pending(0, "host.test")
	h.answer(h.sent(), reply{flags: dnswire.RcodeServFail})
	h.answer(h.sent(), reply{answers: []rr{aRecord("host.test", 60, addr(1))}})
	expectResult(t, h.completion(0), ResultOK, SourceUpstream)
}

func TestFormErrRetriesWithoutEDNS(t *testing.T) {
	h := newHarness(t, serverA, serverB)
	h.pending(0, "old.test")
	first := h.sent()
	if !first.opt {
		t.Fatal("first query without OPT")
	}
	h.answer(first, reply{flags: dnswire.RcodeFormErr, noQuestion: true})
	retry := h.sent()
	if retry.opt || retry.Server != serverA || retry.id == first.id {
		t.Fatalf("retry %+v", retry)
	}
	if len(retry.raw) > ClassicSize {
		t.Fatal("classic query too long")
	}
	// A second FORMERR from the same server makes it unusable.
	h.answer(retry, reply{flags: dnswire.RcodeFormErr})
	next := h.sent()
	if next.Server != serverB || !next.opt {
		t.Fatalf("next %+v", next)
	}
	h.answer(next, reply{answers: []rr{aRecord("old.test", 60, addr(1))}})
	expectResult(t, h.completion(0), ResultOK, SourceUpstream)
	if stats := h.r.Stats(); stats.Queries != 3 {
		t.Fatalf("queries %d, want 3 datagrams", stats.Queries)
	}
}

func TestFormErrCannotExceedTransmissionLimit(t *testing.T) {
	h := newHarness(t, serverA)
	policy := DefaultPolicy()
	policy.Attempts = 3
	h.r.SetPolicy(policy, h.now)
	h.pending(0, "old.test")
	first := h.sent()
	h.answer(first, reply{flags: dnswire.RcodeFormErr})
	retry := h.sent()
	if retry.opt || retry.id == first.id {
		t.Fatalf("classic retry %+v", retry)
	}
	h.toDeadline()
	third := h.sent()
	if third.Server != serverA || third.opt {
		t.Fatalf("third query %+v", third)
	}
	h.toDeadline()
	h.noQuery()
	expectResult(t, h.completion(0), ResultTimeout, SourceUpstream)
	if stats := h.r.Stats(); stats.Queries != maxTransmissions {
		t.Fatalf("queries %d, want hard limit %d", stats.Queries, maxTransmissions)
	}
}

func TestExhaustedServerMovesToNextServer(t *testing.T) {
	h := newHarness(t, serverA, serverB)
	policy := DefaultPolicy()
	policy.Attempts = 3
	h.r.SetPolicy(policy, h.now)
	h.pending(0, "a.test")
	h.answer(h.sent(), reply{answers: []rr{cnameRecord("a.test", "b.test", 60)}})
	h.answer(h.sent(), reply{answers: []rr{cnameRecord("b.test", "c.test", 60)}})
	h.answer(h.sent(), reply{answers: []rr{cnameRecord("c.test", "d.test", 60)}})
	next := h.sent()
	if next.Server != serverB || !next.opt {
		t.Fatalf("next server query %+v", next)
	}
	h.answer(next, reply{answers: []rr{aRecord("d.test", 60, addr(1))}})
	expectResult(t, h.completion(0), ResultOK, SourceUpstream)
}

func TestFormErrWithOPTIsFailure(t *testing.T) {
	h := newHarness(t, serverA)
	h.pending(0, "host.test")
	h.answer(h.sent(), reply{flags: dnswire.RcodeFormErr, opt: true})
	expectResult(t, h.completion(0), ResultTemporaryFailure, SourceUpstream)
	h.noQuery()
}

func TestClassicServerStaysClassic(t *testing.T) {
	h := newHarness(t, serverA)
	h.pending(0, "host.test")
	h.answer(h.sent(), reply{flags: dnswire.RcodeFormErr})
	retry := h.sent()
	h.toDeadline()
	again := h.sent()
	if again.opt || again.Server != serverA {
		t.Fatalf("after the timeout %+v", again)
	}
	_ = retry
}

func TestExtendedRcodeIsFailure(t *testing.T) {
	h := newHarness(t, serverA)
	h.pending(0, "host.test")
	q := h.sent()
	payload := reply{id: q.id, question: q.name, opt: true}.bytes()
	// BADVERS: extended rcode 1 in the OPT TTL's top byte.
	payload[len(payload)-6] = 1
	h.deliver(q, payload)
	expectResult(t, h.completion(0), ResultTemporaryFailure, SourceUpstream)
}

func TestCompressedNames(t *testing.T) {
	h := newHarness(t, serverA)
	h.pending(0, "www.example.com")
	q := h.sent()
	payload := reply{id: q.id, question: "www.example.com"}.bytes()
	binary.BigEndian.PutUint16(payload[6:], 2)
	// www.example.com CNAME cdn.<example.com>, then <cdn.example.com> A.
	payload = append(payload, 0xc0, 12)
	payload = binary.BigEndian.AppendUint16(payload, dnswire.TypeCNAME)
	payload = binary.BigEndian.AppendUint16(payload, dnswire.ClassIN)
	payload = binary.BigEndian.AppendUint32(payload, 60)
	payload = binary.BigEndian.AppendUint16(payload, 6)
	target := len(payload)
	payload = append(payload, 3, 'C', 'D', 'N', 0xc0, 16)
	payload = append(payload, 0xc0, byte(target))
	payload = binary.BigEndian.AppendUint16(payload, dnswire.TypeA)
	payload = binary.BigEndian.AppendUint16(payload, dnswire.ClassIN)
	payload = binary.BigEndian.AppendUint32(payload, 60)
	payload = binary.BigEndian.AppendUint16(payload, 4)
	payload = append(payload, 10, 9, 9, 9)
	h.deliver(q, payload)
	answer := h.completion(0)
	expectResult(t, answer, ResultOK, SourceUpstream)
	expectAddresses(t, answer, [4]byte{10, 9, 9, 9})
}

// malformedCase builds a payload for the outstanding query.
type malformedCase struct {
	name  string
	build func(q query) []byte
}

func appendRecordHeader(payload []byte, kind uint16, length int) []byte {
	payload = binary.BigEndian.AppendUint16(payload, kind)
	payload = binary.BigEndian.AppendUint16(payload, dnswire.ClassIN)
	payload = binary.BigEndian.AppendUint32(payload, 60)
	return binary.BigEndian.AppendUint16(payload, uint16(length))
}

// withAnswers returns the reply header and question with the answer count
// set, ready for raw records.
func withAnswers(q query, count uint16) []byte {
	payload := reply{id: q.id, question: q.name}.bytes()
	binary.BigEndian.PutUint16(payload[6:], count)
	return payload
}

func TestMalformedResponses(t *testing.T) {
	long := make([]byte, 0, 300)
	for index := 0; index < 5; index++ {
		long = append(long, 60)
		for byteIndex := 0; byteIndex < 60; byteIndex++ {
			long = append(long, 'a')
		}
	}
	long = append(long, 0)
	cases := []malformedCase{
		{"short header", func(q query) []byte { return withAnswers(q, 0)[:11] }},
		{"query not response", func(q query) []byte {
			payload := withAnswers(q, 0)
			payload[2] &^= 0x80
			return payload
		}},
		{"opcode", func(q query) []byte {
			payload := withAnswers(q, 0)
			payload[2] |= 0x10
			return payload
		}},
		{"pointer loop", func(q query) []byte {
			payload := withAnswers(q, 1)
			at := len(payload)
			payload = append(payload, 0xc0, byte(at))
			return appendRecordHeader(payload, dnswire.TypeA, 4)
		}},
		{"two pointers loop", func(q query) []byte {
			payload := withAnswers(q, 1)
			at := len(payload)
			payload = append(payload, 1, 'x', 0xc0, byte(at))
			return append(appendRecordHeader(payload, dnswire.TypeA, 4), 1, 2, 3, 4)
		}},
		{"pointer outside packet", func(q query) []byte {
			payload := withAnswers(q, 1)
			payload = append(payload, 0xc3, 0xff)
			return append(appendRecordHeader(payload, dnswire.TypeA, 4), 1, 2, 3, 4)
		}},
		{"forward pointer", func(q query) []byte {
			payload := withAnswers(q, 1)
			at := len(payload)
			payload = append(payload, 0xc0, byte(at+2), 0)
			return append(appendRecordHeader(payload, dnswire.TypeA, 4), 1, 2, 3, 4)
		}},
		{"truncated pointer", func(q query) []byte {
			payload := withAnswers(q, 1)
			return append(payload, 0xc0)
		}},
		{"label over 63", func(q query) []byte {
			payload := withAnswers(q, 1)
			payload = append(payload, 64)
			for index := 0; index < 64; index++ {
				payload = append(payload, 'a')
			}
			payload = append(payload, 0)
			return append(appendRecordHeader(payload, dnswire.TypeA, 4), 1, 2, 3, 4)
		}},
		{"extended label type", func(q query) []byte {
			payload := withAnswers(q, 1)
			payload = append(payload, 0x41, 0)
			return append(appendRecordHeader(payload, dnswire.TypeA, 4), 1, 2, 3, 4)
		}},
		{"name too long", func(q query) []byte {
			payload := withAnswers(q, 1)
			payload = append(payload, long...)
			return append(appendRecordHeader(payload, dnswire.TypeA, 4), 1, 2, 3, 4)
		}},
		{"rdlength past end", func(q query) []byte {
			payload := withAnswers(q, 1)
			payload = append(payload, wire(q.name)...)
			return append(appendRecordHeader(payload, dnswire.TypeA, 40), 1, 2, 3, 4)
		}},
		{"A rdata of 5 bytes", func(q query) []byte {
			payload := withAnswers(q, 1)
			payload = append(payload, wire(q.name)...)
			return append(appendRecordHeader(payload, dnswire.TypeA, 5), 1, 2, 3, 4, 5)
		}},
		{"CNAME rdata longer than the name", func(q query) []byte {
			payload := withAnswers(q, 1)
			payload = append(payload, wire(q.name)...)
			target := wire("x.test")
			payload = appendRecordHeader(payload, dnswire.TypeCNAME, len(target)+1)
			return append(append(payload, target...), 0)
		}},
		{"CNAME name crossing rdata", func(q query) []byte {
			payload := withAnswers(q, 1)
			payload = append(payload, wire(q.name)...)
			target := wire("x.test")
			payload = appendRecordHeader(payload, dnswire.TypeCNAME, 2)
			return append(payload, target...)
		}},
		{"SOA short", func(q query) []byte {
			payload := reply{id: q.id, question: q.name, flags: dnswire.RcodeNXDomain}.bytes()
			binary.BigEndian.PutUint16(payload[8:], 1)
			payload = append(payload, wire("test")...)
			data := append(append(wire("ns.test"), wire("admin.test")...), make([]byte, 19)...)
			return append(appendRecordHeader(payload, dnswire.TypeSOA, len(data)), data...)
		}},
		{"huge counts", func(q query) []byte {
			payload := withAnswers(q, 0xffff)
			binary.BigEndian.PutUint16(payload[8:], 0xffff)
			binary.BigEndian.PutUint16(payload[10:], 0xffff)
			return append(payload, make([]byte, 64)...)
		}},
		{"count past the records", func(q query) []byte {
			payload := withAnswers(q, 2)
			payload = append(payload, wire(q.name)...)
			payload = append(appendRecordHeader(payload, dnswire.TypeA, 4), 1, 2, 3, 4)
			return append(payload, make([]byte, 5)...)
		}},
		{"two OPT records", func(q query) []byte {
			payload := reply{id: q.id, question: q.name, opt: true,
				additional: []rr{{name: ".", kind: dnswire.TypeOPT, class: 512}}}.bytes()
			return payload
		}},
		{"OPT in answers", func(q query) []byte {
			return reply{id: q.id, question: q.name, answers: []rr{{name: ".", kind: dnswire.TypeOPT, class: 512}}}.bytes()
		}},
		{"OPT not at the root", func(q query) []byte {
			return reply{id: q.id, question: q.name, additional: []rr{{name: "x", kind: dnswire.TypeOPT, class: 512}}}.bytes()
		}},
		{"too long", func(q query) []byte {
			return append(withAnswers(q, 0), make([]byte, DatagramSize)...)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, serverA, serverB)
			h.pending(0, "host.test")
			q := h.sent()
			h.deliver(q, c.build(q))
			if stats := h.r.Stats(); stats.Malformed != 1 {
				t.Fatalf("stats %+v", stats)
			}
			next := h.sent()
			if next.Server != serverB {
				t.Fatalf("malformed answer did not move on: %+v", next)
			}
			h.answer(next, reply{flags: dnswire.RcodeServFail})
			expectResult(t, h.completion(0), ResultTemporaryFailure, SourceUpstream)
			if _, done := h.lookup(1, "host.test"); !done || h.r.Stats().Positive != 0 {
				t.Fatal("cache touched by a malformed answer")
			}
		})
	}
}

func TestTrailingBytesIgnored(t *testing.T) {
	h := newHarness(t, serverA)
	h.pending(0, "host.test")
	q := h.sent()
	payload := reply{id: q.id, question: q.name, answers: []rr{aRecord("host.test", 60, addr(1))}}.bytes()
	h.deliver(q, append(payload, 0xde, 0xad, 0xc0))
	expectResult(t, h.completion(0), ResultOK, SourceUpstream)
}

func TestUnknownTypesSkipped(t *testing.T) {
	h := newHarness(t, serverA)
	answer := h.resolve("host.test", reply{
		answers: []rr{
			{name: "host.test", kind: 28, ttl: 60, data: make([]byte, 16)},
			{name: "host.test", kind: 99, ttl: 60, data: []byte{0xc0, 0xff, 0xff}},
			aRecord("host.test", 60, addr(1)),
		},
		additional: []rr{{name: "host.test", kind: dnswire.TypeA, class: 255, ttl: 60, data: []byte{1}}},
	})
	expectAddresses(t, answer, addr(1))
}

func TestMismatchedResponses(t *testing.T) {
	cases := []struct {
		name    string
		server  [4]byte
		payload func(q query) []byte
	}{
		{"id", serverA, func(q query) []byte {
			return reply{id: q.id + 1, question: q.name, answers: []rr{aRecord(q.name, 60, addr(6))}}.bytes()
		}},
		{"server", serverB, func(q query) []byte {
			return reply{id: q.id, question: q.name, answers: []rr{aRecord(q.name, 60, addr(6))}}.bytes()
		}},
		{"question name", serverA, func(q query) []byte {
			return reply{id: q.id, question: "other.test", answers: []rr{aRecord("other.test", 60, addr(6))}}.bytes()
		}},
		{"question type", serverA, func(q query) []byte {
			return reply{id: q.id, question: q.name, qtype: 28}.bytes()
		}},
		{"no question", serverA, func(q query) []byte {
			return reply{id: q.id, noQuestion: true}.bytes()
		}},
		{"two questions", serverA, func(q query) []byte {
			payload := reply{id: q.id, question: q.name}.bytes()
			binary.BigEndian.PutUint16(payload[4:], 2)
			return payload
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, serverA, serverB)
			h.pending(0, "host.test")
			q := h.sent()
			h.r.Receive(q.Transaction, c.server, c.payload(q), h.now)
			if stats := h.r.Stats(); stats.Mismatched != 1 || stats.Malformed != 0 {
				t.Fatalf("stats %+v", stats)
			}
			h.noCompletion()
			h.noQuery()
			h.answer(q, reply{answers: []rr{aRecord("HOST.test", 60, addr(1))}})
			answer := h.completion(0)
			expectAddresses(t, answer, addr(1))
		})
	}
}

func TestReceiveWithoutQuery(t *testing.T) {
	h := newHarness(t, serverA)
	h.r.Receive(-1, serverA, nil, h.now)
	h.r.Receive(MaxTransactions, serverA, nil, h.now)
	h.r.Receive(0, serverA, make([]byte, 40), h.now)
	h.pending(0, "host.test")
	// Not handed out yet: nothing is outstanding.
	h.r.Receive(0, serverA, make([]byte, 40), h.now)
	if stats := h.r.Stats(); stats.Mismatched != 4 {
		t.Fatalf("stats %+v", stats)
	}
	h.r.SendFailed(0, h.now)
	h.r.SendFailed(-1, h.now)
	h.sent()
}

func TestTimeoutsAndRotation(t *testing.T) {
	h := newHarness(t, serverA, serverB)
	h.pending(0, "slow.test")
	start := h.now
	var servers [][4]byte
	var waits []uint64
	for {
		out, ok := h.r.NextQuery(h.buf[:])
		if !ok {
			break
		}
		q := decodeQuery(t, out, h.buf[:out.Length])
		servers = append(servers, q.Server)
		before := h.now
		h.toDeadline()
		waits = append(waits, h.now-before)
	}
	want := [][4]byte{serverA, serverB, serverA}
	if len(servers) != len(want) {
		t.Fatalf("servers %v", servers)
	}
	for index := range want {
		if servers[index] != want[index] {
			t.Fatalf("servers %v", servers)
		}
	}
	// Two servers, 3 s timeout, 2 attempts: round 0 waits 1.5 s each, round 1
	// 3 s, which reaches the 6 s deadline before the second server's turn.
	wantWaits := []uint64{1500 * millisecond, 1500 * millisecond, 3000 * millisecond}
	if waits[0] != wantWaits[0] || waits[1] != wantWaits[1] || waits[2] != wantWaits[2] {
		t.Fatalf("waits %v", waits)
	}
	if h.now-start != 6*second {
		t.Fatalf("resolution took %d, want the 6 s deadline", h.now-start)
	}
	expectResult(t, h.completion(0), ResultTimeout, SourceUpstream)
	if stats := h.r.Stats(); stats.Timeouts != 3 || stats.Queries != 3 {
		t.Fatalf("stats %+v", stats)
	}
	cached, done := h.lookup(0, "slow.test")
	if !done || cached.Result != ResultTimeout || cached.TTL != 5 {
		t.Fatalf("failure entry %+v %v", cached, done)
	}
	h.advance(5 * second)
	if _, done := h.lookup(0, "slow.test"); done {
		t.Fatal("failure entry outlived FailureTTL")
	}
}

func TestOneServerSchedule(t *testing.T) {
	h := newHarness(t, serverA)
	policy := DefaultPolicy()
	policy.Attempts = 3
	policy.QueryTimeout = 1000 * millisecond
	h.r.SetPolicy(policy, h.now)
	h.pending(0, "slow.test")
	start := h.now
	sends := 0
	for {
		if _, ok := h.r.NextQuery(h.buf[:]); !ok {
			break
		}
		sends++
		h.toDeadline()
	}
	if sends != 3 || h.now-start != 3*second {
		t.Fatalf("%d sends over %d ns", sends, h.now-start)
	}
	expectResult(t, h.completion(0), ResultTimeout, SourceUpstream)
}

func TestManyServersMinimumWait(t *testing.T) {
	h := newHarness(t)
	h.r.SetServers([][4]byte{{10, 0, 0, 1}, {10, 0, 0, 2}, {10, 0, 0, 3}, {10, 0, 0, 4}},
		[][4]byte{{10, 0, 0, 5}, {10, 0, 0, 6}, {10, 0, 0, 7}, {10, 0, 0, 8}})
	h.pending(0, "slow.test")
	start := h.now
	sends := 0
	for {
		if _, ok := h.r.NextQuery(h.buf[:]); !ok {
			break
		}
		sends++
		before := h.now
		h.toDeadline()
		if h.now-before < minWait && h.now-start < 6*second {
			t.Fatalf("wait %d below the floor", h.now-before)
		}
	}
	if h.now-start != 6*second || sends > MaxServers*2 {
		t.Fatalf("%d sends over %d ns", sends, h.now-start)
	}
	expectResult(t, h.completion(0), ResultTimeout, SourceUpstream)
}

func TestTimeoutThenAnswer(t *testing.T) {
	h := newHarness(t, serverA, serverB)
	h.pending(0, "host.test")
	first := h.sent()
	h.toDeadline()
	second := h.sent()
	// The first server answers late; that is a mismatch now.
	h.answer(first, reply{answers: []rr{aRecord("host.test", 60, addr(1))}})
	h.noCompletion()
	h.answer(second, reply{answers: []rr{aRecord("host.test", 60, addr(2))}})
	expectAddresses(t, h.completion(0), addr(2))
}

func TestSendFailed(t *testing.T) {
	h := newHarness(t, serverA, serverB)
	h.pending(0, "host.test")
	first := h.sent()
	h.r.SendFailed(first.Transaction, h.now)
	second := h.sent()
	if second.Server != serverB {
		t.Fatalf("second %+v", second)
	}
	h.r.SendFailed(second.Transaction, h.now)
	expectResult(t, h.completion(0), ResultTemporaryFailure, SourceUpstream)
	if stats := h.r.Stats(); stats.ServerFailures != 2 {
		t.Fatalf("stats %+v", stats)
	}
}

func TestIDsDiffer(t *testing.T) {
	h := newHarness(t, serverA)
	policy := DefaultPolicy()
	policy.Attempts = 3
	h.r.SetPolicy(policy, h.now)
	seen := map[uint16]bool{}
	for round := 0; round < 20; round++ {
		h.r.Flush()
		h.pending(0, "host.test")
		previous := -1
		for {
			out, ok := h.r.NextQuery(h.buf[:])
			if !ok {
				break
			}
			id := int(binary.BigEndian.Uint16(h.buf[:2]))
			if id == previous {
				t.Fatal("ID repeated between transmissions")
			}
			previous = id
			seen[uint16(id)] = true
			_ = out
			h.toDeadline()
		}
		h.completion(0)
	}
	if len(seen) < 55 {
		t.Fatalf("only %d distinct IDs in 60 transmissions", len(seen))
	}

	h.r.Flush()
	other := newHarness(t, serverA)
	other.r.Reseed(42)
	other.pending(0, "host.test")
	h.pending(0, "host.test")
	if other.sent().id == h.sent().id {
		t.Fatal("reseeding changed nothing")
	}
}

func TestOverallDeadline(t *testing.T) {
	h := newHarness(t, serverA)
	policy := DefaultPolicy()
	policy.QueryTimeout = 500 * millisecond
	policy.Attempts = 1
	h.r.SetPolicy(policy, h.now)
	h.pending(0, "a.test")
	start := h.now
	q := h.sent()
	h.advance(400 * millisecond)
	h.answer(q, reply{answers: []rr{cnameRecord("a.test", "b.test", 60)}})
	second := h.sent()
	if deadline, _ := h.r.Deadline(); deadline != start+500*millisecond {
		t.Fatalf("CNAME query deadline %d, want the original", deadline-start)
	}
	h.toDeadline()
	_ = second
	expectResult(t, h.completion(0), ResultTimeout, SourceUpstream)
	h.noQuery()
}

func TestCNAMEKeepsTransmissionLimit(t *testing.T) {
	h := newHarness(t, serverA)
	policy := DefaultPolicy()
	policy.Attempts = 3
	h.r.SetPolicy(policy, h.now)
	h.pending(0, "a.test")
	h.answer(h.sent(), reply{answers: []rr{cnameRecord("a.test", "b.test", 60)}})
	h.answer(h.sent(), reply{answers: []rr{cnameRecord("b.test", "c.test", 60)}})
	h.answer(h.sent(), reply{answers: []rr{cnameRecord("c.test", "d.test", 60)}})
	h.noQuery()
	expectResult(t, h.completion(0), ResultTemporaryFailure, SourceUpstream)
	if stats := h.r.Stats(); stats.Queries != maxTransmissions {
		t.Fatalf("queries %d, want hard limit %d", stats.Queries, maxTransmissions)
	}
}

func TestCoalescing(t *testing.T) {
	h := newHarness(t, serverA)
	h.pending(0, "shared.test")
	h.pending(1, "SHARED.test")
	q := h.sent()
	h.noQuery()
	h.answer(q, reply{answers: []rr{aRecord("shared.test", 60, addr(1))}})
	first := h.completion(0)
	second := h.completion(1)
	if first != second {
		t.Fatalf("waiters got %+v and %+v", first, second)
	}
	if stats := h.r.Stats(); stats.Queries != 1 {
		t.Fatalf("stats %+v", stats)
	}
}

func TestBusy(t *testing.T) {
	h := newHarness(t, serverA)
	names := []string{"a.test", "b.test", "c.test", "d.test"}
	for index, text := range names {
		h.pending(index, text)
	}
	answer, done := h.lookup(4, "e.test")
	if !done || answer.Result != ResultBusy {
		t.Fatalf("fifth transaction %+v", answer)
	}
	// The client already waiting is busy even for a name being resolved.
	if answer, done = h.lookup(0, "b.test"); !done || answer.Result != ResultBusy {
		t.Fatalf("waiting client %+v", answer)
	}
	if answer, done = h.lookup(MaxClients, "b.test"); !done || answer.Result != ResultBusy {
		t.Fatalf("client out of range %+v", answer)
	}
	for client := 4; client < 4+MaxWaiters-1; client++ {
		h.pending(client, "a.test")
	}
	if answer, done = h.lookup(4+MaxWaiters, "a.test"); !done || answer.Result != ResultBusy {
		t.Fatalf("waiter overflow %+v", answer)
	}
	// Immediate answers need no slot.
	if answer, done = h.lookup(4+MaxWaiters, "localhost"); !done || answer.Result != ResultOK {
		t.Fatalf("localhost while busy %+v", answer)
	}
}

func TestCancel(t *testing.T) {
	h := newHarness(t, serverA)
	h.pending(0, "a.test")
	h.pending(1, "a.test")
	q := h.sent()
	h.r.Cancel(0)
	h.r.Cancel(-1)
	h.r.Cancel(MaxClients)
	h.answer(q, reply{answers: []rr{aRecord("a.test", 60, addr(1))}})
	h.completion(1)
	h.noCompletion()

	// The last waiter going away drops the transaction.
	h.pending(2, "b.test")
	q = h.sent()
	h.r.Cancel(2)
	if _, ok := h.r.Deadline(); ok {
		t.Fatal("transaction kept without waiters")
	}
	h.answer(q, reply{answers: []rr{aRecord("b.test", 60, addr(1))}})
	h.noCompletion()
	h.pending(2, "b.test")

	// Cancel also drops an untaken completion.
	h.r.Reset(h.now)
	h.r.Cancel(2)
	h.noCompletion()
}

func TestCacheExpiry(t *testing.T) {
	h := newHarness(t, serverA)
	h.resolve("host.test", reply{answers: []rr{aRecord("host.test", 10, addr(1))}})
	h.advance(9*second + 500*millisecond)
	answer, done := h.lookup(0, "host.test")
	if !done || answer.TTL != 1 {
		t.Fatalf("near expiry %+v %v", answer, done)
	}
	h.advance(500 * millisecond)
	if _, done := h.lookup(0, "host.test"); done {
		t.Fatal("expired entry answered")
	}
	if stats := h.r.Stats(); stats.CacheUsed != 0 {
		t.Fatalf("expired entry kept: %+v", stats)
	}
}

func TestTTLZeroNotCached(t *testing.T) {
	h := newHarness(t, serverA)
	answer := h.resolve("host.test", reply{answers: []rr{aRecord("host.test", 0, addr(1))}})
	expectAddresses(t, answer, addr(1))
	if answer.TTL != 0 {
		t.Fatalf("TTL %d", answer.TTL)
	}
	h.notCached("host.test")
	// A TTL with the top bit set means zero.
	answer = h.resolve("host.test", reply{answers: []rr{aRecord("host.test", 0x80000001, addr(1))}})
	if answer.TTL != 0 {
		t.Fatalf("TTL %d", answer.TTL)
	}
}

func TestMaxTTLCap(t *testing.T) {
	h := newHarness(t, serverA)
	policy := DefaultPolicy()
	policy.MaxTTL = 100
	h.r.SetPolicy(policy, h.now)
	answer := h.resolve("host.test", reply{answers: []rr{aRecord("host.test", 1_000_000, addr(1))}})
	if answer.TTL != 100 {
		t.Fatalf("TTL %d", answer.TTL)
	}
	h.advance(100 * second)
	if _, done := h.lookup(0, "host.test"); done {
		t.Fatal("entry outlived MaxTTL")
	}
}

func TestFailureCacheHit(t *testing.T) {
	h := newHarness(t, serverA)
	h.resolve("host.test", reply{flags: dnswire.RcodeServFail})
	answer, done := h.lookup(0, "host.test")
	if !done {
		t.Fatal("failure not cached")
	}
	expectResult(t, answer, ResultTemporaryFailure, SourceCache)
	if stats := h.r.Stats(); stats.FailureHits != 1 || stats.Failures != 1 {
		t.Fatalf("stats %+v", stats)
	}
	h.advance(5 * second)
	h.pending(0, "host.test")
}

func TestEvictionOrder(t *testing.T) {
	h := newHarness(t, serverA)
	policy := DefaultPolicy()
	policy.CacheEntries = 3
	h.r.SetPolicy(policy, h.now)
	h.resolve("a.test", reply{answers: []rr{aRecord("a.test", 100, addr(1))}})
	h.resolve("b.test", reply{answers: []rr{aRecord("b.test", 10, addr(2))}})
	h.resolve("c.test", reply{answers: []rr{aRecord("c.test", 100, addr(3))}})
	h.lookup(0, "a.test") // a is now the most recently used
	h.resolve("d.test", reply{answers: []rr{aRecord("d.test", 100, addr(4))}})
	// b was least recently used.
	if _, done := h.lookup(0, "b.test"); done {
		t.Fatal("b kept")
	}
	h.r.Cancel(0)
	h.r.Reset(h.now)
	h.r.Completion()
	for _, text := range []string{"a.test", "c.test", "d.test"} {
		if _, done := h.lookup(0, text); !done {
			t.Fatalf("%s evicted", text)
		}
	}
	// An expired entry goes before the least recently used one.
	// a was used first in that loop, so e replaces it.
	h.resolve("e.test", reply{answers: []rr{aRecord("e.test", 1, addr(5))}})
	h.notCached("a.test")
	h.cachedOK("c.test")
	h.cachedOK("d.test")
	h.cachedOK("e.test")
	// e expired, and goes before c, the least recently used.
	h.advance(2 * second)
	h.resolve("f.test", reply{answers: []rr{aRecord("f.test", 100, addr(6))}})
	if stats := h.r.Stats(); stats.CacheUsed != 3 {
		t.Fatalf("stats %+v", stats)
	}
	for _, text := range []string{"c.test", "d.test", "f.test"} {
		h.cachedOK(text)
	}
}

func TestShrinkEvictsAtOnce(t *testing.T) {
	h := newHarness(t, serverA)
	for index, text := range []string{"a.test", "b.test", "c.test", "d.test"} {
		ttl := uint32(100)
		if text == "c.test" {
			ttl = 1
		}
		h.resolve(text, reply{answers: []rr{aRecord(text, ttl, addr(byte(index)))}})
	}
	h.lookup(0, "a.test")
	h.advance(2 * second)
	policy := DefaultPolicy()
	policy.CacheEntries = 2
	h.r.SetPolicy(policy, h.now)
	if stats := h.r.Stats(); stats.CacheUsed != 2 || stats.CacheLimit != 2 {
		t.Fatalf("stats %+v", stats)
	}
	// c expired and went first, then b, the least recently used.
	for _, text := range []string{"a.test", "d.test"} {
		if _, done := h.lookup(0, text); !done {
			t.Fatalf("%s evicted", text)
		}
	}
	// An invalid policy changes nothing.
	policy.CacheEntries = 0
	h.r.SetPolicy(policy, h.now)
	if h.r.Policy().CacheEntries != 2 {
		t.Fatal("invalid policy applied")
	}
}

func TestSetServersKeepsCache(t *testing.T) {
	h := newHarness(t, serverA)
	h.resolve("host.test", reply{answers: []rr{aRecord("host.test", 100, addr(1))}})
	h.r.SetServers(nil, nil)
	answer, done := h.lookup(0, "host.test")
	if !done || answer.Source != SourceCache {
		t.Fatalf("after SetServers %+v", answer)
	}
	if answer, done = h.lookup(0, "other.test"); !done || answer.Result != ResultNoServers {
		t.Fatalf("no servers %+v", answer)
	}
}

func TestSetServersOrder(t *testing.T) {
	h := newHarness(t)
	h.r.SetServers(
		[][4]byte{serverB, {127, 0, 0, 1}, serverB, {0, 0, 0, 0}, {255, 255, 255, 255}, serverA, {10, 0, 0, 1},
			{10, 0, 0, 2}, {10, 0, 0, 3}},
		[][4]byte{{}, serverA, serverC, {224, 0, 0, 1}, {10, 0, 0, 9}})
	servers, count := h.r.Servers()
	want := []Server{{serverB, ServerDHCP}, {serverA, ServerDHCP}, {[4]byte{10, 0, 0, 1}, ServerDHCP},
		{[4]byte{10, 0, 0, 2}, ServerDHCP}, {serverC, ServerConfig}, {[4]byte{10, 0, 0, 9}, ServerConfig}}
	if count != len(want) {
		t.Fatalf("servers %v", servers[:count])
	}
	for index := range want {
		if servers[index] != want[index] {
			t.Fatalf("servers %v", servers[:count])
		}
	}
}

func TestFlush(t *testing.T) {
	h := newHarness(t, serverA)
	h.resolve("pos.test", reply{answers: []rr{aRecord("pos.test", 100, addr(1))}})
	h.resolve("neg.test", reply{flags: dnswire.RcodeNXDomain, authority: []rr{soaRecord("test", 100, 100)}})
	h.resolve("fail.test", reply{flags: dnswire.RcodeServFail})
	if stats := h.r.Stats(); stats.Positive != 1 || stats.Negative != 1 || stats.Failures != 1 || stats.CacheUsed != 3 {
		t.Fatalf("stats %+v", stats)
	}
	h.r.Flush()
	if stats := h.r.Stats(); stats.CacheUsed != 0 || stats.Positive+stats.Negative+stats.Failures != 0 {
		t.Fatalf("stats %+v", stats)
	}
	h.pending(0, "pos.test")
}

func TestLocalNames(t *testing.T) {
	h := newHarness(t, serverA)
	for _, text := range []string{"localhost", "LOCALHOST.", "db.localhost", "a.b.localhost"} {
		answer, done := h.lookup(0, text)
		if !done {
			t.Fatalf("%s queried", text)
		}
		expectResult(t, answer, ResultOK, SourceLocalhost)
		expectAddresses(t, answer, [4]byte{127, 0, 0, 1})
		if answer.TTL != 0 {
			t.Fatalf("TTL %d", answer.TTL)
		}
	}
	for _, text := range []string{"local", "printer.local"} {
		answer, done := h.lookup(0, text)
		if !done || answer.Result != ResultUnsupportedNameScope {
			t.Fatalf("%s: %+v", text, answer)
		}
	}
	h.noQuery()
	if stats := h.r.Stats(); stats.CacheUsed != 0 || stats.Misses != 0 {
		t.Fatalf("stats %+v", stats)
	}
	// Names that only end in the same letters are ordinary.
	h.pending(0, "notlocalhost")
	h.sent()
	if answer, done := h.r.Lookup(1, nil, h.now); !done || answer.Result != ResultInvalidName {
		t.Fatalf("nil name %+v", answer)
	}
	var empty = *name(t, "x")
	empty.Size = 0
	if answer, done := h.r.Lookup(1, &empty, h.now); !done || answer.Result != ResultInvalidName {
		t.Fatalf("empty name %+v", answer)
	}
}

func TestNoServers(t *testing.T) {
	h := newHarness(t)
	answer, done := h.lookup(0, "host.test")
	if !done || answer.Result != ResultNoServers {
		t.Fatalf("%+v", answer)
	}
	if answer, done = h.lookup(0, "localhost"); !done || answer.Result != ResultOK {
		t.Fatalf("localhost without servers %+v", answer)
	}
}

func TestReset(t *testing.T) {
	h := newHarness(t, serverA)
	h.resolve("cached.test", reply{answers: []rr{aRecord("cached.test", 100, addr(1))}})
	h.pending(0, "a.test")
	h.pending(1, "a.test")
	h.pending(2, "b.test")
	q := h.sent()
	h.r.Reset(h.now)
	seen := 0
	for {
		client, answer, ok := h.r.Completion()
		if !ok {
			break
		}
		expectResult(t, answer, ResultReconfigured, SourceUpstream)
		seen |= 1 << client
	}
	if seen != 7 {
		t.Fatalf("completions for %b", seen)
	}
	h.noCompletion()
	h.noQuery()
	if _, ok := h.r.Deadline(); ok {
		t.Fatal("deadline after Reset")
	}
	h.answer(q, reply{answers: []rr{aRecord("a.test", 100, addr(1))}})
	h.noCompletion()
	if _, done := h.lookup(0, "a.test"); done {
		t.Fatal("Reconfigured cached")
	}
	if _, done := h.lookup(1, "cached.test"); !done {
		t.Fatal("cache lost on Reset")
	}
}

func TestNextQueryShortBuffer(t *testing.T) {
	h := newHarness(t, serverA)
	h.pending(0, "host.test")
	if _, ok := h.r.NextQuery(make([]byte, DatagramSize-1)); ok {
		t.Fatal("short buffer accepted")
	}
	h.sent()
}

func TestCountersSaturate(t *testing.T) {
	counter := maxCounter - 1
	increment(&counter)
	increment(&counter)
	if counter != maxCounter {
		t.Fatal("counter wrapped")
	}
	if addTime(maxCounter-5, 10) != maxCounter {
		t.Fatal("time wrapped")
	}
}

func TestInitRejectsInvalidPolicy(t *testing.T) {
	var r Resolver
	r.Init(1, Policy{})
	if r.Policy() != DefaultPolicy() {
		t.Fatalf("policy %+v", r.Policy())
	}
}

func TestWaiting(t *testing.T) {
	h := newHarness(t, serverA)
	h.pending(0, "host.test")
	if h.r.Waiting(-1) || h.r.Waiting(MaxTransactions) {
		t.Fatal("out-of-range transaction waits")
	}
	first := h.sent()
	if !h.r.Waiting(first.Transaction) {
		t.Fatal("query in flight not waiting")
	}
	h.r.SendFailed(first.Transaction, h.now)
	if h.r.Waiting(first.Transaction) {
		t.Fatal("finished transaction still waiting")
	}
}

// TestEDNSFitsTheSmallestUDPLimit ties the resolver's EDNS(0) payload size
// to the UDP limits. The smallest max-payload the network settings accept,
// and the GUDP1 slot, must both hold the largest answer the resolver
// advertises, so no tuning can make the stack drop an answer the resolver
// asked for.
func TestEDNSFitsTheSmallestUDPLimit(t *testing.T) {
	values := smallestUDPTuning()
	tuning, field := network.ParseTuning(&values)
	if field != network.FieldNone {
		t.Fatalf("smallest UDP limits refused: field %d", field)
	}
	values[network.FieldUDPMaxPayload-network.FieldTTL]--
	if _, field := network.ParseTuning(&values); field != network.FieldUDPMaxPayload {
		t.Fatalf("max-payload below the EDNS size accepted: field %d", field)
	}
	h := newHarness(t, serverA)
	h.pending(0, "host.test")
	q := h.sent()
	if !q.opt || q.payload == 0 {
		t.Fatalf("query without EDNS: %+v", q)
	}
	if int(q.payload) > int(tuning.UDP.MaxPayload) || int(q.payload) > udpapi.MaxPayload || q.payload != DatagramSize {
		t.Fatalf("advertised %d, smallest max-payload %d, GUDP1 slot %d", q.payload, tuning.UDP.MaxPayload, udpapi.MaxPayload)
	}
	if network.UDPMinPayload < DatagramSize {
		t.Fatalf("UDP floor %d below the EDNS size %d", network.UDPMinPayload, DatagramSize)
	}
}

// smallestUDPTuning is the default tuning with every UDP limit at the
// bottom of its range.
func smallestUDPTuning() [network.TuningFields]uint64 {
	tuning := network.DefaultTuning()
	tuning.UDP = network.UDPTuning{EphemeralMin: 65535, EphemeralMax: 65535, ReceiveQueue: 1, MaxEndpoints: 1,
		SessionEndpoints: 1, MaxPayload: network.UDPMinPayload}
	var values [network.TuningFields]uint64
	stored := [...]uint64{uint64(tuning.TTL), uint64(tuning.ProbeCount), tuning.ProbeWait / 1_000_000,
		tuning.ProbeMin / 1_000_000, tuning.ProbeMax / 1_000_000, uint64(tuning.AnnounceCount),
		tuning.AnnounceInterval / 1_000_000, tuning.DefendInterval / 1_000_000, uint64(tuning.MaxConflicts),
		tuning.ConflictRateLimit / 1_000_000, tuning.NeighborLifetime / 1_000_000, tuning.ResolveInterval / 1_000_000,
		uint64(tuning.ResolveAttempts), uint64(tuning.EchoReplyRate), tuning.DHCPInitialRetry / 1_000_000,
		tuning.DHCPMaxRetry / 1_000_000, tuning.DHCPConflictRetry / 1_000_000,
		uint64(tuning.UDP.EphemeralMin), uint64(tuning.UDP.EphemeralMax), uint64(tuning.UDP.ReceiveQueue),
		uint64(tuning.UDP.MaxEndpoints), uint64(tuning.UDP.SessionEndpoints), uint64(tuning.UDP.MaxPayload),
		uint64(tuning.TCP.EphemeralMin), uint64(tuning.TCP.EphemeralMax), uint64(tuning.TCP.MaxEndpoints),
		uint64(tuning.TCP.SessionEndpoints), uint64(tuning.TCP.ListenBacklog), uint64(tuning.TCP.SendBuffer),
		uint64(tuning.TCP.ReceiveBuffer), tuning.TCP.ConnectTimeout / 1_000_000, tuning.TCP.RTOMin / 1_000_000,
		tuning.TCP.RTOMax / 1_000_000, tuning.TCP.TimeWait / 1_000_000}
	copy(values[:], stored[:])
	return values
}
