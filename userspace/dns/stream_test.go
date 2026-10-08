package dns

import (
	"bytes"
	"testing"
)

func TestTruncatedRepeatsOverTCP(t *testing.T) {
	h := newHarness(t, serverA, serverB)
	h.pending(0, "big.test")
	first := h.sent()
	// A truncated answer may be cut short; its records are not read.
	payload := reply{id: first.id, flags: flagTC, question: "big.test", opt: true,
		answers: []rr{aRecord("big.test", 60, addr(1))}}.bytes()
	h.deliver(first, payload[:len(payload)-3])
	h.noCompletion()
	h.noQuery()
	if h.r.Waiting(first.Transaction) {
		t.Fatal("datagram still waited for after TC=1")
	}
	repeat := h.stream()
	if repeat.Server != serverA || repeat.Transaction != first.Transaction || !bytes.Equal(repeat.raw, first.raw) {
		t.Fatalf("TCP query %+v, want the datagram % x", repeat, first.raw)
	}
	if !h.r.StreamWaiting(repeat.Transaction, repeat.Serial) {
		t.Fatal("not waiting for the TCP answer")
	}
	h.noStream()
	h.answerStream(repeat, reply{answers: []rr{aRecord("big.test", 60, addr(2))}})
	answer := h.completion(0)
	expectResult(t, answer, ResultOK, SourceUpstream)
	expectAddresses(t, answer, addr(2))
	if h.r.StreamWaiting(repeat.Transaction, repeat.Serial) {
		t.Fatal("still waiting after the answer")
	}
	h.cachedOK("big.test")
	stats := h.r.Stats()
	if stats.Queries != 1 || stats.Truncated != 1 || stats.TCPFallbacks != 1 || stats.TCPAnswers != 1 ||
		stats.TCPFailures != 0 || stats.TCPTimeouts != 0 {
		t.Fatalf("stats %+v", stats)
	}
}

func TestStreamKeepsClassicForm(t *testing.T) {
	h := newHarness(t, serverA)
	h.pending(0, "old.test")
	h.answer(h.sent(), reply{flags: rcodeFormErr, noQuestion: true})
	classic := h.sent()
	if classic.opt {
		t.Fatal("retry still has OPT")
	}
	h.truncate(classic)
	repeat := h.stream()
	if repeat.opt || repeat.id != classic.id || !bytes.Equal(repeat.raw, classic.raw) {
		t.Fatalf("TCP query %+v, want the classic datagram", repeat)
	}
	// FORMERR over TCP fails the server instead of starting another round
	// of EDNS(0) negotiation.
	h.answerStream(repeat, reply{flags: rcodeFormErr})
	h.noQuery()
	h.noStream()
	expectResult(t, h.completion(0), ResultTemporaryFailure, SourceUpstream)
}

func TestStreamNXDomainIsCached(t *testing.T) {
	h := newHarness(t, serverA)
	h.pending(0, "gone.test")
	h.truncate(h.sent())
	h.answerStream(h.stream(), reply{flags: rcodeNXDomain, authority: []rr{soaRecord("test", 300, 60)}})
	answer := h.completion(0)
	expectResult(t, answer, ResultNotFound, SourceUpstream)
	if answer.TTL != 60 {
		t.Fatalf("negative TTL %d", answer.TTL)
	}
	if cached, done := h.lookup(1, "gone.test"); !done || cached.Result != ResultNotFound || cached.Source != SourceCache {
		t.Fatalf("not negatively cached: %+v", cached)
	}
}

func TestStreamCNAMEInOneResponse(t *testing.T) {
	h := newHarness(t, serverA)
	h.pending(0, "alias.test")
	h.truncate(h.sent())
	h.answerStream(h.stream(), reply{answers: []rr{cnameRecord("alias.test", "real.test", 60),
		aRecord("real.test", 60, addr(7))}})
	answer := h.completion(0)
	expectResult(t, answer, ResultOK, SourceUpstream)
	expectAddresses(t, answer, addr(7))
	h.cachedOK("real.test")
}

// The CNAME target's own query starts over UDP, and only its own TC=1 sends
// it over TCP.
func TestStreamCNAMETargetStartsOverUDP(t *testing.T) {
	h := newHarness(t, serverA)
	h.pending(0, "alias.test")
	h.truncate(h.sent())
	h.answerStream(h.stream(), reply{answers: []rr{cnameRecord("alias.test", "real.test", 60)}})
	h.noStream()
	target := h.sent()
	if target.name != "real.test" || target.Server != serverA {
		t.Fatalf("target query %+v", target)
	}
	h.truncate(target)
	repeat := h.stream()
	if repeat.name != "real.test" || repeat.id != target.id {
		t.Fatalf("target TCP query %+v", repeat)
	}
	h.answerStream(repeat, reply{answers: []rr{aRecord("real.test", 60, addr(8))}})
	expectAddresses(t, h.completion(0), addr(8))
	if stats := h.r.Stats(); stats.TCPFallbacks != 2 || stats.Queries != 2 {
		t.Fatalf("stats %+v", stats)
	}
}

func TestStreamBadAnswersMoveOn(t *testing.T) {
	for _, test := range []struct {
		label string
		bad   func(h *harness, q query)
		check func(stats Stats) bool
	}{
		{"malformed", func(h *harness, q query) {
			payload := reply{id: q.id, question: q.name, answers: []rr{aRecord(q.name, 60, addr(1))}}.bytes()
			h.r.ReceiveStream(q.Transaction, q.Serial, payload[:len(payload)-1], h.now)
		}, func(stats Stats) bool { return stats.Malformed == 1 }},
		{"wrong id", func(h *harness, q query) {
			payload := reply{id: q.id + 1, question: q.name, answers: []rr{aRecord(q.name, 60, addr(1))}}.bytes()
			h.r.ReceiveStream(q.Transaction, q.Serial, payload, h.now)
		}, func(stats Stats) bool { return stats.Mismatched == 1 }},
		{"wrong question", func(h *harness, q query) {
			payload := reply{id: q.id, question: "other.test", answers: []rr{aRecord("other.test", 60, addr(1))}}.bytes()
			h.r.ReceiveStream(q.Transaction, q.Serial, payload, h.now)
		}, func(stats Stats) bool { return stats.Mismatched == 1 }},
		{"truncated again", func(h *harness, q query) {
			h.answerStream(q, reply{flags: flagTC, answers: []rr{aRecord(q.name, 60, addr(1))}})
		}, func(stats Stats) bool { return stats.Truncated == 2 }},
		{"servfail", func(h *harness, q query) {
			h.answerStream(q, reply{flags: rcodeServFail})
		}, func(stats Stats) bool { return stats.ServerFailures == 2 && stats.TCPAnswers == 1 }},
		{"connection failed", func(h *harness, q query) {
			h.r.StreamFailed(q.Transaction, q.Serial, false, h.now)
		}, func(stats Stats) bool { return stats.TCPTimeouts == 0 }},
	} {
		t.Run(test.label, func(t *testing.T) {
			h := newHarness(t, serverA, serverB)
			h.pending(0, "big.test")
			h.truncate(h.sent())
			repeat := h.stream()
			test.bad(h, repeat)
			h.checkDeadline()
			if h.r.StreamWaiting(repeat.Transaction, repeat.Serial) {
				t.Fatal("still waiting for the TCP answer")
			}
			// No second TCP fallback: the next server is asked over UDP.
			h.noStream()
			next := h.sent()
			if next.Server != serverB {
				t.Fatalf("next query %+v", next)
			}
			h.answer(next, reply{flags: rcodeServFail})
			expectResult(t, h.completion(0), ResultTemporaryFailure, SourceUpstream)
			h.cachedFailure("big.test", ResultTemporaryFailure)
			stats := h.r.Stats()
			wantFailures := uint64(1)
			if test.label == "servfail" {
				wantFailures = 0
			}
			if stats.TCPFallbacks != 1 || stats.TCPFailures != wantFailures || stats.Positive != 0 || !test.check(stats) {
				t.Fatalf("stats %+v", stats)
			}
		})
	}
}

// A TC=1 FORMERR without a question cannot prove it answers the query, so
// it is malformed rather than a reason to open TCP.
func TestTruncatedWithoutQuestionIsMalformed(t *testing.T) {
	h := newHarness(t, serverA, serverB)
	h.pending(0, "old.test")
	h.answer(h.sent(), reply{flags: rcodeFormErr | flagTC, noQuestion: true})
	h.noStream()
	if next := h.sent(); next.Server != serverB {
		t.Fatalf("next query %+v", next)
	}
	if stats := h.r.Stats(); stats.Malformed != 1 || stats.Truncated != 0 || stats.TCPFallbacks != 0 {
		t.Fatalf("stats %+v", stats)
	}
}

// A timed-out fallback keeps the server, and the fallback used up no logical
// attempt: with two attempts the server is asked over UDP once more.
func TestStreamTimeoutKeepsServer(t *testing.T) {
	h := newHarness(t, serverA)
	h.pending(0, "big.test")
	first := h.sent()
	h.truncate(first)
	repeat := h.stream()
	h.r.StreamFailed(repeat.Transaction, repeat.Serial, true, h.now)
	second := h.sent()
	if second.Server != serverA || second.id == first.id {
		t.Fatalf("second attempt %+v", second)
	}
	h.answer(second, reply{answers: []rr{aRecord("big.test", 60, addr(3))}})
	expectAddresses(t, h.completion(0), addr(3))
	if stats := h.r.Stats(); stats.Queries != 2 || stats.TCPTimeouts != 1 || stats.Timeouts != 1 {
		t.Fatalf("stats %+v", stats)
	}
}

func TestStreamWaitEnds(t *testing.T) {
	h := newHarness(t, serverA)
	policy := DefaultPolicy()
	policy.Attempts = 1
	h.r.SetPolicy(policy, h.now)
	h.pending(0, "big.test")
	h.truncate(h.sent())
	repeat := h.stream()
	start := h.now
	h.toDeadline()
	if h.now-start != policy.QueryTimeout {
		t.Fatalf("TCP wait %d, want %d", h.now-start, policy.QueryTimeout)
	}
	if h.r.StreamWaiting(repeat.Transaction, repeat.Serial) {
		t.Fatal("still waiting after the deadline")
	}
	expectResult(t, h.completion(0), ResultTimeout, SourceUpstream)
	// A late answer for the finished exchange changes nothing.
	h.answerStream(repeat, reply{answers: []rr{aRecord("big.test", 60, addr(1))}})
	h.noCompletion()
	h.cachedFailure("big.test", ResultTimeout)
	if stats := h.r.Stats(); stats.TCPTimeouts != 1 || stats.Timeouts != 1 || stats.TCPAnswers != 0 {
		t.Fatalf("stats %+v", stats)
	}
}

// A fallback never outlives the resolution's deadline, even when the TCP
// wait would.
func TestStreamCutByDeadline(t *testing.T) {
	h := newHarness(t, serverA)
	policy := DefaultPolicy()
	policy.Attempts = 2
	h.r.SetPolicy(policy, h.now)
	h.pending(0, "big.test")
	h.toDeadline()
	second := h.sent()
	h.advance(policy.QueryTimeout - minWait)
	h.truncate(second)
	h.stream()
	deadline, _ := h.r.Deadline()
	if want := h.now + minWait; deadline != want {
		t.Fatalf("TCP wait ends at %d, want the resolution deadline %d", deadline, want)
	}
}

// A fallback the transport never takes ends with the same wait.
func TestQueuedStreamTimesOut(t *testing.T) {
	h := newHarness(t, serverA)
	policy := DefaultPolicy()
	policy.Attempts = 1
	h.r.SetPolicy(policy, h.now)
	h.pending(0, "big.test")
	h.truncate(h.sent())
	h.toDeadline()
	h.noStream()
	expectResult(t, h.completion(0), ResultTimeout, SourceUpstream)
	if stats := h.r.Stats(); stats.TCPTimeouts != 1 || stats.TCPFallbacks != 0 {
		t.Fatalf("stats %+v", stats)
	}
}

func TestStreamCancelAndReset(t *testing.T) {
	h := newHarness(t, serverA)
	h.pending(0, "big.test")
	h.truncate(h.sent())
	repeat := h.stream()
	h.r.Cancel(0)
	if h.r.StreamWaiting(repeat.Transaction, repeat.Serial) {
		t.Fatal("cancelled lookup still waits for TCP")
	}
	if _, ok := h.r.Deadline(); ok {
		t.Fatal("cancelled lookup keeps a timer")
	}

	h.pending(0, "big.test")
	h.truncate(h.sent())
	h.r.Reset(h.now)
	h.noStream()
	expectResult(t, h.completion(0), ResultReconfigured, SourceUpstream)
}

// A new transaction in the same slot does not take an answer meant for an
// earlier exchange.
func TestStaleStreamIgnored(t *testing.T) {
	h := newHarness(t, serverA)
	h.pending(0, "big.test")
	h.truncate(h.sent())
	old := h.stream()
	h.r.Cancel(0)
	h.pending(0, "big.test")
	h.truncate(h.sent())
	current := h.stream()
	if current.Transaction != old.Transaction || current.Serial == old.Serial {
		t.Fatalf("old %+v current %+v", old.Outgoing, current.Outgoing)
	}
	h.r.StreamFailed(old.Transaction, old.Serial, false, h.now)
	h.r.ReceiveStream(old.Transaction, old.Serial, reply{id: current.id, question: "big.test",
		answers: []rr{aRecord("big.test", 60, addr(9))}}.bytes(), h.now)
	if !h.r.StreamWaiting(current.Transaction, current.Serial) {
		t.Fatal("stale exchange ended the current one")
	}
	h.noCompletion()
	h.answerStream(current, reply{answers: []rr{aRecord("big.test", 60, addr(4))}})
	expectAddresses(t, h.completion(0), addr(4))
}

// Two fallbacks per server: the third TC=1 fails the server.
func TestStreamBudget(t *testing.T) {
	h := newHarness(t, serverA)
	policy := DefaultPolicy()
	policy.Attempts = 3
	h.r.SetPolicy(policy, h.now)
	h.pending(0, "big.test")
	for round := 0; round < maxStreams; round++ {
		h.truncate(h.sent())
		repeat := h.stream()
		h.r.StreamFailed(repeat.Transaction, repeat.Serial, true, h.now)
	}
	h.truncate(h.sent())
	h.noStream()
	h.noQuery()
	expectResult(t, h.completion(0), ResultTemporaryFailure, SourceUpstream)
	if stats := h.r.Stats(); stats.Queries != 3 || stats.TCPFallbacks != maxStreams || stats.Truncated != 3 {
		t.Fatalf("stats %+v", stats)
	}
}

// EDNS UDP, the classic retry and a TCP fallback fit one server's budget
// together with a CNAME target's query and its own fallback.
func TestBudgetsCoexist(t *testing.T) {
	h := newHarness(t, serverA)
	h.pending(0, "alias.test")
	h.answer(h.sent(), reply{flags: rcodeFormErr, noQuestion: true})
	h.truncate(h.sent())
	h.answerStream(h.stream(), reply{answers: []rr{cnameRecord("alias.test", "real.test", 60)}})
	target := h.sent()
	if target.opt {
		t.Fatal("server known to reject EDNS(0) got OPT")
	}
	h.truncate(target)
	h.answerStream(h.stream(), reply{answers: []rr{aRecord("real.test", 60, addr(5))}})
	expectAddresses(t, h.completion(0), addr(5))
	if stats := h.r.Stats(); stats.Queries != maxTransmissions || stats.TCPFallbacks != maxStreams {
		t.Fatalf("stats %+v", stats)
	}
}

// A message too long for a datagram is fine over TCP, up to the largest the
// length prefix can describe.
func TestStreamTakesLongMessages(t *testing.T) {
	h := newHarness(t, serverA)
	h.pending(0, "big.test")
	first := h.sent()
	long := reply{id: first.id, question: "big.test", answers: []rr{aRecord("big.test", 60, addr(1))}}
	for len(long.bytes()) < DatagramSize*4 {
		long.additional = append(long.additional, rr{name: "big.test", kind: 16, ttl: 60, data: make([]byte, 255)})
	}
	payload := long.bytes()
	h.deliver(first, payload)
	h.noStream()
	if stats := h.r.Stats(); stats.Malformed != 1 {
		t.Fatalf("long datagram accepted: %+v", stats)
	}

	h = newHarness(t, serverA)
	h.pending(0, "big.test")
	h.truncate(h.sent())
	repeat := h.stream()
	long.id = repeat.id
	h.r.ReceiveStream(repeat.Transaction, repeat.Serial, long.bytes(), h.now)
	expectAddresses(t, h.completion(0), addr(1))
}

func TestUnusableStreamDst(t *testing.T) {
	h := newHarness(t, serverA)
	h.pending(0, "big.test")
	h.truncate(h.sent())
	if _, ok := h.r.NextStream(h.buf[:MaxQuery-1]); ok {
		t.Fatal("short dst took the query")
	}
	h.stream()
}
