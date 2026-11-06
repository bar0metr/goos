package linuxsock

import (
	"testing"

	"goos/kernel/ipc"
	"goos/userspace/linuxsockapi"
	"goos/userspace/network"
	"goos/userspace/tcpapi"
	"goos/userspace/udpapi"
	"goos/userspace/udpserve"
)

// udpWorld runs linuxsockd's core and its UDPRingLink against netd's real
// GUDP 3 session and stack, over bounded queues, and plays the kernel's
// side of one family's rings. As in world, each party runs only on what
// wakes it, and arms its doorbell and checks its rings before it sleeps.
type udpWorld struct {
	t       *testing.T
	h       *harness
	link    *UDPRingLink
	ch      boundedChannel
	stack   network.Stack
	session udpserve.Session3
	now     uint64
	family  int
	replies []ipc.Message
	in      udpapi.Request
	// Channel traffic between linuxsockd and netd.
	requests, doorbellsToNetd, doorbellsFromNetd int
	txBusy                                       [linuxsockapi.TXSlots]bool
}

type udpWorldNotifier struct{ w *udpWorld }

func (n udpWorldNotifier) Reply(reply *udpapi.Reply, correlation uint64) bool {
	var message ipc.Message
	if !udpapi.EncodeReply(reply, correlation, &message) {
		n.w.t.Fatalf("netd encoded a bad reply %+v", reply)
	}
	n.w.replies = append(n.w.replies, message)
	return true
}

func (n udpWorldNotifier) Doorbell() bool {
	var message ipc.Message
	udpapi.EncodeServiceDoorbell(&message)
	n.w.replies = append(n.w.replies, message)
	n.w.doorbellsFromNetd++
	return true
}

// udpWorldBackend sends UDP to the ring link; TCP goes to a link no one
// serves.
type udpWorldBackend struct {
	w     *udpWorld
	tcp   NativeLink
	tcpCh boundedChannel
}

func (b *udpWorldBackend) Submit(proto Proto, tag uint64, request *NativeRequest) SubmitStatus {
	if proto == UDP {
		return b.w.link.Submit(&b.w.ch, tag, request)
	}
	return b.tcp.Submit(&b.tcpCh, tag, request)
}

func newUDPWorld(t *testing.T) *udpWorld {
	h := newHarness(t)
	w := &udpWorld{t: t, h: h, now: 1_000_000_000, link: new(UDPRingLink)}
	w.ch.capacity = ipc.QueueCapacity
	b := &udpWorldBackend{w: w}
	b.tcpCh.capacity = ipc.QueueCapacity
	b.tcp.Reset(TCP, h.generation[TCP], tcpapi.Layout3, make([]byte, tcpapi.Layout3.TXPool()), make([]byte, tcpapi.Layout3.RXPool()))
	h.svc.backend = b
	w.stack.Init(77)
	tx, rx := make([]byte, udpapi.TXPoolPages3*4096), make([]byte, udpapi.RXPoolPages3*4096)
	if !w.session.Start(&w.stack, 0, network.MaxUDPSessionEndpoints, tx, rx) {
		t.Fatal("start session")
	}
	if !w.link.Reset(h.generation[UDP], tx, rx) {
		t.Fatal("ring link")
	}
	w.session.Idle()
	w.family = h.family(1, 7)
	w.settle()
	return w
}

func (w *udpWorld) netd() bool {
	if len(w.ch.queue) == 0 {
		return false
	}
	queue := w.ch.queue
	w.ch.queue = nil
	for index := range queue {
		message := &queue[index]
		if udpapi.IsDoorbell(message) {
			w.doorbellsToNetd++
			if fault, _ := w.session.Deliver(&w.stack, w.now, udpWorldNotifier{w}); fault != udpserve.FaultNone {
				w.t.Fatalf("netd fault %q", fault)
			}
			continue
		}
		correlation, ok := udpapi.DecodeRequest(message, &w.in)
		if !ok {
			w.t.Fatal("netd: undecodable request")
		}
		w.requests++
		if fault := w.session.Handle(&w.stack, &w.in, correlation, w.now, udpWorldNotifier{w}); fault != udpserve.FaultNone {
			w.t.Fatalf("netd fault %q", fault)
		}
	}
	for more := true; more; {
		var fault udpserve.Fault
		if fault, more = w.session.Deliver(&w.stack, w.now, udpWorldNotifier{w}); fault != udpserve.FaultNone {
			w.t.Fatalf("netd fault %q", fault)
		}
	}
	for !w.session.Idle() {
		w.session.Deliver(&w.stack, w.now, udpWorldNotifier{w})
	}
	if endpoint, op, found := w.session.Runnable(&w.stack); found {
		w.t.Fatalf("netd would sleep with a runnable %s on endpoint %d", op, endpoint)
	}
	return true
}

func (w *udpWorld) linuxsockd() bool {
	moved := false
	for len(w.replies) != 0 {
		message := w.replies[0]
		w.replies = w.replies[1:]
		if !w.link.Receive(&w.ch, &message, w.h.svc) {
			w.t.Fatal("linuxsockd: netd broke the protocol")
		}
		moved = true
	}
	for spins := 0; !w.link.Idle(&w.ch, w.h.svc); spins++ {
		moved = true
		if spins > 1000 {
			w.t.Fatalf("linuxsockd never idles: %+v", w.link.Stats)
		}
	}
	if w.link.Broken() {
		w.t.Fatal("ring link broken")
	}
	if w.link.rings.Pending() || len(w.link.local) != 0 {
		w.t.Fatal("linuxsockd would sleep with work runnable")
	}
	return moved
}

func (w *udpWorld) settle() {
	for round := 0; round < 10000; round++ {
		if !w.netd() && !w.linuxsockd() {
			return
		}
	}
	w.t.Fatal("services never settled")
}

func (w *udpWorld) call(request linuxsockapi.Request) linuxsockapi.Reply {
	w.t.Helper()
	correlation := w.h.send(w.family, request)
	for round := 0; round < 100; round++ {
		if reply, ok := w.h.findReply(w.family, correlation); ok {
			return reply
		}
		w.settle()
	}
	w.t.Fatalf("no reply to %+v", request)
	return linuxsockapi.Reply{}
}

// bound opens a datagram socket bound to a loopback port and returns it
// and the port.
func (w *udpWorld) bound() (uint64, uint16) {
	w.t.Helper()
	id := w.call(linuxsockapi.Request{Opcode: opOpen, Length: linuxsockapi.KindDatagram}).Socket
	if r := w.call(linuxsockapi.Request{Opcode: opBind, Socket: id, Address: [4]byte{127, 0, 0, 1}}); r.Result != linuxsockapi.ResultOK {
		w.t.Fatalf("bind %d", r.Result)
	}
	port := w.call(linuxsockapi.Request{Opcode: linuxsockapi.OpGetSockName, Socket: id}).LocalPort
	return id, port
}

// sendDatagram commits one datagram from socket on a free kernel TX slot.
func (w *udpWorld) sendDatagram(socket uint64, data []byte) {
	w.t.Helper()
	for slot := uint8(0); slot < linuxsockapi.TXSlots; slot++ {
		if !w.txBusy[slot] {
			w.txBusy[slot] = true
			w.h.commit(w.family, slot, socket, data, false, [4]byte{}, 0)
			return
		}
	}
	w.t.Fatal("no free kernel TX slot")
}

// receive runs the world until socket receives a datagram and returns it.
func (w *udpWorld) receive(socket uint64) []byte {
	w.t.Helper()
	for round := 0; round < 100; round++ {
		w.settle()
		for _, entry := range w.h.completions(w.family) {
			switch entry.Kind {
			case linuxsockapi.RingTXDone:
				w.txBusy[entry.Slot] = false
			case linuxsockapi.RingRXData:
				if entry.Socket != socket {
					w.t.Fatalf("datagram for %x, want %x", entry.Socket, socket)
				}
				data := append([]byte(nil), w.h.rxBytes(w.family, entry)...)
				w.h.releaseRX(w.family, entry.Slot, entry.Generation)
				return data
			}
		}
	}
	w.t.Fatal("no datagram")
	return nil
}

func TestUDPRingExchangesDatagramsWithoutRequests(t *testing.T) {
	w := newUDPWorld(t)
	a, portA := w.bound()
	b, portB := w.bound()
	loop := [4]byte{127, 0, 0, 1}
	if r := w.call(linuxsockapi.Request{Opcode: opConnect, Socket: a, Address: loop, Port: portB}); r.Result != linuxsockapi.ResultOK {
		t.Fatalf("connect a %d", r.Result)
	}
	if r := w.call(linuxsockapi.Request{Opcode: opConnect, Socket: b, Address: loop, Port: portA}); r.Result != linuxsockapi.ResultOK {
		t.Fatalf("connect b %d", r.Result)
	}
	w.settle()
	before := w.requests
	for round := 0; round < 200; round++ {
		request := pattern(64, byte(round))
		w.sendDatagram(a, request)
		got := w.receive(b)
		if string(got) != string(request) {
			t.Fatalf("round %d: b got %d bytes", round, len(got))
		}
		w.sendDatagram(b, got)
		back := w.receive(a)
		if string(back) != string(request) {
			t.Fatalf("round %d: a got %d bytes", round, len(back))
		}
	}
	w.settle()
	if w.requests != before {
		t.Fatalf("200 rounds cost %d channel requests", w.requests-before)
	}
	if w.link.Stats.Sends != 400 || w.link.Stats.Datagrams < 400 {
		t.Fatalf("link stats %+v", w.link.Stats)
	}
	t.Logf("doorbells to netd %d, from netd %d over 400 datagrams", w.doorbellsToNetd, w.doorbellsFromNetd)
	if r := w.call(linuxsockapi.Request{Opcode: opClose, Socket: a}); r.Result != linuxsockapi.ResultOK {
		t.Fatalf("close a %d", r.Result)
	}
	if r := w.call(linuxsockapi.Request{Opcode: opClose, Socket: b}); r.Result != linuxsockapi.ResultOK {
		t.Fatalf("close b %d", r.Result)
	}
	w.settle()
	if held, _, _ := w.link.Snapshot(); held != 0 {
		t.Fatalf("TX slots still held: %d", held)
	}
}

func TestUDPRingRejectsForgedEntries(t *testing.T) {
	stack := new(network.Stack)
	stack.Init(5)
	var session udpserve.Session3
	tx, rx := make([]byte, udpapi.TXPoolPages3*4096), make([]byte, udpapi.RXPoolPages3*4096)
	if !session.Start(stack, 0, 8, tx, rx) {
		t.Fatal("start")
	}
	client, _ := udpapi.ClientRings3(tx[udpapi.TXControl3:], rx[udpapi.RXControl3:])
	w := &udpWorld{t: t}
	// A Release of a slot the client never held ends the session.
	if client.Push(udpapi.RingRelease, &udpapi.Entry{Kind: udpapi.EntryRelease, Slot: 3, Generation: 1}) != 0 {
		t.Fatal("push")
	}
	if fault, _ := session.Deliver(stack, 1, udpWorldNotifier{w}); fault != udpserve.FaultProtocol {
		t.Fatalf("forged release: fault %q", fault)
	}
	// A send whose generation skips ahead does too.
	tx, rx = make([]byte, udpapi.TXPoolPages3*4096), make([]byte, udpapi.RXPoolPages3*4096)
	if !session.Start(stack, 0, 8, tx, rx) {
		t.Fatal("restart")
	}
	client, _ = udpapi.ClientRings3(tx[udpapi.TXControl3:], rx[udpapi.RXControl3:])
	if client.Push(udpapi.RingTX, &udpapi.Entry{Kind: udpapi.EntrySend, Slot: 0, Endpoint: 1, Generation: 2}) != 0 {
		t.Fatal("push")
	}
	if fault, _ := session.Deliver(stack, 1, udpWorldNotifier{w}); fault != udpserve.FaultProtocol {
		t.Fatalf("skipped generation: fault %q", fault)
	}
}
