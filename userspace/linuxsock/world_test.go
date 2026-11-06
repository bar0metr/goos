package linuxsock

import (
	"testing"

	"goos/kernel/ipc"
	"goos/userspace/linuxsockapi"
	"goos/userspace/network"
	"goos/userspace/tcpapi"
	"goos/userspace/tcpserve"
	"goos/userspace/udpapi"
)

// world runs linuxsockd's core and its RingLink against the real GTCP 4
// service and stack of netd, over queues bounded as the kernel bounds
// channels, and plays the kernel's side of one family's rings. Each party
// runs only on the events that wake it in GoOS: linuxsockd on a reply or a
// doorbell, a family message, its native channel turning writable while it
// asked for that, or the short timeout it sleeps with while Release entries
// wait for ring room; netd on a request or a doorbell, room for the
// messages it held, and its timers. Before either sleeps it arms its
// doorbell and checks its rings once more, and nothing may be runnable then.
type world struct {
	t       *testing.T
	h       *harness
	ring    *RingLink
	ch      boundedChannel
	stack   network.Stack
	session tcpserve.Session4
	now     uint64
	family  int
	// netd's messages to linuxsockd, bounded, and the ones it held back; a
	// doorbell a full queue refused waits in doorbellHeld.
	replies      []ipc.Message
	held         []ipc.Message
	doorbellHeld bool
	queue        int
	// timers: simulated time moved, so netd's timers run once. idleSteps
	// counts the times exchange moved the clock because nothing else could
	// run.
	timers    bool
	idleSteps int
	in        tcpapi.Request
	// The kernel's TX slots: committed or free, with their generations.
	txBusy [linuxsockapi.TXSlots]bool
	txGen  uint32
	// Channel traffic between linuxsockd and netd, by opcode for requests.
	requests                    [32]uint64
	replyCount, doorbellsToNetd uint64
	doorbellsFromNetd           uint64
}

const worldQueue = ipc.QueueCapacity

type worldReplier struct{ w *world }

func (r worldReplier) Reply(reply *tcpapi.Reply, correlation uint64) bool {
	var message ipc.Message
	if !r.w.session.Encode(reply, correlation, &message) {
		return false
	}
	return r.w.post(message)
}

func (r worldReplier) Doorbell() bool {
	w := r.w
	if w.doorbellHeld {
		return true
	}
	if len(w.held) != 0 || len(w.replies) >= w.queue {
		w.doorbellHeld = true
		return true
	}
	var message ipc.Message
	if !tcpapi.EncodeDoorbell4(&message) {
		return false
	}
	w.replies = append(w.replies, message)
	w.doorbellsFromNetd++
	return true
}

func (w *world) post(message ipc.Message) bool {
	w.replyCount++
	if len(w.held) == 0 && len(w.replies) < w.queue {
		w.replies = append(w.replies, message)
		return true
	}
	if len(w.held) >= 1024 {
		return false
	}
	w.held = append(w.held, message)
	return true
}

// worldBackend sends TCP to the ring link; UDP goes to a GUDP link no one
// serves, since the world runs streams only.
type worldBackend struct {
	w     *world
	udp   NativeLink
	udpCh boundedChannel
}

func (b *worldBackend) Submit(proto Proto, tag uint64, request *NativeRequest) SubmitStatus {
	if proto == TCP {
		return b.w.ring.Submit(&b.w.ch, tag, request)
	}
	return b.udp.Submit(&b.udpCh, tag, request)
}

func newWorld(t *testing.T) *world { return newWorldQueue(t, worldQueue) }

// newWorldQueue bounds both channels at queue messages.
func newWorldQueue(t *testing.T, queue int) *world {
	h := newHarness(t)
	w := &world{t: t, h: h, now: 1_000_000_000, queue: queue, ring: new(RingLink)}
	w.ch.capacity = queue
	b := &worldBackend{w: w}
	b.udpCh.capacity = queue
	b.udp.Reset(UDP, h.generation[UDP], tcpapi.Layout3, make([]byte, udpapi.TXSlots*udpapi.SlotSize), make([]byte, udpapi.RXSlots*udpapi.SlotSize))
	h.svc.backend = b
	w.stack.Init(99)
	tx, rx := make([]byte, tcpapi.Layout4.TXPool()), make([]byte, tcpapi.Layout4.RXPool())
	if !w.session.Start(&w.stack, 0, tcpapi.Endpoints4, tx, rx, w.now) {
		t.Fatal("start session")
	}
	if !w.ring.Reset(h.generation[TCP], tx, rx) {
		t.Fatal("ring link")
	}
	h.svc.SetStreamRing(true)
	h.svc.SetLinkView(w.ring)
	// netd armed before it first slept.
	w.session.Idle()
	h.svc.SetWarm(2)
	w.family = h.family(1, 7)
	w.settle()
	return w
}

// deliver runs netd's data plane until it reports no more work.
func (w *world) deliver() {
	for more := true; more; {
		var fault tcpserve.Fault
		if fault, more = w.session.Deliver(&w.stack, w.now, worldReplier{w}); fault != tcpserve.FaultNone {
			w.t.Fatalf("netd deliver fault %q", fault)
		}
	}
}

// netd takes its queue, handles each request, then delivers what the
// requests made ready, as its event loop does.
func (w *world) netd() bool {
	// netd wakes only on an event: a message, room for the messages it held,
	// or a timer that came due. Without one it sleeps, whatever its own
	// state would let it answer.
	deadline, timed := w.stack.Deadline()
	room := len(w.replies) < w.queue
	if len(w.ch.queue) == 0 && (len(w.held) == 0 && !w.doorbellHeld || !room) && (!timed || deadline > w.now) {
		return false
	}
	w.timers = false
	moved := false
	for len(w.held) != 0 && len(w.replies) < w.queue {
		w.replies = append(w.replies, w.held[0])
		w.held = w.held[1:]
		moved = true
	}
	if w.doorbellHeld && len(w.held) == 0 && len(w.replies) < w.queue {
		w.doorbellHeld = false
		worldReplier{w}.Doorbell()
		moved = true
	}
	queue := w.ch.queue
	w.ch.queue = nil
	for index := range queue {
		correlation, ok := w.session.Decode(&queue[index], &w.in)
		if !ok {
			w.t.Fatal("netd: undecodable request")
		}
		if w.in.Opcode == tcpapi.OpDoorbell {
			w.doorbellsToNetd++
		} else {
			w.requests[w.in.Opcode]++
		}
		if fault := w.session.Handle(&w.stack, &w.in, correlation, w.now, worldReplier{w}); fault != tcpserve.FaultNone {
			w.t.Fatalf("netd fault %q", fault)
		}
		moved = true
	}
	w.stack.Advance(w.now)
	w.deliver()
	// netd arms its doorbell before it sleeps and runs again when the
	// client published meanwhile.
	for !w.session.Idle() {
		w.deliver()
	}
	if endpoint, op, found := w.session.Runnable(&w.stack); found {
		w.t.Fatalf("netd would sleep with a runnable %s on endpoint %d", op, endpoint)
	}
	before := len(w.replies) + len(w.held)
	if fault, _ := w.session.Deliver(&w.stack, w.now, worldReplier{w}); fault != tcpserve.FaultNone {
		w.t.Fatalf("netd deliver fault %q", fault)
	}
	if w.session.Progressed() {
		w.t.Fatal("a delivery pass found work netd left for the next event")
	}
	if after := len(w.replies) + len(w.held); after != before {
		w.t.Fatalf("a delivery pass left %d answers for the next event", after-before)
	}
	return moved
}

// linuxsockd runs on a message from netd, on its channel turning writable
// when it waits for that, or on its timeout while Release entries wait;
// then it arms its doorbell and sleeps once nothing moves.
func (w *world) linuxsockd() bool {
	moved := false
	link := w.ring
	if link.WantsWritable() && len(w.ch.queue) < w.ch.capacity {
		link.Writable(&w.ch, w.h.svc)
		moved = true
	}
	for len(w.replies) != 0 {
		message := w.replies[0]
		w.replies = w.replies[1:]
		if !link.Receive(&w.ch, &message, w.h.svc) {
			w.t.Fatal("linuxsockd: netd broke the protocol")
		}
		moved = true
	}
	for spins := 0; !link.Idle(&w.ch, w.h.svc); spins++ {
		moved = true
		if spins > 1000 {
			for _, l := range link.local {
				w.t.Logf("local %x %+v", l.tag, l.c)
			}
			w.t.Fatalf("linuxsockd never idles: stats %+v", link.Stats)
		}
	}
	if link.Broken() {
		w.t.Fatal("ring link broken")
	}
	// Nothing may be runnable once linuxsockd sleeps: no ring entry, no
	// completion the link answered itself, no Release entry while the ring
	// has room.
	if link.control.ClientPending() || len(link.local) != 0 || link.Backlogged() && link.control.ReleaseRoom() != 0 {
		w.t.Fatal("linuxsockd would sleep with work runnable")
	}
	if validationHoles := w.h.svc.ProgressHoles(); validationHoles != 0 {
		w.t.Fatalf("%d progress holes", validationHoles)
	}
	return moved
}

// settle runs both services until neither has anything to do.
func (w *world) settle() {
	for round := 0; round < 10000; round++ {
		if !w.netd() && !w.linuxsockd() {
			return
		}
	}
	w.t.Fatalf("services never settled: requests %v doorbells %d %d", w.requests, w.doorbellsToNetd, w.doorbellsFromNetd)
}

// call sends a family request and runs the world until its reply.
func (w *world) call(request linuxsockapi.Request) linuxsockapi.Reply {
	w.t.Helper()
	correlation := w.h.send(w.family, request)
	for round := 0; round < 1000; round++ {
		if reply, ok := w.h.findReply(w.family, correlation); ok {
			return reply
		}
		w.settle()
		w.now += 1_000_000
		w.timers = true
	}
	w.t.Fatalf("no reply to %+v", request)
	return linuxsockapi.Reply{}
}

// listen opens a loopback listener with backlog and returns it and its
// port.
func (w *world) listen(backlog uint32) (uint64, uint16) {
	w.t.Helper()
	listener := w.call(linuxsockapi.Request{Opcode: opOpen, Length: linuxsockapi.KindStream}).Socket
	if r := w.call(linuxsockapi.Request{Opcode: opBind, Socket: listener, Address: [4]byte{127, 0, 0, 1}}); r.Result != linuxsockapi.ResultOK {
		w.t.Fatalf("bind %d", r.Result)
	}
	port := w.call(linuxsockapi.Request{Opcode: linuxsockapi.OpGetSockName, Socket: listener}).LocalPort
	if r := w.call(linuxsockapi.Request{Opcode: opListen, Socket: listener, Length: backlog}); r.Result != linuxsockapi.ResultOK {
		w.t.Fatalf("listen %d", r.Result)
	}
	return listener, port
}

// dial opens a stream and starts its connect to port.
func (w *world) dial(port uint16) uint64 {
	w.t.Helper()
	client := w.call(linuxsockapi.Request{Opcode: opOpen, Length: linuxsockapi.KindStream}).Socket
	if r := w.call(linuxsockapi.Request{Opcode: opConnect, Socket: client, Address: [4]byte{127, 0, 0, 1}, Port: port}); r.Result != linuxsockapi.ResultInProgress {
		w.t.Fatalf("connect %d", r.Result)
	}
	return client
}

// accept returns the listener's next child.
func (w *world) accept(listener uint64) uint64 {
	w.t.Helper()
	for round := 0; round < 100; round++ {
		reply := w.call(linuxsockapi.Request{Opcode: opAccept, Socket: listener})
		if reply.Result == linuxsockapi.ResultOK {
			return reply.Value
		}
		w.now += 10_000_000
	}
	w.t.Fatal("no child")
	return 0
}

// pair connects a client stream to an accepted server stream over loopback.
func (w *world) pair() (client, server uint64) {
	listener, port := w.listen(4)
	client = w.dial(port)
	return client, w.accept(listener)
}

func (w *world) rings() []byte { return w.h.rings(w.family) }

// stream sends total bytes from client to server through the rings, the
// kernel committing what credit allows and consuming every published slot
// at once, and fails when the world stops moving before the end.
func (w *world) stream(client, server uint64, total int) {
	w.streamOrdered(client, server, total, 0)
}

// streamOrdered is stream with a seed that picks, every round, which
// parties run and whether the reader takes what it has, so many
// interleavings of the same transfer get tried. Seed 0 runs everyone every
// round.
func (w *world) streamOrdered(client, server uint64, total int, seed uint64) {
	sent, received, idle := 0, 0, 0
	var entry linuxsockapi.RingEntry
	var unread []linuxsockapi.RingEntry
	random := seed*0x9e3779b97f4a7c15 + 1
	coin := func(odds uint64) bool {
		if seed == 0 {
			return true
		}
		random ^= random << 13
		random ^= random >> 7
		random ^= random << 17
		return random%odds != 0
	}
	for received < total {
		progress := false
		everyone := idle > 0
		release := false
		// The reader takes the slots it holds, now or later.
		if everyone || coin(3) {
			for _, held := range unread {
				data := w.h.rxBytes(w.family, held)
				for index := range data {
					if data[index] != byte(received+index) {
						w.t.Fatalf("byte %d is %d", received+index, data[index])
					}
				}
				received += len(data)
				back := linuxsockapi.RingEntry{Kind: linuxsockapi.RingRelease, Slot: held.Slot, Generation: held.Generation}
				if linuxsockapi.PushSubmit(w.rings(), &back) != linuxsockapi.RingOK {
					w.t.Fatal("submit ring")
				}
				release, progress = true, true
			}
			unread = unread[:0]
		}
		// The kernel drains the complete ring.
		linuxsockapi.TakeToken(w.h.pools[w.family])
		for (everyone || coin(2)) && linuxsockapi.PopComplete(w.rings(), &entry) == linuxsockapi.RingOK {
			progress = true
			switch entry.Kind {
			case linuxsockapi.RingTXDone:
				w.txBusy[entry.Slot] = false
			case linuxsockapi.RingRXData:
				if entry.Socket != server {
					w.t.Fatalf("data for %x", entry.Socket)
				}
				unread = append(unread, entry)
			}
		}
		// The kernel commits what slot credit allows.
		state := w.h.readiness(w.family, client)
		held := 0
		for slot := range w.txBusy {
			if w.txBusy[slot] {
				held++
			}
		}
		for slot := uint8(0); slot < linuxsockapi.TXSlots && sent < total && held < linuxsockapi.SocketTXSlots &&
			state.State&linuxsockapi.EntrySend != 0 && (everyone || coin(2)); slot++ {
			if w.txBusy[slot] {
				continue
			}
			length := min(total-sent, linuxsockapi.SlotSize)
			if seed != 0 && coin(2) {
				// Writes of every size, as a program's are.
				length = min(length, int(random%linuxsockapi.SlotSize)+1)
			}
			offset := linuxsockapi.TXOffset(slot)
			chunk := w.h.pools[w.family][offset : offset+uint64(length)]
			for index := range chunk {
				chunk[index] = byte(sent + index)
			}
			w.txGen++
			commit := linuxsockapi.RingEntry{Kind: linuxsockapi.RingTX, Slot: slot, Length: uint32(length), Socket: client, Generation: w.txGen}
			if linuxsockapi.PushSubmit(w.rings(), &commit) != linuxsockapi.RingOK {
				w.t.Fatal("submit ring")
			}
			w.txBusy[slot], sent, held, release, progress = true, sent+length, held+1, true, true
		}
		if release && linuxsockapi.ClaimDoorbell(w.rings()) {
			w.h.send(w.family, linuxsockapi.Request{Opcode: linuxsockapi.OpDoorbell})
		}
		if (everyone || coin(2)) && w.netd() {
			progress = true
		}
		if (everyone || coin(2)) && w.linuxsockd() {
			progress = true
		}
		if progress {
			idle = 0
			continue
		}
		// Nobody had work: everyone runs next round, and only time can
		// move things after that, through netd's timers. A stream that
		// waits for them past a few simulated seconds is stuck.
		if idle++; idle > 1 {
			w.now += 10_000_000
			w.timers = true
		}
		if idle > 500 {
			w.dump(client, server)
			w.t.Fatalf("stream stuck: sent %d received %d of %d", sent, received, total)
		}
	}
}

func (w *world) dump(ids ...uint64) {
	for _, id := range ids {
		for index := 0; index < w.h.svc.socketCap(); index++ {
			if sock := w.h.svc.sock(index); sock.used && sock.id == id {
				w.t.Logf("socket %x: %+v", id, *sock)
			}
		}
	}
	f := &w.h.svc.families[w.family]
	tx, rx := w.ring.Held()
	w.t.Logf("family txHeld %08b rxUsed %08b rxPub %08b; link tx %016b rx %016b wantsWritable %v queue %d held messages %d",
		f.txHeld, f.rxUsed, f.rxPub, tx, rx, w.ring.WantsWritable(), len(w.ch.queue), len(w.held))
}

// traffic is the channel messages both ways since the last mark, besides
// doorbells.
func (w *world) traffic() (requests, replies, doorbells uint64) {
	for _, count := range w.requests {
		requests += count
	}
	return requests, w.replyCount, w.doorbellsToNetd + w.doorbellsFromNetd
}

func (w *world) mark() {
	w.requests = [32]uint64{}
	w.replyCount, w.doorbellsToNetd, w.doorbellsFromNetd = 0, 0, 0
}

func TestWorldStreamThroughRealStack(t *testing.T) {
	w := newWorld(t)
	client, server := w.pair()
	w.stream(client, server, 8<<20)
}

func TestWorldManyStreamsWithSmallQueues(t *testing.T) {
	for seed := 0; seed < 20; seed++ {
		w := newWorld(t)
		client, server := w.pair()
		w.stream(client, server, 256<<10+seed*4099)
	}
}

func TestWorldStreamInterleavings(t *testing.T) {
	seeds := uint64(300)
	if testing.Short() {
		seeds = 30
	}
	for seed := uint64(1); seed <= seeds; seed++ {
		// Small queues fill under one stream, so full queues and the
		// writable events that drain them happen all the time.
		w := newWorldQueue(t, 2+int(seed%3))
		client, server := w.pair()
		w.streamOrdered(client, server, 512<<10, seed)
		if t.Failed() {
			t.Fatalf("seed %d", seed)
		}
	}
}
