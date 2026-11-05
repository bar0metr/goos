package tcpserve

import (
	"bytes"
	"testing"

	"goos/kernel/ipc"
	"goos/userspace/network"
	"goos/userspace/tcpapi"
)

// codec3 sends every reply through the GTCP 3 encoder and decoder.
type codec3 struct{ replies }

func (c *codec3) Reply(reply *tcpapi.Reply, correlation uint64) bool {
	var message ipc.Message
	if !tcpapi.Layout3.EncodeReply(reply, correlation, &message) {
		return false
	}
	var decoded tcpapi.Reply
	got, ok := tcpapi.Layout3.DecodeReply(&message, &decoded)
	if !ok || got != correlation {
		return false
	}
	return c.replies.Reply(&decoded, got)
}

// pair3 is a loopback server and client of GTCP 3 over one real stack.
type pair3 struct {
	t              *testing.T
	stack          *network.Stack
	now            uint64
	server, client Session
	serverPools    pools
	clientPools    pools
	out            *codec3
	correlation    uint64
}

func newPools3() pools {
	return pools{make([]byte, tcpapi.Layout3.TXPool()), make([]byte, tcpapi.Layout3.RXPool())}
}

func newPair3(t *testing.T) *pair3 {
	p := &pair3{t: t, stack: new(network.Stack), now: 1_000_000_000, out: new(codec3), serverPools: newPools3(), clientPools: newPools3()}
	p.stack.Init(1234)
	if !p.server.StartLayout(p.stack, 0, 0, tcpapi.Layout3, p.serverPools.tx, p.serverPools.rx, p.now) ||
		!p.client.StartLayout(p.stack, 1, 0, tcpapi.Layout3, p.clientPools.tx, p.clientPools.rx, p.now) {
		t.Fatal("start")
	}
	return p
}

func (p *pair3) next() uint64 { p.correlation++; return p.correlation }

func (p *pair3) call(s *Session, request tcpapi.Request) tcpapi.Reply {
	p.t.Helper()
	correlation := p.next()
	if fault := s.Handle(p.stack, &request, correlation, p.now, p.out); fault != FaultNone {
		p.t.Fatalf("opcode %d fault %q", request.Opcode, fault)
	}
	return p.out.take(p.t, correlation)
}

func (p *pair3) park(s *Session, request tcpapi.Request) uint64 {
	p.t.Helper()
	correlation := p.next()
	if fault := s.Handle(p.stack, &request, correlation, p.now, p.out); fault != FaultNone {
		p.t.Fatalf("fault %q", fault)
	}
	for _, sent := range p.out.sent {
		if sent.correlation == correlation {
			p.t.Fatalf("opcode %d answered at once: %+v", request.Opcode, sent.reply)
		}
	}
	return correlation
}

func (p *pair3) deliver() {
	p.t.Helper()
	for _, s := range []*Session{&p.server, &p.client} {
		if fault := s.Deliver(p.stack, p.now, p.out); fault != FaultNone {
			p.t.Fatalf("deliver fault %q", fault)
		}
	}
}

func (p *pair3) release(s *Session, slot uint8) {
	p.t.Helper()
	if fault := s.Handle(p.stack, &tcpapi.Request{Opcode: tcpapi.OpRelease, Slot: slot}, 0, p.now, p.out); fault != FaultNone {
		p.t.Fatalf("release fault %q", fault)
	}
}

func (p *pair3) connected(port uint16) (client, server uint32) {
	p.t.Helper()
	listener := p.call(&p.server, tcpapi.Request{Opcode: tcpapi.OpOpen}).Endpoint
	p.call(&p.server, tcpapi.Request{Opcode: tcpapi.OpBind, Endpoint: listener, Address: [4]byte{127, 0, 0, 1}, Port: port})
	p.call(&p.server, tcpapi.Request{Opcode: tcpapi.OpListen, Endpoint: listener, Backlog: 4})
	client = p.call(&p.client, tcpapi.Request{Opcode: tcpapi.OpOpen}).Endpoint
	if reply := p.call(&p.client, tcpapi.Request{Opcode: tcpapi.OpConnect, Endpoint: client, Address: [4]byte{127, 0, 0, 1}, Port: port, Flags: tcpapi.FlagNoWait}); reply.Result != tcpapi.ResultOK {
		p.t.Fatalf("connect %+v", reply)
	}
	accepted := p.call(&p.server, tcpapi.Request{Opcode: tcpapi.OpAccept, Endpoint: listener, Flags: tcpapi.FlagNoWait})
	if accepted.Result != tcpapi.ResultOK {
		p.t.Fatalf("accept %+v", accepted)
	}
	return client, accepted.Endpoint
}

// fill writes a recognizable pattern of length bytes, starting at stream
// offset, into the client's TX slot.
func (p *pair3) fill(slot uint8, offset uint32, length int) {
	base := tcpapi.Layout3.Offset(slot)
	for index := range length {
		p.clientPools.tx[base+uint64(index)] = byte((offset + uint32(index)) % 251)
	}
}

func (p *pair3) write(client uint32, slot uint8, offset uint32, length int) tcpapi.Reply {
	p.t.Helper()
	p.fill(slot, offset, length)
	return p.call(&p.client, tcpapi.Request{Opcode: tcpapi.OpWrite, Endpoint: client, Slot: slot, Length: uint16(length),
		Flags: tcpapi.FlagNoWait, Offset: offset})
}

func pattern(offset uint32, length int) []byte {
	out := make([]byte, length)
	for index := range out {
		out[index] = byte((offset + uint32(index)) % 251)
	}
	return out
}

func TestVersion3StartChecksLayoutAndPools(t *testing.T) {
	stack := new(network.Stack)
	stack.Init(1)
	var s Session
	small := newPools()
	if s.StartLayout(stack, 0, 0, tcpapi.Layout3, small.tx, small.rx, 0) {
		t.Fatal("started GTCP 3 over GTCP 2 pools")
	}
	forged := tcpapi.Layout3
	forged.SlotSize = 65536
	big := newPools3()
	if s.StartLayout(stack, 0, 0, forged, big.tx, big.rx, 0) || s.StartLayout(stack, 0, 0, tcpapi.Layout{}, big.tx, big.rx, 0) {
		t.Fatal("started an unknown layout")
	}
	if !s.StartLayout(stack, 0, 0, tcpapi.Layout3, big.tx, big.rx, 0) || s.Layout() != tcpapi.Layout3 {
		t.Fatal("GTCP 3 start")
	}
	var v2 Session
	v2.Start(stack, 1, small.tx, small.rx, 0)
	if v2.Layout() != tcpapi.Layout2 {
		t.Fatal("Start is not GTCP 2")
	}
}

func TestVersion3SessionDecodesOnlyItsVersion(t *testing.T) {
	stack := new(network.Stack)
	stack.Init(1)
	var v2, v3 Session
	v2.Start(stack, 0, newPools().tx, newPools().rx, 0)
	big := newPools3()
	v3.StartLayout(stack, 1, 0, tcpapi.Layout3, big.tx, big.rx, 0)
	request := tcpapi.Request{Opcode: tcpapi.OpRead, Endpoint: 1, Length: 100}
	var old, current ipc.Message
	if !tcpapi.EncodeRequest(&request, 1, &old) || !tcpapi.Layout3.EncodeRequest(&request, 1, &current) {
		t.Fatal("encode")
	}
	var decoded tcpapi.Request
	if _, ok := v3.Decode(&old, &decoded); ok {
		t.Fatal("GTCP 3 session took a GTCP 2 request")
	}
	if _, ok := v2.Decode(&current, &decoded); ok {
		t.Fatal("GTCP 2 session took a GTCP 3 request")
	}
	if _, ok := v3.Decode(&current, &decoded); !ok || decoded != request {
		t.Fatalf("GTCP 3 decode %+v", decoded)
	}
	var reply ipc.Message
	if !v3.Encode(&tcpapi.Reply{Opcode: tcpapi.OpRead, Endpoint: 1, Length: tcpapi.SlotSize3}, 2, &reply) || reply.Version != tcpapi.Version3 {
		t.Fatal("GTCP 3 reply of a full slot")
	}
	if v2.Encode(&tcpapi.Reply{Opcode: tcpapi.OpRead, Endpoint: 1, Length: tcpapi.SlotSize3}, 2, &reply) {
		t.Fatal("GTCP 2 reply past its slot")
	}
}

func TestVersion3ParkedReadTakesAFullSlot(t *testing.T) {
	p := newPair3(t)
	client, server := p.connected(9000)
	parked := p.park(&p.server, tcpapi.Request{Opcode: tcpapi.OpRead, Endpoint: server, Length: tcpapi.SlotSize3})
	reply := p.write(client, 0, 0, tcpapi.SlotSize3)
	if reply.Result != tcpapi.ResultOK || reply.Length != tcpapi.SlotSize3 || reply.Slot != 0 {
		t.Fatalf("16 KiB write %+v", reply)
	}
	p.deliver()
	read := p.out.take(t, parked)
	if read.Result != tcpapi.ResultOK || read.Length != tcpapi.SlotSize3 {
		t.Fatalf("parked read %+v", read)
	}
	got := p.serverPools.rx[tcpapi.Layout3.Offset(read.Slot):][:read.Length]
	if !bytes.Equal(got, pattern(0, tcpapi.SlotSize3)) {
		t.Fatal("bytes differ")
	}
}

func TestVersion3PipelinedWritesKeepStreamOrder(t *testing.T) {
	p := newPair3(t)
	client, server := p.connected(9001)
	// Four NoWait writes of 16 KiB, each naming the offset it would have if
	// every earlier one were taken whole, as a pipelining client sends them.
	var accepted uint32
	stalled := false
	for index := range uint8(tcpapi.PipelinedWrites3) {
		offset := uint32(index) * tcpapi.SlotSize3
		reply := p.write(client, index, offset, tcpapi.SlotSize3)
		if reply.Slot != index {
			t.Fatalf("slot not returned %+v", reply)
		}
		switch {
		case stalled:
			// Once a write took a prefix or nothing, later ones take nothing.
			if reply.Result != tcpapi.ResultWouldBlock || reply.Length != 0 {
				t.Fatalf("write %d after a short one %+v", index, reply)
			}
		case reply.Result == tcpapi.ResultOK:
			accepted += uint32(reply.Length)
			stalled = reply.Length < tcpapi.SlotSize3
		case reply.Result == tcpapi.ResultWouldBlock:
			stalled = true
		default:
			t.Fatalf("write %d %+v", index, reply)
		}
	}
	if !stalled || accepted < tcpapi.SlotSize3 {
		t.Fatalf("loopback took %d bytes of 64 KiB", accepted)
	}
	// A write behind the accepted count is an error and takes nothing.
	if reply := p.write(client, 0, accepted-1, 1); reply.Result != tcpapi.ResultInvalidArgument || reply.Length != 0 {
		t.Fatalf("write behind %+v", reply)
	}
	// The reader drains; the client resends from the first byte left.
	const total = tcpapi.PipelinedWrites3 * tcpapi.SlotSize3
	var received []byte
	for len(received) < total {
		read := p.call(&p.server, tcpapi.Request{Opcode: tcpapi.OpRead, Endpoint: server, Length: tcpapi.SlotSize3, Flags: tcpapi.FlagNoWait})
		if read.Result == tcpapi.ResultWouldBlock {
			if accepted >= total {
				t.Fatalf("stream stalled at %d of %d", len(received), total)
			}
			reply := p.write(client, 1, accepted, min(tcpapi.SlotSize3, total-int(accepted)))
			if reply.Result != tcpapi.ResultOK && reply.Result != tcpapi.ResultWouldBlock {
				t.Fatalf("resend %+v", reply)
			}
			accepted += uint32(reply.Length)
			p.deliver()
			continue
		}
		if read.Result != tcpapi.ResultOK {
			t.Fatalf("read %+v", read)
		}
		received = append(received, p.serverPools.rx[tcpapi.Layout3.Offset(read.Slot):][:read.Length]...)
		p.release(&p.server, read.Slot)
		p.deliver()
	}
	if !bytes.Equal(received, pattern(0, len(received))) {
		t.Fatal("stream out of order")
	}
}

func TestVersion3QueuedReadsAnswerInOrder(t *testing.T) {
	p := newPair3(t)
	client, server := p.connected(9002)
	var parked [tcpapi.QueuedReads3]uint64
	for index := range parked {
		parked[index] = p.park(&p.server, tcpapi.Request{Opcode: tcpapi.OpRead, Endpoint: server, Length: 100})
		if index != 0 {
			continue
		}
		// A NoWait read behind a parked one would take its bytes.
		if reply := p.call(&p.server, tcpapi.Request{Opcode: tcpapi.OpRead, Endpoint: server, Length: 100, Flags: tcpapi.FlagNoWait}); reply.Result != tcpapi.ResultWouldBlock {
			t.Fatalf("NoWait read behind the queue %+v", reply)
		}
	}
	if reply := p.call(&p.server, tcpapi.Request{Opcode: tcpapi.OpRead, Endpoint: server, Length: 100}); reply.Result != tcpapi.ResultBusy {
		t.Fatalf("read past the queue %+v", reply)
	}
	// A WaitReady parks beside the reads.
	seen := p.call(&p.server, tcpapi.Request{Opcode: tcpapi.OpReady, Endpoint: server})
	wait := p.park(&p.server, tcpapi.Request{Opcode: tcpapi.OpWaitReady, Endpoint: server, Mask: seen.Mask})
	if reply := p.write(client, 0, 0, 250); reply.Length != 250 {
		t.Fatalf("write %+v", reply)
	}
	p.deliver()
	// 250 bytes answer the first three reads with 100, 100 and 50 bytes.
	if len(p.out.sent) != 3 {
		t.Fatalf("replies %+v", p.out.sent)
	}
	for index, want := range []uint16{100, 100, 50} {
		sent := p.out.sent[index]
		if sent.correlation != parked[index] || sent.reply.Result != tcpapi.ResultOK || sent.reply.Length != want {
			t.Fatalf("read %d %+v", index, sent)
		}
		got := p.serverPools.rx[tcpapi.Layout3.Offset(sent.reply.Slot):][:want]
		if !bytes.Equal(got, pattern(uint32(index)*100, int(want))) {
			t.Fatalf("read %d bytes differ", index)
		}
	}
	p.out.sent = nil
	if p.server.Waiting() != 2 {
		t.Fatalf("waiting %d", p.server.Waiting())
	}
	// Close answers the WaitReady, then the last read, then itself.
	closing := p.next()
	p.server.Handle(p.stack, &tcpapi.Request{Opcode: tcpapi.OpClose, Endpoint: server}, closing, p.now, p.out)
	if len(p.out.sent) != 3 || p.out.sent[0].correlation != wait || p.out.sent[1].correlation != parked[3] ||
		p.out.sent[1].reply.Result != tcpapi.ResultClosed || p.out.sent[2].correlation != closing {
		t.Fatalf("close replies %+v", p.out.sent)
	}
	if p.server.Waiting() != 0 {
		t.Fatal("still waiting")
	}
}

func TestVersion3QueuedReadWaitsForARelease(t *testing.T) {
	p := newPair3(t)
	client, server := p.connected(9003)
	for slot := range uint8(tcpapi.RXSlots3) {
		p.write(client, 0, uint32(slot), 1)
		if read := p.call(&p.server, tcpapi.Request{Opcode: tcpapi.OpRead, Endpoint: server, Length: 1, Flags: tcpapi.FlagNoWait}); read.Result != tcpapi.ResultOK {
			t.Fatalf("read %+v", read)
		}
	}
	first := p.park(&p.server, tcpapi.Request{Opcode: tcpapi.OpRead, Endpoint: server, Length: 10})
	second := p.park(&p.server, tcpapi.Request{Opcode: tcpapi.OpRead, Endpoint: server, Length: 10})
	p.write(client, 0, tcpapi.RXSlots3, 4)
	p.deliver()
	if len(p.out.sent) != 0 {
		t.Fatalf("answered with every RX slot lent %+v", p.out.sent)
	}
	p.release(&p.server, 3)
	if len(p.out.sent) != 1 || p.out.sent[0].correlation != first || p.out.sent[0].reply.Slot != 3 || p.out.sent[0].reply.Length != 4 {
		t.Fatalf("after a release %+v", p.out.sent)
	}
	p.out.sent = nil
	p.write(client, 0, tcpapi.RXSlots3+4, 2)
	p.release(&p.server, 5)
	if len(p.out.sent) != 1 || p.out.sent[0].correlation != second || p.out.sent[0].reply.Length != 2 {
		t.Fatalf("second read %+v", p.out.sent)
	}
}

func TestVersion2KeepsOneParkedRead(t *testing.T) {
	p := newPair(t)
	_, _, server := p.connected(9004)
	p.park(&p.server, tcpapi.Request{Opcode: tcpapi.OpRead, Endpoint: server, Length: 10})
	if reply := p.call(&p.server, tcpapi.Request{Opcode: tcpapi.OpRead, Endpoint: server, Length: 10}); reply.Result != tcpapi.ResultBusy {
		t.Fatalf("second parked read %+v", reply)
	}
	if reply := p.call(&p.server, tcpapi.Request{Opcode: tcpapi.OpRead, Endpoint: server, Length: 10, Flags: tcpapi.FlagNoWait}); reply.Result != tcpapi.ResultBusy {
		t.Fatalf("NoWait read beside a parked one %+v", reply)
	}
}

func TestVersion3PartialWriteSaysSo(t *testing.T) {
	p := newPair3(t)
	client, server := p.connected(9005)
	// Loopback holds the receiver's and the sender's buffers: 32 KiB.
	for index := range uint32(2) {
		if reply := p.write(client, 0, index*tcpapi.SlotSize3, tcpapi.SlotSize3); reply.Length != tcpapi.SlotSize3 {
			t.Fatalf("write %d %+v", index, reply)
		}
	}
	// A read that opens the window by half the buffer draws the window
	// update a smaller one would not (receiver silly-window avoidance).
	read := p.call(&p.server, tcpapi.Request{Opcode: tcpapi.OpRead, Endpoint: server, Length: 9000, Flags: tcpapi.FlagNoWait})
	p.release(&p.server, read.Slot)
	p.deliver()
	const offset = 2 * tcpapi.SlotSize3
	short := p.write(client, 0, offset, tcpapi.SlotSize3)
	if short.Result != tcpapi.ResultOK || short.Length == 0 || short.Length >= tcpapi.SlotSize3 {
		t.Fatalf("prefix write %+v", short)
	}
	if next := p.write(client, 1, offset+tcpapi.SlotSize3, tcpapi.SlotSize3); next.Result != tcpapi.ResultWouldBlock || next.Length != 0 {
		t.Fatalf("write after the prefix %+v", next)
	}
	// The resend from the first byte left is taken once there is room.
	read = p.call(&p.server, tcpapi.Request{Opcode: tcpapi.OpRead, Endpoint: server, Length: tcpapi.SlotSize3, Flags: tcpapi.FlagNoWait})
	p.release(&p.server, read.Slot)
	p.deliver()
	resume := offset + uint32(short.Length)
	if again := p.write(client, 1, resume, 10); again.Result != tcpapi.ResultOK || again.Length != 10 {
		t.Fatalf("resend %+v", again)
	}
}
