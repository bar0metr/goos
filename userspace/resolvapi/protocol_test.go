package resolvapi

import (
	"goos/kernel/ipc"
	"goos/userspace/dns"
	"goos/userspace/hostname"
	"strings"
	"testing"
)

func name(t *testing.T, text string) hostname.Name {
	t.Helper()
	parsed, ok := hostname.Parse([]byte(text))
	if !ok {
		t.Fatalf("name %q", text)
	}
	return parsed
}

func TestRequestRoundTrip(t *testing.T) {
	longest := strings.Repeat(strings.Repeat("a", 63)+".", 3) + strings.Repeat("b", 61)
	for _, request := range []Request{
		{Opcode: OpResolve, Name: name(t, "Example.COM.")},
		{Opcode: OpResolve, Name: name(t, longest)},
		{Opcode: OpStatus}, {Opcode: OpFlush}, {Opcode: OpReload},
	} {
		var wire ipc.Message
		if !EncodeRequest(&request, 3, &wire) {
			t.Fatalf("encode %+v", request.Opcode)
		}
		if wire.PayloadSize > ipc.MaxPayload {
			t.Fatalf("payload %d", wire.PayloadSize)
		}
		var decoded Request
		if correlation, ok := DecodeRequest(&wire, &decoded); !ok || correlation != 3 || decoded != request {
			t.Fatalf("decoded %+v", decoded.Opcode)
		}
	}
}

func TestRequestRefused(t *testing.T) {
	var wire ipc.Message
	if EncodeRequest(&Request{Opcode: OpResolve}, 1, &wire) || EncodeRequest(&Request{Opcode: OpStatus, Name: name(t, "a")}, 1, &wire) ||
		EncodeRequest(&Request{Opcode: lastOpcode + 1}, 1, &wire) || EncodeRequest(&Request{Opcode: OpStatus}, 0, &wire) {
		t.Fatal("bad request encoded")
	}
	EncodeRequest(&Request{Opcode: OpResolve, Name: name(t, "a.b")}, 1, &wire)
	for label, mutate := range map[string]func(*ipc.Message){
		"size":    func(m *ipc.Message) { m.PayloadSize++ },
		"length":  func(m *ipc.Message) { m.Payload[1]++ },
		"opcode":  func(m *ipc.Message) { m.Payload[0] = byte(lastOpcode + 1) },
		"version": func(m *ipc.Message) { m.Version++ },
		"kind":    func(m *ipc.Message) { m.Kind = ipc.KindReply },
		"handle":  func(m *ipc.Message) { m.HandleCount = 1 },
	} {
		bad := wire
		mutate(&bad)
		var decoded Request
		if _, ok := DecodeRequest(&bad, &decoded); ok {
			t.Fatalf("accepted %s", label)
		}
	}
	// A name outside the syntax still decodes, empty, for an InvalidName answer.
	wire.Payload[2] = '_'
	var decoded Request
	if _, ok := DecodeRequest(&wire, &decoded); !ok || decoded.Name.Size != 0 {
		t.Fatal("invalid name not passed on")
	}
}

func TestReplyRoundTrip(t *testing.T) {
	for _, reply := range []Reply{
		{Opcode: OpResolve, Count: 2, Source: SourceCache, TTL: 30, Addresses: [MaxAddresses][4]byte{{1, 2, 3, 4}, {5, 6, 7, 8}}},
		{Opcode: OpResolve, Result: ResultNotFound},
		{Opcode: OpStatus, Count: 2, Servers: [MaxServers]Server{{[4]byte{10, 0, 2, 3}, ServerDHCP}, {[4]byte{9, 9, 9, 9}, ServerConfig}},
			Generation: 4, CacheUsed: 3, CacheLimit: 64, Positive: 2, Negative: 1, Queries: 9, Truncated: 1, ServerFailures: 2,
			TCPFallbacks: 1, TCPAnswers: 1, TCPFailures: 3, TCPTimeouts: 4},
		{Opcode: OpFlush},
		{Opcode: OpReload, Result: ResultConfigInvalid, Field: Keys},
	} {
		var wire ipc.Message
		if !EncodeReply(&reply, 5, &wire) {
			t.Fatalf("encode %+v", reply)
		}
		var decoded Reply
		if !DecodeReply(&wire, 5, reply.Opcode, &decoded) || decoded != reply {
			t.Fatalf("decoded %+v", decoded)
		}
	}
	for _, bad := range []Reply{
		{Opcode: OpResolve},
		{Opcode: OpResolve, Result: ResultNotFound, Count: 1},
		{Opcode: OpResolve, Count: 1, Addresses: [MaxAddresses][4]byte{{1}, {2}}},
		{Opcode: OpResolve, Count: 1, Queries: 1},
		{Opcode: OpStatus, Count: 1},
		{Opcode: OpFlush, TTL: 1},
		{Opcode: OpReload, Field: 1},
		{Opcode: OpReload, Result: ResultConfigInvalid, Field: Keys + 1},
		{Opcode: OpResolve, Result: lastResult + 1},
	} {
		var wire ipc.Message
		if EncodeReply(&bad, 5, &wire) && bad.Opcode != OpStatus {
			t.Fatalf("encoded %+v", bad)
		}
	}
}

func TestResultsMatchResolver(t *testing.T) {
	if ResultReconfigured != Result(dns.ResultReconfigured) || ResultUnsupportedNameScope != Result(dns.ResultUnsupportedNameScope) ||
		ResultTruncated != Result(dns.ResultTruncated) || SourceLocalhost != Source(dns.SourceLocalhost) {
		t.Fatal("result numbering differs from the resolver's")
	}
	for result := ResultOK; result <= lastResult; result++ {
		if result.Text() == "" {
			t.Fatalf("result %d has no text", result)
		}
	}
	for field := uint8(1); field <= Keys; field++ {
		if !strings.HasPrefix(Key(field), "system.dns.") {
			t.Fatalf("key %d", field)
		}
	}
}
