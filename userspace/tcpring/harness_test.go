package tcpring

import (
	"runtime"
	"sync"
	"testing"

	"goos/kernel/ipc"
	"goos/userspace/network"
	"goos/userspace/tcpapi"
	"goos/userspace/tcpserve"
)

// netd is an in-process netd: one stack, GTCP 4 sessions and the event
// loop's step. Clients may run on their own goroutines; the lock stands for
// netd's single thread.
type netd struct {
	t        testing.TB
	mu       sync.Mutex
	stack    *network.Stack
	now      uint64
	sessions []*wire
	// idleSteps counts steps that found nothing to do, after which netd
	// armed every session's doorbell and let time pass.
	idleSteps int
}

// wire is one session: the server half and the channel between it and its
// client. It is the client's Transport and the session's Notifier.
type wire struct {
	n      *netd
	server tcpserve.Session4
	tx, rx []byte
	inbox  []ipc.Message
	// The channel messages, as netd's counters see them.
	requests, replies, doorbellsIn, doorbellsOut int
	faulted                                      tcpserve.Fault
}

func newNetd(t testing.TB) *netd {
	n := &netd{t: t, stack: new(network.Stack), now: 1_000_000_000}
	n.stack.Init(77)
	return n
}

// session starts a GTCP 4 session and its client.
func (n *netd) session() (*wire, *Client) {
	n.t.Helper()
	w := &wire{n: n, tx: make([]byte, tcpapi.Layout4.TXPool()), rx: make([]byte, tcpapi.Layout4.RXPool())}
	n.mu.Lock()
	ok := w.server.Start(n.stack, len(n.sessions), 0, w.tx, w.rx, n.now)
	n.sessions = append(n.sessions, w)
	n.mu.Unlock()
	if !ok {
		n.t.Fatal("session start")
	}
	c, ok := New(w, w.tx, w.rx)
	if !ok {
		n.t.Fatal("client start")
	}
	return w, c
}

// step is one pass of netd's loop. A pass with no work arms every session
// and advances the clock, as netd sleeping until its next timer would.
func (n *netd) step() {
	worked := false
	for _, w := range n.sessions {
		if w.faulted != tcpserve.FaultNone {
			continue
		}
		fault, more := w.server.Deliver(n.stack, n.now, w)
		if fault != tcpserve.FaultNone {
			w.faulted = fault
			continue
		}
		worked = worked || more || w.server.Progressed()
	}
	if worked {
		return
	}
	for _, w := range n.sessions {
		if w.faulted == tcpserve.FaultNone && !w.server.Idle() {
			return
		}
	}
	n.idleSteps++
	n.now += 1_000_000
	n.stack.Advance(n.now)
}

func (w *wire) Reply(reply *tcpapi.Reply, correlation uint64) bool {
	var message ipc.Message
	if !w.server.Encode(reply, correlation, &message) {
		w.n.t.Errorf("reply rejected by the encoder: %+v", reply)
		return false
	}
	w.inbox = append(w.inbox, message)
	w.replies++
	return true
}

func (w *wire) Doorbell() bool {
	var message ipc.Message
	if !tcpapi.EncodeDoorbell4(&message) {
		return false
	}
	w.inbox = append(w.inbox, message)
	w.doorbellsOut++
	return true
}

func (w *wire) Send(message *ipc.Message) bool {
	w.n.mu.Lock()
	defer w.n.mu.Unlock()
	if w.faulted != tcpserve.FaultNone {
		return false
	}
	var request tcpapi.Request
	correlation, ok := w.server.Decode(message, &request)
	if !ok {
		w.faulted = "request invalid"
		return false
	}
	if request.Opcode == tcpapi.OpDoorbell {
		w.doorbellsIn++
	} else {
		w.requests++
	}
	if fault := w.server.Handle(w.n.stack, &request, correlation, w.n.now, w); fault != tcpserve.FaultNone {
		w.faulted = fault
		return false
	}
	return true
}

// stepLimit bounds the netd steps a blocked client waits through.
const stepLimit = 200_000

func (w *wire) Receive(wait bool, out *ipc.Message) (bool, bool) {
	for round := 0; ; round++ {
		w.n.mu.Lock()
		if w.faulted != tcpserve.FaultNone {
			w.n.mu.Unlock()
			return false, false
		}
		if len(w.inbox) != 0 {
			*out = w.inbox[0]
			w.inbox = w.inbox[1:]
			w.n.mu.Unlock()
			return true, true
		}
		if !wait {
			w.n.mu.Unlock()
			return false, true
		}
		if round == stepLimit {
			w.n.mu.Unlock()
			w.n.t.Errorf("client waited %d netd steps", stepLimit)
			w.n.dump()
			return false, false
		}
		w.n.step()
		w.n.mu.Unlock()
		runtime.Gosched()
	}
}

func (w *wire) Yield() {
	w.n.mu.Lock()
	w.n.step()
	w.n.mu.Unlock()
	runtime.Gosched()
}

// messages is the channel traffic of the session so far.
func (w *wire) messages() int {
	w.n.mu.Lock()
	defer w.n.mu.Unlock()
	return w.requests + w.replies + w.doorbellsIn + w.doorbellsOut
}

func (w *wire) fault() tcpserve.Fault {
	w.n.mu.Lock()
	defer w.n.mu.Unlock()
	return w.faulted
}

func pattern(offset, length int) []byte {
	out := make([]byte, length)
	for index := range out {
		out[index] = byte((offset + index) % 251)
	}
	return out
}

var loopback = [4]byte{127, 0, 0, 1}

// dump logs every session's table and every stack endpoint, for a stuck
// test's diagnosis.
func (n *netd) dump() {
	n.t.Logf("now %d idle steps %d", n.now, n.idleSteps)
	for owner, w := range n.sessions {
		for index := range tcpapi.Endpoints4 {
			if snap := w.server.Snapshot(index); snap.ID != 0 {
				ready, _ := n.stack.TCPReadiness(owner, network.TCPEndpointID(snap.ID))
				n.t.Logf("session %d endpoint %d %+v ready %+v txheld %b rxlent %b", owner, index, snap, ready, w.server.TXHeld(), w.server.RXLent())
			}
		}
	}
	for index := 0; index < n.stack.TCPEndpointCount(); index++ {
		if e, live := n.stack.TCPEndpointAt(index); live {
			n.t.Logf("tcp %+v", e)
		}
	}
}
