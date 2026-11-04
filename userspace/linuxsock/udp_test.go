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
	if reply := h.call(f, linuxsockapi.Request{Opcode: opRead, Socket: id}); reply.Result != linuxsockapi.ResultWouldBlock || h.has(NativeRead) {
		t.Fatalf("read unbound %+v", reply)
	}
	if reply := h.call(f, linuxsockapi.Request{Opcode: opWrite, Socket: id}); reply.Result != linuxsockapi.ResultDestinationRequired {
		t.Fatalf("send unconnected %d", reply.Result)
	}
	correlation := h.send(f, linuxsockapi.Request{Opcode: opSendTo, Socket: id, Address: [4]byte{127, 0, 0, 1}, Port: 53})
	h.complete(h.take(NativeBind), NativeCompletion{LocalPort: 33333})
	if reply := h.reply(f, correlation); reply.Result != linuxsockapi.ResultOK {
		t.Fatalf("sendto check %+v", reply)
	}
	h.commit(f, 2, id, []byte("query"), true, [4]byte{127, 0, 0, 1}, 53)
	send := h.take(NativeSendTo)
	if !bytes.Equal(send.data, []byte("query")) || send.request.Port != 53 {
		t.Fatalf("sendto %+v %q", send.request, send.data)
	}
	h.complete(send, NativeCompletion{})
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
	if reply := h.call(f, linuxsockapi.Request{Opcode: opSendTo, Socket: id, Address: [4]byte{1, 1, 1, 1}, Port: 53}); reply.Result != linuxsockapi.ResultIsConnected {
		t.Fatalf("sendto another peer %d", reply.Result)
	}
	if reply := h.call(f, linuxsockapi.Request{Opcode: opSendTo, Socket: id, Address: peerAddress, Port: 53}); reply.Result != linuxsockapi.ResultOK {
		t.Fatalf("sendto the peer %d", reply.Result)
	}
	if state := h.readiness(f, id); state.State != linuxsockapi.EntrySend|linuxsockapi.EntryConnected {
		t.Fatalf("connected datagram state %+v", state)
	}
	// netd refuses the datagram; the error waits in SO_ERROR.
	h.commit(f, 1, id, []byte("x"), true, peerAddress, 53)
	h.complete(h.take(NativeWrite), NativeCompletion{Result: NativeMessageSize})
	if state := h.readiness(f, id); state.Error != linuxsockapi.ResultMessageSize {
		t.Fatalf("refused datagram %+v", state)
	}
	if reply := h.call(f, linuxsockapi.Request{Opcode: linuxsockapi.OpGetPeerName, Socket: id}); reply.Address != peerAddress || reply.Port != 53 {
		t.Fatalf("peer %+v", reply)
	}
	if reply := h.call(f, linuxsockapi.Request{Opcode: opShutdown, Socket: id, Length: shutWrite}); reply.Result != linuxsockapi.ResultOK {
		t.Fatalf("shutdown %d", reply.Result)
	}
	if reply := h.call(f, linuxsockapi.Request{Opcode: opWrite, Socket: id}); reply.Result != linuxsockapi.ResultClosed {
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
	// GUDP reports no buffer size; the service answers netd's bounds.
	if reply := h.call(f, linuxsockapi.Request{Opcode: linuxsockapi.OpSetOption, Socket: id, Level: LevelSocket, Name: OptionRecvBuf, Value: 4096}); reply.Value != DatagramReceiveBuffer {
		t.Fatalf("SO_RCVBUF %+v", reply)
	}
	if reply := h.call(f, linuxsockapi.Request{Opcode: linuxsockapi.OpGetOption, Socket: id, Level: LevelSocket, Name: OptionSendBuf}); reply.Value != DatagramSendBuffer {
		t.Fatalf("SO_SNDBUF %+v", reply)
	}
	if h.has(NativeOption) {
		t.Fatal("datagram buffer asked of netd")
	}
}
