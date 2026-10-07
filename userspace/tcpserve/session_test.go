package tcpserve

import (
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
