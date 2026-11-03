package dnsstub

import (
	"encoding/binary"
	"testing"

	"goos/kernel/ipc"
	"goos/userspace/dns"
	"goos/userspace/dnswire"
	"goos/userspace/hostname"
	"goos/userspace/network"
	"goos/userspace/tcpapi"
	"goos/userspace/tcpserve"
	"goos/userspace/tcpupstream"
	"goos/userspace/udpapi"
	"goos/userspace/udpserve"
	"goos/userspace/upstream"
)

// The stub end to end: a client session on the loopback address, the stub
// and the resolver's UDP and TCP clients sharing the resolver's sessions,
// the network service's GUDP1 and GTCP1 session logic over a real stack, and
// an upstream DNS server on the stack's own address. Every message passes
// through the codecs as it does between the services.

var host = network.IPv4{10, 0, 2, 15}

// base is the first resolver client slot of the stub, as in the service.
const base = 16

// Session owners in the stack.
const (
	ownerResolverUDP = iota
	ownerServerUDP
	ownerResolverTCP
	ownerServerTCP
	ownerClientUDP
	ownerClientTCP
)

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

type hostsTable map[string][4]byte

func (table hostsTable) Address(name *hostname.Name) ([4]byte, bool) {
	address, found := table[string(name.Bytes())]
	return address, found
}

// udpPeer is a GUDP1 session driven directly by the test: the upstream
// server or the client.
type udpPeer struct {
	owner   int
	session udpserve.Session
	tx, rx  []byte
	replies []udpMessage
}

type tcpPeer struct {
	owner   int
	session tcpserve.Session
	tx, rx  []byte
	replies []tcpMessage
}

type world struct {
	t        *testing.T
	stack    *network.Stack
	resolver *dns.Resolver
	now      uint64
	stub     *Server
	hosts    hostsTable
	// completions holds what the resolver finished for clients that are
	// not the stub's.
	completions []int

	udp            upstream.Client
	udpServe       udpserve.Session
	udpTX, udpRX   []byte
	toUDP, fromUDP []udpMessage

	tcp            tcpupstream.Client
	tcpServe       tcpserve.Session
	tcpTX, tcpRX   []byte
	toTCP, fromTCP []tcpMessage

	server, client       *udpPeer
	serverTCP, clientTCP *tcpPeer
	serverID, clientID   uint32
	clientPort           uint16
	listener             uint32
	seq                  uint64
	maxLive              int
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
	t    *testing.T
	into *[]udpMessage
}

func (b udpBack) Reply(reply *udpapi.Reply, correlation uint64) bool {
	var wire ipc.Message
	if !udpapi.EncodeReply(reply, correlation, &wire) {
		b.t.Fatalf("invalid UDP reply %+v", reply)
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
	t    *testing.T
	into *[]tcpMessage
}

func (b tcpBack) Reply(reply *tcpapi.Reply, correlation uint64) bool {
	var wire ipc.Message
	if !tcpapi.EncodeReply(reply, correlation, &wire) {
		b.t.Fatalf("invalid TCP reply %+v", reply)
	}
	var decoded tcpapi.Reply
	got, _ := tcpapi.DecodeReply(&wire, &decoded)
	*b.into = append(*b.into, tcpMessage{reply: decoded, correlation: got})
	return true
}

func udpPool() ([]byte, []byte) {
	return make([]byte, udpapi.TXSlots*udpapi.SlotSize), make([]byte, udpapi.RXSlots*udpapi.SlotSize)
}

func tcpPool() ([]byte, []byte) {
	return make([]byte, tcpapi.TXSlots*tcpapi.SlotSize), make([]byte, tcpapi.RXSlots*tcpapi.SlotSize)
}

func newWorld(t *testing.T) *world {
	w := &world{t: t, stack: new(network.Stack), resolver: new(dns.Resolver), stub: new(Server),
		hosts: hostsTable{}, now: 1_000_000_000_000}
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
	w.stub.Init(base, w.hosts)

	w.udpTX, w.udpRX = udpPool()
	w.udp.Bind(w.udpTX, w.udpRX)
	w.udpServe.Start(w.stack, ownerResolverUDP, w.udpTX, w.udpRX)
	w.stub.BindUDP(w.udpTX, w.udpRX, &w.udp, w.now)
	w.tcpTX, w.tcpRX = tcpPool()
	w.tcp.Bind(w.tcpTX, w.tcpRX)
	w.tcpServe.Start(w.stack, ownerResolverTCP, w.tcpTX, w.tcpRX, w.now)
	w.stub.BindTCP(w.tcpTX, w.tcpRX, &w.tcp, w.now)

	w.server = w.newUDPPeer(ownerServerUDP)
	w.serverID = w.udpCall(w.server, udpapi.Request{Opcode: udpapi.OpOpen}).Endpoint
	if bound := w.udpCall(w.server, udpapi.Request{Opcode: udpapi.OpBind, Endpoint: w.serverID, Address: host, Port: Port}); bound.Result != udpapi.ResultOK {
		t.Fatalf("server bind %d", bound.Result)
	}
	w.client = w.newUDPPeer(ownerClientUDP)
	w.clientID = w.udpCall(w.client, udpapi.Request{Opcode: udpapi.OpOpen}).Endpoint
	bound := w.udpCall(w.client, udpapi.Request{Opcode: udpapi.OpBind, Endpoint: w.clientID, Address: Address})
	if bound.Result != udpapi.ResultOK {
		t.Fatalf("client bind %d", bound.Result)
	}
	w.clientPort = bound.Port

	w.serverTCP = w.newTCPPeer(ownerServerTCP)
	w.listener = w.tcpCall(w.serverTCP, tcpapi.Request{Opcode: tcpapi.OpOpen}).Endpoint
	if bound := w.tcpCall(w.serverTCP, tcpapi.Request{Opcode: tcpapi.OpBind, Endpoint: w.listener, Address: host, Port: Port}); bound.Result != tcpapi.ResultOK {
		t.Fatalf("server TCP bind %d", bound.Result)
	}
	if listen := w.tcpCall(w.serverTCP, tcpapi.Request{Opcode: tcpapi.OpListen, Endpoint: w.listener, Backlog: 2}); listen.Result != tcpapi.ResultOK {
		t.Fatalf("server TCP listen %d", listen.Result)
	}
	w.clientTCP = w.newTCPPeer(ownerClientTCP)
	w.pump()
	if w.stub.udp.phase != endpointReady || w.stub.tcp.phase != endpointReady {
		t.Fatalf("stub endpoints %d %d", w.stub.udp.phase, w.stub.tcp.phase)
	}
	return w
}

func (w *world) newUDPPeer(owner int) *udpPeer {
	peer := &udpPeer{owner: owner}
	peer.tx, peer.rx = udpPool()
	peer.session.Start(w.stack, owner, peer.tx, peer.rx)
	return peer
}

func (w *world) newTCPPeer(owner int) *tcpPeer {
	peer := &tcpPeer{owner: owner}
	peer.tx, peer.rx = tcpPool()
	peer.session.Start(w.stack, owner, peer.tx, peer.rx, w.now)
	return peer
}

func (w *world) udpStart(peer *udpPeer, request udpapi.Request) uint64 {
	w.t.Helper()
	w.seq++
	if fault := peer.session.Handle(w.stack, &request, w.seq, w.now, udpBack{w.t, &peer.replies}); fault != udpserve.FaultNone {
		w.t.Fatal(fault)
	}
	return w.seq
}

func (w *world) udpCall(peer *udpPeer, request udpapi.Request) udpapi.Reply {
	w.t.Helper()
	return w.udpWait(peer, w.udpStart(peer, request))
}

func (w *world) udpWait(peer *udpPeer, correlation uint64) udpapi.Reply {
	w.t.Helper()
	for round := 0; round < 50; round++ {
		if reply, found := takeUDP(peer, correlation); found {
			return reply
		}
		w.pump()
	}
	w.t.Fatalf("no UDP reply %d for owner %d", correlation, peer.owner)
	return udpapi.Reply{}
}

func takeUDP(peer *udpPeer, correlation uint64) (udpapi.Reply, bool) {
	for index, sent := range peer.replies {
		if sent.correlation == correlation {
			peer.replies = append(peer.replies[:index], peer.replies[index+1:]...)
			return sent.reply, true
		}
	}
	return udpapi.Reply{}, false
}

func (w *world) tcpStart(peer *tcpPeer, request tcpapi.Request) uint64 {
	w.t.Helper()
	w.seq++
	if fault := peer.session.Handle(w.stack, &request, w.seq, w.now, tcpBack{w.t, &peer.replies}); fault != tcpserve.FaultNone {
		w.t.Fatal(fault)
	}
	return w.seq
}

func (w *world) tcpCall(peer *tcpPeer, request tcpapi.Request) tcpapi.Reply {
	w.t.Helper()
	return w.tcpWait(peer, w.tcpStart(peer, request))
}

func (w *world) tcpWait(peer *tcpPeer, correlation uint64) tcpapi.Reply {
	w.t.Helper()
	for round := 0; round < 50; round++ {
		if reply, found := takeTCP(peer, correlation); found {
			return reply
		}
		w.pump()
	}
	w.t.Fatalf("no TCP reply %d for owner %d", correlation, peer.owner)
	return tcpapi.Reply{}
}

func takeTCP(peer *tcpPeer, correlation uint64) (tcpapi.Reply, bool) {
	for index, sent := range peer.replies {
		if sent.correlation == correlation {
			peer.replies = append(peer.replies[:index], peer.replies[index+1:]...)
			return sent.reply, true
		}
	}
	return tcpapi.Reply{}, false
}

// flush runs the resolver service's loop body once: queries out, finished
// endpoints closed, completions handed to the stub, the stub's own work.
func (w *world) flush() {
	w.t.Helper()
	w.resolver.Advance(w.now)
	if !w.udp.SendQueries(w.resolver, w.now, udpOut{w}) || !w.udp.ReleaseFinished(w.resolver, udpOut{w}) ||
		!w.tcp.Start(w.resolver, w.now, tcpOut{w}) || !w.tcp.ReleaseFinished(w.resolver, tcpOut{w}) {
		w.t.Fatal("upstream client could not send")
	}
	for {
		client, answer, found := w.resolver.Completion()
		if !found {
			break
		}
		if !w.stub.Complete(client, &answer, w.now) {
			w.completions = append(w.completions, client)
		}
	}
	if !w.stub.FlushUDP(w.now, udpOut{w}) || !w.stub.FlushTCP(w.resolver, w.now, tcpOut{w}) {
		w.t.Fatal("stub could not send")
	}
	if live, _ := w.stub.Live(); live > w.maxLive {
		w.maxLive = live
	}
}

func (w *world) queued() int {
	count := len(w.toUDP) + len(w.fromUDP) + len(w.toTCP) + len(w.fromTCP)
	for _, peer := range []*udpPeer{w.server, w.client} {
		if peer != nil {
			count += len(peer.replies)
		}
	}
	for _, peer := range []*tcpPeer{w.serverTCP, w.clientTCP} {
		if peer != nil {
			count += len(peer.replies)
		}
	}
	return count
}

// pump runs every side until nothing is left to say. Replies with a stub
// correlation go to the stub, the rest to the resolver's clients, as the
// service routes them.
func (w *world) pump() {
	w.t.Helper()
	for round := 0; round < 400; round++ {
		w.flush()
		if len(w.toUDP)+len(w.fromUDP)+len(w.toTCP)+len(w.fromTCP) == 0 {
			before := w.queued()
			w.deliver()
			if w.queued() == before {
				return
			}
		}
		for len(w.toUDP) > 0 {
			next := w.toUDP[0]
			w.toUDP = w.toUDP[1:]
			if fault := w.udpServe.Handle(w.stack, &next.request, next.correlation, w.now, udpBack{w.t, &w.fromUDP}); fault != udpserve.FaultNone {
				w.t.Fatalf("%s for %+v", fault, next.request)
			}
		}
		for len(w.fromUDP) > 0 {
			next := w.fromUDP[0]
			w.fromUDP = w.fromUDP[1:]
			if Owns(next.correlation) {
				if !w.stub.AnswerUDP(w.resolver, &next.reply, next.correlation, w.now, udpOut{w}) {
					w.t.Fatalf("stub refused UDP %+v", next.reply)
				}
			} else if !w.udp.Answer(w.resolver, &next.reply, next.correlation, w.now, udpOut{w}) {
				w.t.Fatalf("UDP client refused %+v", next.reply)
			}
		}
		for len(w.toTCP) > 0 {
			next := w.toTCP[0]
			w.toTCP = w.toTCP[1:]
			if fault := w.tcpServe.Handle(w.stack, &next.request, next.correlation, w.now, tcpBack{w.t, &w.fromTCP}); fault != tcpserve.FaultNone {
				w.t.Fatalf("%s for %+v", fault, next.request)
			}
		}
		for len(w.fromTCP) > 0 {
			next := w.fromTCP[0]
			w.fromTCP = w.fromTCP[1:]
			if Owns(next.correlation) {
				if !w.stub.AnswerTCP(w.resolver, &next.reply, next.correlation, w.now, tcpOut{w}) {
					w.t.Fatalf("stub refused TCP %+v", next.reply)
				}
			} else if !w.tcp.Answer(w.resolver, &next.reply, next.correlation, w.now, tcpOut{w}) {
				w.t.Fatalf("TCP client refused %+v", next.reply)
			}
		}
	}
	w.t.Fatal("services never settled")
}

func (w *world) deliver() {
	w.t.Helper()
	if fault := w.udpServe.Deliver(w.stack, udpBack{w.t, &w.fromUDP}); fault != udpserve.FaultNone {
		w.t.Fatal(fault)
	}
	if fault := w.tcpServe.Deliver(w.stack, w.now, tcpBack{w.t, &w.fromTCP}); fault != tcpserve.FaultNone {
		w.t.Fatal(fault)
	}
	for _, peer := range []*udpPeer{w.server, w.client} {
		if peer != nil {
			if fault := peer.session.Deliver(w.stack, udpBack{w.t, &peer.replies}); fault != udpserve.FaultNone {
				w.t.Fatal(fault)
			}
		}
	}
	for _, peer := range []*tcpPeer{w.serverTCP, w.clientTCP} {
		if peer != nil {
			if fault := peer.session.Deliver(w.stack, w.now, tcpBack{w.t, &peer.replies}); fault != tcpserve.FaultNone {
				w.t.Fatal(fault)
			}
		}
	}
}

// advance moves the clock to at, running the stack's timers on the way.
func (w *world) advance(at uint64) {
	w.t.Helper()
	for step := 0; step < 1000; step++ {
		deadline, ok := w.stack.Deadline()
		if !ok || deadline > at {
			break
		}
		w.now = max(w.now, deadline)
		w.stack.Advance(w.now)
		w.pump()
	}
	w.now = max(w.now, at)
	w.stack.Advance(w.now)
	w.pump()
}

// --- the client and the upstream server -------------------------------------

// ask sends one datagram from the client to the stub.
func (w *world) ask(message []byte) {
	w.t.Helper()
	length := copy(w.client.tx, message)
	sent := w.udpCall(w.client, udpapi.Request{Opcode: udpapi.OpSendTo, Endpoint: w.clientID, Address: Address, Port: Port, Length: uint16(length)})
	if sent.Result != udpapi.ResultOK {
		w.t.Fatalf("client send %d", sent.Result)
	}
	w.pump()
}

// pending counts the datagrams waiting for the client.
func (w *world) pending() int {
	count, result := w.stack.UDPQueued(ownerClientUDP, network.EndpointID(w.clientID))
	if result != network.ResultOK {
		w.t.Fatalf("client queue %d", result)
	}
	return count
}

// answer takes the next datagram that reached the client and checks it came
// from the stub's address and port.
func (w *world) answer() []byte {
	w.t.Helper()
	data, _ := w.receive(w.client, w.clientID, Address, Port)
	return data
}

func (w *world) receive(peer *udpPeer, endpoint uint32, from [4]byte, port uint16) ([]byte, uint16) {
	w.t.Helper()
	reply := w.udpCall(peer, udpapi.Request{Opcode: udpapi.OpRecvFrom, Endpoint: endpoint})
	if reply.Result != udpapi.ResultOK || reply.Address != from || (port != 0 && reply.Port != port) {
		w.t.Fatalf("receive %+v", reply)
	}
	offset := udpapi.Offset(reply.Slot)
	data := append([]byte(nil), peer.rx[offset:offset+uint64(reply.Length)]...)
	peer.session.Handle(w.stack, &udpapi.Request{Opcode: udpapi.OpRelease, Slot: reply.Slot}, 0, w.now, udpBack{w.t, &peer.replies})
	return data, reply.Port
}

// upstreamQuery takes the next query that reached the upstream server and
// the resolver's port it came from.
func (w *world) upstreamQuery() ([]byte, uint16) {
	w.t.Helper()
	return w.receive(w.server, w.serverID, host, 0)
}

func (w *world) upstreamReply(port uint16, data []byte) {
	w.t.Helper()
	length := copy(w.server.tx, data)
	if sent := w.udpCall(w.server, udpapi.Request{Opcode: udpapi.OpSendTo, Endpoint: w.serverID, Address: host, Port: port, Length: uint16(length)}); sent.Result != udpapi.ResultOK {
		w.t.Fatalf("server send %d", sent.Result)
	}
	w.pump()
}

func (w *world) upstreamQueued() int {
	count, _ := w.stack.UDPQueued(ownerServerUDP, network.EndpointID(w.serverID))
	return count
}

// --- messages -----------------------------------------------------------------

func wireName(text string) []byte {
	var out []byte
	start := 0
	for index := 0; index <= len(text); index++ {
		if index == len(text) || text[index] == '.' {
			out = append(out, byte(index-start))
			out = append(out, text[start:index]...)
			start = index + 1
		}
	}
	return append(out, 0)
}

// query is a client query; edns adds an OPT record of the given version.
func query(id uint16, name string, qtype uint16, edns bool, version uint8) []byte {
	out := make([]byte, dnswire.HeaderSize)
	binary.BigEndian.PutUint16(out[0:], id)
	binary.BigEndian.PutUint16(out[2:], dnswire.FlagRD)
	binary.BigEndian.PutUint16(out[4:], 1)
	out = append(out, wireName(name)...)
	out = binary.BigEndian.AppendUint16(out, qtype)
	out = binary.BigEndian.AppendUint16(out, dnswire.ClassIN)
	if edns {
		binary.BigEndian.PutUint16(out[10:], 1)
		out = append(out, 0, 0, dnswire.TypeOPT, 0x10, 0, 0, version, 0, 0, 0, 0)
	}
	return out
}

// header copies a query's header and question with the given flags.
func header(query []byte, flags uint16) []byte {
	end := dnswire.HeaderSize
	for query[end] != 0 {
		end += int(query[end]) + 1
	}
	out := append([]byte(nil), query[:end+5]...)
	binary.BigEndian.PutUint16(out[2:], flags)
	binary.BigEndian.PutUint16(out[6:], 0)
	binary.BigEndian.PutUint16(out[8:], 0)
	binary.BigEndian.PutUint16(out[10:], 0)
	return out
}

// answered is an upstream answer to query with one A record per address.
func answered(query []byte, ttl uint32, addresses ...[4]byte) []byte {
	out := header(query, 0x8180)
	binary.BigEndian.PutUint16(out[6:], uint16(len(addresses)))
	for _, address := range addresses {
		out = append(out, 0xc0, 12, 0, 1, 0, 1)
		out = binary.BigEndian.AppendUint32(out, ttl)
		out = append(out, 0, 4)
		out = append(out, address[:]...)
	}
	return out
}

// big pads an answer past one TCP slot.
func big(query []byte, address [4]byte) []byte {
	out := answered(query, 60, address)
	const padding = 24
	for range padding {
		out = append(out, 0xc0, 12, 0, 16, 0, 1, 0, 0, 0, 60, 1, 0, 255)
		out = append(out, make([]byte, 255)...)
	}
	binary.BigEndian.PutUint16(out[10:], padding)
	return out
}

func framed(message []byte) []byte {
	return append(binary.BigEndian.AppendUint16(nil, uint16(len(message))), message...)
}

// parsed is a stub reply as a client reads it.
type parsed struct {
	id        uint16
	flags     uint16
	rcode     uint16
	question  []byte
	addresses [][4]byte
	ttl       uint32
	opt       bool
}

func parse(t *testing.T, reply []byte) parsed {
	t.Helper()
	var name, scratch dnswire.Name
	out := parsed{id: binary.BigEndian.Uint16(reply), flags: binary.BigEndian.Uint16(reply[2:])}
	if binary.BigEndian.Uint16(reply[4:]) == 0 {
		out.rcode = out.flags & dnswire.RcodeMask
		return out
	}
	next, ok := dnswire.DecodeName(reply, dnswire.HeaderSize, &name)
	if !ok {
		t.Fatalf("reply question % x", reply)
	}
	out.question = append([]byte(nil), reply[dnswire.HeaderSize:next+4]...)
	if binary.BigEndian.Uint16(reply[next+2:]) != dnswire.ClassIN {
		out.rcode = out.flags & dnswire.RcodeMask
		return out
	}
	var response dnswire.Response
	qtype := binary.BigEndian.Uint16(reply[next:])
	if result := dnswire.ParseResponse(reply, len(reply), out.id, &name, qtype, &scratch, &response); result != dnswire.ParseOK {
		t.Fatalf("reply does not parse (%d): % x", result, reply)
	}
	out.rcode, out.opt = response.Rcode, response.HasOPT
	position := response.AnswerAt
	var item dnswire.Record
	for index := 0; index < response.Answers; index++ {
		position, _ = dnswire.ReadRecord(reply, position, &scratch, &item)
		if item.Type == dnswire.TypeA {
			out.addresses = append(out.addresses, [4]byte(reply[item.Data:item.Data+4]))
			out.ttl = item.TTL
		}
	}
	return out
}
