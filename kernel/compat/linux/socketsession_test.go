package linux

import (
	"testing"

	"goos/kernel/ipc"
	"goos/kernel/sched"
	"goos/userspace/linuxsockapi"
)

// fakeService is the service end of a family session over a real channel:
// the kernel side is 0, the service side 1.
type fakeService struct {
	t       *testing.T
	channel *ipc.Channel
}

func (service *fakeService) send(side uint8, message *ipc.Message) ipc.Status {
	service.channel.Lock()
	defer service.channel.Unlock()
	return service.channel.SendFrom(side, message, nil, nil, nil)
}

func (service *fakeService) receive(side uint8, message *ipc.Message) ipc.Status {
	service.channel.Lock()
	defer service.channel.Unlock()
	return service.channel.ReceiveInto(side, message)
}

// request takes the next request the kernel sent.
func (service *fakeService) request() (linuxsockapi.Request, uint64) {
	service.t.Helper()
	var message ipc.Message
	if status := service.receive(1, &message); status != ipc.StatusOK {
		service.t.Fatalf("service receive: %v", status)
	}
	wired := roundTripMessage(service.t, &message)
	var request linuxsockapi.Request
	correlation, ok := linuxsockapi.DecodeRequest(wired, &request)
	if !ok {
		service.t.Fatalf("service decode %+v", message)
	}
	return request, correlation
}

func (service *fakeService) reply(reply linuxsockapi.Reply, correlation uint64) {
	service.t.Helper()
	var message ipc.Message
	if !linuxsockapi.EncodeReply(&reply, correlation, &message) {
		service.t.Fatalf("service encode %+v", reply)
	}
	if status := service.send(1, &message); status != ipc.StatusOK {
		service.t.Fatalf("service send: %v", status)
	}
}

func roundTripMessage(t *testing.T, message *ipc.Message) *ipc.Message {
	t.Helper()
	var wire [ipc.WireMaxSize]byte
	size, ok := ipc.EncodeFrom(message, wire[:])
	if !ok {
		t.Fatal("wire encode")
	}
	var out ipc.Message
	if !ipc.DecodeInto(wire[:size], &out) {
		t.Fatal("wire decode")
	}
	return &out
}

const testFamily = uint64(0x5150)

func newTestSession(t *testing.T) (*fakeService, NetEndpoint) {
	t.Helper()
	vfsContexts = [len(vfsContexts)]LinuxVFSContext{}
	for index := range netContexts {
		netContexts[index].reset(0)
	}
	tablesLock.Lock()
	installed := installVFSContext(testFamily) && installNetContextHeld(testFamily)
	tablesLock.Unlock()
	if !installed {
		t.Fatal("install contexts")
	}
	if _, live, _ := NetSession(testFamily); live {
		t.Fatal("a family without a session reports one")
	}
	grant := NetGrant{Holder: 7, ChannelHandle: 3, PoolHandle: 4, Channel: 0x9000, Side: 0, PoolObject: 0x77}
	if _, _, ok := AttachNet(testFamily, grant, false); !ok {
		t.Fatal("attach")
	}
	endpoint, live, _ := NetSession(testFamily)
	if !live || endpoint.Channel != 0x9000 || endpoint.Session == 0 {
		t.Fatalf("session %+v live %v", endpoint, live)
	}
	t.Cleanup(func() {
		tablesLock.Lock()
		releaseNetContextHeld(testFamily)
		releaseVFSContextHeld(testFamily)
		tablesLock.Unlock()
	})
	return &fakeService{t: t, channel: ipc.NewChannel()}, endpoint
}

// issue sends one request of task the way the kernel does.
func issue(t *testing.T, service *fakeService, endpoint NetEndpoint, task sched.TaskID, request linuxsockapi.Request) (int, uint64) {
	t.Helper()
	index, correlation, status := BeginRequest(testFamily, endpoint.Session, task, &request, 100, true)
	if status != RequestStarted {
		t.Fatalf("begin %v", status)
	}
	var message ipc.Message
	if !linuxsockapi.EncodeRequest(&request, correlation, &message) {
		t.Fatalf("encode %+v", request)
	}
	if status := service.send(0, &message); status != ipc.StatusOK {
		t.Fatalf("kernel send %v", status)
	}
	return index, correlation
}

// drain classifies the next message as the pump does and returns its verdict.
func drain(t *testing.T, service *fakeService, endpoint NetEndpoint, reply *linuxsockapi.Reply) (MessageVerdict, sched.TaskID, uint64) {
	t.Helper()
	var message ipc.Message
	if status := service.receive(0, &message); status != ipc.StatusOK {
		t.Fatalf("kernel receive %v", status)
	}
	return ClassifySocketMessage(testFamily, endpoint.Session, roundTripMessage(t, &message), reply)
}

const testTask = sched.TaskID(0x10003)

func TestSocketSessionOpenReply(t *testing.T) {
	service, endpoint := newTestSession(t)
	_, correlation := issue(t, service, endpoint, testTask, linuxsockapi.Request{Opcode: linuxsockapi.OpOpen, Length: linuxsockapi.KindStream})
	request, seen := service.request()
	if request.Opcode != linuxsockapi.OpOpen || seen != correlation || correlation < firstSocketCorrelation {
		t.Fatalf("service saw %+v correlation %d", request, seen)
	}
	identity := linuxsockapi.MakeSocket(5, 1, 2)
	service.reply(linuxsockapi.Reply{Opcode: linuxsockapi.OpOpen, Socket: identity}, seen)
	var reply linuxsockapi.Reply
	if wait, _, _ := TakeReply(testFamily, endpoint.Session, correlation, &reply, nil); wait != ReplyPending {
		t.Fatalf("reply taken before it came: %v", wait)
	}
	verdict, owner, got := drain(t, service, endpoint, &reply)
	if verdict != MessageReply || owner != testTask || got != correlation {
		t.Fatalf("verdict %v owner %v correlation %d", verdict, owner, got)
	}
	if !DeliverReply(testFamily, correlation, &reply) {
		t.Fatal("deliver")
	}
	var taken linuxsockapi.Reply
	if wait, _, _ := TakeReply(testFamily, endpoint.Session, correlation, &taken, nil); wait != ReplyTaken || taken.Socket != identity {
		t.Fatalf("take %v %+v", wait, taken)
	}
	if wait, _, _ := TakeReply(testFamily, endpoint.Session, correlation, &taken, nil); wait != ReplyGone {
		t.Fatal("a reply was taken twice")
	}
	object, created := NewSocket(testFamily, endpoint.Session, identity, false)
	if !created {
		t.Fatal("new socket")
	}
	socket, family, live, found := SocketOf(object)
	if !found || !live || family != testFamily || socket.Identity != identity || socket.Datagram {
		t.Fatalf("socket %+v live %v", socket, live)
	}
	// A second Open answering with the same identity is a broken service.
	_, again := issue(t, service, endpoint, testTask, linuxsockapi.Request{Opcode: linuxsockapi.OpOpen, Length: linuxsockapi.KindStream})
	service.request()
	service.reply(linuxsockapi.Reply{Opcode: linuxsockapi.OpOpen, Socket: identity}, again)
	if verdict, _, _ := drain(t, service, endpoint, &reply); verdict != MessageMalformed {
		t.Fatalf("duplicate identity accepted: %v", verdict)
	}
	closed, closeFamily, send := CloseSocket(object)
	if closed != identity || closeFamily != testFamily || !send {
		t.Fatalf("close %x %d %v", closed, closeFamily, send)
	}
	if _, _, _, found := SocketOf(object); found {
		t.Fatal("a closed socket's description still resolves")
	}
	reused, _ := NewSocket(testFamily, endpoint.Session, linuxsockapi.MakeSocket(5, 2, 2), false)
	if reused == object {
		t.Fatal("a reused slot matches the old description")
	}
}

func TestSocketSessionAbandonedReplies(t *testing.T) {
	service, endpoint := newTestSession(t)
	socket := linuxsockapi.MakeSocket(5, 1, 4)
	var reply linuxsockapi.Reply
	var seen uint64
	// An accept whose caller is gone: the child is closed.
	_, accept := issue(t, service, endpoint, testTask, linuxsockapi.Request{Opcode: linuxsockapi.OpAccept, Socket: socket})
	_, seen = service.request()
	child := linuxsockapi.MakeSocket(5, 3, 9)
	service.reply(linuxsockapi.Reply{Opcode: linuxsockapi.OpAccept, Socket: socket, Value: child, Port: 4000}, seen)
	if verdict, owner, _ := drain(t, service, endpoint, &reply); verdict != MessageReply || owner != testTask {
		t.Fatal("accept reply")
	}
	if cleanup, _ := AbandonReply(testFamily, accept, &reply, nil); cleanup.Close != child {
		t.Fatalf("accept cleanup %+v", cleanup)
	}
	// A reply already filed whose caller then leaves is applied at once.
	_, open := issue(t, service, endpoint, testTask, linuxsockapi.Request{Opcode: linuxsockapi.OpOpen, Length: linuxsockapi.KindDatagram})
	_, seen = service.request()
	opened := linuxsockapi.MakeSocket(5, 4, 10)
	service.reply(linuxsockapi.Reply{Opcode: linuxsockapi.OpOpen, Socket: opened}, seen)
	drain(t, service, endpoint, &reply)
	DeliverReply(testFamily, open, &reply)
	if cleanup, _ := AbandonRequest(testFamily, open, nil); cleanup.Close != opened {
		t.Fatalf("filed open cleanup %+v", cleanup)
	}
}

func TestSocketSessionRejectsMismatchedReplies(t *testing.T) {
	service, endpoint := newTestSession(t)
	socket := linuxsockapi.MakeSocket(5, 1, 4)
	_, correlation := issue(t, service, endpoint, testTask, linuxsockapi.Request{Opcode: linuxsockapi.OpBind, Socket: socket})
	_, seen := service.request()
	service.reply(linuxsockapi.Reply{Opcode: linuxsockapi.OpListen, Socket: socket}, seen)
	var reply linuxsockapi.Reply
	if verdict, _, _ := drain(t, service, endpoint, &reply); verdict != MessageMalformed {
		t.Fatalf("wrong opcode accepted: %v", verdict)
	}
	// The other socket of the family is not this request's.
	_, write := issue(t, service, endpoint, testTask, linuxsockapi.Request{Opcode: linuxsockapi.OpWrite, Socket: socket})
	_, seen = service.request()
	service.reply(linuxsockapi.Reply{Opcode: linuxsockapi.OpWrite, Socket: linuxsockapi.MakeSocket(5, 1, 5)}, seen)
	if verdict, _, _ := drain(t, service, endpoint, &reply); verdict != MessageMalformed {
		t.Fatalf("another socket's reply accepted: %v", verdict)
	}
	_ = correlation
	_ = write
	// A reply carrying a handle fails the session.
	message := ipc.Message{ProtocolID: linuxsockapi.ProtocolID, Version: linuxsockapi.Version, Kind: ipc.KindReply, Correlation: 9, HandleCount: 1}
	if verdict, _, _ := ClassifySocketMessage(testFamily, endpoint.Session, &message, &reply); verdict != MessageMalformed {
		t.Fatal("a reply with a handle accepted")
	}
	// A ReadyChanged notification is recognized.
	var notification ipc.Message
	if !linuxsockapi.EncodeRequest(&linuxsockapi.Request{Opcode: linuxsockapi.OpReadyChanged}, 0, &notification) {
		t.Fatal("notification encode")
	}
	if verdict, _, _ := ClassifySocketMessage(testFamily, endpoint.Session, roundTripMessage(t, &notification), &reply); verdict != MessageReadyChanged {
		t.Fatalf("ReadyChanged: %v", verdict)
	}
}

func TestSocketSessionBoundsRequestsAndWakesWaiters(t *testing.T) {
	_, endpoint := newTestSession(t)
	var correlations [MaxSocketRequests]uint64
	for index := range correlations {
		request := linuxsockapi.Request{Opcode: linuxsockapi.OpQueryReady, Socket: linuxsockapi.MakeSocket(5, 1, 1)}
		_, correlation, status := BeginRequest(testFamily, endpoint.Session, testTask, &request, 1, true)
		if status != RequestStarted {
			t.Fatalf("request %d: %v", index, status)
		}
		correlations[index] = correlation
	}
	waiter := sched.TaskID(0x20004)
	request := linuxsockapi.Request{Opcode: linuxsockapi.OpQueryReady, Socket: linuxsockapi.MakeSocket(5, 1, 1)}
	if _, _, status := BeginRequest(testFamily, endpoint.Session, waiter, &request, 1, true); status != RequestBusy || RequestSlotFree(testFamily) {
		t.Fatalf("ninth request: %v", status)
	}
	var woken [4]sched.TaskID
	if count := CancelRequest(testFamily, correlations[0], woken[:]); count != 1 || woken[0] != waiter {
		t.Fatalf("woken %d %v", count, woken)
	}
	if !RequestSlotFree(testFamily) {
		t.Fatal("no slot after a cancel")
	}
	// A waiter that gives up is not woken.
	BeginRequest(testFamily, endpoint.Session, waiter, &request, 1, true)
	BeginRequest(testFamily, endpoint.Session, waiter, &request, 1, true)
	RemoveSocketWaiter(waiter)
	if count := CancelRequest(testFamily, correlations[1], woken[:]); count != 0 {
		t.Fatalf("a removed waiter woke: %d", count)
	}
}

func TestSocketSessionFailureAndReattach(t *testing.T) {
	service, endpoint := newTestSession(t)
	identity := linuxsockapi.MakeSocket(5, 1, 2)
	object, _ := NewSocket(testFamily, endpoint.Session, identity, true)
	_, correlation := issue(t, service, endpoint, testTask, linuxsockapi.Request{Opcode: linuxsockapi.OpQueryReady, Socket: identity})
	if _, _, failed := FailNet(testFamily, endpoint.Session, nil); !failed {
		t.Fatal("fail")
	}
	if _, _, again := FailNet(testFamily, endpoint.Session, nil); again {
		t.Fatal("a session failed twice")
	}
	if _, _, live, _ := SocketOf(object); live {
		t.Fatal("a socket of a failed session is live")
	}
	if _, _, status := BeginRequest(testFamily, endpoint.Session, testTask, &linuxsockapi.Request{Opcode: linuxsockapi.OpOpen}, 1, true); status != RequestFailed {
		t.Fatal("a failed session took a request")
	}
	var reply linuxsockapi.Reply
	if wait, _, _ := TakeReply(testFamily, endpoint.Session, correlation, &reply, nil); wait != ReplyGone {
		t.Fatal("a failed session's reply wait goes on")
	}
	if _, live, _ := NetSession(testFamily); live {
		t.Fatal("failed session live")
	}
	if _, send := closeOf(object); send {
		t.Fatal("closing a socket of a dead session sends Close")
	}
	grant := NetGrant{Holder: 7, ChannelHandle: 5, PoolHandle: 6, Channel: 0xa000, Side: 0, PoolObject: 0x78}
	old, replaced, ok := AttachNet(testFamily, grant, false)
	if !ok || !replaced || old.ChannelHandle != 3 || old.PoolHandle != 4 {
		t.Fatalf("reattach %+v %v %v", old, replaced, ok)
	}
	fresh, live, _ := NetSession(testFamily)
	if !live || fresh.Session == endpoint.Session || fresh.Channel != 0xa000 {
		t.Fatalf("fresh session %+v", fresh)
	}
	if _, _, ok := AttachNet(testFamily, grant, false); ok {
		t.Fatal("a live session was replaced")
	}
	if _, created := NewSocket(testFamily, endpoint.Session, identity, false); created {
		t.Fatal("a socket joined an old session")
	}
}

func closeOf(object uint64) (uint64, bool) {
	identity, _, send := CloseSocket(object)
	return identity, send
}

func TestSocketDeferredCloses(t *testing.T) {
	_, _ = newTestSession(t)
	for index := 0; index < pendingCloseSlots; index++ {
		if !DeferClose(testFamily, uint64(index+1)) {
			t.Fatalf("defer %d", index)
		}
	}
	if DeferClose(testFamily, 99) {
		t.Fatal("deferred closes grew past their bound")
	}
	count := 0
	for {
		if _, found := TakeDeferredClose(testFamily); !found {
			break
		}
		count++
	}
	if count != pendingCloseSlots {
		t.Fatalf("took %d", count)
	}
}
