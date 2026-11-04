package linuxsock

import (
	"testing"

	"goos/kernel/ipc"
	"goos/userspace/linuxsockapi"
)

type submitted struct {
	proto   Proto
	tag     uint64
	request NativeRequest
	data    []byte
}

// fakeBackend records what the core submits. busy makes every submission
// answer SubmitBusy; unsupported lists operations the session lacks.
type fakeBackend struct {
	log         []submitted
	busy        bool
	unsupported map[NativeOp]bool
}

func (b *fakeBackend) Submit(proto Proto, tag uint64, request *NativeRequest) SubmitStatus {
	if tag == 0 {
		panic("zero tag")
	}
	if b.unsupported[request.Op] {
		return SubmitUnsupported
	}
	if b.busy {
		return SubmitBusy
	}
	entry := submitted{proto: proto, tag: tag, request: *request}
	entry.data = append([]byte(nil), request.Data...)
	entry.request.Data = nil
	b.log = append(b.log, entry)
	return SubmitAccepted
}

// fakeTransport keeps every message the core sends, round-tripped through
// the wire format. full makes Send fail.
type fakeTransport struct {
	sent    map[int][]ipc.Message
	full    map[int]bool
	dropped []int
}

func (t *fakeTransport) Send(family int, message *ipc.Message) bool {
	if t.full[family] {
		return false
	}
	var wire [ipc.WireMaxSize]byte
	size, ok := ipc.EncodeFrom(message, wire[:])
	if !ok {
		panic("wire encode")
	}
	var out ipc.Message
	if !ipc.DecodeInto(wire[:size], &out) {
		panic("wire decode")
	}
	t.sent[family] = append(t.sent[family], out)
	return true
}

func (t *fakeTransport) Drop(family int) { t.dropped = append(t.dropped, family) }

type harness struct {
	t           *testing.T
	svc         *Service
	backend     *fakeBackend
	transport   *fakeTransport
	pools       map[int][]byte
	correlation uint64
	generation  [UDP + 1]uint64
}

func newHarness(t *testing.T) *harness {
	h := &harness{t: t, svc: new(Service), backend: &fakeBackend{unsupported: map[NativeOp]bool{}},
		transport: &fakeTransport{sent: map[int][]ipc.Message{}, full: map[int]bool{}}, pools: map[int][]byte{}}
	h.svc.Init(h.backend, h.transport)
	h.up(TCP)
	h.up(UDP)
	return h
}

func (h *harness) up(proto Proto) {
	h.generation[proto]++
	if result := h.svc.BackendUp(proto, h.generation[proto]); result != linuxsockapi.ControlOK {
		h.t.Fatalf("backend up %d: %d", proto, result)
	}
}

func (h *harness) family(token uint64, session uint32) int {
	pool := make([]byte, linuxsockapi.PoolBytes)
	index, result := h.svc.AddFamily(token, session, pool)
	if result != linuxsockapi.ControlOK {
		h.t.Fatalf("add family: %d", result)
	}
	h.pools[index] = pool
	return index
}

func (h *harness) send(family int, request linuxsockapi.Request) uint64 {
	h.t.Helper()
	correlation := uint64(0)
	if request.Opcode != linuxsockapi.OpDoorbell && request.Opcode != linuxsockapi.OpReadyChanged {
		h.correlation++
		correlation = h.correlation
	}
	var message ipc.Message
	if !linuxsockapi.EncodeRequest(&request, correlation, &message) {
		h.t.Fatalf("encode %+v", request)
	}
	h.svc.Receive(family, roundTrip(h.t, &message))
	return correlation
}

func roundTrip(t *testing.T, message *ipc.Message) *ipc.Message {
	t.Helper()
	var wire [ipc.WireMaxSize]byte
	size, ok := ipc.EncodeFrom(message, wire[:])
	if !ok {
		t.Fatal("wire encode")
	}
	var out ipc.Message
	if !ipc.DecodeInto(wire[:size], &out) {
		t.Fatal("wire decode")
	}
	return &out
}

// reply returns the reply to correlation, failing when there is none.
func (h *harness) reply(family int, correlation uint64) linuxsockapi.Reply {
	h.t.Helper()
	reply, ok := h.findReply(family, correlation)
	if !ok {
		h.t.Fatalf("no reply to %d", correlation)
	}
	return reply
}

func (h *harness) findReply(family int, correlation uint64) (linuxsockapi.Reply, bool) {
	h.t.Helper()
	var found linuxsockapi.Reply
	count := 0
	for _, message := range h.transport.sent[family] {
		if message.Kind != ipc.KindReply || message.Correlation != correlation {
			continue
		}
		var reply linuxsockapi.Reply
		if _, ok := linuxsockapi.DecodeReply(&message, &reply); !ok {
			h.t.Fatalf("undecodable reply %+v", message)
		}
		found = reply
		count++
	}
	if count > 1 {
		h.t.Fatalf("%d replies to %d", count, correlation)
	}
	return found, count == 1
}

func (h *harness) noReply(family int, correlation uint64) {
	h.t.Helper()
	if _, ok := h.findReply(family, correlation); ok {
		h.t.Fatalf("unexpected reply to %d", correlation)
	}
}

func (h *harness) call(family int, request linuxsockapi.Request) linuxsockapi.Reply {
	h.t.Helper()
	return h.reply(family, h.send(family, request))
}

// notifications counts ReadyChanged messages sent to family.
func (h *harness) notifications(family int) int {
	count := 0
	for _, message := range h.transport.sent[family] {
		var request linuxsockapi.Request
		if message.Kind == ipc.KindNotification {
			if _, ok := linuxsockapi.DecodeRequest(&message, &request); !ok || request.Opcode != linuxsockapi.OpReadyChanged {
				h.t.Fatalf("bad notification %+v", message)
			}
			count++
		}
	}
	return count
}

// take removes and returns the oldest submission of op.
func (h *harness) take(op NativeOp) submitted {
	h.t.Helper()
	for index, entry := range h.backend.log {
		if entry.request.Op == op {
			h.backend.log = append(h.backend.log[:index], h.backend.log[index+1:]...)
			return entry
		}
	}
	h.t.Fatalf("no native %d submitted; log %+v", op, h.backend.log)
	return submitted{}
}

func (h *harness) has(op NativeOp) bool {
	for _, entry := range h.backend.log {
		if entry.request.Op == op {
			return true
		}
	}
	return false
}

func (h *harness) complete(entry submitted, c NativeCompletion) {
	c.Op = entry.request.Op
	h.svc.Complete(entry.proto, h.generation[entry.proto], entry.tag, &c)
}

var nextEndpoint uint32 = 100

// open opens a socket of kind and completes its native Open.
func (h *harness) open(family int, kind uint32) uint64 {
	h.t.Helper()
	correlation := h.send(family, linuxsockapi.Request{Opcode: linuxsockapi.OpOpen, Length: kind})
	nextEndpoint++
	h.complete(h.take(NativeOpen), NativeCompletion{Endpoint: nextEndpoint})
	reply := h.reply(family, correlation)
	if reply.Result != linuxsockapi.ResultOK {
		h.t.Fatalf("open: %d", reply.Result)
	}
	return reply.Socket
}

// connected opens a stream and connects it at once.
func (h *harness) connected(family int) uint64 {
	h.t.Helper()
	id := h.open(family, linuxsockapi.KindStream)
	correlation := h.send(family, linuxsockapi.Request{Opcode: linuxsockapi.OpConnect, Socket: id, Address: peerAddress, Port: 80})
	h.complete(h.take(NativeConnect), NativeCompletion{Local: localAddress, LocalPort: 40000})
	// A stream connect netd finished at once still answers InProgress;
	// the kernel reads the outcome through GetPendingError.
	if reply := h.reply(family, correlation); reply.Result != linuxsockapi.ResultInProgress {
		h.t.Fatalf("connect: %d", reply.Result)
	}
	if reply := h.call(family, linuxsockapi.Request{Opcode: linuxsockapi.OpGetPendingError, Socket: id}); reply.Result != linuxsockapi.ResultOK || reply.Value != 0 {
		h.t.Fatalf("connect outcome: %+v", reply)
	}
	return id
}

// wake answers the socket's held wait with mask.
func (h *harness) wake(endpoint uint32, mask NativeMask, readable uint32) {
	h.t.Helper()
	for index, entry := range h.backend.log {
		if entry.request.Op == NativeWaitReady && entry.request.Endpoint == endpoint {
			h.backend.log = append(h.backend.log[:index], h.backend.log[index+1:]...)
			h.complete(entry, NativeCompletion{Mask: mask, Readable: readable})
			return
		}
	}
	h.t.Fatalf("no wait held on %d", endpoint)
}

func (h *harness) waiting(endpoint uint32) bool {
	count := 0
	for _, entry := range h.backend.log {
		if entry.request.Op == NativeWaitReady && entry.request.Endpoint == endpoint {
			count++
		}
	}
	if count > 1 {
		h.t.Fatalf("%d waits held on %d", count, endpoint)
	}
	return count == 1
}

func (h *harness) endpoint(id uint64) uint32 {
	for index := range h.svc.sockets {
		if h.svc.sockets[index].used && h.svc.sockets[index].id == id {
			return h.svc.sockets[index].endpoint
		}
	}
	h.t.Fatalf("no socket %x", id)
	return 0
}

func (h *harness) readiness(family int, id uint64) linuxsockapi.Readiness {
	h.t.Helper()
	index, _ := linuxsockapi.SocketIndex(id)
	state, ok := linuxsockapi.Read(h.pools[family], index, 4)
	if !ok {
		h.t.Fatal("readiness entry unreadable")
	}
	return state
}

func (h *harness) dropped(family int) bool {
	for _, index := range h.transport.dropped {
		if index == family {
			return true
		}
	}
	return false
}

var (
	peerAddress  = [4]byte{10, 0, 2, 2}
	localAddress = [4]byte{10, 0, 2, 15}
)
