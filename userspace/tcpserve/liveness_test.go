package tcpserve

import (
	"testing"

	"goos/userspace/network"
	"goos/userspace/tcpapi"
)

// chainLength is longer than the passes one Deliver call may run, so the
// chain below cannot settle in one call.
const chainLength = 40

// chain answers like codec, and on the WaitReady answer for server child
// k writes a byte from its peer to child k-1, which sits earlier in the
// server's table: each answer makes one more possible, behind the pass
// that just ran.
type chain struct {
	codec
	t        *testing.T
	stack    *network.Stack
	children []uint32
	peers    []network.TCPEndpointID
	now      uint64
}

func (c *chain) Reply(reply *tcpapi.Reply, correlation uint64) bool {
	if !c.codec.Reply(reply, correlation) {
		return false
	}
	if reply.Opcode != tcpapi.OpWaitReady {
		return true
	}
	for k := 1; k < len(c.children); k++ {
		if reply.Endpoint == c.children[k] {
			c.write(k - 1)
		}
	}
	return true
}

func (c *chain) write(k int) {
	c.t.Helper()
	if written, result := c.stack.TCPWrite(1, c.peers[k], []byte{byte(k)}, c.now); written != 1 || result != network.ResultOK {
		c.t.Fatalf("write to child %d: %d %v", k, written, result)
	}
}

// TestDeliverReportsWorkPastItsBudget parks a WaitReady on each of
// chainLength accepted children and starts a chain that wakes them from
// the last table entry to the first. Deliver runs on its budget and
// reports more work; calling it again while it does, with no clock advance
// and no other event, answers every wait.
func TestDeliverReportsWorkPastItsBudget(t *testing.T) {
	p := newPair(t)
	out := &chain{t: t, stack: p.stack, now: p.now}
	p.out = &out.codec
	p.server.StartQuota(p.stack, 0, network.MaxTCPSessionQuota, p.serverPools.tx, p.serverPools.rx, p.now)
	p.client.StartQuota(p.stack, 1, network.MaxTCPSessionQuota, p.clientPools.tx, p.clientPools.rx, p.now)
	listener := p.call(&p.server, tcpapi.Request{Opcode: tcpapi.OpOpen}).Endpoint
	if reply := p.call(&p.server, tcpapi.Request{Opcode: tcpapi.OpBind, Endpoint: listener, Address: [4]byte{127, 0, 0, 1}, Port: 8090}); reply.Result != tcpapi.ResultOK {
		t.Fatalf("bind %+v", reply)
	}
	p.call(&p.server, tcpapi.Request{Opcode: tcpapi.OpListen, Endpoint: listener, Backlog: 4})
	waits := map[uint64]int{}
	for k := range chainLength {
		peer := p.call(&p.client, tcpapi.Request{Opcode: tcpapi.OpOpen}).Endpoint
		if reply := p.call(&p.client, tcpapi.Request{Opcode: tcpapi.OpConnect, Endpoint: peer, Address: [4]byte{127, 0, 0, 1}, Port: 8090, Flags: tcpapi.FlagNoWait}); reply.Result != tcpapi.ResultOK {
			t.Fatalf("connect %d %+v", k, reply)
		}
		child := p.call(&p.server, tcpapi.Request{Opcode: tcpapi.OpAccept, Endpoint: listener, Flags: tcpapi.FlagNoWait})
		if child.Result != tcpapi.ResultOK {
			t.Fatalf("accept %d %+v", k, child)
		}
		out.children, out.peers = append(out.children, child.Endpoint), append(out.peers, network.TCPEndpointID(peer))
		seen := p.call(&p.server, tcpapi.Request{Opcode: tcpapi.OpReady, Endpoint: child.Endpoint})
		waits[p.park(&p.server, tcpapi.Request{Opcode: tcpapi.OpWaitReady, Endpoint: child.Endpoint, Mask: seen.Mask})] = k
	}
	if endpoint, op, found := p.server.Runnable(p.stack); found {
		t.Fatalf("runnable before the chain starts: endpoint %d %s", endpoint, op)
	}
	out.write(chainLength - 1)
	if endpoint, op, found := p.server.Runnable(p.stack); !found || endpoint != out.children[chainLength-1] || op != "wait-ready" {
		t.Fatalf("Runnable %d %q %v", endpoint, op, found)
	}

	calls := 0
	for {
		fault, more := p.server.Deliver(p.stack, p.now, out)
		calls++
		if fault != FaultNone {
			t.Fatalf("deliver fault %q", fault)
		}
		if calls == 1 && !more {
			t.Fatal("first Deliver call settled a chain longer than its budget")
		}
		if !p.server.Progressed() {
			t.Fatalf("Deliver call %d answered waits but reports no progress", calls)
		}
		if !more {
			break
		}
		if calls > chainLength {
			t.Fatal("Deliver keeps reporting more work")
		}
	}
	answered := map[int]bool{}
	for _, sent := range out.sent {
		k, known := waits[sent.correlation]
		if !known || sent.reply.Opcode != tcpapi.OpWaitReady || sent.reply.Mask&tcpapi.ReadyReadable == 0 {
			t.Fatalf("unexpected reply %+v", sent)
		}
		answered[k] = true
	}
	if len(answered) != chainLength || p.server.Waiting() != 0 {
		t.Fatalf("answered %d of %d waits, %d still parked", len(answered), chainLength, p.server.Waiting())
	}
	if endpoint, op, found := p.server.Runnable(p.stack); found {
		t.Fatalf("runnable after settling: endpoint %d %s", endpoint, op)
	}
	if fault, more := p.server.Deliver(p.stack, p.now, out); fault != FaultNone || more || p.server.Progressed() {
		t.Fatalf("settled Deliver: fault %q, more %v, progressed %v", fault, more, p.server.Progressed())
	}
}
