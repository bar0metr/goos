package tcpserve

import (
	"goos/kernel/ipc"
	"goos/userspace/network"
	"goos/userspace/tcpapi"
	"testing"
)

type sentReply struct {
	reply       tcpapi.Reply
	correlation uint64
}
type replies struct{ sent []sentReply }

func (r *replies) Reply(reply *tcpapi.Reply, correlation uint64) bool {
	r.sent = append(r.sent, sentReply{reply: *reply, correlation: correlation})
	return true
}
func (r *replies) take(t *testing.T, correlation uint64) tcpapi.Reply {
	t.Helper()
	for index, sent := range r.sent {
		if sent.correlation == correlation {
			r.sent = append(r.sent[:index], r.sent[index+1:]...)
			return sent.reply
		}
	}
	t.Fatalf("no reply for %d", correlation)
	return tcpapi.Reply{}
}

// codec sends every reply through the GTCP1 encoder and decoder, and refuses
// a reply the encoder rejects, as netd does.
type codec struct{ replies }

func (c *codec) Reply(reply *tcpapi.Reply, correlation uint64) bool {
	var message ipc.Message
	if !tcpapi.EncodeReply(reply, correlation, &message) {
		return false
	}
	var decoded tcpapi.Reply
	got, ok := tcpapi.DecodeReply(&message, &decoded)
	if !ok || got != correlation {
		return false
	}
	return c.replies.Reply(&decoded, got)
}

func (r *replies) call(t *testing.T, stack *network.Stack, s *Session, request tcpapi.Request, correlation, now uint64, out Replier) tcpapi.Reply {
	t.Helper()
	if fault := s.Handle(stack, &request, correlation, now, out); fault != FaultNone {
		t.Fatalf("opcode %d fault %q", request.Opcode, fault)
	}
	return r.take(t, correlation)
}

func TestGTCP1OpenExhaustionRepliesBusy(t *testing.T) {
	stack := new(network.Stack)
	stack.Init(1234)
	now := uint64(1_000_000_000)
	var session Session
	session.Start(stack, 0, make([]byte, tcpapi.TXSlots*tcpapi.SlotSize), make([]byte, tcpapi.RXSlots*tcpapi.SlotSize), now)
	out := new(codec)
	correlation := uint64(0)
	var opened []uint32
	for range network.MaxTCPEndpointsPerSession {
		correlation++
		reply := out.call(t, stack, &session, tcpapi.Request{Opcode: tcpapi.OpOpen}, correlation, now, out)
		if reply.Result != tcpapi.ResultOK || reply.Endpoint == 0 {
			t.Fatalf("open %d: %+v", len(opened), reply)
		}
		opened = append(opened, reply.Endpoint)
	}
	correlation++
	reply := out.call(t, stack, &session, tcpapi.Request{Opcode: tcpapi.OpOpen}, correlation, now, out)
	if reply.Result != tcpapi.ResultBusy || reply.Endpoint != 0 {
		t.Fatalf("exhausted open %+v", reply)
	}
	correlation++
	if reply = out.call(t, stack, &session, tcpapi.Request{Opcode: tcpapi.OpClose, Endpoint: opened[0]}, correlation, now, out); reply.Result != tcpapi.ResultOK {
		t.Fatalf("close %+v", reply)
	}
	correlation++
	if reply = out.call(t, stack, &session, tcpapi.Request{Opcode: tcpapi.OpOpen}, correlation, now, out); reply.Result != tcpapi.ResultOK || reply.Endpoint == 0 {
		t.Fatalf("open after close %+v", reply)
	}
}

func TestGTCP1OpenStackExhaustionRepliesBusy(t *testing.T) {
	stack := new(network.Stack)
	stack.Init(1234)
	now := uint64(1_000_000_000)
	sessions := make([]Session, network.MaxTCPEndpoints/network.MaxTCPEndpointsPerSession+1)
	out := new(codec)
	for index := range sessions {
		sessions[index].Start(stack, index, make([]byte, tcpapi.TXSlots*tcpapi.SlotSize), make([]byte, tcpapi.RXSlots*tcpapi.SlotSize), now)
	}
	correlation := uint64(0)
	for index := range network.MaxTCPEndpoints {
		correlation++
		session := &sessions[index/network.MaxTCPEndpointsPerSession]
		if reply := out.call(t, stack, session, tcpapi.Request{Opcode: tcpapi.OpOpen}, correlation, now, out); reply.Result != tcpapi.ResultOK {
			t.Fatalf("open %d %+v", index, reply)
		}
	}
	correlation++
	last := &sessions[len(sessions)-1]
	if reply := out.call(t, stack, last, tcpapi.Request{Opcode: tcpapi.OpOpen}, correlation, now, out); reply.Result != tcpapi.ResultBusy || reply.Endpoint != 0 {
		t.Fatalf("open with a full table %+v", reply)
	}
}

func TestGTCP1PassiveCloseDoesNotLeakEndpoints(t *testing.T) {
	stack := new(network.Stack)
	stack.Init(1234)
	now := uint64(1_000_000_000)
	var server, client Session
	server.Start(stack, 0, make([]byte, tcpapi.TXSlots*tcpapi.SlotSize), make([]byte, tcpapi.RXSlots*tcpapi.SlotSize), now)
	client.Start(stack, 1, make([]byte, tcpapi.TXSlots*tcpapi.SlotSize), make([]byte, tcpapi.RXSlots*tcpapi.SlotSize), now)
	out := new(codec)
	correlation := uint64(1)
	listener := out.call(t, stack, &server, tcpapi.Request{Opcode: tcpapi.OpOpen}, correlation, now, out).Endpoint
	correlation++
	out.call(t, stack, &server, tcpapi.Request{Opcode: tcpapi.OpBind, Endpoint: listener, Address: [4]byte{127, 0, 0, 1}, Port: 8080}, correlation, now, out)
	correlation++
	out.call(t, stack, &server, tcpapi.Request{Opcode: tcpapi.OpListen, Endpoint: listener, Backlog: 1}, correlation, now, out)
	for round := range 3 * network.MaxTCPEndpointsPerSession {
		correlation++
		clientID := out.call(t, stack, &client, tcpapi.Request{Opcode: tcpapi.OpOpen}, correlation, now, out).Endpoint
		correlation++
		if reply := out.call(t, stack, &client, tcpapi.Request{Opcode: tcpapi.OpConnect, Endpoint: clientID, Address: [4]byte{127, 0, 0, 1}, Port: 8080}, correlation, now, out); reply.Result != tcpapi.ResultOK {
			t.Fatalf("round %d connect %+v", round, reply)
		}
		correlation++
		accepted := out.call(t, stack, &server, tcpapi.Request{Opcode: tcpapi.OpAccept, Endpoint: listener}, correlation, now, out)
		if accepted.Result != tcpapi.ResultOK {
			t.Fatalf("round %d accept %+v", round, accepted)
		}
		correlation++
		out.call(t, stack, &client, tcpapi.Request{Opcode: tcpapi.OpShutdownWrite, Endpoint: clientID}, correlation, now, out)
		correlation++
		if reply := out.call(t, stack, &server, tcpapi.Request{Opcode: tcpapi.OpRead, Endpoint: accepted.Endpoint, Length: 32}, correlation, now, out); reply.Result != tcpapi.ResultEOF {
			t.Fatalf("round %d EOF %+v", round, reply)
		}
		correlation++
		out.call(t, stack, &server, tcpapi.Request{Opcode: tcpapi.OpClose, Endpoint: accepted.Endpoint}, correlation, now, out)
		correlation++
		out.call(t, stack, &client, tcpapi.Request{Opcode: tcpapi.OpClose, Endpoint: clientID}, correlation, now, out)
		if stats := stack.TCPStats(); stats.Endpoints != 1 {
			t.Fatalf("round %d endpoints %d states %v", round, stats.Endpoints, stats.States)
		}
	}
	if len(out.sent) != 0 {
		t.Fatalf("unexpected replies %+v", out.sent)
	}
}

func TestGTCP1LoopbackDeferredAcceptReadAndEOF(t *testing.T) {
	stack := new(network.Stack)
	stack.Init(1234)
	now := uint64(1_000_000_000)
	serverTX, serverRX := make([]byte, tcpapi.TXSlots*tcpapi.SlotSize), make([]byte, tcpapi.RXSlots*tcpapi.SlotSize)
	clientTX, clientRX := make([]byte, tcpapi.TXSlots*tcpapi.SlotSize), make([]byte, tcpapi.RXSlots*tcpapi.SlotSize)
	var server, client Session
	server.Start(stack, 0, serverTX, serverRX, now)
	client.Start(stack, 1, clientTX, clientRX, now)
	var out replies

	server.Handle(stack, &tcpapi.Request{Opcode: tcpapi.OpOpen}, 1, now, &out)
	listener := out.take(t, 1).Endpoint
	server.Handle(stack, &tcpapi.Request{Opcode: tcpapi.OpBind, Endpoint: listener, Address: [4]byte{127, 0, 0, 1}, Port: 8080}, 2, now, &out)
	if got := out.take(t, 2).Result; got != tcpapi.ResultOK {
		t.Fatal(got)
	}
	server.Handle(stack, &tcpapi.Request{Opcode: tcpapi.OpListen, Endpoint: listener, Backlog: 2}, 3, now, &out)
	if got := out.take(t, 3).Result; got != tcpapi.ResultOK {
		t.Fatal(got)
	}
	server.Handle(stack, &tcpapi.Request{Opcode: tcpapi.OpAccept, Endpoint: listener}, 4, now, &out)
	if len(out.sent) != 0 {
		t.Fatal("accept did not defer")
	}

	client.Handle(stack, &tcpapi.Request{Opcode: tcpapi.OpOpen}, 5, now, &out)
	clientID := out.take(t, 5).Endpoint
	client.Handle(stack, &tcpapi.Request{Opcode: tcpapi.OpConnect, Endpoint: clientID, Address: [4]byte{127, 0, 0, 1}, Port: 8080}, 6, now, &out)
	if got := out.take(t, 6).Result; got != tcpapi.ResultOK {
		t.Fatal(got)
	}
	server.Deliver(stack, now, &out)
	accepted := out.take(t, 4)
	if accepted.Result != tcpapi.ResultOK || accepted.Endpoint == listener {
		t.Fatalf("accept %+v", accepted)
	}

	server.Handle(stack, &tcpapi.Request{Opcode: tcpapi.OpRead, Endpoint: accepted.Endpoint, Length: 32}, 7, now, &out)
	if len(out.sent) != 0 {
		t.Fatal("read did not defer")
	}
	copy(clientTX, []byte("hello"))
	client.Handle(stack, &tcpapi.Request{Opcode: tcpapi.OpWrite, Endpoint: clientID, Length: 5}, 8, now, &out)
	if got := out.take(t, 8); got.Result != tcpapi.ResultOK || got.Length != 5 {
		t.Fatalf("write %+v", got)
	}
	server.Deliver(stack, now, &out)
	read := out.take(t, 7)
	if read.Result != tcpapi.ResultOK || string(serverRX[tcpapi.Offset(read.Slot):tcpapi.Offset(read.Slot)+uint64(read.Length)]) != "hello" {
		t.Fatalf("read %+v", read)
	}
	server.Handle(stack, &tcpapi.Request{Opcode: tcpapi.OpRelease, Slot: read.Slot}, 0, now, &out)
	client.Handle(stack, &tcpapi.Request{Opcode: tcpapi.OpShutdownWrite, Endpoint: clientID}, 9, now, &out)
	out.take(t, 9)
	server.Handle(stack, &tcpapi.Request{Opcode: tcpapi.OpRead, Endpoint: accepted.Endpoint, Length: 32}, 10, now, &out)
	if got := out.take(t, 10).Result; got != tcpapi.ResultEOF {
		t.Fatalf("EOF %d", got)
	}
}
