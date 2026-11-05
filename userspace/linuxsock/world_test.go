package linuxsock

import (
	"testing"

	"goos/kernel/ipc"
	"goos/userspace/linuxsockapi"
	"goos/userspace/network"
	"goos/userspace/tcpapi"
	"goos/userspace/tcpserve"
)

// world runs linuxsockd's core against the real native TCP service and
// stack of netd, over queues bounded as the kernel bounds channels, and
// plays the kernel's side of one family's rings. Each party runs only on
// the events that wake it in GoOS: linuxsockd on a reply, a family message
// or its native channel turning writable while it asked for that; netd on
// a request or room for the replies it held, and on its timers.
type world struct {
	t       *testing.T
	h       *harness
	b       *linkBackend
	stack   network.Stack
	session tcpserve.Session
	now     uint64
	family  int
	// netd's replies to linuxsockd, bounded, and the ones it held back.
	replies []ipc.Message
	held    []ipc.Message
	queue   int
	in      tcpapi.Request
	// The kernel's TX slots: committed or free, with their generations.
	txBusy [linuxsockapi.TXSlots]bool
	txGen  uint32
}

const worldQueue = ipc.QueueCapacity

type worldReplier struct{ w *world }

func (r worldReplier) Reply(reply *tcpapi.Reply, correlation uint64) bool {
	var message ipc.Message
	if !r.w.session.Encode(reply, correlation, &message) {
		return false
	}
	if len(r.w.held) == 0 && len(r.w.replies) < r.w.queue {
		r.w.replies = append(r.w.replies, message)
		return true
	}
	if len(r.w.held) >= 128 {
		return false
	}
	r.w.held = append(r.w.held, message)
	return true
}

func newWorld(t *testing.T) *world { return newWorldQueue(t, worldQueue) }

// newWorldQueue bounds both channels at queue messages.
func newWorldQueue(t *testing.T, queue int) *world {
	h, b := newLinkHarness(t, queue)
	w := &world{t: t, h: h, b: b, now: 1_000_000_000, queue: queue}
	w.stack.Init(99)
	tx, rx := b.links[TCP].tx, b.links[TCP].rx
	if !w.session.StartLayout(&w.stack, 0, 64, tcpapi.Layout3, tx, rx, w.now) {
		t.Fatal("start session")
	}
	h.svc.SetWarm(2)
	w.family = h.family(1, 7)
	w.settle()
	return w
}

// netd takes its queue, handles each request, then delivers what the
// requests made ready, as its event loop does.
func (w *world) netd() bool {
	moved := false
	for len(w.held) != 0 && len(w.replies) < w.queue {
		w.replies = append(w.replies, w.held[0])
		w.held = w.held[1:]
		moved = true
	}
	queue := w.b.channels[TCP].queue
	w.b.channels[TCP].queue = nil
	for index := range queue {
		correlation, ok := tcpapi.Layout3.DecodeRequest(&queue[index], &w.in)
		if !ok {
			w.t.Fatal("netd: undecodable request")
		}
		if fault := w.session.Handle(&w.stack, &w.in, correlation, w.now, worldReplier{w}); fault != tcpserve.FaultNone {
			w.t.Fatalf("netd fault %q", fault)
		}
		moved = true
	}
	w.stack.Advance(w.now)
	if fault := w.session.Deliver(&w.stack, w.now, worldReplier{w}); fault != tcpserve.FaultNone {
		w.t.Fatalf("netd deliver fault %q", fault)
	}
	return moved
}

// linuxsockd runs on a reply, or on its channel turning writable when it
// waits for that.
func (w *world) linuxsockd() bool {
	moved := false
	link, channel := &w.b.links[TCP], &w.b.channels[TCP]
	if link.WantsWritable() && len(channel.queue) < channel.capacity {
		if link.Writable(channel, w.h.svc) != SendOK {
			w.t.Fatal("link gone")
		}
		moved = true
	}
	for len(w.replies) != 0 {
		message := w.replies[0]
		w.replies = w.replies[1:]
		tag, completion, ok := link.Decode(&message)
		if !ok {
			w.t.Fatal("linuxsockd: undecodable reply")
		}
		w.h.svc.Complete(TCP, w.h.generation[TCP], tag, completion)
		link.Done(channel)
		moved = true
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
	w.t.Fatal("services never settled")
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
	}
	w.t.Fatalf("no reply to %+v", request)
	return linuxsockapi.Reply{}
}

// pair connects a client stream to an accepted server stream over loopback.
func (w *world) pair() (client, server uint64) {
	listener := w.call(linuxsockapi.Request{Opcode: opOpen, Length: linuxsockapi.KindStream}).Socket
	if r := w.call(linuxsockapi.Request{Opcode: opBind, Socket: listener, Address: [4]byte{127, 0, 0, 1}}); r.Result != linuxsockapi.ResultOK {
		w.t.Fatalf("bind %d", r.Result)
	}
	port := w.call(linuxsockapi.Request{Opcode: linuxsockapi.OpGetSockName, Socket: listener}).LocalPort
	w.call(linuxsockapi.Request{Opcode: opListen, Socket: listener, Length: 4})
	client = w.call(linuxsockapi.Request{Opcode: opOpen, Length: linuxsockapi.KindStream}).Socket
	if r := w.call(linuxsockapi.Request{Opcode: opConnect, Socket: client, Address: [4]byte{127, 0, 0, 1}, Port: port}); r.Result != linuxsockapi.ResultInProgress {
		w.t.Fatalf("connect %d", r.Result)
	}
	for round := 0; round < 100; round++ {
		reply := w.call(linuxsockapi.Request{Opcode: opAccept, Socket: listener})
		if reply.Result == linuxsockapi.ResultOK {
			return client, reply.Value
		}
		w.now += 1_000_000
	}
	w.t.Fatal("no child")
	return 0, 0
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
		}
		if idle > 500 {
			w.dump(client, server)
			w.t.Fatalf("stream stuck: sent %d received %d of %d", sent, received, total)
		}
	}
}

func (w *world) dump(ids ...uint64) {
	for _, id := range ids {
		for index := range w.h.svc.sockets {
			if sock := &w.h.svc.sockets[index]; sock.used && sock.id == id {
				w.t.Logf("socket %x: %+v", id, *sock)
			}
		}
	}
	f := &w.h.svc.families[w.family]
	w.t.Logf("family txHeld %08b rxUsed %08b rxPub %08b; link wantsWritable %v queue %d held replies %d",
		f.txHeld, f.rxUsed, f.rxPub, w.b.links[TCP].WantsWritable(), len(w.b.channels[TCP].queue), len(w.held))
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
