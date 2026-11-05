package udpserve

import (
	"testing"

	"goos/userspace/network"
	"goos/userspace/udpapi"
)

// chainLength is longer than the passes one Deliver call may run, so the
// chain below cannot settle in one call.
const chainLength = 40

// chain answers like recorder, and on the WaitReady answer for endpoint k
// sends a datagram to endpoint k-1, which sits earlier in the session's
// table: each answer makes one more possible, behind the pass that just
// ran.
type chain struct {
	recorder
	stack     *network.Stack
	sender    network.EndpointID
	endpoints []network.EndpointID
	ports     []uint16
	now       uint64
}

func (c *chain) Reply(reply *udpapi.Reply, correlation uint64) bool {
	if !c.recorder.Reply(reply, correlation) {
		return false
	}
	if reply.Opcode != udpapi.OpWaitReady {
		return true
	}
	for k := 1; k < len(c.endpoints); k++ {
		if reply.Endpoint == uint32(c.endpoints[k]) {
			c.send(k - 1)
		}
	}
	return true
}

func (c *chain) send(k int) {
	c.t.Helper()
	if result := c.stack.UDPSendTo(1, c.sender, loopback, c.ports[k], []byte{byte(k)}, c.now); result != network.ResultOK {
		c.t.Fatalf("send to endpoint %d: %v", k, result)
	}
}

// TestDeliverReportsWorkPastItsBudget parks a WaitReady on each of
// chainLength endpoints and starts a chain that wakes them from the last
// table entry to the first. Deliver runs on its budget and reports more
// work; calling it again while it does, with no clock advance and no other
// event, answers every wait.
func TestDeliverReportsWorkPastItsBudget(t *testing.T) {
	stack := newStack()
	now := uint64(1_000_000_000)
	var session Session
	if !session.StartQuota(stack, 0, network.MaxUDPSessionQuota, make([]byte, udpapi.TXSlots*udpapi.SlotSize), make([]byte, udpapi.RXSlots*udpapi.SlotSize)) {
		t.Fatal("quota refused")
	}
	out := &chain{recorder: recorder{t: t}, stack: stack, now: now}
	sender, result := stack.UDPOpen(1)
	if result != network.ResultOK {
		t.Fatalf("sender open %v", result)
	}
	if _, _, result := stack.UDPBind(1, sender, network.IPv4(loopback), 9000); result != network.ResultOK {
		t.Fatalf("sender bind %v", result)
	}
	out.sender = sender
	correlation := uint64(0)
	call := func(request udpapi.Request) udpapi.Reply {
		t.Helper()
		correlation++
		if fault := session.Handle(stack, &request, correlation, now, out); fault != FaultNone {
			t.Fatalf("opcode %d fault %q", request.Opcode, fault)
		}
		replies := out.take()
		if len(replies) != 1 || replies[0].correlation != correlation {
			t.Fatalf("opcode %d replies %+v", request.Opcode, replies)
		}
		return replies[0].reply
	}
	waits := map[uint64]int{}
	for k := range chainLength {
		id := network.EndpointID(call(udpapi.Request{Opcode: udpapi.OpOpen}).Endpoint)
		port := uint16(9100 + k)
		if _, _, result := stack.UDPBind(0, id, network.IPv4(loopback), port); result != network.ResultOK {
			t.Fatalf("bind %d %v", k, result)
		}
		out.endpoints, out.ports = append(out.endpoints, id), append(out.ports, port)
		seen := call(udpapi.Request{Opcode: udpapi.OpReady, Endpoint: uint32(id)})
		correlation++
		request := udpapi.Request{Opcode: udpapi.OpWaitReady, Endpoint: uint32(id), Mask: seen.Mask}
		if fault := session.Handle(stack, &request, correlation, now, out); fault != FaultNone || len(out.take()) != 0 {
			t.Fatalf("wait %d answered at once or failed: %q", k, fault)
		}
		waits[correlation] = k
	}
	if endpoint, op, found := session.Runnable(stack); found {
		t.Fatalf("runnable before the chain starts: endpoint %d %s", endpoint, op)
	}
	out.send(chainLength - 1)
	if endpoint, op, found := session.Runnable(stack); !found || endpoint != uint32(out.endpoints[chainLength-1]) || op != "wait-ready" {
		t.Fatalf("Runnable %d %q %v", endpoint, op, found)
	}

	calls := 0
	for {
		fault, more := session.Deliver(stack, out)
		calls++
		if fault != FaultNone {
			t.Fatalf("deliver fault %q", fault)
		}
		if calls == 1 && !more {
			t.Fatal("first Deliver call settled a chain longer than its budget")
		}
		if !more {
			break
		}
		if calls > chainLength {
			t.Fatal("Deliver keeps reporting more work")
		}
	}
	answered := map[int]bool{}
	for _, reply := range out.take() {
		k, known := waits[reply.correlation]
		if !known || reply.reply.Opcode != udpapi.OpWaitReady || reply.reply.Mask&udpapi.ReadyReadable == 0 {
			t.Fatalf("unexpected reply %+v", reply)
		}
		answered[k] = true
	}
	if len(answered) != chainLength || session.Waiting() != 0 {
		t.Fatalf("answered %d of %d waits, %d still parked", len(answered), chainLength, session.Waiting())
	}
	if endpoint, op, found := session.Runnable(stack); found {
		t.Fatalf("runnable after settling: endpoint %d %s", endpoint, op)
	}
}
