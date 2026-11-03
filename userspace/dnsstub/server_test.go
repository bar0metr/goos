package dnsstub

import (
	"testing"

	"goos/userspace/dns"
	"goos/userspace/dnswire"
	"goos/userspace/udpapi"
	"goos/userspace/upstream"
)

// recorder keeps the last correlation sent for each GUDP1 opcode.
type recorder struct {
	last [udpapi.OpClose + 1]uint64
	slot uint8
}

func (r *recorder) Send(request *udpapi.Request, correlation uint64) bool {
	r.last[request.Opcode] = correlation
	if request.Opcode == udpapi.OpSendTo {
		r.slot = request.Slot
	}
	return true
}

// A query answered at once costs no allocation: the datagram, the parse,
// the reply and its SendTo all use the server's fixed storage.
func TestDatagramPathDoesNotAllocate(t *testing.T) {
	var resolver dns.Resolver
	resolver.Init(1, dns.DefaultPolicy())
	tx, rx := udpPool()
	var client upstream.Client
	client.Bind(tx, rx)
	server := new(Server)
	server.Init(base, nil)
	server.BindUDP(tx, rx, &client, 0)
	out := &recorder{}
	if !server.FlushUDP(0, out) || !Owns(out.last[udpapi.OpOpen]) {
		t.Fatal("no Open")
	}
	server.AnswerUDP(&resolver, &udpapi.Reply{Opcode: udpapi.OpOpen, Endpoint: 5}, out.last[udpapi.OpOpen], 0, out)
	server.AnswerUDP(&resolver, &udpapi.Reply{Opcode: udpapi.OpBind, Endpoint: 5, Address: Address, Port: Port}, out.last[udpapi.OpBind], 0, out)
	server.FlushUDP(0, out)
	message := query(0x77, "localhost", dnswire.TypeA, true, 0)
	const slot = 3
	round := func() {
		copy(rx[udpapi.Offset(slot):], message)
		received := udpapi.Reply{Opcode: udpapi.OpRecvFrom, Endpoint: 5, Slot: slot, Address: Address, Port: 40000,
			Length: uint16(len(message)), Local: Address}
		if !server.AnswerUDP(&resolver, &received, out.last[udpapi.OpRecvFrom], 0, out) || !server.FlushUDP(0, out) {
			t.Fatal("datagram refused")
		}
		sent := udpapi.Reply{Opcode: udpapi.OpSendTo, Endpoint: 5, Slot: out.slot}
		if !server.AnswerUDP(&resolver, &sent, out.last[udpapi.OpSendTo], 0, out) || !server.FlushUDP(0, out) {
			t.Fatal("send reply refused")
		}
	}
	round()
	if stats := server.Stats(); stats.Replies != 1 {
		t.Fatalf("stats %+v", stats)
	}
	if allocs := testing.AllocsPerRun(100, round); allocs != 0 {
		t.Fatalf("%.1f allocations per query", allocs)
	}
	reply := parse(t, tx[udpapi.Offset(out.slot):udpapi.Offset(out.slot)+uint64(server.requests[0].length)])
	if reply.id != 0x77 || len(reply.addresses) != 1 || reply.addresses[0] != [4]byte{127, 0, 0, 1} {
		t.Fatalf("%+v", reply)
	}
}

func TestOwnsAndCorrelations(t *testing.T) {
	var server Server
	server.correlation = ^ownBit
	for range 3 {
		if correlation := server.next(); !Owns(correlation) || correlation == ownBit {
			t.Fatalf("correlation %#x", correlation)
		}
	}
	if Owns(1) || Owns(ownBit-1) {
		t.Fatal("a client correlation counted as the stub's")
	}
}
