package udpserve

import (
	"testing"

	"goos/kernel/ipc"
	"goos/userspace/network"
	"goos/userspace/udpapi"
)

// answered is one reply a session sent, as the client decodes it.
type answered struct {
	reply       udpapi.Reply
	correlation uint64
}

// recorder is the session's channel: it passes every reply through the
// GUDP1 codec, as the service does, and keeps it.
type recorder struct {
	t       *testing.T
	replies []answered
	broken  bool
}

func (r *recorder) Reply(reply *udpapi.Reply, correlation uint64) bool {
	r.t.Helper()
	if r.broken {
		return false
	}
	var message ipc.Message
	if !udpapi.EncodeReply(reply, correlation, &message) {
		r.t.Fatalf("service produced an invalid reply %+v", reply)
	}
	var decoded udpapi.Reply
	got, ok := udpapi.DecodeReply(&message, &decoded)
	if !ok || got != correlation {
		r.t.Fatalf("reply does not decode: %+v", reply)
	}
	r.replies = append(r.replies, answered{decoded, got})
	return true
}

// take returns the replies sent since the last call.
func (r *recorder) take() []answered {
	out := r.replies
	r.replies = nil
	return out
}

// client is one session with its pools and a correlation counter.
type client struct {
	t       *testing.T
	stack   *network.Stack
	session Session
	out     recorder
	tx, rx  []byte
	next    uint64
	now     uint64
}

var loopback = [4]byte{127, 0, 0, 1}

func newStack() *network.Stack {
	stack := new(network.Stack)
	stack.Init(0x5eed)
	return stack
}

func newClient(t *testing.T, stack *network.Stack, owner int) *client {
	c := &client{t: t, stack: stack, out: recorder{t: t}, now: 1_000_000_000,
		tx: make([]byte, udpapi.TXSlots*udpapi.SlotSize), rx: make([]byte, udpapi.RXSlots*udpapi.SlotSize)}
	c.session.Start(stack, owner, c.tx, c.rx)
	return c
}

// send passes a request through the codec into the session and returns its
// correlation.
func (c *client) send(request udpapi.Request) uint64 {
	c.t.Helper()
	correlation := uint64(0)
	if request.Opcode != udpapi.OpRelease {
		c.next++
		correlation = c.next
	}
	var message ipc.Message
	if !udpapi.EncodeRequest(&request, correlation, &message) {
		c.t.Fatalf("invalid request %+v", request)
	}
	var decoded udpapi.Request
	if _, ok := udpapi.DecodeRequest(&message, &decoded); !ok {
		c.t.Fatalf("request does not decode: %+v", request)
	}
	if fault := c.session.Handle(c.stack, &decoded, correlation, c.now, &c.out); fault != FaultNone {
		c.t.Fatalf("session fault %q on %+v", fault, request)
	}
	return correlation
}

// call sends a request and expects exactly its own reply.
func (c *client) call(request udpapi.Request) udpapi.Reply {
	c.t.Helper()
	correlation := c.send(request)
	replies := c.out.take()
	if len(replies) != 1 || replies[0].correlation != correlation || replies[0].reply.Opcode != request.Opcode {
		c.t.Fatalf("replies %+v to %+v", replies, request)
	}
	return replies[0].reply
}

func (c *client) open() uint32 {
	c.t.Helper()
	reply := c.call(udpapi.Request{Opcode: udpapi.OpOpen})
	if reply.Result != udpapi.ResultOK {
		c.t.Fatalf("open %d", reply.Result)
	}
	return reply.Endpoint
}

func (c *client) bind(endpoint uint32, port uint16) {
	c.t.Helper()
	if reply := c.call(udpapi.Request{Opcode: udpapi.OpBind, Endpoint: endpoint, Address: loopback, Port: port}); reply.Result != udpapi.ResultOK {
		c.t.Fatalf("bind %d", reply.Result)
	}
}

// sendTo sends text from slot 0 of the TX pool.
func (c *client) sendTo(endpoint uint32, port uint16, text string) {
	c.t.Helper()
	length := copy(c.tx, text)
	reply := c.call(udpapi.Request{Opcode: udpapi.OpSendTo, Endpoint: endpoint, Address: loopback, Port: port, Length: uint16(length)})
	if reply.Result != udpapi.ResultOK || reply.Slot != 0 {
		c.t.Fatalf("send %+v", reply)
	}
}

func (c *client) payload(reply udpapi.Reply) string {
	offset := udpapi.Offset(reply.Slot)
	return string(c.rx[offset : offset+uint64(reply.Length)])
}

// pair is a listener on 127.0.0.1:port in one session and a sender in
// another.
func pair(t *testing.T, port uint16) (*network.Stack, *client, uint32, *client, uint32) {
	stack := newStack()
	listener := newClient(t, stack, 0)
	bound := listener.open()
	listener.bind(bound, port)
	sender := newClient(t, stack, 1)
	return stack, listener, bound, sender, sender.open()
}

func TestDeferredRecvFromCompletesItsOwnRequest(t *testing.T) {
	stack, listener, bound, sender, from := pair(t, 7000)
	waiting := listener.send(udpapi.Request{Opcode: udpapi.OpRecvFrom, Endpoint: bound})
	if replies := listener.out.take(); len(replies) != 0 || listener.session.Waiting() != 1 {
		t.Fatalf("RecvFrom with nothing queued answered at once: %+v", replies)
	}
	if fault := listener.session.Deliver(stack, &listener.out); fault != FaultNone || len(listener.out.take()) != 0 {
		t.Fatal("Deliver answered without a datagram")
	}
	sender.sendTo(from, 7000, "late")
	if fault := listener.session.Deliver(stack, &listener.out); fault != FaultNone {
		t.Fatal(fault)
	}
	replies := listener.out.take()
	if len(replies) != 1 || replies[0].correlation != waiting || replies[0].reply.Result != udpapi.ResultOK ||
		listener.payload(replies[0].reply) != "late" || replies[0].reply.Local != loopback || replies[0].reply.Endpoint != bound {
		t.Fatalf("delivery %+v", replies)
	}
	if listener.session.Waiting() != 0 || !listener.session.RXLent(replies[0].reply.Slot) {
		t.Fatal("completed receive still held, or its slot not lent")
	}
	if fault := listener.session.Deliver(stack, &listener.out); fault != FaultNone || len(listener.out.take()) != 0 {
		t.Fatal("one datagram answered twice")
	}
	listener.send(udpapi.Request{Opcode: udpapi.OpRelease, Slot: replies[0].reply.Slot})
	if listener.session.RXLent(replies[0].reply.Slot) {
		t.Fatal("released slot still lent")
	}
}

func TestQueuedDatagramAnswersAtOnce(t *testing.T) {
	_, listener, bound, sender, from := pair(t, 7001)
	sender.sendTo(from, 7001, "early")
	reply := listener.call(udpapi.Request{Opcode: udpapi.OpRecvFrom, Endpoint: bound})
	if reply.Result != udpapi.ResultOK || listener.payload(reply) != "early" {
		t.Fatalf("reply %+v", reply)
	}
}

func TestSecondRecvFromIsBusy(t *testing.T) {
	_, listener, bound, _, _ := pair(t, 7002)
	listener.send(udpapi.Request{Opcode: udpapi.OpRecvFrom, Endpoint: bound})
	if reply := listener.call(udpapi.Request{Opcode: udpapi.OpRecvFrom, Endpoint: bound}); reply.Result != udpapi.ResultBusy {
		t.Fatalf("second RecvFrom %d", reply.Result)
	}
	if listener.session.Waiting() != 1 {
		t.Fatal("Busy dropped the first receive")
	}
	unbound := listener.open()
	if reply := listener.call(udpapi.Request{Opcode: udpapi.OpRecvFrom, Endpoint: unbound}); reply.Result != udpapi.ResultInvalidArgument {
		t.Fatalf("RecvFrom on an unbound endpoint %d", reply.Result)
	}
}

func TestCloseAnswersTheWaitingReceiveFirst(t *testing.T) {
	stack, listener, bound, sender, from := pair(t, 7003)
	waiting := listener.send(udpapi.Request{Opcode: udpapi.OpRecvFrom, Endpoint: bound})
	closing := listener.send(udpapi.Request{Opcode: udpapi.OpClose, Endpoint: bound})
	replies := listener.out.take()
	if len(replies) != 2 || replies[0].correlation != waiting || replies[0].reply.Result != udpapi.ResultClosed ||
		replies[1].correlation != closing || replies[1].reply.Result != udpapi.ResultOK {
		t.Fatalf("close replies %+v", replies)
	}
	if listener.session.Waiting() != 0 {
		t.Fatal("closed endpoint still waits")
	}
	sender.sendTo(from, 7003, "too late")
	if fault := listener.session.Deliver(stack, &listener.out); fault != FaultNone || len(listener.out.take()) != 0 {
		t.Fatal("datagram after Close reached the client")
	}
	if stats := stack.UDPStats(); stats.DropNoEndpoint != 1 || stats.Endpoints != 1 {
		t.Fatalf("stats %+v", stats)
	}
}

// TestStaleCompletionCannotSatisfyANewRequest reuses a closed endpoint's
// slot. A late request naming the old endpoint is refused, and a datagram
// for the new endpoint completes the new request only.
func TestStaleCompletionCannotSatisfyANewRequest(t *testing.T) {
	stack, listener, old, sender, from := pair(t, 7004)
	stale := listener.send(udpapi.Request{Opcode: udpapi.OpRecvFrom, Endpoint: old})
	listener.send(udpapi.Request{Opcode: udpapi.OpClose, Endpoint: old})
	if replies := listener.out.take(); len(replies) != 2 || replies[0].correlation != stale {
		t.Fatalf("close replies %+v", replies)
	}
	reopened := listener.open()
	if network.EndpointID(reopened) == network.EndpointID(old) {
		t.Fatal("new endpoint reused the old ID")
	}
	listener.bind(reopened, 7004)
	if reply := listener.call(udpapi.Request{Opcode: udpapi.OpRecvFrom, Endpoint: old}); reply.Result != udpapi.ResultInvalidArgument {
		t.Fatalf("RecvFrom on the closed ID %d", reply.Result)
	}
	fresh := listener.send(udpapi.Request{Opcode: udpapi.OpRecvFrom, Endpoint: reopened})
	sender.sendTo(from, 7004, "new")
	listener.session.Deliver(stack, &listener.out)
	replies := listener.out.take()
	if len(replies) != 1 || replies[0].correlation != fresh || replies[0].correlation == stale ||
		replies[0].reply.Endpoint != reopened || listener.payload(replies[0].reply) != "new" {
		t.Fatalf("replies %+v", replies)
	}
}

func TestDeliveryWaitsForAFreeRXSlot(t *testing.T) {
	stack, listener, bound, sender, from := pair(t, 7005)
	var slots []uint8
	for count := 0; count < udpapi.RXSlots; count++ {
		sender.sendTo(from, 7005, "x")
		reply := listener.call(udpapi.Request{Opcode: udpapi.OpRecvFrom, Endpoint: bound})
		slots = append(slots, reply.Slot)
	}
	sender.sendTo(from, 7005, "ninth")
	waiting := listener.send(udpapi.Request{Opcode: udpapi.OpRecvFrom, Endpoint: bound})
	if len(listener.out.take()) != 0 {
		t.Fatal("answered with every RX slot lent")
	}
	listener.send(udpapi.Request{Opcode: udpapi.OpRelease, Slot: slots[3]})
	replies := listener.out.take()
	if len(replies) != 1 || replies[0].correlation != waiting || replies[0].reply.Slot != slots[3] ||
		listener.payload(replies[0].reply) != "ninth" {
		t.Fatalf("after release %+v", replies)
	}
	_ = stack
}

func TestTeardownCancelsWaitingReceives(t *testing.T) {
	stack, listener, bound, sender, from := pair(t, 7006)
	sender.sendTo(from, 7006, "held")
	held := listener.call(udpapi.Request{Opcode: udpapi.OpRecvFrom, Endpoint: bound})
	listener.send(udpapi.Request{Opcode: udpapi.OpRecvFrom, Endpoint: bound})
	sender.sendTo(from, 7006, "queued")
	second := listener.open()
	listener.bind(second, 7007)
	listener.send(udpapi.Request{Opcode: udpapi.OpRecvFrom, Endpoint: second})
	listener.out.take()
	listener.session.End(stack)
	if listener.session.Waiting() != 0 || listener.session.RXLent(held.Slot) || len(listener.out.take()) != 0 {
		t.Fatal("teardown kept a receive or a lent slot, or answered the gone client")
	}
	if stats := stack.UDPStats(); stats.Endpoints != 1 {
		t.Fatalf("endpoints after teardown %d", stats.Endpoints)
	}
	// The owner starts clean: the ports are free and nothing queued
	// survives.
	listener.session.Start(stack, 0, listener.tx, listener.rx)
	again := listener.open()
	listener.bind(again, 7006)
	listener.send(udpapi.Request{Opcode: udpapi.OpRecvFrom, Endpoint: again})
	if replies := listener.out.take(); len(replies) != 0 || listener.session.Waiting() != 1 {
		t.Fatalf("queued datagram outlived its session: %+v", replies)
	}
}

func TestReleaseOfUnheldSlotFaults(t *testing.T) {
	_, listener, _, _, _ := pair(t, 7008)
	request := udpapi.Request{Opcode: udpapi.OpRelease, Slot: 2}
	if fault := listener.session.Handle(listener.stack, &request, 0, listener.now, &listener.out); fault != FaultRelease {
		t.Fatalf("fault %q", fault)
	}
}

func TestUnreachableClientFaults(t *testing.T) {
	stack, listener, bound, sender, from := pair(t, 7009)
	listener.send(udpapi.Request{Opcode: udpapi.OpRecvFrom, Endpoint: bound})
	sender.sendTo(from, 7009, "x")
	listener.out.broken = true
	if fault := listener.session.Deliver(stack, &listener.out); fault != FaultUnreachable {
		t.Fatalf("fault %q", fault)
	}
}

func TestOpenRespectsTheSessionShare(t *testing.T) {
	stack := newStack()
	c := newClient(t, stack, 0)
	for count := 0; count < network.MaxUDPSessionEndpoints; count++ {
		c.open()
	}
	if reply := c.call(udpapi.Request{Opcode: udpapi.OpOpen}); reply.Result != udpapi.ResultBusy || reply.Endpoint != 0 {
		t.Fatalf("open past the share %+v", reply)
	}
	other := newClient(t, stack, 1)
	other.open()
}

func TestStackResultsMap(t *testing.T) {
	for result := network.ResultOK; result <= network.ResultAddressInUse; result++ {
		if Result(result).Text() == "" {
			t.Fatalf("result %d maps to nothing", result)
		}
	}
	if Result(network.ResultAddressInUse) != udpapi.ResultAddressInUse || Result(network.ResultTimeout) != udpapi.ResultInvalidArgument {
		t.Fatal("mapping")
	}
}
