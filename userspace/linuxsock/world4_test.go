package linuxsock

import (
	"testing"

	"goos/userspace/linuxsockapi"
	"goos/userspace/tcpapi"
)

// flow is one stream the kernel drives in exchange: client sends total
// bytes to server, then shuts its write side or closes when asked.
type flow struct {
	client, server  uint64
	total           int
	sent, received  int
	held            int
	shutdown, close bool
	finished, eof   bool
	seed            byte
	// reading is the correlation of the server's read out, zero for none.
	reading uint64
}

// exchange runs the kernel's side of every flow until each server read all
// bytes and, for a flow that shut down or closed, saw the end of the
// stream. The kernel commits what slot credit and each socket's share
// allow, consumes every published slot at once and checks every byte.
func (w *world) exchange(flows []*flow) {
	w.t.Helper()
	byServer := map[uint64]*flow{}
	for _, f := range flows {
		byServer[f.server] = f
	}
	var owner [linuxsockapi.TXSlots]*flow
	var entry linuxsockapi.RingEntry
	idle, next, rounds := 0, 0, 0
	for {
		done := true
		for _, f := range flows {
			if f.received < f.total || (f.shutdown || f.close) && !f.eof {
				done = false
			}
		}
		if done {
			return
		}
		progress := false
		release := false
		if rounds++; rounds > 200000 {
			for _, f := range flows {
				w.t.Logf("flow %x: sent %d received %d of %d eof %v finished %v", f.client, f.sent, f.received, f.total, f.eof, f.finished)
				w.dump(f.client, f.server)
			}
			w.t.Fatalf("exchange spins: requests %v doorbells %d %d", w.requests, w.doorbellsToNetd, w.doorbellsFromNetd)
		}
		linuxsockapi.TakeToken(w.h.pools[w.family])
		for linuxsockapi.PopComplete(w.rings(), &entry) == linuxsockapi.RingOK {
			progress = true
			switch entry.Kind {
			case linuxsockapi.RingTXDone:
				if owner[entry.Slot] != nil {
					owner[entry.Slot].held--
				}
				w.txBusy[entry.Slot], owner[entry.Slot] = false, nil
			case linuxsockapi.RingRXData:
				f := byServer[entry.Socket]
				if f == nil {
					w.t.Fatalf("data for %x", entry.Socket)
				}
				data := w.h.rxBytes(w.family, entry)
				for index := range data {
					if data[index] != byte(f.received+index)+f.seed {
						w.t.Fatalf("flow %x byte %d is %d", f.client, f.received+index, data[index])
					}
				}
				f.received += len(data)
				back := linuxsockapi.RingEntry{Kind: linuxsockapi.RingRelease, Slot: entry.Slot, Generation: entry.Generation}
				if linuxsockapi.PushSubmit(w.rings(), &back) != linuxsockapi.RingOK {
					w.t.Fatal("submit ring")
				}
				release = true
			}
		}
		// Commit round robin, so every flow gets slots.
		for turn := range flows {
			f := flows[(next+turn)%len(flows)]
			if f.finished || f.sent == f.total {
				continue
			}
			state := w.h.readiness(w.family, f.client)
			for slot := uint8(0); slot < linuxsockapi.TXSlots && f.sent < f.total && f.held < linuxsockapi.SocketTXSlots &&
				state.State&linuxsockapi.EntrySend != 0; slot++ {
				if w.txBusy[slot] {
					continue
				}
				length := min(f.total-f.sent, linuxsockapi.SlotSize)
				offset := linuxsockapi.TXOffset(slot)
				chunk := w.h.pools[w.family][offset : offset+uint64(length)]
				for index := range chunk {
					chunk[index] = byte(f.sent+index) + f.seed
				}
				w.txGen++
				commit := linuxsockapi.RingEntry{Kind: linuxsockapi.RingTX, Slot: slot, Length: uint32(length), Socket: f.client, Generation: w.txGen}
				if linuxsockapi.PushSubmit(w.rings(), &commit) != linuxsockapi.RingOK {
					w.t.Fatal("submit ring")
				}
				w.txBusy[slot], owner[slot] = true, f
				f.sent, f.held, release, progress = f.sent+length, f.held+1, true, true
			}
		}
		next++
		if release && linuxsockapi.ClaimDoorbell(w.rings()) {
			w.h.send(w.family, linuxsockapi.Request{Opcode: linuxsockapi.OpDoorbell})
		}
		// A flow that committed everything shuts down or closes right away,
		// with its bytes still queued.
		for _, f := range flows {
			if f.finished || f.sent < f.total || !f.shutdown && !f.close {
				continue
			}
			f.finished = true
			if f.shutdown {
				w.h.send(w.family, linuxsockapi.Request{Opcode: linuxsockapi.OpShutdown, Socket: f.client, Length: shutWrite})
			} else {
				w.h.send(w.family, linuxsockapi.Request{Opcode: opClose, Socket: f.client})
			}
			progress = true
		}
		// A server whose readiness shows bytes asks for them, as a program's
		// read does: parked reads of idle sockets may hold every RX slot but
		// the reserve.
		for _, f := range flows {
			if f.received >= f.total {
				continue
			}
			if f.reading != 0 {
				if _, ok := w.h.findReply(w.family, f.reading); !ok {
					continue
				}
				f.reading = 0
			}
			if state := w.h.readiness(w.family, f.server); state.Ready&linuxsockapi.ReadyIn != 0 {
				f.reading = w.h.send(w.family, linuxsockapi.Request{Opcode: linuxsockapi.OpRead, Socket: f.server})
				progress = true
			}
		}
		// The end of the stream counts only after every byte.
		for _, f := range flows {
			if f.eof || !f.finished || f.received < f.total {
				continue
			}
			if state := w.h.readiness(w.family, f.server); state.Ready&linuxsockapi.ReadyIn == 0 {
				continue
			}
			correlation := w.h.send(w.family, linuxsockapi.Request{Opcode: linuxsockapi.OpRead, Socket: f.server})
			w.settle()
			reply, ok := w.h.findReply(w.family, correlation)
			if ok && reply.Result == linuxsockapi.ResultOK && reply.Flags&linuxsockapi.FlagEOF != 0 {
				f.eof, progress = true, true
			}
		}
		if w.netd() {
			progress = true
		}
		if w.linuxsockd() {
			progress = true
		}
		if progress {
			idle = 0
			continue
		}
		if idle++; idle > 1 {
			w.now += 100_000_000
			w.timers = true
			w.idleSteps++
		}
		if idle > 500 {
			complete, readable, pages := 0, 0, map[uint16]int{}
			for _, f := range flows {
				if f.received == f.total {
					complete++
				}
				sock := w.h.svc.sock(w.global(f.server))
				if index := w.ring.find(sock.endpoint); index >= 0 {
					page, _ := w.ring.control.LoadReadiness(index)
					pages[page.Mask]++
					if page.Readable != 0 {
						readable++
					}
				}
				if index := w.ring.find(w.h.svc.sock(w.global(f.client)).endpoint); index >= 0 {
					page, _ := w.ring.control.LoadReadiness(index)
					pages[page.Mask|0x8000]++
				}
			}
			w.t.Logf("%d of %d flows complete, %d servers readable, masks %v", complete, len(flows), readable, pages)
			w.t.Logf("tcp stats %+v", w.stack.TCPStats())
			for _, f := range flows {
				if f.received < f.total || !f.eof && f.finished {
					w.t.Logf("flow %x: sent %d received %d of %d eof %v", f.client, f.sent, f.received, f.total, f.eof)
					w.dump(f.client, f.server)
					if index := w.ring.find(w.h.svc.sock(w.global(f.server)).endpoint); index >= 0 {
						page, _ := w.ring.control.LoadReadiness(index)
						w.t.Logf("link %d: %+v netd %+v page %+v", index, w.ring.endpoints[index], w.session.Snapshot(index), page)
					}
				}
			}
			w.t.Fatal("exchange stuck")
		}
	}
}

// A megabyte through linuxsockd costs no channel request or reply: the
// family slots go into GTCP 4 TX slots and the RX slots come back on the
// rings. Only doorbells cross the channel, when a side slept.
func TestWorldMegabyteStreamNeedsNoChannelRequests(t *testing.T) {
	w := newWorld(t)
	client, server := w.pair()
	// The first bytes find the server's wait held in netd from before its
	// first read; the steady stream comes after them.
	w.stream(client, server, 256<<10)
	w.settle()
	w.mark()
	before := w.ring.Stats
	const total = 1 << 20
	w.stream(client, server, total)
	requests, replies, doorbells := w.traffic()
	if requests != 0 || replies != 0 {
		t.Fatalf("%d requests and %d replies crossed the channel for the stream: %v", requests, replies, w.requests)
	}
	stats := w.ring.Stats
	if writes := stats.Writes - before.Writes; writes < total/tcpapi.SlotSize4 {
		t.Fatalf("%d ring writes for %d bytes", writes, total)
	}
	if data := stats.Data - before.Data; data < total/tcpapi.SlotSize4 {
		t.Fatalf("%d ring fills for %d bytes", data, total)
	}
	t.Logf("1 MiB: %d requests, %d replies, %d doorbells (%.2f per 64 KiB); ring writes %d, fills %d, credits %d, local answers %d",
		requests, replies, doorbells, float64(doorbells)*65536/total, stats.Writes-before.Writes, stats.Data-before.Data,
		stats.Credits-before.Credits, stats.LocalCompletions-before.LocalCompletions)
}

// 300 sockets stream 32 KiB or more each at once through one listener and
// one family. Every socket belongs to linuxsockd's one session, so the
// session's ring budget is the limit: at the default, and at a quarter of
// the old 8 MiB default, every flow completes and none waits for the clock.
func TestWorldThreeHundredSockets(t *testing.T) {
	for _, budget := range []int{0, 4 << 20} {
		w := newWorld(t)
		w.stack.SetTCPMemoryBudget(budget, budget)
		w.threeHundredSockets(32 << 10)
		stats := w.stack.TCPStats()
		t.Logf("budget %d: ring bytes %d, capped growths %d, refusals %d", budget, stats.MemoryBytes, stats.MemoryCapped, stats.DropMemory)
		if w.idleSteps != 0 || stats.DropMemory != 0 || stats.Retransmits != 0 {
			t.Fatalf("budget %d: %d idle clock steps, %d refusals, %d retransmissions", budget, w.idleSteps, stats.DropMemory, stats.Retransmits)
		}
		if budget != 0 && stats.MemoryCapped == 0 {
			t.Fatalf("budget %d never capped a ring", budget)
		}
	}
}

func (w *world) threeHundredSockets(perFlow int) {
	t := w.t
	t.Helper()
	listener, port := w.listen(128)
	const pairs = 150
	flows := make([]*flow, pairs)
	w.mark()
	for index := range flows {
		flows[index] = &flow{client: w.dial(port), total: perFlow + index*37, seed: byte(index)}
		flows[index].server = w.accept(listener)
	}
	w.exchange(flows)
	if live := w.h.svc.sockets.live; live != 2*pairs+1 {
		t.Fatalf("%d sockets live", live)
	}
	if open := w.ring.Open(); open < 2*pairs+1 || open > 2*pairs+1+ringAcceptAhead {
		t.Fatalf("%d endpoints open in the link", open)
	}
	requests, replies, doorbells := w.traffic()
	t.Logf("%d accepts and %d streams: %d requests %v, %d replies, %d doorbells", pairs, pairs, requests, w.requests, replies, doorbells)
	if w.requests[tcpapi.OpRead] != 0 || w.requests[tcpapi.OpWrite] != 0 || w.requests[tcpapi.OpAccept] != 0 {
		t.Fatal("data or accepts went through channel requests")
	}
}

// A connect to a port no one listens on answers InProgress, then the
// socket reports the refusal in SO_ERROR and the next connect starts over.
func TestWorldConnectRefused(t *testing.T) {
	w := newWorld(t)
	listener, port := w.listen(1)
	w.call(linuxsockapi.Request{Opcode: opClose, Socket: listener})
	w.settle()
	client := w.dial(port)
	for round := 0; round < 200; round++ {
		w.settle()
		if w.h.readiness(w.family, client).Ready&linuxsockapi.ReadyError != 0 {
			break
		}
		w.now += 10_000_000
		w.timers = true
	}
	state := w.h.readiness(w.family, client)
	if state.Ready&linuxsockapi.ReadyError == 0 || state.Ready&linuxsockapi.ReadyHangup == 0 {
		t.Fatalf("refused connect readiness %+v", state)
	}
	if reply := w.call(linuxsockapi.Request{Opcode: linuxsockapi.OpGetPendingError, Socket: client}); reply.Value != uint64(linuxsockapi.ResultRefused) {
		t.Fatalf("SO_ERROR %d", reply.Value)
	}
	listener, port = w.listen(1)
	if r := w.call(linuxsockapi.Request{Opcode: opConnect, Socket: client, Address: [4]byte{127, 0, 0, 1}, Port: port}); r.Result != linuxsockapi.ResultInProgress {
		t.Fatalf("second connect %d", r.Result)
	}
	server := w.accept(listener)
	f := &flow{client: client, server: server, total: 40000, shutdown: true}
	w.exchange([]*flow{f})
	if w.requests[tcpapi.OpOpenConnect] != 2 {
		t.Fatalf("%d open-and-connect requests", w.requests[tcpapi.OpOpenConnect])
	}
}

// SHUT_WR right after the last commit sends the FIN after every byte.
func TestWorldShutdownFollowsQueuedBytes(t *testing.T) {
	w := newWorld(t)
	client, server := w.pair()
	f := &flow{client: client, server: server, total: 200000, shutdown: true, seed: 3}
	w.exchange([]*flow{f})
	if reply := w.call(linuxsockapi.Request{Opcode: linuxsockapi.OpWrite, Socket: client}); reply.Result != linuxsockapi.ResultClosed {
		t.Fatalf("write after SHUT_WR %d", reply.Result)
	}
}

// close right after the last commit still delivers every byte, then the
// end of the stream, and the link gives the endpoint up.
func TestWorldCloseWithQueuedBytes(t *testing.T) {
	w := newWorld(t)
	client, server := w.pair()
	open := w.ring.Open()
	f := &flow{client: client, server: server, total: 300000, close: true, seed: 9}
	w.exchange([]*flow{f})
	w.settle()
	if now := w.ring.Open(); now != open-1 {
		t.Fatalf("%d endpoints open, %d before the close", now, open)
	}
	if tx, _ := w.ring.Held(); tx != 0 {
		t.Fatalf("TX slots %b still in netd", tx)
	}
	if w.ring.Stats.Forgets == 0 {
		t.Fatal("no endpoint forgotten")
	}
}

// Clients arrive faster than the server accepts: netd accepts ahead only
// what the link asks for, the rest waits in the stack's backlog, and every
// client gets its child once the server accepts.
func TestWorldAcceptAheadBacklogSaturation(t *testing.T) {
	w := newWorld(t)
	listener, port := w.listen(2)
	// More clients than the backlog and both accept-ahead queues hold; the
	// rest wait in SYN retransmits, well inside the connect timeout.
	const clients = 12
	var dialed []uint64
	for range clients {
		dialed = append(dialed, w.dial(port))
	}
	for round := 0; round < 10; round++ {
		w.now += 100_000_000
		w.timers = true
		w.settle()
	}
	index := w.h.svc.sock(w.global(listener))
	if queued := int(index.acceptLen) + int(index.accepting); queued > AcceptAhead {
		t.Fatalf("%d children ahead in the core", queued)
	}
	for position := 0; position < w.ring.limit; position++ {
		if e := &w.ring.endpoints[position]; e.listening && int(e.acceptCount)+int(e.granted) > int(e.want) {
			t.Fatalf("listener holds %d children and asked for %d more, wants %d", e.acceptCount, e.granted, e.want)
		}
	}
	w.mark()
	var flows []*flow
	for position := range clients {
		var server uint64
		for round := 0; round < 400 && server == 0; round++ {
			reply := w.call(linuxsockapi.Request{Opcode: opAccept, Socket: listener})
			if reply.Result == linuxsockapi.ResultOK {
				server = reply.Value
				break
			}
			w.now += 50_000_000
			w.timers = true
		}
		if server == 0 {
			for i := 0; i < w.ring.limit; i++ {
				if e := &w.ring.endpoints[i]; e.listening || e.child {
					t.Logf("ep %d: %+v", i, *e)
				}
			}
			w.dump(listener)
			t.Logf("netd listener %+v", w.session.Snapshot(0))
			t.Fatalf("child %d never came", position)
		}
		flows = append(flows, &flow{server: server})
	}
	// Match each child to its client by the ports.
	peers := map[uint16]uint64{}
	for _, client := range dialed {
		peers[w.call(linuxsockapi.Request{Opcode: linuxsockapi.OpGetSockName, Socket: client}).LocalPort] = client
	}
	for position, f := range flows {
		reply := w.call(linuxsockapi.Request{Opcode: linuxsockapi.OpGetPeerName, Socket: f.server})
		if f.client = peers[reply.Port]; f.client == 0 {
			t.Fatalf("child %d from unknown port %d", position, reply.Port)
		}
		delete(peers, reply.Port)
		f.total, f.seed = 5000, byte(position)
	}
	w.exchange(flows)
	requests, replies, doorbells := w.traffic()
	t.Logf("%d children: %d requests %v, %d replies, %d doorbells", clients, requests, w.requests, replies, doorbells)
	// An accept goes to netd only when the stack holds a child netd has not
	// accepted ahead yet: the link's queue was full when it arrived.
	if accepts := w.requests[tcpapi.OpAccept]; accepts*4 > clients {
		t.Fatalf("%d of %d accepts went through channel requests", accepts, clients)
	}
}

// global finds a socket's table index.
func (w *world) global(id uint64) int {
	for index := 0; index < w.h.svc.socketCap(); index++ {
		if w.h.svc.sock(index).used && w.h.svc.sock(index).id == id {
			return index
		}
	}
	w.t.Fatalf("no socket %x", id)
	return -1
}
