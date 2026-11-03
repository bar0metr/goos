package tcpserve

import (
	"testing"

	"goos/userspace/network"
	"goos/userspace/tcpapi"
)

// pools are one session's TX and RX pools.
type pools struct{ tx, rx []byte }

func newPools() pools {
	return pools{make([]byte, tcpapi.TXSlots*tcpapi.SlotSize), make([]byte, tcpapi.RXSlots*tcpapi.SlotSize)}
}

// pair is a loopback server and client session over one real stack.
type pair struct {
	t              *testing.T
	stack          *network.Stack
	now            uint64
	server, client Session
	serverPools    pools
	clientPools    pools
	out            *codec
	correlation    uint64
}

func newPair(t *testing.T) *pair {
	p := &pair{t: t, stack: new(network.Stack), now: 1_000_000_000, out: new(codec), serverPools: newPools(), clientPools: newPools()}
	p.stack.Init(1234)
	p.server.Start(p.stack, 0, p.serverPools.tx, p.serverPools.rx, p.now)
	p.client.Start(p.stack, 1, p.clientPools.tx, p.clientPools.rx, p.now)
	return p
}

func (p *pair) next() uint64 { p.correlation++; return p.correlation }

func (p *pair) call(s *Session, request tcpapi.Request) tcpapi.Reply {
	p.t.Helper()
	return p.out.call(p.t, p.stack, s, request, p.next(), p.now, p.out)
}

// park sends a request that must not be answered yet and returns its
// correlation.
func (p *pair) park(s *Session, request tcpapi.Request) uint64 {
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

func (p *pair) deliver() {
	p.t.Helper()
	for _, s := range []*Session{&p.server, &p.client} {
		if fault := s.Deliver(p.stack, p.now, p.out); fault != FaultNone {
			p.t.Fatalf("deliver fault %q", fault)
		}
	}
}

// connected returns a listener, the client's endpoint and the accepted
// server child.
func (p *pair) connected(port uint16) (listener, client, server uint32) {
	p.t.Helper()
	listener = p.call(&p.server, tcpapi.Request{Opcode: tcpapi.OpOpen}).Endpoint
	if reply := p.call(&p.server, tcpapi.Request{Opcode: tcpapi.OpBind, Endpoint: listener, Address: [4]byte{127, 0, 0, 1}, Port: port, Flags: tcpapi.FlagReuseAddr}); reply.Result != tcpapi.ResultOK {
		p.t.Fatalf("bind %+v", reply)
	}
	p.call(&p.server, tcpapi.Request{Opcode: tcpapi.OpListen, Endpoint: listener, Backlog: 4})
	client = p.call(&p.client, tcpapi.Request{Opcode: tcpapi.OpOpen}).Endpoint
	if reply := p.call(&p.client, tcpapi.Request{Opcode: tcpapi.OpConnect, Endpoint: client, Address: [4]byte{127, 0, 0, 1}, Port: port, Flags: tcpapi.FlagNoWait}); reply.Result != tcpapi.ResultOK {
		p.t.Fatalf("loopback NoWait connect %+v", reply)
	}
	accepted := p.call(&p.server, tcpapi.Request{Opcode: tcpapi.OpAccept, Endpoint: listener, Flags: tcpapi.FlagNoWait})
	if accepted.Result != tcpapi.ResultOK {
		p.t.Fatalf("accept %+v", accepted)
	}
	return listener, client, accepted.Endpoint
}

func (p *pair) write(endpoint uint32, data string, flags uint8) tcpapi.Reply {
	p.t.Helper()
	copy(p.clientPools.tx, data)
	return p.call(&p.client, tcpapi.Request{Opcode: tcpapi.OpWrite, Endpoint: endpoint, Length: uint16(len(data)), Flags: flags})
}

func TestReadyBitsMatchTheStack(t *testing.T) {
	for _, pair := range [][2]uint16{
		{tcpapi.ReadyAcceptable, uint16(network.TCPReadyAcceptable)},
		{tcpapi.ReadyConnected, uint16(network.TCPReadyConnected)},
		{tcpapi.ReadyConnectFailed, uint16(network.TCPReadyConnectFailed)},
		{tcpapi.ReadyReadable, uint16(network.TCPReadyReadable)},
		{tcpapi.ReadyReadEOF, uint16(network.TCPReadyReadEOF)},
		{tcpapi.ReadyWritable, uint16(network.TCPReadyWritable)},
		{tcpapi.ReadyWriteClosed, uint16(network.TCPReadyWriteClosed)},
		{tcpapi.ReadyError, uint16(network.TCPReadyError)},
		{tcpapi.ReadyHangUp, uint16(network.TCPReadyHangUp)},
		{tcpapi.ReadyConnecting, uint16(network.TCPReadyConnecting)},
		{tcpapi.ReadyListening, uint16(network.TCPReadyListening)},
		{tcpapi.ReadyAll, uint16(network.TCPReadyAll)},
	} {
		if pair[0] != pair[1] {
			t.Fatalf("bit %#x is %#x in the stack", pair[0], pair[1])
		}
	}
}

func TestNoWaitAcceptReadAndPeek(t *testing.T) {
	p := newPair(t)
	listener := p.call(&p.server, tcpapi.Request{Opcode: tcpapi.OpOpen}).Endpoint
	p.call(&p.server, tcpapi.Request{Opcode: tcpapi.OpBind, Endpoint: listener, Address: [4]byte{127, 0, 0, 1}, Port: 8080})
	p.call(&p.server, tcpapi.Request{Opcode: tcpapi.OpListen, Endpoint: listener, Backlog: 2})
	if reply := p.call(&p.server, tcpapi.Request{Opcode: tcpapi.OpAccept, Endpoint: listener, Flags: tcpapi.FlagNoWait}); reply.Result != tcpapi.ResultWouldBlock {
		t.Fatalf("empty accept %+v", reply)
	}
	if ready := p.call(&p.server, tcpapi.Request{Opcode: tcpapi.OpReady, Endpoint: listener}); ready.Mask != tcpapi.ReadyListening {
		t.Fatalf("idle listener %+v", ready)
	}
	client := p.call(&p.client, tcpapi.Request{Opcode: tcpapi.OpOpen}).Endpoint
	p.call(&p.client, tcpapi.Request{Opcode: tcpapi.OpConnect, Endpoint: client, Address: [4]byte{127, 0, 0, 1}, Port: 8080})
	if ready := p.call(&p.server, tcpapi.Request{Opcode: tcpapi.OpReady, Endpoint: listener}); ready.Mask != tcpapi.ReadyListening|tcpapi.ReadyAcceptable {
		t.Fatalf("listener with a child %+v", ready)
	}
	server := p.call(&p.server, tcpapi.Request{Opcode: tcpapi.OpAccept, Endpoint: listener, Flags: tcpapi.FlagNoWait}).Endpoint

	if reply := p.call(&p.server, tcpapi.Request{Opcode: tcpapi.OpRead, Endpoint: server, Length: 64, Flags: tcpapi.FlagNoWait}); reply.Result != tcpapi.ResultWouldBlock {
		t.Fatalf("empty read %+v", reply)
	}
	p.write(client, "peekaboo", 0)
	peek := p.call(&p.server, tcpapi.Request{Opcode: tcpapi.OpRead, Endpoint: server, Length: 4, Flags: tcpapi.FlagNoWait | tcpapi.FlagPeek})
	if peek.Result != tcpapi.ResultOK || string(p.serverPools.rx[tcpapi.Offset(peek.Slot):][:peek.Length]) != "peek" {
		t.Fatalf("peek %+v", peek)
	}
	p.server.Handle(p.stack, &tcpapi.Request{Opcode: tcpapi.OpRelease, Slot: peek.Slot}, 0, p.now, p.out)
	if ready := p.call(&p.server, tcpapi.Request{Opcode: tcpapi.OpReady, Endpoint: server}); ready.Value != 8 || ready.Mask&tcpapi.ReadyReadable == 0 {
		t.Fatalf("peek consumed bytes %+v", ready)
	}
	read := p.call(&p.server, tcpapi.Request{Opcode: tcpapi.OpRead, Endpoint: server, Length: 64, Flags: tcpapi.FlagNoWait})
	if read.Result != tcpapi.ResultOK || string(p.serverPools.rx[tcpapi.Offset(read.Slot):][:read.Length]) != "peekaboo" {
		t.Fatalf("read %+v", read)
	}
	p.server.Handle(p.stack, &tcpapi.Request{Opcode: tcpapi.OpRelease, Slot: read.Slot}, 0, p.now, p.out)
	// A read without NoWait still parks.
	parked := p.park(&p.server, tcpapi.Request{Opcode: tcpapi.OpRead, Endpoint: server, Length: 64})
	p.write(client, "late", 0)
	p.deliver()
	if reply := p.out.take(t, parked); reply.Result != tcpapi.ResultOK || reply.Length != 4 {
		t.Fatalf("parked read %+v", reply)
	}
	if len(p.out.sent) != 0 {
		t.Fatalf("extra replies %+v", p.out.sent)
	}
}

func TestNoWaitReadWithEveryRXSlotLentIsBusy(t *testing.T) {
	p := newPair(t)
	_, client, server := p.connected(8081)
	for range tcpapi.RXSlots {
		p.write(client, "x", 0)
		if reply := p.call(&p.server, tcpapi.Request{Opcode: tcpapi.OpRead, Endpoint: server, Length: 1, Flags: tcpapi.FlagNoWait}); reply.Result != tcpapi.ResultOK {
			t.Fatalf("read %+v", reply)
		}
	}
	p.write(client, "y", 0)
	if reply := p.call(&p.server, tcpapi.Request{Opcode: tcpapi.OpRead, Endpoint: server, Length: 1, Flags: tcpapi.FlagNoWait}); reply.Result != tcpapi.ResultBusy {
		t.Fatalf("read with no slot %+v", reply)
	}
}

func TestNoWaitWriteTakesWhatFits(t *testing.T) {
	p := newPair(t)
	_, client, _ := p.connected(8082)
	total := 0
	for round := 0; ; round++ {
		reply := p.call(&p.client, tcpapi.Request{Opcode: tcpapi.OpWrite, Endpoint: client, Slot: 1, Length: tcpapi.SlotSize, Flags: tcpapi.FlagNoWait})
		if reply.Slot != 1 {
			t.Fatalf("slot not returned %+v", reply)
		}
		if reply.Result == tcpapi.ResultWouldBlock {
			if reply.Length != 0 {
				t.Fatalf("would block with bytes %+v", reply)
			}
			break
		}
		if reply.Result != tcpapi.ResultOK || round > 16 {
			t.Fatalf("write %d %+v", round, reply)
		}
		total += int(reply.Length)
	}
	// The receiver's window and buffer and the sender's buffer hold it all.
	if total < network.TCPSendCapacity {
		t.Fatalf("took %d bytes", total)
	}
	ready := p.call(&p.client, tcpapi.Request{Opcode: tcpapi.OpReady, Endpoint: client})
	if ready.Mask&tcpapi.ReadyWritable != 0 || ready.Room != 0 {
		t.Fatalf("full send buffer %+v", ready)
	}
}

func TestWaitReadyNoLostWakeup(t *testing.T) {
	p := newPair(t)
	_, client, server := p.connected(8083)
	seen := p.call(&p.server, tcpapi.Request{Opcode: tcpapi.OpReady, Endpoint: server})
	if seen.Mask != tcpapi.ReadyConnected|tcpapi.ReadyWritable {
		t.Fatalf("idle %+v", seen)
	}
	// The change lands between the client's Ready and its WaitReady: the
	// wait names the mask the client saw, so it is answered at once.
	p.write(client, "data", 0)
	wait := p.call(&p.server, tcpapi.Request{Opcode: tcpapi.OpWaitReady, Endpoint: server, Mask: seen.Mask})
	if wait.Result != tcpapi.ResultOK || wait.Mask != seen.Mask|tcpapi.ReadyReadable || wait.Value != 4 {
		t.Fatalf("wait after a change %+v", wait)
	}
	// A wait on the current mask parks, and more bytes on an already
	// readable endpoint do not change the mask.
	parked := p.park(&p.server, tcpapi.Request{Opcode: tcpapi.OpWaitReady, Endpoint: server, Mask: wait.Mask})
	if reply := p.call(&p.server, tcpapi.Request{Opcode: tcpapi.OpWaitReady, Endpoint: server, Mask: wait.Mask}); reply.Result != tcpapi.ResultBusy {
		t.Fatalf("second wait %+v", reply)
	}
	p.write(client, "more", 0)
	p.deliver()
	if len(p.out.sent) != 0 {
		t.Fatalf("woke on an unchanged mask %+v", p.out.sent)
	}
	// The peer's FIN changes it.
	p.call(&p.client, tcpapi.Request{Opcode: tcpapi.OpShutdownWrite, Endpoint: client})
	p.deliver()
	reply := p.out.take(t, parked)
	if reply.Result != tcpapi.ResultOK || reply.Mask != wait.Mask|tcpapi.ReadyReadEOF || reply.Value != 8 {
		t.Fatalf("wait for FIN %+v", reply)
	}
	// Readiness never consumed a byte.
	read := p.call(&p.server, tcpapi.Request{Opcode: tcpapi.OpRead, Endpoint: server, Length: 64})
	if read.Result != tcpapi.ResultOK || string(p.serverPools.rx[tcpapi.Offset(read.Slot):][:read.Length]) != "datamore" {
		t.Fatalf("read %+v", read)
	}
}

func TestCloseAnswersHeldWaitFirst(t *testing.T) {
	p := newPair(t)
	_, _, server := p.connected(8084)
	seen := p.call(&p.server, tcpapi.Request{Opcode: tcpapi.OpReady, Endpoint: server})
	parked := p.park(&p.server, tcpapi.Request{Opcode: tcpapi.OpWaitReady, Endpoint: server, Mask: seen.Mask})
	closing := p.next()
	p.server.Handle(p.stack, &tcpapi.Request{Opcode: tcpapi.OpClose, Endpoint: server}, closing, p.now, p.out)
	if len(p.out.sent) != 2 || p.out.sent[0].correlation != parked || p.out.sent[0].reply.Result != tcpapi.ResultClosed ||
		p.out.sent[1].correlation != closing || p.out.sent[1].reply.Result != tcpapi.ResultOK {
		t.Fatalf("replies %+v", p.out.sent)
	}
	p.out.sent = nil
	if p.server.Waiting() != 0 {
		t.Fatal("wait still held")
	}
}

func TestNoWaitConnectRefusedOnLoopback(t *testing.T) {
	p := newPair(t)
	client := p.call(&p.client, tcpapi.Request{Opcode: tcpapi.OpOpen}).Endpoint
	reply := p.call(&p.client, tcpapi.Request{Opcode: tcpapi.OpConnect, Endpoint: client, Address: [4]byte{127, 0, 0, 1}, Port: 9, Flags: tcpapi.FlagNoWait})
	if reply.Result != tcpapi.ResultConnectionRefused {
		t.Fatalf("connect %+v", reply)
	}
	// The reply carried the error, so it is no longer pending.
	if reply := p.call(&p.client, tcpapi.Request{Opcode: tcpapi.OpError, Endpoint: client}); reply.Result != tcpapi.ResultOK || reply.Error != tcpapi.ResultOK {
		t.Fatalf("error %+v", reply)
	}
	ready := p.call(&p.client, tcpapi.Request{Opcode: tcpapi.OpReady, Endpoint: client})
	if ready.Mask != tcpapi.ReadyConnectFailed|tcpapi.ReadyWriteClosed|tcpapi.ReadyHangUp {
		t.Fatalf("failed connect %+v", ready)
	}
}

func TestNoWaitConnectInProgressThenWaitReady(t *testing.T) {
	r := newRemote(t)
	var session Session
	out := new(codec)
	session.Start(r.stack, 0, make([]byte, tcpapi.TXSlots*tcpapi.SlotSize), make([]byte, tcpapi.RXSlots*tcpapi.SlotSize), r.now)
	id := out.call(t, r.stack, &session, tcpapi.Request{Opcode: tcpapi.OpOpen}, 1, r.now, out).Endpoint
	reply := out.call(t, r.stack, &session, tcpapi.Request{Opcode: tcpapi.OpConnect, Endpoint: id, Address: peerIP, Port: 7, Flags: tcpapi.FlagNoWait}, 2, r.now, out)
	if reply.Result != tcpapi.ResultInProgress || reply.Address != peerIP || reply.Port != 7 || reply.Local != hostIP || reply.LocalPort == 0 {
		t.Fatalf("connect %+v", reply)
	}
	syn := r.sent()
	if len(syn) != 1 || syn[0].flags != flagSYN {
		t.Fatalf("SYN %+v", syn)
	}
	ready := out.call(t, r.stack, &session, tcpapi.Request{Opcode: tcpapi.OpReady, Endpoint: id}, 3, r.now, out)
	if ready.Mask != tcpapi.ReadyConnecting {
		t.Fatalf("connecting %+v", ready)
	}
	session.Handle(r.stack, &tcpapi.Request{Opcode: tcpapi.OpWaitReady, Endpoint: id, Mask: ready.Mask}, 4, r.now, out)
	session.Deliver(r.stack, r.now, out)
	if len(out.sent) != 0 {
		t.Fatalf("woke while connecting %+v", out.sent)
	}
	r.send(segment{sourcePort: 7, destinationPort: syn[0].sourcePort, sequence: 5000, acknowledgment: syn[0].sequence + 1,
		flags: flagSYN | flagACK, window: 65535})
	session.Deliver(r.stack, r.now, out)
	if wait := out.take(t, 4); wait.Result != tcpapi.ResultOK || wait.Mask != tcpapi.ReadyConnected|tcpapi.ReadyWritable || wait.Room == 0 {
		t.Fatalf("wait %+v", wait)
	}
	option := out.call(t, r.stack, &session, tcpapi.Request{Opcode: tcpapi.OpOption, Endpoint: id, Option: tcpapi.OptionSendMSS}, 5, r.now, out)
	if option.Result != tcpapi.ResultOK || option.Value != 536 { // the peer announced no MSS
		t.Fatalf("MSS %+v", option)
	}
	for selector, want := range map[uint8]uint32{tcpapi.OptionSendBuffer: network.TCPSendCapacity, tcpapi.OptionReceiveBuffer: network.TCPReceiveCapacity, tcpapi.OptionTTL: 64} {
		if option := out.call(t, r.stack, &session, tcpapi.Request{Opcode: tcpapi.OpOption, Endpoint: id, Option: selector}, 6, r.now, out); option.Value != want {
			t.Fatalf("option %d %+v", selector, option)
		}
	}

	// A reset while a wait is held: the wait sees the error and OpError
	// takes it once.
	session.Handle(r.stack, &tcpapi.Request{Opcode: tcpapi.OpWaitReady, Endpoint: id, Mask: tcpapi.ReadyConnected | tcpapi.ReadyWritable}, 7, r.now, out)
	r.send(segment{sourcePort: 7, destinationPort: syn[0].sourcePort, sequence: 5001, flags: flagRST})
	session.Deliver(r.stack, r.now, out)
	want := tcpapi.ReadyConnected | tcpapi.ReadyWriteClosed | tcpapi.ReadyHangUp | tcpapi.ReadyError
	if wait := out.take(t, 7); wait.Mask != want {
		t.Fatalf("wait for reset %+v", wait)
	}
	if reply := out.call(t, r.stack, &session, tcpapi.Request{Opcode: tcpapi.OpError, Endpoint: id}, 8, r.now, out); reply.Error != tcpapi.ResultReset {
		t.Fatalf("error %+v", reply)
	}
	if reply := out.call(t, r.stack, &session, tcpapi.Request{Opcode: tcpapi.OpError, Endpoint: id}, 9, r.now, out); reply.Error != tcpapi.ResultOK {
		t.Fatalf("error not cleared %+v", reply)
	}
}

func TestQuotaSessionHoldsSixtyFourEndpoints(t *testing.T) {
	stack := new(network.Stack)
	stack.Init(1234)
	now := uint64(1_000_000_000)
	var big, small Session
	if big.StartQuota(stack, 0, network.MaxTCPSessionQuota+1, nil, nil, now) {
		t.Fatal("quota past the maximum")
	}
	big.StartQuota(stack, 0, network.MaxTCPSessionQuota, make([]byte, tcpapi.TXSlots*tcpapi.SlotSize), make([]byte, tcpapi.RXSlots*tcpapi.SlotSize), now)
	small.StartQuota(stack, 1, 8, make([]byte, tcpapi.TXSlots*tcpapi.SlotSize), make([]byte, tcpapi.RXSlots*tcpapi.SlotSize), now)
	out := new(codec)
	correlation := uint64(0)
	open := func(s *Session) tcpapi.Reply {
		correlation++
		return out.call(t, stack, s, tcpapi.Request{Opcode: tcpapi.OpOpen}, correlation, now, out)
	}
	for count := range network.MaxTCPSessionQuota {
		if reply := open(&big); reply.Result != tcpapi.ResultOK {
			t.Fatalf("open %d %+v", count, reply)
		}
	}
	if reply := open(&big); reply.Result != tcpapi.ResultBusy {
		t.Fatalf("past the quota %+v", reply)
	}
	for count := range 8 {
		if reply := open(&small); reply.Result != tcpapi.ResultOK {
			t.Fatalf("ordinary open %d %+v", count, reply)
		}
	}
	if reply := open(&small); reply.Result != tcpapi.ResultBusy {
		t.Fatalf("past the ordinary quota %+v", reply)
	}
	big.End(stack, now)
	if reply := open(&small); reply.Result != tcpapi.ResultBusy {
		t.Fatalf("ending another session raised the quota %+v", reply)
	}
}
