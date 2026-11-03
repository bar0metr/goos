package linuxsock

import (
	"bytes"
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
	opRelease  = linuxsockapi.OpRelease
	opClose    = linuxsockapi.OpClose
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
	if reply := h.call(f, linuxsockapi.Request{Opcode: opRead, Socket: id, Length: 10}); reply.Result != linuxsockapi.ResultNotConnected {
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
	for h.svc.families[f].count < linuxsockapi.MaxSockets {
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
	if reply := h.call(f, linuxsockapi.Request{Opcode: opRead, Socket: id, Length: 1}); reply.Result != linuxsockapi.ResultWouldBlock {
		t.Fatalf("read while connecting %d", reply.Result)
	}
	if reply := h.call(f, linuxsockapi.Request{Opcode: linuxsockapi.OpGetSockName, Socket: id}); reply.Local != localAddress || reply.LocalPort != 40000 {
		t.Fatalf("getsockname while connecting %+v", reply)
	}
	h.wake(h.endpoint(id), MaskConnected|MaskWritable, 0)
	if state := h.readiness(f, id); state.Ready != linuxsockapi.ReadyOut {
		t.Fatalf("connected readiness %+v", state)
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
	if reply := h.reply(f, correlation); reply.Result != linuxsockapi.ResultOK || h.endpoint(id) != 777 {
		t.Fatalf("reconnect %+v", reply)
	}
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

func TestStreamReadAndRetainedBytes(t *testing.T) {
	h := newHarness(t)
	f := h.family(1, 7)
	id := h.connected(f)
	h.wake(h.endpoint(id), MaskWritable|MaskReadable, 10)
	if state := h.readiness(f, id); state.Ready != linuxsockapi.ReadyIn|linuxsockapi.ReadyOut || state.Readable != 10 {
		t.Fatalf("readable %+v", state)
	}
	correlation := h.send(f, linuxsockapi.Request{Opcode: opRead, Socket: id, Length: 100})
	read := h.take(NativeRead)
	if read.request.Length != 100 || read.request.Flags != NativeNoWait {
		t.Fatalf("read %+v", read.request)
	}
	h.complete(read, NativeCompletion{Data: []byte("0123456789")})
	reply := h.reply(f, correlation)
	if reply.Result != linuxsockapi.ResultOK || string(readBytes(h, f, reply)) != "0123456789" {
		t.Fatalf("read %+v", reply)
	}
	// A second read waits for the slot's Release, so bytes stay in order.
	second := h.send(f, linuxsockapi.Request{Opcode: opRead, Socket: id, Length: 100})
	h.noReply(f, second)
	if h.has(NativeRead) {
		t.Fatal("read past a lent slot")
	}
	// The kernel took four bytes; six come back.
	h.send(f, linuxsockapi.Request{Opcode: opRelease, Socket: id, Slot: reply.Slot, Length: 6, Value: 4})
	again := h.reply(f, second)
	if again.Result != linuxsockapi.ResultOK || string(readBytes(h, f, again)) != "456789" {
		t.Fatalf("retained read %+v %q", again, readBytes(h, f, again))
	}
	if h.has(NativeRead) {
		t.Fatal("retained bytes read from netd")
	}
	h.send(f, linuxsockapi.Request{Opcode: opRelease, Socket: id, Slot: again.Slot})
	if h.svc.families[f].rxUsed != 0 {
		t.Fatalf("slots still used %b", h.svc.families[f].rxUsed)
	}
}

func TestShortReadOfRetainedKeepsTail(t *testing.T) {
	h := newHarness(t)
	f := h.family(1, 7)
	id := h.connected(f)
	correlation := h.send(f, linuxsockapi.Request{Opcode: opRead, Socket: id, Length: 100})
	h.complete(h.take(NativeRead), NativeCompletion{Data: []byte("abcdefghij")})
	first := h.reply(f, correlation)
	h.send(f, linuxsockapi.Request{Opcode: opRelease, Socket: id, Slot: first.Slot, Length: 10})
	short := h.call(f, linuxsockapi.Request{Opcode: opRead, Socket: id, Length: 3})
	if string(readBytes(h, f, short)) != "abc" {
		t.Fatalf("short %q", readBytes(h, f, short))
	}
	// One of the three comes back unread; it and the tail are next.
	h.send(f, linuxsockapi.Request{Opcode: opRelease, Socket: id, Slot: short.Slot, Length: 1, Value: 2})
	rest := h.call(f, linuxsockapi.Request{Opcode: opRead, Socket: id, Length: 100})
	if string(readBytes(h, f, rest)) != "cdefghij" {
		t.Fatalf("rest %q", readBytes(h, f, rest))
	}
	// Everything consumed: the next read goes to netd.
	h.send(f, linuxsockapi.Request{Opcode: opRelease, Socket: id, Slot: rest.Slot})
	h.send(f, linuxsockapi.Request{Opcode: opRead, Socket: id, Length: 1})
	h.complete(h.take(NativeRead), NativeCompletion{Data: []byte("0123")})
	if h.dropped(f) {
		t.Fatal("dropped")
	}
	// A Release that leaves a gap before the tail is malformed.
	other := h.connected(f)
	correlation = h.send(f, linuxsockapi.Request{Opcode: opRead, Socket: other, Length: 100})
	h.complete(h.take(NativeRead), NativeCompletion{Data: []byte("abcdefghij")})
	lent := h.reply(f, correlation)
	h.send(f, linuxsockapi.Request{Opcode: opRelease, Socket: other, Slot: lent.Slot, Length: 10})
	short = h.call(f, linuxsockapi.Request{Opcode: opRead, Socket: other, Length: 3})
	h.send(f, linuxsockapi.Request{Opcode: opRelease, Socket: other, Slot: short.Slot, Length: 1})
	if !h.dropped(f) {
		t.Fatal("a Release with a gap before the tail was accepted")
	}
}

func TestPeekOfNativeDataIsNotRetained(t *testing.T) {
	h := newHarness(t)
	f := h.family(1, 7)
	id := h.connected(f)
	correlation := h.send(f, linuxsockapi.Request{Opcode: opRead, Socket: id, Length: 100, Flags: linuxsockapi.FlagPeek})
	read := h.take(NativeRead)
	if read.request.Flags&NativePeek == 0 {
		t.Fatal("peek not passed to netd")
	}
	h.complete(read, NativeCompletion{Data: []byte("xyz")})
	reply := h.reply(f, correlation)
	h.send(f, linuxsockapi.Request{Opcode: opRelease, Socket: id, Slot: reply.Slot, Length: 3})
	h.send(f, linuxsockapi.Request{Opcode: opRead, Socket: id, Length: 100})
	if !h.has(NativeRead) {
		t.Fatal("peeked bytes were retained as well as left in netd")
	}
}

func TestReadEOFAndReset(t *testing.T) {
	h := newHarness(t)
	f := h.family(1, 7)
	id := h.connected(f)
	correlation := h.send(f, linuxsockapi.Request{Opcode: opRead, Socket: id, Length: 10})
	h.complete(h.take(NativeRead), NativeCompletion{Result: NativeEOF})
	if reply := h.reply(f, correlation); reply.Result != linuxsockapi.ResultOK || reply.Length != 0 || reply.Flags != linuxsockapi.FlagEOF {
		t.Fatalf("eof %+v", reply)
	}
	other := h.connected(f)
	h.wake(h.endpoint(other), MaskPendingError|MaskHungUp, 0)
	h.complete(h.take(NativeError), NativeCompletion{Value: uint32(NativeReset)})
	if state := h.readiness(f, other); state.Ready&linuxsockapi.ReadyError == 0 || state.Ready&linuxsockapi.ReadyHangup == 0 {
		t.Fatalf("reset readiness %+v", state)
	}
	correlation = h.send(f, linuxsockapi.Request{Opcode: opRead, Socket: other, Length: 10})
	h.complete(h.take(NativeRead), NativeCompletion{Result: NativeEOF})
	if reply := h.reply(f, correlation); reply.Result != linuxsockapi.ResultOK {
		t.Fatalf("buffered EOF first %+v", reply)
	}
	correlation = h.send(f, linuxsockapi.Request{Opcode: opRead, Socket: other, Length: 10})
	h.complete(h.take(NativeRead), NativeCompletion{Result: NativeWouldBlock})
	if reply := h.reply(f, correlation); reply.Result != linuxsockapi.ResultReset {
		t.Fatalf("reset read %+v", reply)
	}
}

func TestWriteCopiesTXSlotAndReturnsIt(t *testing.T) {
	h := newHarness(t)
	f := h.family(1, 7)
	id := h.connected(f)
	offset := linuxsockapi.TXOffset(3)
	copy(h.pools[f][offset:], "hello world")
	correlation := h.send(f, linuxsockapi.Request{Opcode: opWrite, Socket: id, Slot: 3, Length: 11})
	write := h.take(NativeWrite)
	if !bytes.Equal(write.data, []byte("hello world")) || write.request.Flags&NativeNoWait == 0 {
		t.Fatalf("write %+v %q", write.request, write.data)
	}
	// The slot is the service's until the reply: a second write naming it
	// is malformed.
	h.complete(write, NativeCompletion{Length: 5})
	reply := h.reply(f, correlation)
	if reply.Result != linuxsockapi.ResultOK || reply.Slot != 3 || reply.Length != 5 {
		t.Fatalf("partial write %+v", reply)
	}
	correlation = h.send(f, linuxsockapi.Request{Opcode: opWrite, Socket: id, Slot: 3, Length: 6})
	h.complete(h.take(NativeWrite), NativeCompletion{Result: NativeWouldBlock})
	if reply := h.reply(f, correlation); reply.Result != linuxsockapi.ResultWouldBlock || reply.Slot != 3 {
		t.Fatalf("full write %+v", reply)
	}
	h.send(f, linuxsockapi.Request{Opcode: opWrite, Socket: id, Slot: 1, Length: 1})
	h.send(f, linuxsockapi.Request{Opcode: opWrite, Socket: id, Slot: 1, Length: 1})
	if !h.dropped(f) {
		t.Fatal("a TX slot lent twice did not end the session")
	}
}

func TestWriteOnUnconnectedStream(t *testing.T) {
	h := newHarness(t)
	f := h.family(1, 7)
	id := h.open(f, linuxsockapi.KindStream)
	if reply := h.call(f, linuxsockapi.Request{Opcode: opWrite, Socket: id, Slot: 0, Length: 1}); reply.Result != linuxsockapi.ResultClosed || reply.Slot != 0 {
		t.Fatalf("write unconnected %+v", reply)
	}
}

func TestShutdown(t *testing.T) {
	h := newHarness(t)
	f := h.family(1, 7)
	id := h.connected(f)
	if reply := h.call(f, linuxsockapi.Request{Opcode: opShutdown, Socket: h.open(f, linuxsockapi.KindStream), Length: shutWrite}); reply.Result != linuxsockapi.ResultNotConnected {
		t.Fatalf("shutdown unconnected %d", reply.Result)
	}
	// Bytes retained before SHUT_RD are dropped; reads then end.
	correlation := h.send(f, linuxsockapi.Request{Opcode: opRead, Socket: id, Length: 10})
	h.complete(h.take(NativeRead), NativeCompletion{Data: []byte("abc")})
	first := h.reply(f, correlation)
	h.send(f, linuxsockapi.Request{Opcode: opRelease, Socket: id, Slot: first.Slot, Length: 3})
	if reply := h.call(f, linuxsockapi.Request{Opcode: opShutdown, Socket: id, Length: shutRead}); reply.Result != linuxsockapi.ResultOK {
		t.Fatalf("SHUT_RD %d", reply.Result)
	}
	if h.svc.families[f].rxUsed != 0 {
		t.Fatal("retained bytes kept after SHUT_RD")
	}
	if reply := h.call(f, linuxsockapi.Request{Opcode: opRead, Socket: id, Length: 10}); reply.Result != linuxsockapi.ResultOK || reply.Flags != linuxsockapi.FlagEOF {
		t.Fatalf("read after SHUT_RD %+v", reply)
	}
	// What still arrives is drained in netd.
	h.wake(h.endpoint(id), MaskReadable|MaskWritable, 20)
	drain := h.take(NativeRead)
	h.complete(drain, NativeCompletion{Data: make([]byte, 20)})
	if state := h.readiness(f, id); state.Ready&linuxsockapi.ReadyIn == 0 || state.Ready&linuxsockapi.ReadyReadHangup == 0 || state.Readable != 0 {
		t.Fatalf("SHUT_RD readiness %+v", state)
	}
	correlation = h.send(f, linuxsockapi.Request{Opcode: opShutdown, Socket: id, Length: shutWrite})
	h.complete(h.take(NativeShutdownWrite), NativeCompletion{})
	if reply := h.reply(f, correlation); reply.Result != linuxsockapi.ResultOK {
		t.Fatalf("SHUT_WR %d", reply.Result)
	}
	if reply := h.call(f, linuxsockapi.Request{Opcode: opShutdown, Socket: id, Length: shutBoth}); reply.Result != linuxsockapi.ResultOK || h.has(NativeShutdownWrite) {
		t.Fatalf("repeated shutdown %+v", reply)
	}
	if reply := h.call(f, linuxsockapi.Request{Opcode: opWrite, Socket: id, Slot: 2, Length: 1}); reply.Result != linuxsockapi.ResultClosed {
		t.Fatalf("write after SHUT_WR %d", reply.Result)
	}
	if state := h.readiness(f, id); state.Ready&linuxsockapi.ReadyHangup == 0 || state.Ready&linuxsockapi.ReadyOut == 0 {
		t.Fatalf("both shut readiness %+v", state)
	}
}

func TestCloseReleasesEndpointAndIdentity(t *testing.T) {
	h := newHarness(t)
	f := h.family(1, 7)
	id := h.connected(f)
	endpoint := h.endpoint(id)
	correlation := h.send(f, linuxsockapi.Request{Opcode: opRead, Socket: id, Length: 10})
	h.complete(h.take(NativeRead), NativeCompletion{Data: []byte("abc")})
	lent := h.reply(f, correlation)
	if reply := h.call(f, linuxsockapi.Request{Opcode: opClose, Socket: id}); reply.Result != linuxsockapi.ResultOK {
		t.Fatalf("close %d", reply.Result)
	}
	if closed := h.take(NativeClose); closed.request.Endpoint != endpoint {
		t.Fatalf("closed %d", closed.request.Endpoint)
	}
	if state := h.readiness(f, id); state.Socket != 0 {
		t.Fatalf("entry not cleared %+v", state)
	}
	// The lent slot comes back after the close, unread bytes and all.
	h.send(f, linuxsockapi.Request{Opcode: opRelease, Socket: id, Slot: lent.Slot, Length: 3})
	if h.dropped(f) || h.svc.families[f].rxUsed != 0 {
		t.Fatal("late Release mishandled")
	}
	if reply := h.call(f, linuxsockapi.Request{Opcode: opRead, Socket: id, Length: 1}); reply.Result != linuxsockapi.ResultStale {
		t.Fatalf("closed identity %d", reply.Result)
	}
	// The slot is reused under a new generation.
	again := h.open(f, linuxsockapi.KindStream)
	oldIndex, _ := linuxsockapi.SocketIndex(id)
	newIndex, _ := linuxsockapi.SocketIndex(again)
	if oldIndex != newIndex || again == id {
		t.Fatalf("reuse %x %x", id, again)
	}
	if reply := h.call(f, linuxsockapi.Request{Opcode: linuxsockapi.OpGetSockName, Socket: id}); reply.Result != linuxsockapi.ResultStale {
		t.Fatalf("old generation %d", reply.Result)
	}
}
