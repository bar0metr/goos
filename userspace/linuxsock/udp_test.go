package linuxsock

import (
	"bytes"
	"testing"

	"goos/userspace/linuxsockapi"
)

func TestDatagramSendToBindsFirst(t *testing.T) {
	h := newHarness(t)
	f := h.family(1, 7)
	id := h.open(f, linuxsockapi.KindDatagram)
	if state := h.readiness(f, id); state.Ready != linuxsockapi.ReadyOut {
		t.Fatalf("fresh datagram readiness %+v", state)
	}
	if reply := h.call(f, linuxsockapi.Request{Opcode: opRead, Socket: id, Length: 10}); reply.Result != linuxsockapi.ResultWouldBlock || h.has(NativeRead) {
		t.Fatalf("read unbound %+v", reply)
	}
	if reply := h.call(f, linuxsockapi.Request{Opcode: opWrite, Socket: id, Slot: 0, Length: 1}); reply.Result != linuxsockapi.ResultDestinationRequired {
		t.Fatalf("send unconnected %d", reply.Result)
	}
	copy(h.pools[f][linuxsockapi.TXOffset(2):], "query")
	correlation := h.send(f, linuxsockapi.Request{Opcode: opSendTo, Socket: id, Slot: 2, Length: 5, Address: [4]byte{127, 0, 0, 1}, Port: 53})
	h.complete(h.take(NativeBind), NativeCompletion{LocalPort: 33333})
	send := h.take(NativeSendTo)
	if !bytes.Equal(send.data, []byte("query")) || send.request.Port != 53 {
		t.Fatalf("sendto %+v %q", send.request, send.data)
	}
	h.complete(send, NativeCompletion{})
	if reply := h.reply(f, correlation); reply.Result != linuxsockapi.ResultOK || reply.Length != 5 || reply.Slot != 2 {
		t.Fatalf("sendto reply %+v", reply)
	}
	if reply := h.call(f, linuxsockapi.Request{Opcode: linuxsockapi.OpGetSockName, Socket: id}); reply.LocalPort != 33333 {
		t.Fatalf("auto-bound port %+v", reply)
	}
	if h.svc.Counters().DatagramsOut != 1 {
		t.Fatal("datagram not counted")
	}
}

func boundDatagram(h *harness, f int) uint64 {
	id := h.open(f, linuxsockapi.KindDatagram)
	h.send(f, linuxsockapi.Request{Opcode: opBind, Socket: id, Port: 9000})
	h.complete(h.take(NativeBind), NativeCompletion{LocalPort: 9000})
	return id
}

func TestDatagramReadTruncatesAndPeeks(t *testing.T) {
	h := newHarness(t)
	f := h.family(1, 7)
	id := boundDatagram(h, f)
	h.wake(h.endpoint(id), MaskReadable|MaskWritable, 100)
	if state := h.readiness(f, id); state.Ready != linuxsockapi.ReadyIn|linuxsockapi.ReadyOut || state.Readable != 100 {
		t.Fatalf("readable datagram %+v", state)
	}
	payload := bytes.Repeat([]byte{'d'}, 100)
	correlation := h.send(f, linuxsockapi.Request{Opcode: opRead, Socket: id, Length: 10, Flags: linuxsockapi.FlagPeek})
	read := h.take(NativeRead)
	if read.request.Flags != NativeNoWait|NativePeek || read.request.Length != 10 {
		t.Fatalf("peek %+v", read.request)
	}
	h.complete(read, NativeCompletion{Data: payload[:10], Full: 100, Address: peerAddress, Port: 53})
	reply := h.reply(f, correlation)
	if reply.Length != 10 || reply.Value != 100 || reply.Flags != linuxsockapi.FlagTruncated || reply.Address != peerAddress || reply.Port != 53 {
		t.Fatalf("truncated peek %+v", reply)
	}
	// A datagram's unread part is gone with its Release: nothing is kept.
	h.send(f, linuxsockapi.Request{Opcode: opRelease, Socket: id, Slot: reply.Slot, Length: 10})
	if h.svc.families[f].rxUsed != 0 {
		t.Fatal("datagram bytes retained")
	}
	correlation = h.send(f, linuxsockapi.Request{Opcode: opRead, Socket: id, Length: 4096})
	h.complete(h.take(NativeRead), NativeCompletion{Data: payload, Full: 100, Address: peerAddress, Port: 53})
	if reply := h.reply(f, correlation); reply.Length != 100 || reply.Flags != 0 || reply.Value != 100 {
		t.Fatalf("whole datagram %+v", reply)
	}
	// Two reads may hold slots at once on a datagram socket.
	correlation = h.send(f, linuxsockapi.Request{Opcode: opRead, Socket: id, Length: 4096})
	h.complete(h.take(NativeRead), NativeCompletion{})
	if reply := h.reply(f, correlation); reply.Result != linuxsockapi.ResultOK || reply.Length != 0 || reply.Slot != 0 {
		t.Fatalf("empty datagram %+v", reply)
	}
}

func TestConnectedDatagram(t *testing.T) {
	h := newHarness(t)
	f := h.family(1, 7)
	id := boundDatagram(h, f)
	correlation := h.send(f, linuxsockapi.Request{Opcode: opConnect, Socket: id, Address: peerAddress, Port: 53})
	connect := h.take(NativeConnect)
	if connect.request.Flags != 0 {
		t.Fatal("datagram connect flags")
	}
	h.complete(connect, NativeCompletion{Local: localAddress, LocalPort: 9000})
	if reply := h.reply(f, correlation); reply.Result != linuxsockapi.ResultOK || reply.Local != localAddress {
		t.Fatalf("connect %+v", reply)
	}
	if reply := h.call(f, linuxsockapi.Request{Opcode: opSendTo, Socket: id, Slot: 1, Length: 1, Address: [4]byte{1, 1, 1, 1}, Port: 53}); reply.Result != linuxsockapi.ResultIsConnected {
		t.Fatalf("sendto another peer %d", reply.Result)
	}
	correlation = h.send(f, linuxsockapi.Request{Opcode: opSendTo, Socket: id, Slot: 1, Length: 1, Address: peerAddress, Port: 53})
	h.complete(h.take(NativeWrite), NativeCompletion{Result: NativeMessageSize})
	if reply := h.reply(f, correlation); reply.Result != linuxsockapi.ResultMessageSize {
		t.Fatalf("oversize %d", reply.Result)
	}
	if reply := h.call(f, linuxsockapi.Request{Opcode: linuxsockapi.OpGetPeerName, Socket: id}); reply.Address != peerAddress || reply.Port != 53 {
		t.Fatalf("peer %+v", reply)
	}
	if reply := h.call(f, linuxsockapi.Request{Opcode: opShutdown, Socket: id, Length: shutWrite}); reply.Result != linuxsockapi.ResultOK {
		t.Fatalf("shutdown %d", reply.Result)
	}
	if reply := h.call(f, linuxsockapi.Request{Opcode: opWrite, Socket: id, Slot: 1, Length: 1}); reply.Result != linuxsockapi.ResultClosed {
		t.Fatalf("send after SHUT_WR %d", reply.Result)
	}
}

func TestDatagramOptionsAndNoTCPLevel(t *testing.T) {
	h := newHarness(t)
	f := h.family(1, 7)
	id := boundDatagram(h, f)
	if reply := h.call(f, linuxsockapi.Request{Opcode: linuxsockapi.OpGetOption, Socket: id, Level: LevelTCP, Name: OptionNoDelay}); reply.Result != linuxsockapi.ResultNoOption {
		t.Fatalf("TCP_NODELAY on UDP %d", reply.Result)
	}
	correlation := h.send(f, linuxsockapi.Request{Opcode: linuxsockapi.OpGetOption, Socket: id, Level: LevelSocket, Name: OptionRecvBuf})
	option := h.take(NativeOption)
	if option.request.Option != OptionReceiveBuffer || option.proto != UDP {
		t.Fatalf("option %+v", option)
	}
	h.complete(option, NativeCompletion{Value: 32768})
	if reply := h.reply(f, correlation); reply.Value != 32768 {
		t.Fatalf("SO_RCVBUF %+v", reply)
	}
}
