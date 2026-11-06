package linuxsock

import (
	"testing"

	"goos/userspace/linuxsockapi"
)

const (
	opOpen     = linuxsockapi.OpOpen
	opBind     = linuxsockapi.OpBind
	opListen   = linuxsockapi.OpListen
	opAccept   = linuxsockapi.OpAccept
	opConnect  = linuxsockapi.OpConnect
	opRead     = linuxsockapi.OpRead
	opWrite    = linuxsockapi.OpWrite
	opSendTo   = linuxsockapi.OpSendTo
	opShutdown = linuxsockapi.OpShutdown
	opClose    = linuxsockapi.OpClose
	opGetName  = linuxsockapi.OpGetSockName
)

func TestOpenAssignsIdentityAndArmsWait(t *testing.T) {
	h := newHarness(t)
	f := h.family(1, 7)
	id := h.open(f, linuxsockapi.KindStream)
	if linuxsockapi.SessionOf(id) != 7 {
		t.Fatalf("session of %x", id)
	}
	wait := h.take(NativeWaitReady)
	if wait.request.Endpoint != h.endpoint(id) || wait.request.Mask != 0 {
		t.Fatalf("wait %+v", wait.request)
	}
	// A fresh stream is TCP_CLOSE: writable and hung up.
	if state := h.readiness(f, id); state.Socket != id || state.Ready != linuxsockapi.ReadyOut|linuxsockapi.ReadyHangup {
		t.Fatalf("fresh stream readiness %+v", state)
	}
	if h.svc.Counters().Opens != 1 {
		t.Fatal("open not counted")
	}
}

// A backlog above the old 128 cap reaches netd as asked, so a raised
// system.net.tcp.listen-backlog applies to Linux listeners; netd clamps it
// to the tuning.
func TestListenPassesBacklogAboveDefault(t *testing.T) {
	h := newHarness(t)
	f := h.family(1, 7)
	id := h.open(f, linuxsockapi.KindStream)
	correlation := h.send(f, linuxsockapi.Request{Opcode: opListen, Socket: id, Length: 200})
	h.complete(h.take(NativeBind), NativeCompletion{LocalPort: 50001})
	if listen := h.take(NativeListen); listen.request.Length != 200 {
		t.Fatalf("backlog %d", listen.request.Length)
	} else {
		h.complete(listen, NativeCompletion{})
	}
	if reply := h.reply(f, correlation); reply.Result != linuxsockapi.ResultOK {
		t.Fatalf("listen %d", reply.Result)
	}
}

func TestListenBindsFirstAndClampsBacklog(t *testing.T) {
	h := newHarness(t)
	f := h.family(1, 7)
	id := h.open(f, linuxsockapi.KindStream)
	correlation := h.send(f, linuxsockapi.Request{Opcode: opListen, Socket: id, Length: 4096})
	bind := h.take(NativeBind)
	if bind.request.Port != 0 || bind.request.Address != ([4]byte{}) {
		t.Fatalf("listen binds %+v", bind.request)
	}
	h.noReply(f, correlation)
	h.complete(bind, NativeCompletion{LocalPort: 50000})
	listen := h.take(NativeListen)
	if listen.request.Length != MaxBacklog {
		t.Fatalf("backlog %d", listen.request.Length)
	}
	h.complete(listen, NativeCompletion{})
	if reply := h.reply(f, correlation); reply.Result != linuxsockapi.ResultOK {
		t.Fatalf("listen %d", reply.Result)
	}
	// A second listen changes the backlog in the service only.
	if reply := h.call(f, linuxsockapi.Request{Opcode: opListen, Socket: id, Length: 2}); reply.Result != linuxsockapi.ResultOK || h.has(NativeListen) {
		t.Fatalf("relisten %+v", reply)
	}
	if reply := h.call(f, linuxsockapi.Request{Opcode: linuxsockapi.OpGetSockName, Socket: id}); reply.LocalPort != 50000 {
		t.Fatalf("getsockname %+v", reply)
	}
	// connect on a listener is EISCONN, read ENOTCONN.
	if reply := h.call(f, linuxsockapi.Request{Opcode: opConnect, Socket: id, Address: peerAddress, Port: 1}); reply.Result != linuxsockapi.ResultIsConnected {
		t.Fatalf("connect on listener %d", reply.Result)
	}
	if reply := h.call(f, linuxsockapi.Request{Opcode: opRead, Socket: id}); reply.Result != linuxsockapi.ResultNotConnected {
		t.Fatalf("read on listener %d", reply.Result)
	}
}

func listener(h *harness, f int) uint64 {
	id := h.open(f, linuxsockapi.KindStream)
	h.send(f, linuxsockapi.Request{Opcode: opBind, Socket: id, Port: 80})
	h.complete(h.take(NativeBind), NativeCompletion{LocalPort: 80})
	h.send(f, linuxsockapi.Request{Opcode: opListen, Socket: id, Length: 4})
	h.complete(h.take(NativeListen), NativeCompletion{})
	return id
}

func TestAcceptWouldBlockThenChild(t *testing.T) {
	h := newHarness(t)
	f := h.family(1, 7)
	id := listener(h, f)
	correlation := h.send(f, linuxsockapi.Request{Opcode: opAccept, Socket: id})
	accept := h.take(NativeAccept)
	if accept.request.Flags&NativeNoWait == 0 {
		t.Fatal("accept must not wait in netd")
	}
	h.complete(accept, NativeCompletion{Result: NativeWouldBlock})
	if reply := h.reply(f, correlation); reply.Result != linuxsockapi.ResultWouldBlock {
		t.Fatalf("accept %d", reply.Result)
	}
	if h.svc.families[f].count != 1 {
		t.Fatal("a failed accept kept its child")
	}
	// The listener becomes readable once netd queues a child.
	// The kernel took the notification the open drew.
	linuxsockapi.TakeToken(h.pools[f])
	before := h.notifications(f)
	h.wake(h.endpoint(id), MaskAcceptable, 0)
	if state := h.readiness(f, id); state.Ready != linuxsockapi.ReadyIn {
		t.Fatalf("listener readiness %+v", state)
	}
	if h.notifications(f) != before+1 {
		t.Fatal("no ReadyChanged")
	}
	correlation = h.send(f, linuxsockapi.Request{Opcode: opAccept, Socket: id})
	h.complete(h.take(NativeAccept), NativeCompletion{Endpoint: 900, Address: peerAddress, Port: 5555, Local: localAddress, LocalPort: 80})
	reply := h.reply(f, correlation)
	if reply.Result != linuxsockapi.ResultOK || reply.Address != peerAddress || reply.Port != 5555 || reply.LocalPort != 80 {
		t.Fatalf("accept reply %+v", reply)
	}
	child := reply.Value
	if h.endpoint(child) != 900 || linuxsockapi.SessionOf(child) != 7 {
		t.Fatal("child not adopted")
	}
	if !h.waiting(900) {
		t.Fatal("no wait on the child")
	}
	if peer := h.call(f, linuxsockapi.Request{Opcode: linuxsockapi.OpGetPeerName, Socket: child}); peer.Port != 5555 {
		t.Fatalf("child peer %+v", peer)
	}
}

func TestAcceptWithFullFamilyLeavesChildInNetd(t *testing.T) {
	h := newHarness(t)
	f := h.family(1, 7)
	id := listener(h, f)
	for h.svc.families[f].count < h.svc.Limits().Family {
		h.open(f, linuxsockapi.KindDatagram)
	}
	if reply := h.call(f, linuxsockapi.Request{Opcode: opAccept, Socket: id}); reply.Result != linuxsockapi.ResultFamilyLimit {
		t.Fatalf("accept %d", reply.Result)
	}
	if h.has(NativeAccept) {
		t.Fatal("asked netd for a child with no room for it")
	}
}

func TestNonblockingConnect(t *testing.T) {
	h := newHarness(t)
	f := h.family(1, 7)
	id := h.open(f, linuxsockapi.KindStream)
	correlation := h.send(f, linuxsockapi.Request{Opcode: opConnect, Socket: id, Address: peerAddress, Port: 80})
	connect := h.take(NativeConnect)
	if connect.request.Flags&NativeNoWait == 0 || connect.request.Address != peerAddress || connect.request.Port != 80 {
		t.Fatalf("connect %+v", connect.request)
	}
	h.complete(connect, NativeCompletion{Result: NativeInProgress, Local: localAddress, LocalPort: 40000})
	if reply := h.reply(f, correlation); reply.Result != linuxsockapi.ResultInProgress {
		t.Fatalf("connect %d", reply.Result)
	}
	if state := h.readiness(f, id); state.Ready != 0 {
		t.Fatalf("connecting readiness %+v", state)
	}
	if reply := h.call(f, linuxsockapi.Request{Opcode: opConnect, Socket: id, Address: peerAddress, Port: 80}); reply.Result != linuxsockapi.ResultAlready {
		t.Fatalf("second connect %d", reply.Result)
	}
	if reply := h.call(f, linuxsockapi.Request{Opcode: opRead, Socket: id}); reply.Result != linuxsockapi.ResultWouldBlock {
		t.Fatalf("read while connecting %d", reply.Result)
	}
	if reply := h.call(f, linuxsockapi.Request{Opcode: linuxsockapi.OpGetSockName, Socket: id}); reply.Local != localAddress || reply.LocalPort != 40000 {
		t.Fatalf("getsockname while connecting %+v", reply)
	}
	h.wake(h.endpoint(id), MaskConnected|MaskWritable, 0)
	if state := h.readiness(f, id); state.Ready != linuxsockapi.ReadyOut {
		t.Fatalf("connected readiness %+v", state)
	}
	// Linux: the first connect after the nonblocking one finished returns
	// 0, the next EISCONN.
	if reply := h.call(f, linuxsockapi.Request{Opcode: opConnect, Socket: id, Address: peerAddress, Port: 80}); reply.Result != linuxsockapi.ResultOK || reply.LocalPort != 40000 {
		t.Fatalf("connect after it finished %+v", reply)
	}
	if reply := h.call(f, linuxsockapi.Request{Opcode: opConnect, Socket: id, Address: peerAddress, Port: 80}); reply.Result != linuxsockapi.ResultIsConnected {
		t.Fatalf("connect after connected %d", reply.Result)
	}
	if h.has(NativeConnect) {
		t.Fatal("a second native connection")
	}
	if peer := h.call(f, linuxsockapi.Request{Opcode: linuxsockapi.OpGetPeerName, Socket: id}); peer.Address != peerAddress || peer.Port != 80 {
		t.Fatalf("peer %+v", peer)
	}
}

func TestFailedConnectReportsAndClearsError(t *testing.T) {
	h := newHarness(t)
	f := h.family(1, 7)
	id := h.open(f, linuxsockapi.KindStream)
	h.send(f, linuxsockapi.Request{Opcode: opConnect, Socket: id, Address: peerAddress, Port: 81})
	h.complete(h.take(NativeConnect), NativeCompletion{Result: NativeInProgress})
	h.wake(h.endpoint(id), MaskConnectFailed|MaskPendingError, 0)
	fetch := h.take(NativeError)
	if state := h.readiness(f, id); state.Ready&linuxsockapi.ReadyError == 0 {
		t.Fatalf("readiness while fetching %+v", state)
	}
	h.complete(fetch, NativeCompletion{Value: uint32(NativeRefused)})
	state := h.readiness(f, id)
	want := linuxsockapi.ReadyIn | linuxsockapi.ReadyOut | linuxsockapi.ReadyError | linuxsockapi.ReadyHangup | linuxsockapi.ReadyReadHangup
	if state.Ready != want || state.Error != linuxsockapi.ResultRefused {
		t.Fatalf("failed connect readiness %+v", state)
	}
	if reply := h.call(f, linuxsockapi.Request{Opcode: linuxsockapi.OpGetPendingError, Socket: id}); reply.Value != uint64(linuxsockapi.ResultRefused) {
		t.Fatalf("SO_ERROR %+v", reply)
	}
	if reply := h.call(f, linuxsockapi.Request{Opcode: linuxsockapi.OpGetPendingError, Socket: id}); reply.Value != 0 {
		t.Fatalf("SO_ERROR not cleared %+v", reply)
	}
	if state := h.readiness(f, id); state.Ready&linuxsockapi.ReadyError != 0 {
		t.Fatalf("error bit stays %+v", state)
	}
	// Connecting again replaces the spent native endpoint.
	old := h.endpoint(id)
	correlation := h.send(f, linuxsockapi.Request{Opcode: opConnect, Socket: id, Address: peerAddress, Port: 80})
	if closed := h.take(NativeClose); closed.request.Endpoint != old {
		t.Fatalf("closed %d", closed.request.Endpoint)
	}
	h.complete(h.take(NativeOpen), NativeCompletion{Endpoint: 777})
	h.complete(h.take(NativeConnect), NativeCompletion{Local: localAddress, LocalPort: 40001})
	if reply := h.reply(f, correlation); reply.Result != linuxsockapi.ResultInProgress || h.endpoint(id) != 777 {
		t.Fatalf("reconnect %+v", reply)
	}
}

// A refusal netd reports inside the connect, as loopback does, arrives the
// way a remote one does: InProgress, then the error in SO_ERROR.
func TestImmediateRefusalIsInProgress(t *testing.T) {
	h := newHarness(t)
	f := h.family(1, 7)
	id := h.open(f, linuxsockapi.KindStream)
	correlation := h.send(f, linuxsockapi.Request{Opcode: opConnect, Socket: id, Address: peerAddress, Port: 81})
	h.complete(h.take(NativeConnect), NativeCompletion{Result: NativeRefused})
	if reply := h.reply(f, correlation); reply.Result != linuxsockapi.ResultInProgress {
		t.Fatalf("connect %+v", reply)
	}
	if state := h.readiness(f, id); state.Ready&(linuxsockapi.ReadyOut|linuxsockapi.ReadyError) != linuxsockapi.ReadyOut|linuxsockapi.ReadyError {
		t.Fatalf("readiness %+v", state)
	}
	if reply := h.call(f, linuxsockapi.Request{Opcode: linuxsockapi.OpGetPendingError, Socket: id}); reply.Value != uint64(linuxsockapi.ResultRefused) {
		t.Fatalf("SO_ERROR %+v", reply)
	}
}

func TestConnectAfterFailureReportsErrorOnce(t *testing.T) {
	h := newHarness(t)
	f := h.family(1, 7)
	id := h.open(f, linuxsockapi.KindStream)
	h.send(f, linuxsockapi.Request{Opcode: opConnect, Socket: id, Address: peerAddress, Port: 81})
	h.complete(h.take(NativeConnect), NativeCompletion{Result: NativeInProgress})
	h.wake(h.endpoint(id), MaskConnectFailed|MaskPendingError, 0)
	fetch := h.take(NativeError)
	// A connect while the error is being fetched waits for it.
	correlation := h.send(f, linuxsockapi.Request{Opcode: opConnect, Socket: id, Address: peerAddress, Port: 81})
	h.noReply(f, correlation)
	h.complete(fetch, NativeCompletion{Value: uint32(NativeRefused)})
	if reply := h.reply(f, correlation); reply.Result != linuxsockapi.ResultRefused {
		t.Fatalf("connect after failure %d", reply.Result)
	}
	h.send(f, linuxsockapi.Request{Opcode: opConnect, Socket: id, Address: peerAddress, Port: 81})
	h.take(NativeClose)
	h.take(NativeOpen)
}

func TestImmediateConnectFailure(t *testing.T) {
	h := newHarness(t)
	f := h.family(1, 7)
	id := h.open(f, linuxsockapi.KindStream)
	correlation := h.send(f, linuxsockapi.Request{Opcode: opConnect, Socket: id, Address: peerAddress, Port: 80})
	h.complete(h.take(NativeConnect), NativeCompletion{Result: NativeNoRoute})
	if reply := h.reply(f, correlation); reply.Result != linuxsockapi.ResultNoRoute {
		t.Fatalf("connect %d", reply.Result)
	}
	h.send(f, linuxsockapi.Request{Opcode: opConnect, Socket: id, Address: peerAddress, Port: 80})
	h.take(NativeClose)
	h.take(NativeOpen)
}

func readBytes(h *harness, f int, reply linuxsockapi.Reply) []byte {
	offset := linuxsockapi.RXOffset(reply.Slot)
	return h.pools[f][offset : offset+uint64(reply.Length)]
}

func TestWriteOnUnconnectedStream(t *testing.T) {
	h := newHarness(t)
	f := h.family(1, 7)
	id := h.open(f, linuxsockapi.KindStream)
	if reply := h.call(f, linuxsockapi.Request{Opcode: opWrite, Socket: id}); reply.Result != linuxsockapi.ResultClosed {
		t.Fatalf("write unconnected %+v", reply)
	}
}
