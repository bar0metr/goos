package tcpupstream

import (
	"encoding/binary"
	"testing"

	"goos/kernel/ipc"
	"goos/userspace/dns"
	"goos/userspace/hostname"
	"goos/userspace/network"
	"goos/userspace/tcpapi"
	"goos/userspace/tcpserve"
	"goos/userspace/udpapi"
	"goos/userspace/udpserve"
	"goos/userspace/upstream"
)

// The fallback end to end: the resolver core, its UDP and TCP clients, the
// network service's GUDP1 and GTCP1 session logic and a real stack, with
// every message passing through the codecs as it does between the services.
// The DNS server is a UDP session and a TCP session bound to the stack's
// own address, so packets travel by local delivery.

var host = network.IPv4{10, 0, 2, 15}

type udpMessage struct {
	request     udpapi.Request
	reply       udpapi.Reply
	correlation uint64
}

type tcpMessage struct {
	request     tcpapi.Request
	reply       tcpapi.Reply
	correlation uint64
}

type world struct {
	t        *testing.T
	stack    *network.Stack
	resolver *dns.Resolver
	now      uint64

	udp                      upstream.Client
	udpServe                 udpserve.Session
	udpServer                udpserve.Session
	toUDP                    []udpMessage
	fromUDP                  []udpMessage
	udpReplies               []udpMessage
	udpTX, udpRX             []byte
	udpServerTX, udpServerRX []byte
	udpServerID              uint32

	tcp                      Client
	tcpServe                 tcpserve.Session
	tcpServer                tcpserve.Session
	toTCP                    []tcpMessage
	fromTCP                  []tcpMessage
	tcpReplies               []tcpMessage
	tcpTX, tcpRX             []byte
	tcpServerTX, tcpServerRX []byte
	listener                 uint32

	serverSeq uint64
}

type udpOut struct{ w *world }

func (o udpOut) Send(request *udpapi.Request, correlation uint64) bool {
	o.w.t.Helper()
	var wire ipc.Message
	if !udpapi.EncodeRequest(request, correlation, &wire) {
		o.w.t.Fatalf("invalid UDP request %+v", request)
	}
	var decoded udpapi.Request
	got, ok := udpapi.DecodeRequest(&wire, &decoded)
	if !ok {
		o.w.t.Fatalf("UDP request does not decode: %+v", request)
	}
	o.w.toUDP = append(o.w.toUDP, udpMessage{request: decoded, correlation: got})
	return true
}

type udpBack struct {
	w    *world
	into *[]udpMessage
}

func (b udpBack) Reply(reply *udpapi.Reply, correlation uint64) bool {
	var wire ipc.Message
	if !udpapi.EncodeReply(reply, correlation, &wire) {
		b.w.t.Fatalf("invalid UDP reply %+v", reply)
	}
	var decoded udpapi.Reply
	got, _ := udpapi.DecodeReply(&wire, &decoded)
	*b.into = append(*b.into, udpMessage{reply: decoded, correlation: got})
	return true
}

type tcpOut struct{ w *world }

func (o tcpOut) Send(request *tcpapi.Request, correlation uint64) bool {
	o.w.t.Helper()
	var wire ipc.Message
	if !tcpapi.EncodeRequest(request, correlation, &wire) {
		o.w.t.Fatalf("invalid TCP request %+v", request)
	}
	var decoded tcpapi.Request
	got, ok := tcpapi.DecodeRequest(&wire, &decoded)
	if !ok {
		o.w.t.Fatalf("TCP request does not decode: %+v", request)
	}
	o.w.toTCP = append(o.w.toTCP, tcpMessage{request: decoded, correlation: got})
	return true
}

type tcpBack struct {
	w    *world
	into *[]tcpMessage
}

func (b tcpBack) Reply(reply *tcpapi.Reply, correlation uint64) bool {
	var wire ipc.Message
	if !tcpapi.EncodeReply(reply, correlation, &wire) {
		b.w.t.Fatalf("invalid TCP reply %+v", reply)
	}
	var decoded tcpapi.Reply
	got, _ := tcpapi.DecodeReply(&wire, &decoded)
	*b.into = append(*b.into, tcpMessage{reply: decoded, correlation: got})
	return true
}

func newWorld(t *testing.T) *world {
	w := &world{t: t, stack: new(network.Stack), resolver: new(dns.Resolver), now: 1_000_000_000_000}
	w.stack.Init(0x1234)
	config := network.Config{Mode: network.ModeStatic, Address: host, Prefix: 24, Gateway: network.IPv4{10, 0, 2, 2}}
	if field := w.stack.Configure(config, w.now); field != network.FieldNone {
		t.Fatalf("configure %d", field)
	}
	w.stack.LinkUp(network.MAC{0x52, 0x54, 0, 0x12, 0x34, 0x56}, 1500, w.now)
	var frame [network.MaxFrame]byte
	for step := 0; step < 100 && w.stack.Snapshot(w.now).State != network.StateReady; step++ {
		if deadline, ok := w.stack.Deadline(); ok {
			w.now = deadline
		}
		w.stack.Advance(w.now)
		for {
			if _, ok := w.stack.NextFrame(frame[:]); !ok {
				break
			}
		}
	}
	if w.stack.Snapshot(w.now).State != network.StateReady {
		t.Fatal("address never claimed")
	}
	w.resolver.Init(0x5678, dns.DefaultPolicy())
	w.resolver.SetServers(nil, [][4]byte{host})

	udpPool := func() ([]byte, []byte) {
		return make([]byte, udpapi.TXSlots*udpapi.SlotSize), make([]byte, udpapi.RXSlots*udpapi.SlotSize)
	}
	w.udpTX, w.udpRX = udpPool()
	w.udp.Bind(w.udpTX, w.udpRX)
	w.udpServe.Start(w.stack, 0, w.udpTX, w.udpRX)
	w.udpServerTX, w.udpServerRX = udpPool()
	w.udpServer.Start(w.stack, 1, w.udpServerTX, w.udpServerRX)
	w.udpServerID = w.udpCall(udpapi.Request{Opcode: udpapi.OpOpen}).Endpoint
	if bound := w.udpCall(udpapi.Request{Opcode: udpapi.OpBind, Endpoint: w.udpServerID, Address: host, Port: Port}); bound.Result != udpapi.ResultOK {
		t.Fatalf("UDP server bind %d", bound.Result)
	}

	tcpPool := func() ([]byte, []byte) {
		return make([]byte, tcpapi.TXSlots*tcpapi.SlotSize), make([]byte, tcpapi.RXSlots*tcpapi.SlotSize)
	}
	w.tcpTX, w.tcpRX = tcpPool()
	w.tcp.Bind(w.tcpTX, w.tcpRX)
	w.tcpServe.Start(w.stack, 2, w.tcpTX, w.tcpRX, w.now)
	w.tcpServerTX, w.tcpServerRX = tcpPool()
	w.tcpServer.Start(w.stack, 3, w.tcpServerTX, w.tcpServerRX, w.now)
	w.listener = w.tcpCall(tcpapi.Request{Opcode: tcpapi.OpOpen}).Endpoint
	if bound := w.tcpCall(tcpapi.Request{Opcode: tcpapi.OpBind, Endpoint: w.listener, Address: host, Port: Port}); bound.Result != tcpapi.ResultOK {
		t.Fatalf("TCP server bind %d", bound.Result)
	}
	if listen := w.tcpCall(tcpapi.Request{Opcode: tcpapi.OpListen, Endpoint: w.listener, Backlog: 2}); listen.Result != tcpapi.ResultOK {
		t.Fatalf("TCP server listen %d", listen.Result)
	}
	return w
}

func (w *world) udpCall(request udpapi.Request) udpapi.Reply {
	w.t.Helper()
	w.serverSeq++
	if fault := w.udpServer.Handle(w.stack, &request, w.serverSeq, w.now, udpBack{w, &w.udpReplies}); fault != udpserve.FaultNone {
		w.t.Fatal(fault)
	}
	return w.udpTake(w.serverSeq)
}

func (w *world) udpTake(correlation uint64) udpapi.Reply {
	w.t.Helper()
	for index, sent := range w.udpReplies {
		if sent.correlation == correlation {
			w.udpReplies = append(w.udpReplies[:index], w.udpReplies[index+1:]...)
			return sent.reply
		}
	}
	w.t.Fatalf("no UDP server reply %d", correlation)
	return udpapi.Reply{}
}

// tcpStart sends a request from the TCP server and returns its correlation.
func (w *world) tcpStart(request tcpapi.Request) uint64 {
	w.t.Helper()
	w.serverSeq++
	if fault := w.tcpServer.Handle(w.stack, &request, w.serverSeq, w.now, tcpBack{w, &w.tcpReplies}); fault != tcpserve.FaultNone {
		w.t.Fatal(fault)
	}
	return w.serverSeq
}

func (w *world) tcpCall(request tcpapi.Request) tcpapi.Reply {
	w.t.Helper()
	return w.tcpWait(w.tcpStart(request))
}

// tcpWait runs both sides until the TCP server's request is answered.
func (w *world) tcpWait(correlation uint64) tcpapi.Reply {
	w.t.Helper()
	for round := 0; round < 50; round++ {
		for index, sent := range w.tcpReplies {
			if sent.correlation == correlation {
				w.tcpReplies = append(w.tcpReplies[:index], w.tcpReplies[index+1:]...)
				return sent.reply
			}
		}
		w.pump()
		if fault := w.tcpServer.Deliver(w.stack, w.now, tcpBack{w, &w.tcpReplies}); fault != tcpserve.FaultNone {
			w.t.Fatal(fault)
		}
	}
	w.t.Fatalf("no TCP server reply %d", correlation)
	return tcpapi.Reply{}
}

// pump runs the resolver's side until it has nothing left to say.
func (w *world) pump() {
	w.t.Helper()
	for round := 0; round < 200; round++ {
		if !w.udp.SendQueries(w.resolver, w.now, udpOut{w}) || !w.udp.ReleaseFinished(w.resolver, udpOut{w}) ||
			!w.tcp.Start(w.resolver, w.now, tcpOut{w}) || !w.tcp.ReleaseFinished(w.resolver, tcpOut{w}) {
			w.t.Fatal("client could not send")
		}
		if len(w.toUDP)+len(w.fromUDP)+len(w.toTCP)+len(w.fromTCP) == 0 {
			if fault := w.udpServe.Deliver(w.stack, udpBack{w, &w.fromUDP}); fault != udpserve.FaultNone {
				w.t.Fatal(fault)
			}
			if fault := w.tcpServe.Deliver(w.stack, w.now, tcpBack{w, &w.fromTCP}); fault != tcpserve.FaultNone {
				w.t.Fatal(fault)
			}
			if len(w.fromUDP)+len(w.fromTCP) == 0 {
				return
			}
		}
		for len(w.toUDP) > 0 {
			next := w.toUDP[0]
			w.toUDP = w.toUDP[1:]
			if fault := w.udpServe.Handle(w.stack, &next.request, next.correlation, w.now, udpBack{w, &w.fromUDP}); fault != udpserve.FaultNone {
				w.t.Fatal(fault)
			}
		}
		for len(w.fromUDP) > 0 {
			next := w.fromUDP[0]
			w.fromUDP = w.fromUDP[1:]
			if !w.udp.Answer(w.resolver, &next.reply, next.correlation, w.now, udpOut{w}) {
				w.t.Fatalf("UDP client refused %+v", next.reply)
			}
		}
		for len(w.toTCP) > 0 {
			next := w.toTCP[0]
			w.toTCP = w.toTCP[1:]
			if fault := w.tcpServe.Handle(w.stack, &next.request, next.correlation, w.now, tcpBack{w, &w.fromTCP}); fault != tcpserve.FaultNone {
				w.t.Fatal(fault)
			}
		}
		for len(w.fromTCP) > 0 {
			next := w.fromTCP[0]
			w.fromTCP = w.fromTCP[1:]
			if !w.tcp.Answer(w.resolver, &next.reply, next.correlation, w.now, tcpOut{w}) {
				w.t.Fatalf("TCP client refused %+v", next.reply)
			}
		}
	}
	w.t.Fatal("services never settled")
}

// receiveUDP takes the next datagram that reached the server.
func (w *world) receiveUDP() ([]byte, uint16) {
	w.t.Helper()
	reply := w.udpCall(udpapi.Request{Opcode: udpapi.OpRecvFrom, Endpoint: w.udpServerID})
	if reply.Result != udpapi.ResultOK {
		w.t.Fatalf("UDP server receive %+v", reply)
	}
	offset := udpapi.Offset(reply.Slot)
	data := append([]byte(nil), w.udpServerRX[offset:offset+uint64(reply.Length)]...)
	w.udpServer.Handle(w.stack, &udpapi.Request{Opcode: udpapi.OpRelease, Slot: reply.Slot}, 0, w.now, udpBack{w, &w.udpReplies})
	return data, reply.Port
}

func (w *world) sendUDP(port uint16, data []byte) {
	w.t.Helper()
	length := copy(w.udpServerTX, data)
	if sent := w.udpCall(udpapi.Request{Opcode: udpapi.OpSendTo, Endpoint: w.udpServerID, Address: host, Port: port, Length: uint16(length)}); sent.Result != udpapi.ResultOK {
		w.t.Fatalf("UDP server send %d", sent.Result)
	}
}

// readTCP reads exactly count bytes from endpoint.
func (w *world) readTCP(endpoint uint32, count int) []byte {
	w.t.Helper()
	var data []byte
	for len(data) < count {
		reply := w.tcpCall(tcpapi.Request{Opcode: tcpapi.OpRead, Endpoint: endpoint, Length: uint16(min(count-len(data), tcpapi.SlotSize))})
		if reply.Result != tcpapi.ResultOK {
			w.t.Fatalf("TCP server read %+v", reply)
		}
		offset := tcpapi.Offset(reply.Slot)
		data = append(data, w.tcpServerRX[offset:offset+uint64(reply.Length)]...)
		w.tcpServer.Handle(w.stack, &tcpapi.Request{Opcode: tcpapi.OpRelease, Slot: reply.Slot}, 0, w.now, tcpBack{w, &w.tcpReplies})
	}
	return data
}

// writeTCP writes data in one Write and lets the resolver's side run, so
// every piece reaches the client in Reads of its own.
func (w *world) writeTCP(endpoint uint32, data []byte) {
	w.t.Helper()
	for len(data) > 0 {
		length := copy(w.tcpServerTX[:tcpapi.SlotSize], data)
		reply := w.tcpCall(tcpapi.Request{Opcode: tcpapi.OpWrite, Endpoint: endpoint, Length: uint16(length)})
		if reply.Result != tcpapi.ResultOK || reply.Length == 0 {
			w.t.Fatalf("TCP server write %+v", reply)
		}
		data = data[reply.Length:]
		w.pump()
	}
}

func (w *world) lookup(client int, text string) (dns.Answer, bool) {
	w.t.Helper()
	name, ok := hostname.Parse([]byte(text))
	if !ok {
		w.t.Fatalf("name %q", text)
	}
	return w.resolver.Lookup(client, &name, w.now)
}

// header copies a query's header and question with no other records and
// the given flags.
func header(query []byte, flags uint16) []byte {
	end := 12
	for query[end] != 0 {
		end += int(query[end]) + 1
	}
	out := append([]byte(nil), query[:end+5]...)
	binary.BigEndian.PutUint16(out[2:], flags)
	binary.BigEndian.PutUint16(out[10:], 0)
	return out
}

// big answers query with address and pads the answer past one RX slot.
func big(query []byte, address [4]byte) []byte {
	out := header(query, 0x8180)
	binary.BigEndian.PutUint16(out[6:], 1)
	out = append(out, 0xc0, 12, 0, 1, 0, 1, 0, 0, 0, 60, 0, 4)
	out = append(out, address[:]...)
	const padding = 24
	for range padding {
		out = append(out, 0xc0, 12, 0, 16, 0, 1, 0, 0, 0, 60, 1, 0, 255)
		out = append(out, make([]byte, 255)...)
	}
	binary.BigEndian.PutUint16(out[10:], padding)
	return out
}

// settled runs the stack's timers until only the listener is left, as
// TIME_WAIT holds the side that closed first for a while.
func (w *world) settled() {
	w.t.Helper()
	for step := 0; step < 1000 && w.stack.TCPStats().Endpoints > 1; step++ {
		deadline, ok := w.stack.Deadline()
		if !ok {
			break
		}
		w.now = max(w.now, deadline)
		w.stack.Advance(w.now)
		w.pump()
	}
	if stats := w.stack.TCPStats(); stats.Endpoints != 1 {
		w.t.Fatalf("%d TCP endpoints left, want the listener: %v", stats.Endpoints, stats.States)
	}
}

func TestFallbackOverRealStack(t *testing.T) {
	w := newWorld(t)
	if _, done := w.lookup(0, "big.test"); done {
		t.Fatal("lookup answered at once")
	}
	w.pump()
	query, port := w.receiveUDP()
	w.sendUDP(port, header(query, 0x8380))
	w.pump()
	if w.tcp.Live() != 1 {
		t.Fatalf("live fallbacks %d", w.tcp.Live())
	}

	accepted := w.tcpCall(tcpapi.Request{Opcode: tcpapi.OpAccept, Endpoint: w.listener})
	if accepted.Result != tcpapi.ResultOK || accepted.Address != host {
		t.Fatalf("accept %+v", accepted)
	}
	prefix := w.readTCP(accepted.Endpoint, 2)
	length := int(binary.BigEndian.Uint16(prefix))
	if got := w.readTCP(accepted.Endpoint, length); length != len(query) || string(got) != string(query) {
		t.Fatalf("TCP query % x, want the datagram % x", got, query)
	}

	response := big(query, [4]byte{10, 9, 8, 7})
	if len(response) <= tcpapi.SlotSize {
		t.Fatalf("response of %d bytes fits one slot", len(response))
	}
	stream := binary.BigEndian.AppendUint16(nil, uint16(len(response)))
	stream = append(stream, response...)
	// Split across the prefix and then across RX slots.
	w.writeTCP(accepted.Endpoint, stream[:1])
	w.writeTCP(accepted.Endpoint, stream[1:102])
	w.writeTCP(accepted.Endpoint, stream[102:])
	w.pump()

	client, answer, ok := w.resolver.Completion()
	if !ok || client != 0 || answer.Result != dns.ResultOK || answer.Count != 1 || answer.Addresses[0] != [4]byte{10, 9, 8, 7} {
		t.Fatalf("completion %d %+v %v", client, answer, ok)
	}
	if w.tcp.Live() != 0 || w.udp.Live() != 0 {
		t.Fatalf("live TCP %d UDP %d", w.tcp.Live(), w.udp.Live())
	}
	// The client closed its end; the server sees EOF and closes too.
	if eof := w.tcpCall(tcpapi.Request{Opcode: tcpapi.OpRead, Endpoint: accepted.Endpoint, Length: 16}); eof.Result != tcpapi.ResultEOF {
		t.Fatalf("server read after the answer %+v", eof)
	}
	w.tcpCall(tcpapi.Request{Opcode: tcpapi.OpClose, Endpoint: accepted.Endpoint})

	cached, done := w.lookup(1, "big.test")
	if !done || cached.Source != dns.SourceCache || cached.Addresses[0] != [4]byte{10, 9, 8, 7} {
		t.Fatalf("second lookup %+v %v", cached, done)
	}
	w.pump()
	stats := w.resolver.Stats()
	if stats.Queries != 1 || stats.TCPFallbacks != 1 || stats.TCPAnswers != 1 || stats.Truncated != 1 {
		t.Fatalf("stats %+v", stats)
	}
	w.settled()
}

// A server that closes the connection partway through its answer fails the
// lookup without caching anything, and leaves no endpoint behind.
func TestFallbackCutShortOverRealStack(t *testing.T) {
	w := newWorld(t)
	policy := dns.DefaultPolicy()
	policy.Attempts = 1
	w.resolver.SetPolicy(policy, w.now)
	w.lookup(0, "big.test")
	w.pump()
	query, port := w.receiveUDP()
	w.sendUDP(port, header(query, 0x8380))
	w.pump()
	accepted := w.tcpCall(tcpapi.Request{Opcode: tcpapi.OpAccept, Endpoint: w.listener})
	w.readTCP(accepted.Endpoint, 2+len(query))
	partial := binary.BigEndian.AppendUint16(nil, 500)
	partial = append(partial, header(query, 0x8180)...)
	w.writeTCP(accepted.Endpoint, partial)
	w.tcpCall(tcpapi.Request{Opcode: tcpapi.OpClose, Endpoint: accepted.Endpoint})
	w.pump()
	_, answer, ok := w.resolver.Completion()
	if !ok || answer.Result != dns.ResultTemporaryFailure {
		t.Fatalf("completion %+v %v", answer, ok)
	}
	if w.tcp.Live() != 0 {
		t.Fatal("fallback still live")
	}
	if stats := w.resolver.Stats(); stats.Positive != 0 || stats.TCPFailures != 1 {
		t.Fatalf("stats %+v", stats)
	}
	w.settled()
}
