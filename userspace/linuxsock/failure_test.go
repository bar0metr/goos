package linuxsock

import (
	"testing"

	"goos/userspace/linuxsockapi"
)

// The kernel side of a readiness page: it takes the token, then reads every
// entry, as ADR 0210 has it.
func kernelScan(h *harness, family int) map[uint64]linuxsockapi.Readiness {
	linuxsockapi.TakeToken(h.pools[family])
	seen := map[uint64]linuxsockapi.Readiness{}
	for index := range linuxsockapi.MaxSockets {
		if state, ok := linuxsockapi.Read(h.pools[family], index, 4); ok && state.Socket != 0 {
			seen[state.Socket] = state
		}
	}
	return seen
}

func TestNoReadinessLostUnderFullQueue(t *testing.T) {
	h := newHarness(t)
	f := h.family(1, 7)
	id := h.connected(f)
	kernelScan(h, f)
	endpoint := h.endpoint(id)
	// The channel is full: the change is published, but its notification
	// cannot go and the token stays clear.
	h.transport.full[f] = true
	before := h.notifications(f)
	h.wake(endpoint, MaskWritable|MaskReadable, 5)
	if h.notifications(f) != before || !h.svc.Pending(f) {
		t.Fatal("a notification went through a full queue or was not kept")
	}
	if h.svc.Counters().DeferredNotifications == 0 {
		t.Fatal("deferral not counted")
	}
	// More changes while it is owed coalesce into it.
	h.wake(endpoint, MaskWritable|MaskReadable|MaskReceiveEOF, 5)
	h.transport.full[f] = false
	h.svc.Flush(f)
	if h.notifications(f) != before+1 || h.svc.Pending(f) {
		t.Fatalf("flush sent %d notifications", h.notifications(f)-before)
	}
	state := kernelScan(h, f)[id]
	if state.Ready&linuxsockapi.ReadyReadHangup == 0 || state.Readable != 5 {
		t.Fatalf("the kernel saw %+v, not the last change", state)
	}
	// While a notification is outstanding no second one is sent; a change
	// after the kernel takes the token draws a new one.
	h.wake(endpoint, MaskWritable, 0)
	if h.notifications(f) != before+2 {
		t.Fatal("change after the scan not notified")
	}
	h.wake(endpoint, MaskWritable|MaskReadable, 1)
	if h.notifications(f) != before+2 {
		t.Fatal("second notification while one is outstanding")
	}
	if state := kernelScan(h, f)[id]; state.Readable != 1 {
		t.Fatalf("coalesced change lost: %+v", state)
	}
}

func TestRepliesWaitForRoom(t *testing.T) {
	h := newHarness(t)
	f := h.family(1, 7)
	id := h.open(f, linuxsockapi.KindStream)
	h.transport.full[f] = true
	correlation := h.send(f, linuxsockapi.Request{Opcode: linuxsockapi.OpGetSockName, Socket: id})
	h.noReply(f, correlation)
	if !h.svc.Pending(f) {
		t.Fatal("held reply not pending")
	}
	h.transport.full[f] = false
	h.svc.Flush(f)
	h.reply(f, correlation)
	if h.svc.Pending(f) || h.svc.families[f].inflight != 0 {
		t.Fatal("reply not released")
	}
}

func TestBackendLossFailsSocketsAndRecovers(t *testing.T) {
	h := newHarness(t)
	f := h.family(1, 7)
	stream := h.connected(f)
	datagram := boundDatagram(h, f)
	read := h.send(f, linuxsockapi.Request{Opcode: linuxsockapi.OpQueryReady, Socket: stream})
	nativeRead := h.readAhead(h.endpoint(stream))
	oldGeneration := h.generation[TCP]
	kernelScan(h, f)
	h.svc.BackendDown(TCP, oldGeneration)
	if reply := h.reply(f, read); reply.Result != linuxsockapi.ResultServiceLost {
		t.Fatalf("read in flight %d", reply.Result)
	}
	state := kernelScan(h, f)[stream]
	all := linuxsockapi.ReadyIn | linuxsockapi.ReadyOut | linuxsockapi.ReadyError | linuxsockapi.ReadyHangup | linuxsockapi.ReadyReadHangup
	if state.Ready != all || state.Error != linuxsockapi.ResultServiceLost {
		t.Fatalf("dead stream %+v", state)
	}
	if h.notifications(f) == 0 {
		t.Fatal("waiters not woken")
	}
	if dgram := kernelScan(h, f)[datagram]; dgram.Ready&linuxsockapi.ReadyError != 0 {
		t.Fatal("UDP socket failed with the TCP session")
	}
	if reply := h.call(f, linuxsockapi.Request{Opcode: linuxsockapi.OpGetPendingError, Socket: stream}); reply.Value != uint64(linuxsockapi.ResultServiceLost) {
		t.Fatalf("SO_ERROR %+v", reply)
	}
	if reply := h.call(f, linuxsockapi.Request{Opcode: opWrite, Socket: stream}); reply.Result != linuxsockapi.ResultServiceLost {
		t.Fatalf("write on dead %d", reply.Result)
	}
	if reply := h.call(f, linuxsockapi.Request{Opcode: opOpen, Length: linuxsockapi.KindStream}); reply.Result != linuxsockapi.ResultDown {
		t.Fatalf("open without TCP %d", reply.Result)
	}
	// A late completion from the dead session changes nothing.
	h.svc.Complete(TCP, oldGeneration, nativeRead.tag, &NativeCompletion{Op: NativeRead, Data: []byte("late")})
	if h.svc.BackendUp(TCP, oldGeneration) != linuxsockapi.ControlStale {
		t.Fatal("an old generation was accepted")
	}
	h.up(TCP)
	fresh := h.open(f, linuxsockapi.KindStream)
	if reply := h.call(f, linuxsockapi.Request{Opcode: opClose, Socket: stream}); reply.Result != linuxsockapi.ResultOK || h.has(NativeClose) {
		t.Fatalf("closing a dead socket %+v", reply)
	}
	if h.endpoint(fresh) == 0 || h.svc.Counters().BackendLosses != 1 {
		t.Fatal("recovery")
	}
}

func TestBackendReplacedTakesOldDown(t *testing.T) {
	h := newHarness(t)
	f := h.family(1, 7)
	id := boundDatagram(h, f)
	h.up(UDP)
	if reply := h.call(f, linuxsockapi.Request{Opcode: opRead, Socket: id}); reply.Result != linuxsockapi.ResultServiceLost {
		t.Fatalf("read after replacement %d", reply.Result)
	}
}

func TestUnsupportedWaitLeavesSocketUsable(t *testing.T) {
	h := newHarness(t)
	h.backend.unsupported[NativeWaitReady] = true
	f := h.family(1, 7)
	id := h.connected(f)
	if h.has(NativeWaitReady) {
		t.Fatal("wait submitted")
	}
	h.complete(h.readAhead(h.endpoint(id)), NativeCompletion{Data: []byte("ok")})
	if published := h.completions(f); len(published) != 1 || published[0].Length != 2 {
		t.Fatalf("read ahead %+v", published)
	}
}
