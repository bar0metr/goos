package upstream

import (
	"encoding/binary"
	"testing"

	"goos/kernel/ipc"
	"goos/userspace/dns"
	"goos/userspace/hostname"
	"goos/userspace/network"
	"goos/userspace/udpapi"
	"goos/userspace/udpserve"
)

// The resolver's query path end to end: the resolver core and this client,
// the network service's GUDP1 session logic, and a real stack, with every
// request and reply passing through the GUDP1 codec as it does between the
// two services. The DNS server is a second session bound to the stack's own
// address, so datagrams travel by local delivery and no frame is built.

var host = network.IPv4{10, 0, 2, 15}

type message struct {
	request     udpapi.Request
	reply       udpapi.Reply
	correlation uint64
}

// world is the two services and the server, joined by in-memory queues.
type world struct {
	t        *testing.T
	stack    *network.Stack
	resolver *dns.Resolver
	client   Client
	session  udpserve.Session
	server   udpserve.Session
	now      uint64
	// toService and toClient are the two directions of the resolver's
	// channel; toServer collects the server session's replies.
	toService []message
	toClient  []message
	toServer  []message
	clientTX  []byte
	clientRX  []byte
	serverTX  []byte
	serverRX  []byte
	serverID  uint32
	serverSeq uint64
}

type clientChannel struct{ w *world }

func (c clientChannel) Send(request *udpapi.Request, correlation uint64) bool {
	c.w.t.Helper()
	var wire ipc.Message
	if !udpapi.EncodeRequest(request, correlation, &wire) {
		c.w.t.Fatalf("client sent an invalid request %+v", request)
	}
	var decoded udpapi.Request
	got, ok := udpapi.DecodeRequest(&wire, &decoded)
	if !ok || got != correlation {
		c.w.t.Fatalf("request does not decode: %+v", request)
	}
	c.w.toService = append(c.w.toService, message{request: decoded, correlation: got})
	return true
}

type replies struct {
	w    *world
	into *[]message
}

func (r replies) Reply(reply *udpapi.Reply, correlation uint64) bool {
	r.w.t.Helper()
	var wire ipc.Message
	if !udpapi.EncodeReply(reply, correlation, &wire) {
		r.w.t.Fatalf("service sent an invalid reply %+v", reply)
	}
	var decoded udpapi.Reply
	got, ok := udpapi.DecodeReply(&wire, &decoded)
	if !ok || got != correlation {
		r.w.t.Fatalf("reply does not decode: %+v", reply)
	}
	*r.into = append(*r.into, message{reply: decoded, correlation: got})
	return true
}

func newWorld(t *testing.T) *world {
	w := &world{t: t, stack: new(network.Stack), resolver: new(dns.Resolver), now: 1_000_000_000_000}
	w.stack.Init(0x1234)
	w.ready()
	w.resolver.Init(0x5678, dns.DefaultPolicy())
	w.resolver.SetServers(nil, [][4]byte{host})
	w.clientTX, w.clientRX = make([]byte, udpapi.TXSlots*udpapi.SlotSize), make([]byte, udpapi.RXSlots*udpapi.SlotSize)
	w.client.Bind(w.clientTX, w.clientRX)
	w.session.Start(w.stack, 0, w.clientTX, w.clientRX)
	w.serverTX, w.serverRX = make([]byte, udpapi.TXSlots*udpapi.SlotSize), make([]byte, udpapi.RXSlots*udpapi.SlotSize)
	w.server.Start(w.stack, 1, w.serverTX, w.serverRX)
	open := w.serverCall(udpapi.Request{Opcode: udpapi.OpOpen})
	w.serverID = open.Endpoint
	if bound := w.serverCall(udpapi.Request{Opcode: udpapi.OpBind, Endpoint: w.serverID, Address: host, Port: Port}); bound.Result != udpapi.ResultOK {
		t.Fatalf("server bind %d", bound.Result)
	}
	return w
}

// ready claims the static address; nothing answers the probes.
func (w *world) ready() {
	config := network.Config{Mode: network.ModeStatic, Address: host, Prefix: 24, Gateway: network.IPv4{10, 0, 2, 2}}
	if field := w.stack.Configure(config, w.now); field != network.FieldNone {
		w.t.Fatalf("configure %d", field)
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
		w.t.Fatal("address never claimed")
	}
}

// serverCall drives the server session synchronously.
func (w *world) serverCall(request udpapi.Request) udpapi.Reply {
	w.t.Helper()
	w.serverSeq++
	if fault := w.server.Handle(w.stack, &request, w.serverSeq, w.now, replies{w, &w.toServer}); fault != udpserve.FaultNone {
		w.t.Fatal(fault)
	}
	for index, sent := range w.toServer {
		if sent.correlation == w.serverSeq {
			w.toServer = append(w.toServer[:index], w.toServer[index+1:]...)
			return sent.reply
		}
	}
	w.t.Fatalf("no reply to %+v", request)
	return udpapi.Reply{}
}

// pump runs both services until neither has anything left to say.
func (w *world) pump() {
	w.t.Helper()
	out := clientChannel{w}
	for round := 0; round < 100; round++ {
		if !w.client.SendQueries(w.resolver, w.now, out) || !w.client.ReleaseFinished(w.resolver, out) {
			w.t.Fatal("client could not send")
		}
		if len(w.toService) == 0 && len(w.toClient) == 0 {
			if fault := w.session.Deliver(w.stack, replies{w, &w.toClient}); fault != udpserve.FaultNone {
				w.t.Fatal(fault)
			}
			if len(w.toClient) == 0 {
				return
			}
		}
		for len(w.toService) > 0 {
			next := w.toService[0]
			w.toService = w.toService[1:]
			if fault := w.session.Handle(w.stack, &next.request, next.correlation, w.now, replies{w, &w.toClient}); fault != udpserve.FaultNone {
				w.t.Fatalf("service closed the session: %s", fault)
			}
		}
		for len(w.toClient) > 0 {
			next := w.toClient[0]
			w.toClient = w.toClient[1:]
			if !w.client.Answer(w.resolver, &next.reply, next.correlation, w.now, out) {
				w.t.Fatalf("client refused %+v", next.reply)
			}
		}
	}
	w.t.Fatal("services never settled")
}

func (w *world) lookup(text string) {
	w.t.Helper()
	name, ok := hostname.Parse([]byte(text))
	if !ok {
		w.t.Fatalf("name %q", text)
	}
	if _, done := w.resolver.Lookup(0, &name, w.now); done {
		w.t.Fatal("lookup answered without a query")
	}
}

// received is one DNS query the server received.
type received struct {
	id     uint16
	port   uint16
	length int
	data   []byte
}

// receive takes the next query that reached the server.
func (w *world) receive() received {
	w.t.Helper()
	reply := w.serverCall(udpapi.Request{Opcode: udpapi.OpRecvFrom, Endpoint: w.serverID})
	if reply.Result != udpapi.ResultOK || reply.Address != host {
		w.t.Fatalf("server receive %+v", reply)
	}
	offset := udpapi.Offset(reply.Slot)
	data := append([]byte(nil), w.serverRX[offset:offset+uint64(reply.Length)]...)
	w.server.Handle(w.stack, &udpapi.Request{Opcode: udpapi.OpRelease, Slot: reply.Slot}, 0, w.now, replies{w, &w.toServer})
	return received{id: binary.BigEndian.Uint16(data), port: reply.Port, length: int(reply.Length), data: data}
}

// answer sends an A record for the query's name to port, with id.
func (w *world) answer(q received, port uint16, id uint16, address [4]byte) {
	w.t.Helper()
	end := 12
	for q.data[end] != 0 {
		end += int(q.data[end]) + 1
	}
	end += 5
	response := append([]byte(nil), q.data[:end]...)
	binary.BigEndian.PutUint16(response[0:2], id)
	binary.BigEndian.PutUint16(response[2:4], 0x8180)
	binary.BigEndian.PutUint16(response[6:8], 1)
	binary.BigEndian.PutUint16(response[8:10], 0)
	binary.BigEndian.PutUint16(response[10:12], 0)
	response = append(response, 0xc0, 12, 0, 1, 0, 1, 0, 0, 0, 60, 0, 4)
	response = append(response, address[:]...)
	length := copy(w.serverTX, response)
	sent := w.serverCall(udpapi.Request{Opcode: udpapi.OpSendTo, Endpoint: w.serverID, Address: host, Port: port, Length: uint16(length)})
	if sent.Result != udpapi.ResultOK {
		w.t.Fatalf("server send %d", sent.Result)
	}
}

func (w *world) completion() dns.Answer {
	w.t.Helper()
	client, answer, ok := w.resolver.Completion()
	if !ok || client != 0 {
		w.t.Fatal("no completion")
	}
	return answer
}

func (w *world) noCompletion() {
	w.t.Helper()
	if _, answer, ok := w.resolver.Completion(); ok {
		w.t.Fatalf("unexpected completion %+v", answer)
	}
}

func TestQueryWaitsInDeferredRecvFrom(t *testing.T) {
	w := newWorld(t)
	w.lookup("host.test")
	w.pump()
	if w.session.Waiting() != 1 || w.client.Live() != 1 {
		t.Fatalf("waiting %d live %d: the query's RecvFrom is not held", w.session.Waiting(), w.client.Live())
	}
	q := w.receive()
	if q.port < 49152 {
		t.Fatalf("query from port %d", q.port)
	}
	w.answer(q, q.port, q.id, [4]byte{192, 0, 2, 80})
	w.pump()
	answer := w.completion()
	if answer.Result != dns.ResultOK || answer.Count != 1 || answer.Addresses[0] != ([4]byte{192, 0, 2, 80}) {
		t.Fatalf("answer %+v", answer)
	}
	if w.session.Waiting() != 0 || w.client.Live() != 0 {
		t.Fatal("finished query kept its receive or endpoint")
	}
	for slot := uint8(0); slot < udpapi.RXSlots; slot++ {
		if w.session.RXLent(slot) {
			t.Fatalf("RX slot %d never released", slot)
		}
	}
	if stats := w.stack.UDPStats(); stats.Endpoints != 1 {
		t.Fatalf("endpoints %d: the query's endpoint was not closed", stats.Endpoints)
	}
}

// TestRetransmissionDropsTheStaleAnswer lets the first query time out. The
// retransmission comes from a new endpoint and port; an answer to the old
// port finds no endpoint and cannot complete the lookup, and the answer to
// the new one does.
func TestRetransmissionDropsTheStaleAnswer(t *testing.T) {
	w := newWorld(t)
	w.lookup("host.test")
	w.pump()
	first := w.receive()
	deadline, ok := w.resolver.Deadline()
	if !ok {
		t.Fatal("no resolver deadline")
	}
	w.now = deadline
	w.resolver.Advance(w.now)
	w.pump()
	second := w.receive()
	if second.port == first.port || w.client.Live() != 1 || w.session.Waiting() != 1 {
		t.Fatalf("retransmission from port %d after %d, live %d", second.port, first.port, w.client.Live())
	}
	drops := w.stack.UDPStats().DropNoEndpoint
	w.answer(first, first.port, first.id, [4]byte{198, 51, 100, 1})
	w.pump()
	w.noCompletion()
	if w.stack.UDPStats().DropNoEndpoint != drops+1 {
		t.Fatal("answer to the closed port was not dropped")
	}
	w.answer(second, second.port, second.id, [4]byte{192, 0, 2, 81})
	w.pump()
	if answer := w.completion(); answer.Addresses[0] != ([4]byte{192, 0, 2, 81}) {
		t.Fatalf("answer %+v", answer)
	}
}

// TestMismatchedAnswerRearmsTheReceive answers with the wrong transaction
// ID first: the resolver drops it, the client asks for the next datagram on
// the same endpoint, and the real answer completes the lookup.
func TestMismatchedAnswerRearmsTheReceive(t *testing.T) {
	w := newWorld(t)
	w.lookup("host.test")
	w.pump()
	q := w.receive()
	w.answer(q, q.port, q.id^0x5a5a, [4]byte{203, 0, 113, 9})
	w.pump()
	w.noCompletion()
	if w.session.Waiting() != 1 || w.client.Live() != 1 {
		t.Fatal("mismatched answer ended the query")
	}
	w.answer(q, q.port, q.id, [4]byte{192, 0, 2, 82})
	w.pump()
	if answer := w.completion(); answer.Addresses[0] != ([4]byte{192, 0, 2, 82}) {
		t.Fatalf("answer %+v", answer)
	}
	if stats := w.resolver.Stats(); stats.Mismatched != 1 {
		t.Fatalf("stats %+v", stats)
	}
}

// TestSessionLossCancelsTheQuery ends the resolver's session while a query
// waits, as when the network service restarts: the waiting receive and the
// endpoint go, the server's answer finds nothing, and a new session starts
// clean.
func TestSessionLossCancelsTheQuery(t *testing.T) {
	w := newWorld(t)
	w.lookup("host.test")
	w.pump()
	q := w.receive()
	w.session.End(w.stack)
	w.client.Unbind()
	w.resolver.Reset(w.now)
	if answer := w.completion(); answer.Result != dns.ResultReconfigured {
		t.Fatalf("lookup after session loss %+v", answer)
	}
	if w.session.Waiting() != 0 || w.client.Live() != 0 || w.stack.UDPStats().Endpoints != 1 {
		t.Fatal("session loss left a receive or an endpoint")
	}
	w.answer(q, q.port, q.id, [4]byte{192, 0, 2, 83})
	w.toService, w.toClient = nil, nil
	w.client.Bind(w.clientTX, w.clientRX)
	w.session.Start(w.stack, 0, w.clientTX, w.clientRX)
	w.pump()
	w.noCompletion()
	w.lookup("other.test")
	w.pump()
	next := w.receive()
	if next.port == q.port {
		t.Fatal("new session reused the old query's port")
	}
	w.answer(next, next.port, next.id, [4]byte{192, 0, 2, 84})
	w.pump()
	if answer := w.completion(); answer.Addresses[0] != ([4]byte{192, 0, 2, 84}) {
		t.Fatalf("answer %+v", answer)
	}
}

func TestOfflineQueriesFailAtOnce(t *testing.T) {
	w := newWorld(t)
	w.client.Unbind()
	w.lookup("host.test")
	w.pump()
	if len(w.toService) != 0 || w.client.Live() != 0 {
		t.Fatal("offline client sent a request")
	}
	if answer := w.completion(); answer.Result == dns.ResultOK {
		t.Fatalf("answer %+v", answer)
	}
}
