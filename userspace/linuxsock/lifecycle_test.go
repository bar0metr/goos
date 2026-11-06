package linuxsock

import (
	"testing"

	"goos/userspace/linuxsockapi"
)

func TestWarmPoolAnswersOpenWithoutNetd(t *testing.T) {
	h := newHarness(t)
	h.svc.SetWarm(2)
	for range 2 {
		h.complete(h.take(NativeOpen), NativeCompletion{Endpoint: 700 + uint32(len(h.backend.log))})
	}
	// UDP fills too.
	for h.has(NativeOpen) {
		h.complete(h.take(NativeOpen), NativeCompletion{Endpoint: 800})
	}
	f := h.family(1, 7)
	reply := h.call(f, linuxsockapi.Request{Opcode: opOpen, Length: linuxsockapi.KindStream})
	if reply.Result != linuxsockapi.ResultOK || h.endpoint(reply.Socket) < 700 {
		t.Fatalf("open from the pool %+v", reply)
	}
	if !h.waiting(h.endpoint(reply.Socket)) {
		t.Fatal("no wait on a pooled endpoint")
	}
	// The pool refills behind the open.
	refill := h.take(NativeOpen)
	if refill.proto != TCP {
		t.Fatal("refill on the wrong session")
	}
	// netd refuses for the quota: the pool stops asking until a close.
	h.complete(refill, NativeCompletion{Result: NativeBusy})
	h.call(f, linuxsockapi.Request{Opcode: opOpen, Length: linuxsockapi.KindStream})
	if h.has(NativeOpen) {
		t.Fatal("refilled against a full quota")
	}
	h.call(f, linuxsockapi.Request{Opcode: opClose, Socket: reply.Socket})
	if !h.has(NativeOpen) {
		t.Fatal("no refill after a close")
	}
	// A lost session takes the pool with it.
	h.svc.BackendDown(TCP, h.generation[TCP])
	if h.svc.warm[TCP].count != 0 {
		t.Fatal("pool kept a dead session's endpoints")
	}
}

func TestAcceptAheadAnswersAcceptAtOnce(t *testing.T) {
	h := newHarness(t)
	f := h.family(1, 7)
	id := listener(h, f)
	endpoint := h.endpoint(id)
	h.wake(endpoint, MaskAcceptable|MaskListening, 0)
	ahead := h.take(NativeAccept)
	if ahead.request.Flags&NativeNoWait == 0 {
		t.Fatal("accept ahead must not wait in netd")
	}
	h.complete(ahead, NativeCompletion{Endpoint: 950, Address: peerAddress, Port: 6000, Local: localAddress, LocalPort: 80})
	// The child reads nothing before the kernel has it.
	for _, entry := range h.backend.log {
		if entry.request.Op == NativeRead && entry.request.Endpoint == 950 {
			t.Fatal("read ahead on a child nobody accepted")
		}
	}
	if state := h.readiness(f, id); state.Ready&linuxsockapi.ReadyIn == 0 {
		t.Fatalf("listener with a child ahead %+v", state)
	}
	reply := h.call(f, linuxsockapi.Request{Opcode: opAccept, Socket: id})
	if reply.Result != linuxsockapi.ResultOK || reply.Port != 6000 || h.endpoint(reply.Value) != 950 {
		t.Fatalf("accept %+v", reply)
	}
	h.readAhead(950)
	if !h.waiting(950) {
		t.Fatal("no wait on the child")
	}
}

func TestListenerCloseClosesChildrenAhead(t *testing.T) {
	h := newHarness(t)
	f := h.family(1, 7)
	id := listener(h, f)
	h.wake(h.endpoint(id), MaskAcceptable|MaskListening, 0)
	h.complete(h.take(NativeAccept), NativeCompletion{Endpoint: 960, Address: peerAddress, Port: 6000, Local: localAddress, LocalPort: 80})
	h.call(f, linuxsockapi.Request{Opcode: opClose, Socket: id})
	closed := map[uint32]bool{}
	for h.has(NativeClose) {
		closed[h.take(NativeClose).request.Endpoint] = true
	}
	if !closed[960] {
		t.Fatal("child ahead leaked")
	}
	// The second accept ahead netd answers for a listener that is gone.
	for h.has(NativeAccept) {
		h.complete(h.take(NativeAccept), NativeCompletion{Result: NativeClosed})
	}
	if h.svc.families[f].count != 0 {
		t.Fatalf("%d sockets left", h.svc.families[f].count)
	}
}
