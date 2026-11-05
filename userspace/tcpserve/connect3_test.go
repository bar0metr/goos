package tcpserve

import (
	"testing"

	"goos/userspace/tcpapi"
)

// TestVersion3ConnectAcceptCloseLoop runs netbench3's lifecycle loop on one
// GTCP 3 session holding both ends: blocking Connect and Accept, then both
// closes, thirty times.
func TestVersion3ConnectAcceptCloseLoop(t *testing.T) {
	p := newPair3(t)
	s := &p.client
	listener := p.call(s, tcpapi.Request{Opcode: tcpapi.OpOpen}).Endpoint
	port := p.call(s, tcpapi.Request{Opcode: tcpapi.OpBind, Endpoint: listener, Address: [4]byte{127, 0, 0, 1}}).Port
	p.call(s, tcpapi.Request{Opcode: tcpapi.OpListen, Endpoint: listener, Backlog: 8})
	// The pair the stream and request-reply measurements use first.
	client := p.call(s, tcpapi.Request{Opcode: tcpapi.OpOpen}).Endpoint
	if r := p.call(s, tcpapi.Request{Opcode: tcpapi.OpConnect, Endpoint: client, Address: [4]byte{127, 0, 0, 1}, Port: port}); r.Result != tcpapi.ResultOK {
		t.Fatalf("pair connect %+v", r)
	}
	server := p.call(s, tcpapi.Request{Opcode: tcpapi.OpAccept, Endpoint: listener}).Endpoint
	offset := uint32(0)
	for round := 0; round < 20; round++ {
		read := p.next()
		if fault := s.Handle(p.stack, &tcpapi.Request{Opcode: tcpapi.OpRead, Endpoint: server, Length: 16384}, read, p.now, p.out); fault != FaultNone {
			t.Fatal(fault)
		}
		length := 1 + round*700
		p.fill(0, offset, length)
		w := p.call(s, tcpapi.Request{Opcode: tcpapi.OpWrite, Endpoint: client, Slot: 0, Length: uint16(length), Offset: offset})
		if w.Result != tcpapi.ResultOK {
			t.Fatalf("write %+v", w)
		}
		offset += uint32(w.Length)
		p.deliver()
		got := p.out.take(t, read)
		p.release(s, got.Slot)
	}
	p.call(s, tcpapi.Request{Opcode: tcpapi.OpClose, Endpoint: client})
	p.call(s, tcpapi.Request{Opcode: tcpapi.OpClose, Endpoint: server})
	for round := 0; round < 30; round++ {
		open := p.call(s, tcpapi.Request{Opcode: tcpapi.OpOpen})
		if open.Result != tcpapi.ResultOK {
			t.Fatalf("round %d open %+v", round, open)
		}
		connect := p.next()
		if fault := s.Handle(p.stack, &tcpapi.Request{Opcode: tcpapi.OpConnect, Endpoint: open.Endpoint, Address: [4]byte{127, 0, 0, 1}, Port: port}, connect, p.now, p.out); fault != FaultNone {
			t.Fatal(fault)
		}
		p.deliver()
		if reply := p.out.take(t, connect); reply.Result != tcpapi.ResultOK {
			t.Fatalf("round %d connect %+v", round, reply)
		}
		accept := p.next()
		if fault := s.Handle(p.stack, &tcpapi.Request{Opcode: tcpapi.OpAccept, Endpoint: listener}, accept, p.now, p.out); fault != FaultNone {
			t.Fatal(fault)
		}
		p.deliver()
		child := p.out.take(t, accept)
		if child.Result != tcpapi.ResultOK {
			t.Fatalf("round %d accept %+v", round, child)
		}
		for _, endpoint := range []uint32{open.Endpoint, child.Endpoint} {
			if reply := p.call(s, tcpapi.Request{Opcode: tcpapi.OpClose, Endpoint: endpoint}); reply.Result != tcpapi.ResultOK {
				t.Fatalf("round %d close %+v", round, reply)
			}
		}
		p.now += 1_000_000
	}
}
