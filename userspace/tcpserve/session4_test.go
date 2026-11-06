package tcpserve

import (
	"bytes"
	"testing"

	"goos/kernel/ipc"
	"goos/userspace/network"
	"goos/userspace/shmring"
	"goos/userspace/tcpapi"
)

// notes4 collects a GTCP 4 session's replies through the codec, and its
// doorbells.
type notes4 struct {
	replies
	doorbells int
}

func (n *notes4) Reply(reply *tcpapi.Reply, correlation uint64) bool {
	var message ipc.Message
	if !tcpapi.Layout4.EncodeReply(reply, correlation, &message) {
		return false
	}
	var decoded tcpapi.Reply
	got, ok := tcpapi.Layout4.DecodeReply(&message, &decoded)
	if !ok || got != correlation {
		return false
	}
	return n.replies.Reply(&decoded, got)
}

func (n *notes4) Doorbell() bool { n.doorbells++; return true }

// end4 is one GTCP 4 session driven by hand: the server and the client's
// view of the same pools.
type end4 struct {
	s       Session4
	tx, rx  []byte
	control tcpapi.Control
	txGen   [tcpapi.TXSlots4]uint32
}

type pair4 struct {
	t           *testing.T
	stack       *network.Stack
	now         uint64
	a, b        end4
	out         *notes4
	correlation uint64
}

func newPair4(t *testing.T) *pair4 {
	p := &pair4{t: t, stack: new(network.Stack), now: 1_000_000_000, out: new(notes4)}
	p.stack.Init(99)
	for owner, e := range []*end4{&p.a, &p.b} {
		e.tx, e.rx = make([]byte, tcpapi.Layout4.TXPool()), make([]byte, tcpapi.Layout4.RXPool())
		if !e.s.Start(p.stack, owner, 0, e.tx, e.rx, p.now) {
			t.Fatal("start")
		}
		e.control, _ = tcpapi.ControlOf(e.tx, e.rx)
		if !e.control.ServiceReady() {
			t.Fatal("service page not marked")
		}
	}
	return p
}

func (p *pair4) call(e *end4, request tcpapi.Request) tcpapi.Reply {
	p.t.Helper()
	p.correlation++
	if fault := e.s.Handle(p.stack, &request, p.correlation, p.now, p.out); fault != FaultNone {
		p.t.Fatalf("opcode %d fault %q", request.Opcode, fault)
	}
	return p.out.take(p.t, p.correlation)
}

func (p *pair4) deliver() {
	p.t.Helper()
	for _, e := range []*end4{&p.a, &p.b} {
		if fault, _ := e.s.Deliver(p.stack, p.now, p.out); fault != FaultNone {
			p.t.Fatalf("deliver fault %q", fault)
		}
	}
}

// connected listens on a, dials from b with OpOpenConnect and accepts
// ahead; it returns b's endpoint and index, then a's.
func (p *pair4) connected(port uint16) (client tcpapi.Reply, server tcpapi.Entry) {
	p.t.Helper()
	listener := p.call(&p.a, tcpapi.Request{Opcode: tcpapi.OpOpen})
	p.call(&p.a, tcpapi.Request{Opcode: tcpapi.OpBind, Endpoint: listener.Endpoint, Address: [4]byte{127, 0, 0, 1}, Port: port})
	p.call(&p.a, tcpapi.Request{Opcode: tcpapi.OpListen, Endpoint: listener.Endpoint, Backlog: 4})
	if reply := p.call(&p.a, tcpapi.Request{Opcode: tcpapi.OpAcceptAhead, Endpoint: listener.Endpoint, Backlog: 2}); reply.Result != tcpapi.ResultOK {
		p.t.Fatalf("accept ahead %+v", reply)
	}
	client = p.call(&p.b, tcpapi.Request{Opcode: tcpapi.OpOpenConnect, Address: [4]byte{127, 0, 0, 1}, Port: port})
	if client.Result != tcpapi.ResultOK || client.Endpoint == 0 {
		p.t.Fatalf("open connect %+v", client)
	}
	p.deliver()
	if status := p.a.control.PopRX(&server); status != shmring.OK || server.Kind != tcpapi.EntryAccepted ||
		server.Offset != uint32(listener.Index) || server.Generation != listener.Endpoint || server.Port != client.LocalPort {
		p.t.Fatalf("accepted %d %+v", status, server)
	}
	return client, server
}

func (p *pair4) write(e *end4, slot uint8, index uint16, endpoint uint32, offset uint32, data []byte) {
	p.t.Helper()
	base := tcpapi.Layout4.Offset(slot)
	copy(e.tx[base:], data)
	e.txGen[slot]++
	entry := tcpapi.Entry{Kind: tcpapi.EntryWrite, Slot: slot, Index: index, Endpoint: endpoint, Length: uint32(len(data)),
		Generation: e.txGen[slot], Offset: offset}
	if e.control.PushTX(&entry) != shmring.OK {
		p.t.Fatal("push write")
	}
}

func (p *pair4) push(e *end4, entry tcpapi.Entry) {
	p.t.Helper()
	if e.control.PushRelease(&entry) != shmring.OK {
		p.t.Fatalf("push %+v", entry)
	}
}

func (p *pair4) fault(e *end4) Fault {
	fault, _ := e.s.Deliver(p.stack, p.now, p.out)
	return fault
}

func TestSession4StreamsWithoutReplies(t *testing.T) {
	p := newPair4(t)
	client, server := p.connected(9100)
	p.push(&p.a, tcpapi.Entry{Kind: tcpapi.EntryCredit, Index: server.Index, Endpoint: server.Endpoint, Length: 2})
	data := pattern(0, 20000)
	p.write(&p.b, 0, client.Index, client.Endpoint, 0, data[:tcpapi.SlotSize4])
	p.write(&p.b, 1, client.Index, client.Endpoint, tcpapi.SlotSize4, data[tcpapi.SlotSize4:])
	p.deliver()
	p.deliver()
	if len(p.out.sent) != 0 {
		t.Fatalf("replies %+v", p.out.sent)
	}
	var got []byte
	var e tcpapi.Entry
	for p.a.control.PopRX(&e) == shmring.OK {
		if e.Kind != tcpapi.EntryData || e.Endpoint != server.Endpoint || e.Generation != 1 {
			t.Fatalf("rx %+v", e)
		}
		got = append(got, p.a.rx[tcpapi.Layout4.Offset(e.Slot):][:e.Length]...)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("got %d bytes", len(got))
	}
	for slot := range uint8(2) {
		if p.b.control.PopTXDone(&e) != shmring.OK || e.Kind != tcpapi.EntryWriteDone || e.Slot != slot || e.Generation != 1 ||
			e.Result != tcpapi.ResultOK || e.Length != uint32(min(tcpapi.SlotSize4, 20000-int(slot)*tcpapi.SlotSize4)) {
			t.Fatalf("write done %+v", e)
		}
	}
	// The readiness page shows the endpoint with no request.
	ready, ok := p.a.control.LoadReadiness(int(server.Index))
	if !ok || ready.Endpoint != server.Endpoint || ready.Mask&tcpapi.ReadyConnected == 0 || ready.Credit != 0 {
		t.Fatalf("readiness %+v", ready)
	}
	// A write at the wrong offset comes back unsent.
	p.write(&p.b, 0, client.Index, client.Endpoint, 5, []byte("x"))
	p.deliver()
	if p.b.control.PopTXDone(&e) != shmring.OK || e.Result != tcpapi.ResultInvalidArgument || e.Length != 0 || e.Generation != 2 {
		t.Fatalf("misplaced write %+v", e)
	}
}

func TestSession4RejectsOwnershipViolations(t *testing.T) {
	for name, violate := range map[string]func(p *pair4, client tcpapi.Reply, server tcpapi.Entry){
		"write of a held slot": func(p *pair4, client tcpapi.Reply, _ tcpapi.Entry) {
			// The second entry names the slot before netd handed it back.
			p.write(&p.b, 3, client.Index, client.Endpoint, 0, pattern(0, 100))
			p.write(&p.b, 3, client.Index, client.Endpoint, 100, pattern(0, 100))
		},
		"stale TX generation": func(p *pair4, client tcpapi.Reply, _ tcpapi.Entry) {
			p.b.txGen[2] = 5
			p.write(&p.b, 2, client.Index, client.Endpoint, 0, []byte("x"))
		},
		"release of a slot not lent": func(p *pair4, _ tcpapi.Reply, _ tcpapi.Entry) {
			p.push(&p.a, tcpapi.Entry{Kind: tcpapi.EntryRelease, Slot: 4, Generation: 0})
		},
		"release with a stale generation": func(p *pair4, client tcpapi.Reply, server tcpapi.Entry) {
			p.push(&p.a, tcpapi.Entry{Kind: tcpapi.EntryCredit, Index: server.Index, Endpoint: server.Endpoint, Length: 1})
			p.write(&p.b, 0, client.Index, client.Endpoint, 0, []byte("abc"))
			p.deliver()
			p.deliver()
			var e tcpapi.Entry
			if p.a.control.PopRX(&e) != shmring.OK || e.Kind != tcpapi.EntryData {
				p.t.Fatalf("data %+v", e)
			}
			p.push(&p.a, tcpapi.Entry{Kind: tcpapi.EntryRelease, Slot: e.Slot, Generation: e.Generation + 1})
		},
		"credit past the bound": func(p *pair4, _ tcpapi.Reply, server tcpapi.Entry) {
			p.push(&p.a, tcpapi.Entry{Kind: tcpapi.EntryCredit, Index: server.Index, Endpoint: server.Endpoint, Length: tcpapi.MaxCredit4})
			p.push(&p.a, tcpapi.Entry{Kind: tcpapi.EntryCredit, Index: server.Index, Endpoint: server.Endpoint, Length: 1})
		},
		"forget of an open endpoint": func(p *pair4, _ tcpapi.Reply, server tcpapi.Entry) {
			p.push(&p.a, tcpapi.Entry{Kind: tcpapi.EntryForget, Index: server.Index, Endpoint: server.Endpoint})
		},
		"corrupt ring indices": func(p *pair4, _ tcpapi.Reply, _ tcpapi.Entry) {
			base := tcpapi.Layout4.Offset(tcpapi.TXSlots4)
			*shmring.Word(p.b.tx, int(base)+16) = 1000
		},
		"entry of a kind the ring does not carry": func(p *pair4, _ tcpapi.Reply, _ tcpapi.Entry) {
			base := int(tcpapi.Layout4.Offset(tcpapi.TXSlots4))
			p.b.tx[base+64] = byte(tcpapi.EntryData)
			*shmring.Word(p.b.tx, base+16) = 1
		},
	} {
		t.Run(name, func(t *testing.T) {
			p := newPair4(t)
			client, server := p.connected(9101)
			violate(p, client, server)
			target := &p.a
			if name == "write of a held slot" || name == "stale TX generation" || name[:7] == "corrupt" || name[:5] == "entry" {
				target = &p.b
			}
			if fault := p.fault(target); fault != FaultProtocol {
				t.Fatalf("fault %q", fault)
			}
		})
	}
}

func TestSession4CloseIsAnOwnershipTransfer(t *testing.T) {
	p := newPair4(t)
	smallBuffers(t, p.stack)
	client, server := p.connected(9102)
	// Queue more than loopback buffers so slots are still held at Close.
	var offset uint32
	for slot := range uint8(8) {
		p.write(&p.b, slot, client.Index, client.Endpoint, offset, pattern(offset, tcpapi.SlotSize4))
		offset += tcpapi.SlotSize4
	}
	p.deliver()
	if p.b.s.TXHeld() == 0 {
		t.Fatal("every slot left at once")
	}
	if reply := p.call(&p.b, tcpapi.Request{Opcode: tcpapi.OpClose, Endpoint: client.Endpoint}); reply.Result != tcpapi.ResultOK {
		t.Fatalf("close %+v", reply)
	}
	// A write after Close comes back closed; a second Close is refused.
	p.write(&p.b, 9, client.Index, client.Endpoint, offset, []byte("late"))
	if reply := p.call(&p.b, tcpapi.Request{Opcode: tcpapi.OpClose, Endpoint: client.Endpoint}); reply.Result != tcpapi.ResultInvalidArgument {
		t.Fatalf("second close %+v", reply)
	}
	// The reader drains; the queued slots come back, then the end.
	var got []byte
	var e tcpapi.Entry
	done, closed, late, ended, credit := 0, false, false, false, 0
	for round := 0; round < 200 && !(closed && ended); round++ {
		if credit == 0 {
			p.push(&p.a, tcpapi.Entry{Kind: tcpapi.EntryCredit, Index: server.Index, Endpoint: server.Endpoint, Length: 1})
			credit++
		}
		p.deliver()
		for p.a.control.PopRX(&e) == shmring.OK {
			credit--
			switch e.Kind {
			case tcpapi.EntryData:
				got = append(got, p.a.rx[tcpapi.Layout4.Offset(e.Slot):][:e.Length]...)
				p.push(&p.a, tcpapi.Entry{Kind: tcpapi.EntryRelease, Slot: e.Slot, Generation: e.Generation})
			case tcpapi.EntryEnd:
				ended = e.Result == tcpapi.ResultEOF
			}
		}
		for p.b.control.PopTXDone(&e) == shmring.OK {
			switch {
			case e.Kind == tcpapi.EntryClosed:
				if done != 8 || !late {
					t.Fatalf("closed before its slots: %d back", done)
				}
				closed = true
			case e.Slot == 9:
				late = e.Result == tcpapi.ResultClosed
			case e.Result == tcpapi.ResultOK && e.Length == tcpapi.SlotSize4:
				done++
			default:
				t.Fatalf("write done %+v", e)
			}
		}
		p.now += 1_000_000
		p.stack.Advance(p.now)
	}
	if !closed || !bytes.Equal(got, pattern(0, 8*tcpapi.SlotSize4)) {
		t.Fatalf("closed %v, got %d bytes", closed, len(got))
	}
	// The ID stays reserved until the client forgets it; then a stale
	// write naming it comes back invalid.
	if snap := p.b.s.Snapshot(int(client.Index)); snap.ID != client.Endpoint || snap.State != uint8(endpointClosed) {
		t.Fatalf("entry %+v", snap)
	}
	p.push(&p.b, tcpapi.Entry{Kind: tcpapi.EntryForget, Index: client.Index, Endpoint: client.Endpoint})
	p.deliver()
	if snap := p.b.s.Snapshot(int(client.Index)); snap.ID != 0 {
		t.Fatalf("forgotten entry %+v", snap)
	}
	again := p.call(&p.b, tcpapi.Request{Opcode: tcpapi.OpOpen})
	if again.Index != client.Index || again.Endpoint == client.Endpoint {
		t.Fatalf("reopen %+v", again)
	}
	p.write(&p.b, 0, client.Index, client.Endpoint, 0, []byte("stale"))
	p.deliver()
	if p.b.control.PopTXDone(&e) != shmring.OK || e.Result != tcpapi.ResultInvalidArgument || e.Endpoint != client.Endpoint {
		t.Fatalf("stale write %+v", e)
	}
}

// EntryClose rides the TX ring behind the Writes before it: netd sends
// them, hands their slots back and then announces the end, with no channel
// request. A second EntryClose for the endpoint ends the session.
func TestSession4CloseEntryFollowsQueuedWrites(t *testing.T) {
	p := newPair4(t)
	smallBuffers(t, p.stack)
	client, server := p.connected(9106)
	var offset uint32
	for slot := range uint8(6) {
		p.write(&p.b, slot, client.Index, client.Endpoint, offset, pattern(offset, tcpapi.SlotSize4))
		offset += tcpapi.SlotSize4
	}
	if p.b.control.PushTX(&tcpapi.Entry{Kind: tcpapi.EntryClose, Index: client.Index, Endpoint: client.Endpoint}) != shmring.OK {
		t.Fatal("push close")
	}
	var got []byte
	var e tcpapi.Entry
	done, closed, ended, credit := 0, false, false, 0
	// The FIN meets a closed window and waits for its retransmission.
	for round := 0; round < 2000 && !(closed && ended); round++ {
		if credit == 0 {
			p.push(&p.a, tcpapi.Entry{Kind: tcpapi.EntryCredit, Index: server.Index, Endpoint: server.Endpoint, Length: 1})
			credit++
		}
		p.deliver()
		for p.a.control.PopRX(&e) == shmring.OK {
			credit--
			switch e.Kind {
			case tcpapi.EntryData:
				got = append(got, p.a.rx[tcpapi.Layout4.Offset(e.Slot):][:e.Length]...)
				p.push(&p.a, tcpapi.Entry{Kind: tcpapi.EntryRelease, Slot: e.Slot, Generation: e.Generation})
			case tcpapi.EntryEnd:
				ended = e.Result == tcpapi.ResultEOF
			}
		}
		for p.b.control.PopTXDone(&e) == shmring.OK {
			switch {
			case e.Kind == tcpapi.EntryClosed:
				if done != 6 {
					t.Fatalf("closed before its slots: %d back", done)
				}
				closed = true
			case e.Result == tcpapi.ResultOK && e.Length == tcpapi.SlotSize4:
				done++
			default:
				t.Fatalf("write done %+v", e)
			}
		}
		p.now += 1_000_000
		p.stack.Advance(p.now)
	}
	if !closed || !ended || !bytes.Equal(got, pattern(0, 6*tcpapi.SlotSize4)) {
		t.Fatalf("closed %v ended %v, got %d bytes", closed, ended, len(got))
	}
	if len(p.out.sent) != 0 {
		t.Fatal("EntryClose drew a reply")
	}
	if p.b.control.PushTX(&tcpapi.Entry{Kind: tcpapi.EntryClose, Index: client.Index, Endpoint: client.Endpoint}) != shmring.OK {
		t.Fatal("push second close")
	}
	if fault := p.fault(&p.b); fault != FaultProtocol {
		t.Fatalf("second close: fault %q", fault)
	}
}

func TestSession4ShutdownFollowsQueuedSlots(t *testing.T) {
	p := newPair4(t)
	client, server := p.connected(9103)
	p.write(&p.b, 0, client.Index, client.Endpoint, 0, pattern(0, 1000))
	if reply := p.call(&p.b, tcpapi.Request{Opcode: tcpapi.OpShutdownWrite, Endpoint: client.Endpoint}); reply.Result != tcpapi.ResultOK {
		t.Fatalf("shutdown %+v", reply)
	}
	p.push(&p.a, tcpapi.Entry{Kind: tcpapi.EntryCredit, Index: server.Index, Endpoint: server.Endpoint, Length: 3})
	p.deliver()
	p.deliver()
	var e tcpapi.Entry
	if p.a.control.PopRX(&e) != shmring.OK || e.Kind != tcpapi.EntryData || e.Length != 1000 {
		t.Fatalf("data %+v", e)
	}
	if p.a.control.PopRX(&e) != shmring.OK || e.Kind != tcpapi.EntryEnd || e.Result != tcpapi.ResultEOF {
		t.Fatalf("end %+v", e)
	}
}

func TestSession4DoorbellOnlyWhenArmed(t *testing.T) {
	p := newPair4(t)
	client, server := p.connected(9104)
	p.out.doorbells = 0
	p.push(&p.a, tcpapi.Entry{Kind: tcpapi.EntryCredit, Index: server.Index, Endpoint: server.Endpoint, Length: 4})
	p.write(&p.b, 0, client.Index, client.Endpoint, 0, []byte("one"))
	p.deliver()
	if p.out.doorbells != 0 {
		t.Fatalf("%d doorbells to clients that never armed", p.out.doorbells)
	}
	p.a.control.ClientArm()
	p.b.control.ClientArm()
	p.write(&p.b, 1, client.Index, client.Endpoint, 3, []byte("two"))
	p.write(&p.b, 2, client.Index, client.Endpoint, 6, []byte("three"))
	p.deliver()
	p.deliver()
	// Each armed client hears one doorbell, however many entries followed.
	if p.out.doorbells != 2 {
		t.Fatalf("doorbells %d", p.out.doorbells)
	}
	// netd arms when it goes idle; the next client entry must ring it.
	if !p.b.s.Idle() {
		t.Fatal("rings pending")
	}
	if p.b.control.ClientShouldRing() != true || p.b.control.ClientShouldRing() {
		t.Fatal("one arm, one ring")
	}
}

func TestSession4OpenConnectFailureLeavesNothing(t *testing.T) {
	p := newPair4(t)
	reply := p.call(&p.b, tcpapi.Request{Opcode: tcpapi.OpOpenConnect, Address: [4]byte{127, 0, 0, 1}, Port: 9199})
	if reply.Result == tcpapi.ResultOK || reply.Endpoint != 0 {
		t.Fatalf("connect to nothing %+v", reply)
	}
	for index := range tcpapi.Endpoints4 {
		if snap := p.b.s.Snapshot(index); snap.ID != 0 {
			t.Fatalf("entry %d left %+v", index, snap)
		}
	}
	// The data opcodes of version 3 are not version 4's.
	var message ipc.Message
	for _, request := range []tcpapi.Request{{Opcode: tcpapi.OpWrite, Endpoint: 1, Length: 1}, {Opcode: tcpapi.OpRead, Endpoint: 1, Length: 1}} {
		if tcpapi.Layout4.EncodeRequest(&request, 1, &message) {
			t.Fatalf("encoded %+v", request)
		}
	}
}

func TestSession4RunnableFindsDeliverWork(t *testing.T) {
	p := newPair4(t)
	client, server := p.connected(9105)
	if fault, more := p.b.s.Deliver(p.stack, p.now, p.out); fault != FaultNone || more {
		t.Fatal("settle")
	}
	if _, op, found := p.b.s.Runnable(p.stack); found {
		t.Fatalf("idle session runnable %q", op)
	}
	p.write(&p.b, 0, client.Index, client.Endpoint, 0, []byte("x"))
	if _, op, found := p.b.s.Runnable(p.stack); !found || op != "rings" {
		t.Fatalf("runnable %q", op)
	}
	p.deliver()
	p.push(&p.a, tcpapi.Entry{Kind: tcpapi.EntryCredit, Index: server.Index, Endpoint: server.Endpoint, Length: 1})
	p.deliver()
	if _, op, found := p.a.s.Runnable(p.stack); found {
		t.Fatalf("after deliver %q", op)
	}
}

// The stack grants larger quotas than a GTCP 4 table can index; such a
// session does not start.
func TestSession4QuotaStopsAtTheTable(t *testing.T) {
	stack := new(network.Stack)
	stack.Init(99)
	tx, rx := make([]byte, tcpapi.Layout4.TXPool()), make([]byte, tcpapi.Layout4.RXPool())
	var s Session4
	if s.Start(stack, 0, tcpapi.Endpoints4+1, tx, rx, 1) {
		t.Fatal("quota past the table accepted")
	}
	if !s.Start(stack, 0, tcpapi.Endpoints4, tx, rx, 1) {
		t.Fatal("quota of the whole table refused")
	}
}
