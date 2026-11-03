package linuxsock

import (
	"testing"

	"goos/kernel/ipc"
	"goos/userspace/linuxsockapi"
)

func TestOptions(t *testing.T) {
	h := newHarness(t)
	f := h.family(1, 7)
	id := h.connected(f)
	get := func(level, name uint32) linuxsockapi.Reply {
		return h.call(f, linuxsockapi.Request{Opcode: linuxsockapi.OpGetOption, Socket: id, Level: level, Name: name})
	}
	set := func(level, name uint32, value uint64) linuxsockapi.Reply {
		return h.call(f, linuxsockapi.Request{Opcode: linuxsockapi.OpSetOption, Socket: id, Level: level, Name: name, Value: value})
	}
	if reply := get(LevelTCP, OptionNoDelay); reply.Result != linuxsockapi.ResultOK || reply.Value != 1 {
		t.Fatalf("TCP_NODELAY %+v", reply)
	}
	if reply := set(LevelTCP, OptionNoDelay, 1); reply.Result != linuxsockapi.ResultOK {
		t.Fatalf("set TCP_NODELAY 1 %d", reply.Result)
	}
	if reply := set(LevelTCP, OptionNoDelay, 0); reply.Result != linuxsockapi.ResultNotSupported {
		t.Fatalf("set TCP_NODELAY 0 %d", reply.Result)
	}
	if reply := set(LevelTCP, OptionMaxSeg, 500); reply.Result != linuxsockapi.ResultNotSupported {
		t.Fatalf("set TCP_MAXSEG %d", reply.Result)
	}
	if reply := set(LevelSocket, OptionReuseAddr, 1); reply.Value != 1 {
		t.Fatalf("SO_REUSEADDR %+v", reply)
	}
	if reply := get(LevelSocket, OptionReuseAddr); reply.Value != 1 {
		t.Fatalf("SO_REUSEADDR read back %+v", reply)
	}
	if reply := get(LevelSocket, 15); reply.Result != linuxsockapi.ResultNoOption { // SO_REUSEPORT
		t.Fatalf("SO_REUSEPORT %d", reply.Result)
	}
	if reply := get(LevelSocket, 9); reply.Result != linuxsockapi.ResultNoOption { // SO_KEEPALIVE
		t.Fatalf("SO_KEEPALIVE %d", reply.Result)
	}
	correlation := h.send(f, linuxsockapi.Request{Opcode: linuxsockapi.OpGetOption, Socket: id, Level: LevelTCP, Name: OptionMaxSeg})
	mss := h.take(NativeOption)
	if mss.request.Option != OptionMSS {
		t.Fatal("TCP_MAXSEG asks netd for the MSS")
	}
	h.complete(mss, NativeCompletion{Value: 1460})
	if reply := h.reply(f, correlation); reply.Value != 1460 {
		t.Fatalf("TCP_MAXSEG %+v", reply)
	}
	correlation = h.send(f, linuxsockapi.Request{Opcode: linuxsockapi.OpSetOption, Socket: id, Level: LevelIP, Name: OptionIPTTL, Value: 64})
	h.complete(h.take(NativeOption), NativeCompletion{Value: 64})
	if reply := h.reply(f, correlation); reply.Result != linuxsockapi.ResultOK || reply.Value != 64 {
		t.Fatalf("IP_TTL unchanged %+v", reply)
	}
	correlation = h.send(f, linuxsockapi.Request{Opcode: linuxsockapi.OpSetOption, Socket: id, Level: LevelIP, Name: OptionIPTTL, Value: 5})
	h.complete(h.take(NativeOption), NativeCompletion{Value: 64})
	if reply := h.reply(f, correlation); reply.Result != linuxsockapi.ResultNotSupported {
		t.Fatalf("IP_TTL change %d", reply.Result)
	}
	correlation = h.send(f, linuxsockapi.Request{Opcode: linuxsockapi.OpSetOption, Socket: id, Level: LevelSocket, Name: OptionSendBuf, Value: 1 << 30})
	h.complete(h.take(NativeOption), NativeCompletion{Value: 65536})
	if reply := h.reply(f, correlation); reply.Result != linuxsockapi.ResultOK || reply.Value != 65536 {
		t.Fatalf("SO_SNDBUF clamp %+v", reply)
	}
	// SO_REUSEADDR set on a socket goes with its bind.
	other := h.open(f, linuxsockapi.KindStream)
	h.call(f, linuxsockapi.Request{Opcode: linuxsockapi.OpSetOption, Socket: other, Level: LevelSocket, Name: OptionReuseAddr, Value: 1})
	h.send(f, linuxsockapi.Request{Opcode: opBind, Socket: other, Port: 8080})
	if bind := h.take(NativeBind); bind.request.Flags != NativeReuseAddr {
		t.Fatalf("bind flags %d", bind.request.Flags)
	}
	h.send(f, linuxsockapi.Request{Opcode: opBind, Socket: h.open(f, linuxsockapi.KindStream), Flags: linuxsockapi.FlagReuseAddr, Port: 8081})
	if bind := h.take(NativeBind); bind.request.Flags != NativeReuseAddr {
		t.Fatal("bind flag not passed")
	}
}

func TestStaleAndCrossFamilyIdentities(t *testing.T) {
	h := newHarness(t)
	a := h.family(1, 7)
	b := h.family(2, 8)
	idA := h.open(a, linuxsockapi.KindStream)
	idB := h.open(b, linuxsockapi.KindStream)
	if reply := h.call(a, linuxsockapi.Request{Opcode: linuxsockapi.OpGetSockName, Socket: idB}); reply.Result != linuxsockapi.ResultStale {
		t.Fatalf("other family's socket %d", reply.Result)
	}
	index, _ := linuxsockapi.SocketIndex(idA)
	forged := linuxsockapi.MakeSocket(7, 99, index)
	if reply := h.call(a, linuxsockapi.Request{Opcode: linuxsockapi.OpGetSockName, Socket: forged}); reply.Result != linuxsockapi.ResultStale {
		t.Fatalf("other generation %d", reply.Result)
	}
	if reply := h.call(a, linuxsockapi.Request{Opcode: linuxsockapi.OpGetSockName, Socket: linuxsockapi.MakeSocket(7, 1, 20)}); reply.Result != linuxsockapi.ResultStale {
		t.Fatalf("free slot %d", reply.Result)
	}
	if _, result := h.svc.AddFamily(3, 7, make([]byte, linuxsockapi.PoolBytes)); result != linuxsockapi.ControlInvalidArgument {
		t.Fatal("a session generation in use was accepted twice")
	}
	if _, result := h.svc.AddFamily(1, 9, make([]byte, linuxsockapi.PoolBytes)); result != linuxsockapi.ControlInvalidArgument {
		t.Fatal("a token in use was accepted twice")
	}
	if _, result := h.svc.AddFamily(4, 9, make([]byte, linuxsockapi.PoolBytes-1)); result != linuxsockapi.ControlInvalidArgument {
		t.Fatal("a short pool was accepted")
	}
}

func TestMalformedMessageFailsOnlyThatFamily(t *testing.T) {
	h := newHarness(t)
	a := h.family(1, 7)
	b := h.family(2, 8)
	idA := h.connected(a)
	endpoint := h.endpoint(idA)
	idB := h.open(b, linuxsockapi.KindStream)
	var message ipc.Message
	request := linuxsockapi.Request{Opcode: linuxsockapi.OpGetSockName, Socket: idA}
	linuxsockapi.EncodeRequest(&request, 1, &message)
	message.Payload[3] = 1 // a reserved byte
	h.svc.Receive(a, &message)
	if !h.dropped(a) || h.dropped(b) || h.svc.Family(a) {
		t.Fatal("malformed message did not end exactly its family")
	}
	if closed := h.take(NativeClose); closed.request.Endpoint != endpoint {
		t.Fatal("the failed family's endpoint stayed open")
	}
	if reply := h.call(b, linuxsockapi.Request{Opcode: linuxsockapi.OpGetSockName, Socket: idB}); reply.Result != linuxsockapi.ResultOK {
		t.Fatalf("other family %d", reply.Result)
	}
	if h.svc.Counters().SessionFailures != 1 {
		t.Fatal("session failure not counted")
	}
	// A Release of a slot the service never lent fails the family, as does
	// a ReadyChanged from the kernel.
	c := h.family(3, 9)
	h.send(c, linuxsockapi.Request{Opcode: opRelease, Socket: linuxsockapi.MakeSocket(9, 1, 0), Slot: 2})
	if !h.dropped(c) {
		t.Fatal("forged Release accepted")
	}
	d := h.family(4, 10)
	h.send(d, linuxsockapi.Request{Opcode: linuxsockapi.OpReadyChanged})
	if !h.dropped(d) {
		t.Fatal("ReadyChanged from the kernel accepted")
	}
}

func TestRemoveFamilyClosesEverything(t *testing.T) {
	h := newHarness(t)
	f := h.family(1, 7)
	stream := h.connected(f)
	datagram := boundDatagram(h, f)
	endpoints := map[uint32]bool{h.endpoint(stream): true, h.endpoint(datagram): true}
	// An open in flight completes after the family is gone.
	h.send(f, linuxsockapi.Request{Opcode: opOpen, Length: linuxsockapi.KindStream})
	late := h.take(NativeOpen)
	h.svc.RemoveFamily(f)
	if !h.dropped(f) || h.svc.families[f].used {
		t.Fatal("family not dropped")
	}
	for range 2 {
		closed := h.take(NativeClose)
		if !endpoints[closed.request.Endpoint] {
			t.Fatalf("closed %d", closed.request.Endpoint)
		}
		delete(endpoints, closed.request.Endpoint)
	}
	h.complete(late, NativeCompletion{Endpoint: 4242})
	if closed := h.take(NativeClose); closed.request.Endpoint != 4242 {
		t.Fatal("late endpoint leaked")
	}
	for index := range h.svc.sockets {
		if h.svc.sockets[index].used {
			t.Fatal("socket left")
		}
	}
	for index := range h.svc.ops {
		if h.svc.ops[index].state != opFree {
			t.Fatalf("op left %+v", h.svc.ops[index])
		}
	}
	// The table entry is reused by the next family.
	if again := h.family(2, 8); again != f {
		t.Fatalf("family %d", again)
	}
}

func TestLimits(t *testing.T) {
	h := newHarness(t)
	a := h.family(1, 7)
	b := h.family(2, 8)
	c := h.family(3, 9)
	for range linuxsockapi.MaxSockets {
		h.open(a, linuxsockapi.KindStream)
	}
	if reply := h.call(a, linuxsockapi.Request{Opcode: opOpen, Length: linuxsockapi.KindDatagram}); reply.Result != linuxsockapi.ResultFamilyLimit || reply.Socket != 0 {
		t.Fatalf("family limit %+v", reply)
	}
	for range MaxSockets - linuxsockapi.MaxSockets {
		h.open(b, linuxsockapi.KindDatagram)
	}
	if reply := h.call(c, linuxsockapi.Request{Opcode: opOpen, Length: linuxsockapi.KindStream}); reply.Result != linuxsockapi.ResultServiceLimit {
		t.Fatalf("service limit %d", reply.Result)
	}
	if h.has(NativeOpen) {
		t.Fatal("asked netd for an endpoint past the limit")
	}
	for index := 0; index < MaxFamilies-3; index++ {
		h.family(uint64(10+index), uint32(10+index))
	}
	if _, result := h.svc.AddFamily(99, 99, make([]byte, linuxsockapi.PoolBytes)); result != linuxsockapi.ControlBusy {
		t.Fatalf("family table %d", result)
	}
}

func TestOpsPerFamilyBound(t *testing.T) {
	h := newHarness(t)
	f := h.family(1, 7)
	for range FamilyOps {
		h.send(f, linuxsockapi.Request{Opcode: opOpen, Length: linuxsockapi.KindStream})
	}
	if h.svc.CanReceive(f) {
		t.Fatal("receives past FamilyOps")
	}
	h.complete(h.take(NativeOpen), NativeCompletion{Endpoint: 1})
	if !h.svc.CanReceive(f) {
		t.Fatal("a completed request did not free its place")
	}
}

func TestBusyBackendRetries(t *testing.T) {
	h := newHarness(t)
	f := h.family(1, 7)
	id := h.connected(f)
	other := h.connected(f)
	h.backend.busy = true
	correlation := h.send(f, linuxsockapi.Request{Opcode: opWrite, Socket: id, Slot: 0, Length: 3})
	h.noReply(f, correlation)
	h.backend.busy = false
	// Any later completion from the backend retries what waited.
	h.wake(h.endpoint(other), MaskWritable, 0)
	h.complete(h.take(NativeWrite), NativeCompletion{Length: 3})
	if reply := h.reply(f, correlation); reply.Length != 3 {
		t.Fatalf("retried write %+v", reply)
	}
}

func TestFamiliesProgressIndependently(t *testing.T) {
	h := newHarness(t)
	a := h.family(1, 7)
	b := h.family(2, 8)
	idA := h.connected(a)
	idB := h.connected(b)
	readA := h.send(a, linuxsockapi.Request{Opcode: opRead, Socket: idA, Length: 10})
	readB := h.send(b, linuxsockapi.Request{Opcode: opRead, Socket: idB, Length: 10})
	nativeA, nativeB := h.take(NativeRead), h.take(NativeRead)
	if nativeA.request.Endpoint != h.endpoint(idA) {
		nativeA, nativeB = nativeB, nativeA
	}
	h.complete(nativeB, NativeCompletion{Data: []byte("b")})
	h.reply(b, readB)
	h.noReply(a, readA)
	h.complete(nativeA, NativeCompletion{Result: NativeWouldBlock})
	if reply := h.reply(a, readA); reply.Result != linuxsockapi.ResultWouldBlock {
		t.Fatalf("a %d", reply.Result)
	}
}

func TestReadsParkedOnReleaseAreBounded(t *testing.T) {
	h := newHarness(t)
	f := h.family(1, 7)
	var sockets []uint64
	for range linuxsockapi.RXSlots {
		id := h.connected(f)
		correlation := h.send(f, linuxsockapi.Request{Opcode: opRead, Socket: id, Length: 10})
		h.complete(h.take(NativeRead), NativeCompletion{Data: []byte("x")})
		h.reply(f, correlation)
		sockets = append(sockets, id)
	}
	// Every RX slot is with the kernel: new reads wait, up to FamilyParked.
	fresh := h.connected(f)
	var parked []uint64
	for range FamilyParked {
		parked = append(parked, h.send(f, linuxsockapi.Request{Opcode: opRead, Socket: fresh, Length: 10}))
	}
	if reply := h.call(f, linuxsockapi.Request{Opcode: opRead, Socket: fresh, Length: 10}); reply.Result != linuxsockapi.ResultWouldBlock {
		t.Fatalf("read past the parking bound %d", reply.Result)
	}
	if !h.svc.CanReceive(f) {
		t.Fatal("parked reads stopped the family's channel")
	}
	h.send(f, linuxsockapi.Request{Opcode: opRelease, Socket: sockets[0], Slot: 0})
	read := h.take(NativeRead)
	h.complete(read, NativeCompletion{Result: NativeWouldBlock})
	if reply := h.reply(f, parked[0]); reply.Result != linuxsockapi.ResultWouldBlock {
		t.Fatalf("resumed read %d", reply.Result)
	}
}

func TestNoAnswerWithoutNative(t *testing.T) {
	h := newHarness(t)
	f := h.family(1, 7)
	if reply := h.call(f, linuxsockapi.Request{Opcode: linuxsockapi.OpCancel, Value: 5}); reply.Result != linuxsockapi.ResultOK || reply.Socket != 0 {
		t.Fatalf("cancel %+v", reply)
	}
	id := h.open(f, linuxsockapi.KindStream)
	correlation := h.send(f, linuxsockapi.Request{Opcode: linuxsockapi.OpQueryReady, Socket: id})
	h.complete(h.take(NativeReady), NativeCompletion{Mask: MaskWritable})
	if reply := h.reply(f, correlation); linuxsockapi.Ready(reply.Value) != linuxsockapi.ReadyOut|linuxsockapi.ReadyHangup {
		t.Fatalf("query %+v", reply)
	}
}
