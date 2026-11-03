package dnsstub

import (
	"bytes"
	"encoding/binary"
	"testing"

	"goos/userspace/dns"
	"goos/userspace/dnswire"
	"goos/userspace/network"
	"goos/userspace/tcpapi"
)

func TestUDPQueryOverRealStack(t *testing.T) {
	w := newWorld(t)
	asked := query(0x4242, "WWW.Example.test", dnswire.TypeA, true, 0)
	w.ask(asked)
	upstreamQuery, port := w.upstreamQuery()
	// The resolver asks with its own ID and the folded name: nothing the
	// client wrote goes upstream but the name.
	if id := binary.BigEndian.Uint16(upstreamQuery); id == 0x4242 {
		t.Fatal("client ID reached the upstream server")
	}
	if !bytes.Contains(upstreamQuery, wireName("www.example.test")) {
		t.Fatalf("upstream query % x", upstreamQuery)
	}
	w.upstreamReply(port, answered(upstreamQuery, 60, [4]byte{10, 1, 2, 3}, [4]byte{10, 1, 2, 4}))
	got := parse(t, w.answer())
	question := asked[dnswire.HeaderSize : len(asked)-dnswire.OPTSize]
	if got.id != 0x4242 || !bytes.Equal(got.question, question) || got.rcode != dnswire.RcodeNoError || !got.opt ||
		got.flags&(dnswire.FlagQR|dnswire.FlagRD|dnswire.FlagRA) != dnswire.FlagQR|dnswire.FlagRD|dnswire.FlagRA ||
		len(got.addresses) != 2 || got.addresses[0] != [4]byte{10, 1, 2, 3} || got.ttl != 60 {
		t.Fatalf("%+v", got)
	}

	// The cache answers the same name, and the AAAA question for it, with
	// no upstream query.
	queries := w.resolver.Stats().Queries
	w.ask(query(0x4343, "www.example.test", dnswire.TypeA, false, 0))
	if cached := parse(t, w.answer()); cached.id != 0x4343 || len(cached.addresses) != 2 || cached.opt {
		t.Fatalf("cached %+v", cached)
	}
	w.ask(query(0x4444, "www.example.test", dnswire.TypeAAAA, false, 0))
	if empty := parse(t, w.answer()); empty.id != 0x4444 || empty.rcode != dnswire.RcodeNoError || len(empty.addresses) != 0 {
		t.Fatalf("AAAA %+v", empty)
	}
	// localhost and the hosts table never go upstream either.
	w.ask(query(1, "localhost", dnswire.TypeA, false, 0))
	if local := parse(t, w.answer()); len(local.addresses) != 1 || local.addresses[0] != [4]byte{127, 0, 0, 1} {
		t.Fatalf("localhost %+v", local)
	}
	w.hosts["printer.lan"] = [4]byte{192, 168, 1, 9}
	w.ask(query(2, "Printer.LAN", dnswire.TypeA, false, 0))
	if listed := parse(t, w.answer()); len(listed.addresses) != 1 || listed.addresses[0] != [4]byte{192, 168, 1, 9} || listed.ttl != 0 {
		t.Fatalf("hosts %+v", listed)
	}
	if stats := w.resolver.Stats(); stats.Queries != queries || w.upstreamQueued() != 0 {
		t.Fatalf("upstream queries %d, was %d", stats.Queries, queries)
	}
	if live, _ := w.stub.Live(); live != 0 {
		t.Fatalf("%d requests left", live)
	}
	if stats := w.stub.Stats(); stats.UDPQueries != 5 || stats.Replies != 5 {
		t.Fatalf("stub stats %+v", stats)
	}
}

// A truncated upstream answer is repeated over TCP; the client gets the
// address over UDP, in a reply that fits a classic datagram.
func TestTruncationFallsBackToTCP(t *testing.T) {
	w := newWorld(t)
	w.ask(query(0x5151, "big.test", dnswire.TypeA, false, 0))
	upstreamQuery, port := w.upstreamQuery()
	w.upstreamReply(port, header(upstreamQuery, 0x8380))
	accepted := w.tcpCall(w.serverTCP, tcpapi.Request{Opcode: tcpapi.OpAccept, Endpoint: w.listener})
	if accepted.Result != tcpapi.ResultOK {
		t.Fatalf("accept %+v", accepted)
	}
	prefix := w.readTCP(w.serverTCP, accepted.Endpoint, 2)
	streamed := w.readTCP(w.serverTCP, accepted.Endpoint, int(binary.BigEndian.Uint16(prefix)))
	if !bytes.Equal(streamed, upstreamQuery) {
		t.Fatal("TCP query differs from the datagram")
	}
	w.writeTCP(w.serverTCP, accepted.Endpoint, framed(big(streamed, [4]byte{10, 9, 8, 7})))
	reply := w.answer()
	got := parse(t, reply)
	if got.id != 0x5151 || len(got.addresses) != 1 || got.addresses[0] != [4]byte{10, 9, 8, 7} || len(reply) > 512 ||
		got.flags&dnswire.FlagTC != 0 {
		t.Fatalf("%+v (%d bytes)", got, len(reply))
	}
	if stats := w.resolver.Stats(); stats.TCPFallbacks != 1 || stats.TCPAnswers != 1 {
		t.Fatalf("stats %+v", stats)
	}
	w.tcpCall(w.serverTCP, tcpapi.Request{Opcode: tcpapi.OpClose, Endpoint: accepted.Endpoint})
}

func TestFailuresMapToRcodes(t *testing.T) {
	w := newWorld(t)
	policy := dns.DefaultPolicy()
	policy.Attempts, policy.FailureTTL = 1, 1
	w.resolver.SetPolicy(policy, w.now)

	w.ask(query(1, "missing.test", dnswire.TypeAAAA, false, 0))
	upstreamQuery, port := w.upstreamQuery()
	w.upstreamReply(port, header(upstreamQuery, 0x8183))
	if got := parse(t, w.answer()); got.id != 1 || got.rcode != dnswire.RcodeNXDomain {
		t.Fatalf("NXDOMAIN %+v", got)
	}

	w.ask(query(2, "broken.test", dnswire.TypeA, false, 0))
	upstreamQuery, port = w.upstreamQuery()
	w.upstreamReply(port, header(upstreamQuery, 0x8182))
	if got := parse(t, w.answer()); got.id != 2 || got.rcode != dnswire.RcodeServFail {
		t.Fatalf("SERVFAIL %+v", got)
	}

	// No answer at all: the resolver's deadline ends the lookup.
	w.ask(query(3, "silent.test", dnswire.TypeA, false, 0))
	w.upstreamQuery()
	w.advance(w.now + policy.QueryTimeout + 1)
	if got := parse(t, w.answer()); got.id != 3 || got.rcode != dnswire.RcodeServFail {
		t.Fatalf("timeout %+v", got)
	}
	if stats := w.resolver.Stats(); stats.Timeouts == 0 {
		t.Fatalf("stats %+v", stats)
	}
}

func TestMalformedAndUnsupportedQueries(t *testing.T) {
	w := newWorld(t)
	two := query(7, "a.test", dnswire.TypeA, false, 0)
	binary.BigEndian.PutUint16(two[4:], 2)
	mx := query(8, "a.test", dnswire.TypeMX, false, 0)
	chaos := query(9, "a.test", dnswire.TypeA, false, 0)
	binary.BigEndian.PutUint16(chaos[len(chaos)-2:], 3)
	update := query(10, "a.test", dnswire.TypeA, false, 0)
	update[2] |= 0x28
	for _, test := range []struct {
		name    string
		message []byte
		id      uint16
		rcode   uint16
	}{
		{"two questions", two, 7, dnswire.RcodeFormErr},
		{"MX", mx, 8, dnswire.RcodeNotImp},
		{"class CH", chaos, 9, dnswire.RcodeRefused},
		{"opcode UPDATE", update, 10, dnswire.RcodeNotImp},
		{"EDNS version 1", query(11, "a.test", dnswire.TypeA, true, 1), 11, dnswire.RcodeBadVers},
		{"not a host name", query(12, "_sip._tcp.test", dnswire.TypeA, false, 0), 12, dnswire.RcodeRefused},
		{".local", query(13, "printer.local", dnswire.TypeA, false, 0), 13, dnswire.RcodeRefused},
	} {
		w.ask(test.message)
		if got := parse(t, w.answer()); got.id != test.id || got.rcode != test.rcode {
			t.Errorf("%s: %+v, want rcode %d", test.name, got, test.rcode)
		}
	}
	// A response and a runt get nothing back.
	response := query(14, "a.test", dnswire.TypeA, false, 0)
	response[2] |= 0x80
	w.ask(response)
	w.ask([]byte{1, 2, 3})
	if w.pending() != 0 || w.upstreamQueued() != 0 {
		t.Fatalf("replies %d upstream %d", w.pending(), w.upstreamQueued())
	}
	if stats := w.stub.Stats(); stats.Ignored != 2 || stats.Malformed != 1 {
		t.Fatalf("stats %+v", stats)
	}
}

// The stub takes MaxUDPRequests datagrams at a time; the rest wait in the
// network service's queue, which is where a flood ends. Every client gets
// its own ID back.
func TestUDPRequestsAreBounded(t *testing.T) {
	w := newWorld(t)
	// The client's own queue holds 8 datagrams by default; it reads
	// nothing until the end.
	const count = 8
	for id := uint16(1); id <= count; id++ {
		w.ask(query(id, "same.test", dnswire.TypeA, false, 0))
	}
	if live, _ := w.stub.Live(); live != MaxUDPRequests {
		t.Fatalf("%d requests live", live)
	}
	queued, _ := w.stack.UDPQueued(ownerResolverUDP, network.EndpointID(w.stub.udp.endpoint))
	if queued != count-MaxUDPRequests {
		t.Fatalf("%d datagrams queued for the stub", queued)
	}
	upstreamQuery, port := w.upstreamQuery()
	if w.upstreamQueued() != 0 {
		t.Fatal("one name drew more than one upstream query")
	}
	w.upstreamReply(port, answered(upstreamQuery, 30, [4]byte{10, 0, 0, 1}))
	seen := map[uint16]bool{}
	for range count {
		got := parse(t, w.answer())
		if got.rcode != dnswire.RcodeNoError || len(got.addresses) != 1 || seen[got.id] {
			t.Fatalf("%+v", got)
		}
		seen[got.id] = true
	}
	if w.maxLive > MaxUDPRequests {
		t.Fatalf("%d requests were live at once", w.maxLive)
	}

	// Names enough to fill every resolver transaction: the rest are told
	// SERVFAIL at once rather than held.
	for id := uint16(1); id <= dns.MaxTransactions+2; id++ {
		w.ask(query(100+id, "n"+string(rune('a'+id))+".test", dnswire.TypeA, false, 0))
	}
	failed := 0
	for w.pending() > 0 {
		if got := parse(t, w.answer()); got.rcode == dnswire.RcodeServFail {
			failed++
		}
	}
	if failed != 2 {
		t.Fatalf("%d SERVFAIL replies, want 2", failed)
	}
}

func (w *world) readTCP(peer *tcpPeer, endpoint uint32, count int) []byte {
	w.t.Helper()
	var data []byte
	for len(data) < count {
		reply := w.tcpCall(peer, tcpapi.Request{Opcode: tcpapi.OpRead, Endpoint: endpoint, Length: uint16(min(count-len(data), tcpapi.SlotSize))})
		if reply.Result != tcpapi.ResultOK {
			w.t.Fatalf("read %+v", reply)
		}
		offset := tcpapi.Offset(reply.Slot)
		data = append(data, peer.rx[offset:offset+uint64(reply.Length)]...)
		peer.session.Handle(w.stack, &tcpapi.Request{Opcode: tcpapi.OpRelease, Slot: reply.Slot}, 0, w.now, tcpBack{w.t, &peer.replies})
	}
	return data
}

func (w *world) writeTCP(peer *tcpPeer, endpoint uint32, data []byte) {
	w.t.Helper()
	for len(data) > 0 {
		length := copy(peer.tx[:tcpapi.SlotSize], data)
		reply := w.tcpCall(peer, tcpapi.Request{Opcode: tcpapi.OpWrite, Endpoint: endpoint, Length: uint16(length)})
		if reply.Result != tcpapi.ResultOK || reply.Length == 0 {
			w.t.Fatalf("write %+v", reply)
		}
		data = data[reply.Length:]
		w.pump()
	}
}

// connect opens a client connection to the stub.
func (w *world) connect() uint32 {
	w.t.Helper()
	endpoint := w.tcpCall(w.clientTCP, tcpapi.Request{Opcode: tcpapi.OpOpen}).Endpoint
	if connected := w.tcpCall(w.clientTCP, tcpapi.Request{Opcode: tcpapi.OpConnect, Endpoint: endpoint, Address: Address, Port: Port}); connected.Result != tcpapi.ResultOK {
		w.t.Fatalf("connect %+v", connected)
	}
	// Loopback connects at once; let the stub see the connection.
	w.pump()
	return endpoint
}

// readReply reads one framed reply.
func (w *world) readReply(endpoint uint32) parsed {
	w.t.Helper()
	prefix := w.readTCP(w.clientTCP, endpoint, 2)
	return parse(w.t, w.readTCP(w.clientTCP, endpoint, int(binary.BigEndian.Uint16(prefix))))
}

// Two pipelined queries, the first split across its prefix, come back as
// two framed replies in order, each with its own ID.
func TestTCPFramingAndPipelining(t *testing.T) {
	w := newWorld(t)
	w.hosts["one.lan"] = [4]byte{10, 0, 0, 1}
	endpoint := w.connect()
	stream := append(framed(query(0x0101, "one.lan", dnswire.TypeA, false, 0)), framed(query(0x0202, "localhost", dnswire.TypeA, true, 0))...)
	w.writeTCP(w.clientTCP, endpoint, stream[:1])
	w.writeTCP(w.clientTCP, endpoint, stream[1:])
	first, second := w.readReply(endpoint), w.readReply(endpoint)
	if first.id != 0x0101 || len(first.addresses) != 1 || first.addresses[0] != [4]byte{10, 0, 0, 1} {
		t.Fatalf("first %+v", first)
	}
	if second.id != 0x0202 || !second.opt || len(second.addresses) != 1 || second.addresses[0] != [4]byte{127, 0, 0, 1} {
		t.Fatalf("second %+v", second)
	}

	// A query that needs the upstream server waits for it on the stream.
	w.writeTCP(w.clientTCP, endpoint, framed(query(0x0303, "far.test", dnswire.TypeA, false, 0)))
	upstreamQuery, port := w.upstreamQuery()
	w.upstreamReply(port, answered(upstreamQuery, 5, [4]byte{10, 3, 3, 3}))
	if third := w.readReply(endpoint); third.id != 0x0303 || third.addresses[0] != [4]byte{10, 3, 3, 3} {
		t.Fatalf("third %+v", third)
	}

	// A length prefix past MaxStreamQuery ends the connection.
	w.writeTCP(w.clientTCP, endpoint, binary.BigEndian.AppendUint16(nil, MaxStreamQuery+1))
	if eof := w.tcpCall(w.clientTCP, tcpapi.Request{Opcode: tcpapi.OpRead, Endpoint: endpoint, Length: 16}); eof.Result != tcpapi.ResultEOF {
		t.Fatalf("read after an oversized prefix %+v", eof)
	}
	w.tcpCall(w.clientTCP, tcpapi.Request{Opcode: tcpapi.OpClose, Endpoint: endpoint})
	w.pump()
	if _, connections := w.stub.Live(); connections != 0 {
		t.Fatalf("%d connections left", connections)
	}
	if stats := w.stub.Stats(); stats.TCPQueries != 3 || stats.Connections != 1 {
		t.Fatalf("stats %+v", stats)
	}
}

// The stub serves MaxConnections at a time; another waits in the backlog
// until one ends. Idle connections are closed after IdleTimeout, and a
// client that closes first leaves nothing behind on the resolver's session.
func TestTCPConnectionsAreBounded(t *testing.T) {
	w := newWorld(t)
	var endpoints []uint32
	for range MaxConnections + 1 {
		endpoints = append(endpoints, w.connect())
	}
	if _, connections := w.stub.Live(); connections != MaxConnections {
		t.Fatalf("%d connections accepted", connections)
	}
	// The one in the backlog is served once the first client leaves.
	w.tcpCall(w.clientTCP, tcpapi.Request{Opcode: tcpapi.OpClose, Endpoint: endpoints[0]})
	w.pump()
	if stats := w.stub.Stats(); stats.Connections != MaxConnections+1 {
		t.Fatalf("stats %+v", stats)
	}
	last := endpoints[MaxConnections]
	w.writeTCP(w.clientTCP, last, framed(query(9, "localhost", dnswire.TypeA, false, 0)))
	if got := w.readReply(last); got.id != 9 {
		t.Fatalf("%+v", got)
	}
	w.advance(w.now + IdleTimeout + 1)
	if _, connections := w.stub.Live(); connections != 0 {
		t.Fatalf("%d connections after the idle timeout", connections)
	}
	if stats := w.stub.Stats(); stats.Timeouts != MaxConnections {
		t.Fatalf("stats %+v", stats)
	}
	for _, endpoint := range endpoints[1:] {
		if eof := w.tcpCall(w.clientTCP, tcpapi.Request{Opcode: tcpapi.OpRead, Endpoint: endpoint, Length: 16}); eof.Result != tcpapi.ResultEOF {
			t.Fatalf("read after the stub closed %+v", eof)
		}
		w.tcpCall(w.clientTCP, tcpapi.Request{Opcode: tcpapi.OpClose, Endpoint: endpoint})
	}
	// TIME_WAIT on the stub's side ends; only the listener stays on the
	// resolver's session, next to the upstream server's listener.
	w.advance(w.now + 2*60*1_000_000_000)
	if stats := w.stack.TCPStats(); stats.Endpoints != 2 || stats.Listeners != 2 {
		t.Fatalf("TCP endpoints %d listeners %d: %v", stats.Endpoints, stats.Listeners, stats.States)
	}
}

// Dropping the sessions ends what was in progress on them; new sessions
// get a new endpoint and listener on the same address and port.
func TestSessionsReplaced(t *testing.T) {
	w := newWorld(t)
	w.ask(query(1, "slow.test", dnswire.TypeA, false, 0))
	if live, _ := w.stub.Live(); live != 1 {
		t.Fatalf("%d live", live)
	}
	w.udp.Unbind()
	w.resolver.Reset(w.now)
	w.stub.UnbindUDP(w.resolver)
	w.tcp.Unbind(w.resolver, w.now)
	w.stub.UnbindTCP(w.resolver)
	w.udpServe.End(w.stack)
	w.tcpServe.End(w.stack, w.now)
	if live, _ := w.stub.Live(); live != 0 {
		t.Fatalf("%d live after unbind", live)
	}
	w.udpTX, w.udpRX = udpPool()
	w.udp.Bind(w.udpTX, w.udpRX)
	w.udpServe.Start(w.stack, ownerResolverUDP, w.udpTX, w.udpRX)
	w.stub.BindUDP(w.udpTX, w.udpRX, &w.udp, w.now)
	w.tcpTX, w.tcpRX = tcpPool()
	w.tcp.Bind(w.tcpTX, w.tcpRX)
	w.tcpServe.Start(w.stack, ownerResolverTCP, w.tcpTX, w.tcpRX, w.now)
	w.stub.BindTCP(w.tcpTX, w.tcpRX, &w.tcp, w.now)
	w.pump()
	// The query that was in flight went to the old session; the client asks
	// again and gets its answer.
	for w.upstreamQueued() > 0 {
		w.upstreamQuery()
	}
	w.ask(query(2, "slow.test", dnswire.TypeA, false, 0))
	upstreamQuery, port := w.upstreamQuery()
	w.upstreamReply(port, answered(upstreamQuery, 5, [4]byte{10, 5, 5, 5}))
	if got := parse(t, w.answer()); got.id != 2 || got.addresses[0] != [4]byte{10, 5, 5, 5} {
		t.Fatalf("%+v", got)
	}
	endpoint := w.connect()
	w.writeTCP(w.clientTCP, endpoint, framed(query(3, "slow.test", dnswire.TypeA, false, 0)))
	if got := w.readReply(endpoint); got.id != 3 || got.addresses[0] != [4]byte{10, 5, 5, 5} {
		t.Fatalf("TCP %+v", got)
	}
}
