package utility

import (
	"bytes"
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	"goos/kernel/vfs"
	"goos/userspace/tcpapi"
	"goos/userspace/udpapi"
	"sync"
	"testing"
)

// scriptedIPC is a message transport, not a second transaction implementation.
// Tests drive the very exchange used by native CallVFS and attachedVFS.
type scriptedIPC struct{ service *scriptedService }
type scriptedService struct {
	mu         sync.Mutex
	status     kcall.Status
	sendStatus kcall.Status
	raw        []byte
	reply      func(ipc.Message) ipc.Message
	sent       []handle.ID
	last       map[handle.ID]ipc.Message
	queued     map[handle.ID][]ipc.Message
	closed     []handle.ID
	entered    chan handle.ID
	resume     chan struct{}
	waits      []uint64
}

func newScript() *scriptedService {
	return &scriptedService{last: make(map[handle.ID]ipc.Message), queued: make(map[handle.ID][]ipc.Message)}
}

func (s scriptedIPC) send(channel handle.ID, wire []byte) kcall.Status {
	request, ok := ipc.Decode(wire)
	if !ok {
		return kcall.StatusInvalidArgument
	}
	s.service.mu.Lock()
	defer s.service.mu.Unlock()
	if s.service.sendStatus != kcall.StatusOK {
		return s.service.sendStatus
	}
	s.service.sent = append(s.service.sent, channel)
	s.service.last[channel] = request
	return kcall.StatusOK
}

func (s scriptedIPC) waitReceive(channel handle.ID, wire []byte, ticks uint64) kcall.Status {
	service := s.service
	if service.entered != nil {
		service.entered <- channel
		<-service.resume
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	service.waits = append(service.waits, ticks)
	if service.status != kcall.StatusOK {
		return service.status
	}
	if service.raw != nil {
		copy(wire, service.raw)
		return kcall.StatusOK
	}
	var reply ipc.Message
	if queue := service.queued[channel]; len(queue) != 0 {
		reply, service.queued[channel] = queue[0], queue[1:]
	} else {
		reply = service.reply(service.last[channel])
	}
	if _, ok := ipc.Encode(reply, wire); !ok {
		panic("invalid scripted reply")
	}
	return kcall.StatusOK
}

func (s scriptedIPC) close(id handle.ID) {
	s.service.mu.Lock()
	s.service.closed = append(s.service.closed, id)
	s.service.mu.Unlock()
}

type testPaths struct {
	mu      sync.Mutex
	memory  []byte
	retired [][]byte
	next    handle.ID
}

func (p *testPaths) Hold()    { p.mu.Lock() }
func (p *testPaths) Release() { p.mu.Unlock() }
func (p *testPaths) Prepare(request *vfs.Request) (handle.ID, bool) {
	if vfs.FitsInline(*request) && !request.NameReturn {
		return 0, true
	}
	if p.memory == nil {
		p.memory = make([]byte, 8192)
	}
	copy(p.memory, request.Path)
	request.PathsAttached = !vfs.FitsInline(*request)
	p.next++
	return p.next, true
}
func (p *testPaths) Name(size int) []byte { return p.memory[:size] }
func (p *testPaths) Abandon() {
	p.retired = append(p.retired, p.memory)
	p.memory = nil
}

func replyTo(request ipc.Message, name, data []byte) ipc.Message {
	reply, ok := vfs.EncodeReply(vfs.Reply{Operation: vfs.Operation(request.Payload[0]), Status: vfs.StatusOK, Name: name, Data: data}, request.Correlation)
	if !ok {
		panic("invalid VFS fixture")
	}
	return reply
}

func newFiles(service *scriptedService) *vfsSession[scriptedIPC, *testPaths] {
	s := &vfsSession[scriptedIPC, *testPaths]{paths: &testPaths{}}
	s.io = scriptedIPC{service}
	s.bind(7)
	return s
}

func TestVFSAbandonedExchangeQuarantinesEndpoint(t *testing.T) {
	for _, status := range []kcall.Status{kcall.StatusTimeout, kcall.StatusPeerDied, kcall.StatusIOError} {
		t.Run(string(rune('0'+status)), func(t *testing.T) {
			service := newScript()
			service.status = status
			s := newFiles(service)
			request := vfs.Request{Operation: vfs.OperationReadDir, Path: bytes.Repeat([]byte("p"), 300), NameReturn: true}
			if _, ok := s.call(request, 1, 2); ok || s.bound() != 0 || len(s.paths.retired) != 1 {
				t.Fatalf("abandon = ok %v, bound %d, retired %d", ok, s.bound(), len(s.paths.retired))
			}
			// The old server has not stopped: it writes its retained mapping and
			// queues its answer after the caller already returned false.
			copy(s.paths.retired[0], "late write")
			service.status = kcall.StatusOK
			service.queued[7] = []ipc.Message{replyTo(service.last[7], []byte("late"), nil)}
			service.reply = func(m ipc.Message) ipc.Message { return replyTo(m, []byte("fresh"), nil) }
			if _, ok := s.call(request, 1, 1); ok || len(service.sent) != 1 {
				t.Fatal("timed-out channel accepted another request")
			}
			// Switching away and back must not revive the abandoned endpoint.
			s.bind(8)
			s.bind(7)
			if s.bound() != 0 {
				t.Fatal("rebind revived quarantined endpoint")
			}
			s.bind(8)
			reply, ok := s.call(request, 1, 1)
			if !ok || string(reply.Name) != "fresh" || len(service.queued[7]) != 1 || len(service.sent) != 2 || service.sent[1] != 8 {
				t.Fatalf("fresh exchange = %v, %q; sent %v", ok, reply.Name, service.sent)
			}
			copy(s.paths.retired[0], "another late write")
			if bytes.HasPrefix(s.paths.memory, []byte("another")) {
				t.Fatal("fresh request reused abandoned shared object")
			}
			if len(service.closed) != 0 {
				t.Fatalf("closed borrowed endpoints: %v", service.closed)
			}
		})
	}
}

func TestVFSReplyBuffersBelongToCaller(t *testing.T) {
	service := newScript()
	service.reply = func(m ipc.Message) ipc.Message { return replyTo(m, []byte("first"), []byte("payload")) }
	s := newFiles(service)
	request := vfs.Request{Operation: vfs.OperationReadDir}
	first, ok := s.call(request, 1, 1)
	if !ok {
		t.Fatal("first reply rejected")
	}
	service.reply = func(m ipc.Message) ipc.Message { return replyTo(m, []byte("second"), []byte("new data")) }
	if _, ok := s.call(request, 1, 1); !ok {
		t.Fatal("second reply rejected")
	}
	clear(s.incoming.Payload[:])
	clear(s.wire[:])
	if string(first.Name) != "first" || string(first.Data) != "payload" {
		t.Fatalf("caller reply changed: %q %q", first.Name, first.Data)
	}
}

func TestVFSAttachedNameBelongsToCaller(t *testing.T) {
	service := newScript()
	s := newFiles(service)
	name := bytes.Repeat([]byte("n"), vfs.MaxInlineName+1)
	service.reply = func(m ipc.Message) ipc.Message {
		if s.paths.memory == nil {
			reply, _ := vfs.EncodeReply(vfs.Reply{Operation: vfs.OperationReadDir, Status: vfs.StatusNameTooLong}, m.Correlation)
			return reply
		}
		copy(s.paths.memory, name)
		reply, _ := vfs.EncodeReply(vfs.Reply{Operation: vfs.OperationReadDir, Status: vfs.StatusOK, AttachedName: uint16(len(name))}, m.Correlation)
		return reply
	}
	first, ok := s.call(vfs.Request{Operation: vfs.OperationReadDir}, 1, 1)
	if !ok || !bytes.Equal(first.Name, name) {
		t.Fatalf("attached name = %q, %v", first.Name, ok)
	}
	clear(s.paths.memory)
	clear(s.incoming.Payload[:])
	if !bytes.Equal(first.Name, name) {
		t.Fatal("name aliases shared page")
	}
}

func TestVFSMalformedRepliesQuarantineAndCloseAttachments(t *testing.T) {
	for _, fault := range []string{"correlation", "operation", "protocol", "attachment", "unsolicited-name", "oversized-bulk"} {
		t.Run(fault, func(t *testing.T) {
			service := newScript()
			service.reply = func(m ipc.Message) ipc.Message {
				reply := replyTo(m, nil, nil)
				switch fault {
				case "correlation":
					reply.Correlation++
				case "operation":
					reply.Payload[0] = byte(vfs.OperationClose)
				case "protocol":
					reply.ProtocolID++
				case "attachment":
					reply.HandleCount, reply.Handles[0] = 1, 99
				case "unsolicited-name":
					reply, _ = vfs.EncodeReply(vfs.Reply{Operation: vfs.OperationReadDir, Status: vfs.StatusOK, AttachedName: vfs.MaxInlineName + 1}, m.Correlation)
				case "oversized-bulk":
					reply, _ = vfs.EncodeReply(vfs.Reply{Operation: vfs.OperationReadInto, Status: vfs.StatusOK, Size: 17}, m.Correlation)
				}
				return reply
			}
			s := newFiles(service)
			request := vfs.Request{Operation: vfs.OperationReadDir}
			var ok bool
			if fault == "oversized-bulk" {
				_, ok = s.callAttached(vfs.Request{Operation: vfs.OperationReadInto, Size: 16}, 88, 1, 1)
			} else {
				_, ok = s.call(request, 1, 1)
			}
			if ok || s.bound() != 0 {
				t.Fatal("accepted malformed reply or left endpoint usable")
			}
			if fault == "attachment" && (len(service.closed) != 1 || service.closed[0] != 99) {
				t.Fatalf("reply handle leaked: %v", service.closed)
			}
		})
	}
}

func TestVFSRebindWaitsForWholeExchange(t *testing.T) {
	service := newScript()
	service.entered, service.resume = make(chan handle.ID), make(chan struct{})
	service.reply = func(m ipc.Message) ipc.Message { return replyTo(m, nil, nil) }
	s := newFiles(service)
	done := make(chan bool)
	go func() { _, ok := s.call(vfs.Request{Operation: vfs.OperationStat}, 1, 1); done <- ok }()
	if channel := <-service.entered; channel != 7 {
		t.Fatalf("receive on %d, want original endpoint", channel)
	}
	rebinding, rebound := make(chan struct{}), make(chan struct{})
	go func() { close(rebinding); s.bind(8); close(rebound) }()
	<-rebinding
	if s.bound() != 7 {
		t.Fatal("rebind changed endpoint during wait")
	}
	close(service.resume)
	if !<-done {
		t.Fatal("in-flight exchange failed during rebind")
	}
	<-rebound
	if s.bound() != 8 || len(service.closed) != 0 {
		t.Fatal("rebind did not retain borrowed old endpoint")
	}
	// A healthy borrowed endpoint may be restored; only abandoned ones fail.
	s.bind(7)
	if s.bound() != 7 {
		t.Fatal("healthy borrowed endpoint could not be restored")
	}
}

func TestSessionCancellationAndUnsentAttachment(t *testing.T) {
	service := newScript()
	s := newFiles(service)
	s.hold()
	s.abandon() // explicit cancellation under the owner lock
	s.end()
	if _, ok := s.callAttached(vfs.Request{Operation: vfs.OperationReadInto}, 55, 1, 1); ok {
		t.Fatal("cancelled session sent another request")
	}
	if len(service.sent) != 0 || len(service.closed) != 1 || service.closed[0] != 55 {
		t.Fatalf("unsent attachment ownership: sent %v closed %v", service.sent, service.closed)
	}
}

func TestVFSFailedSendClosesOnlyUntransferredDuplicate(t *testing.T) {
	service := newScript()
	service.sendStatus = kcall.StatusResourceExhausted
	s := newFiles(service)
	request := vfs.Request{Operation: vfs.OperationReadDir, NameReturn: true}
	if _, ok := s.call(request, 1, 1); ok {
		t.Fatal("failed send accepted")
	}
	if len(service.sent) != 0 || len(service.closed) != 1 || service.closed[0] != s.paths.next || s.paths.memory == nil || len(s.paths.retired) != 0 {
		t.Fatalf("failed-send ownership: sent %v closed %v retired %d", service.sent, service.closed, len(s.paths.retired))
	}
	s.bind(7)
	if s.bound() != 0 {
		t.Fatal("send-failed endpoint revived")
	}
	service.sendStatus = kcall.StatusOK
	service.reply = func(m ipc.Message) ipc.Message { return replyTo(m, nil, nil) }
	s.bind(8)
	if _, ok := s.call(request, 1, 1); !ok || len(service.closed) != 1 {
		t.Fatal("fresh endpoint could not reuse an unsent page")
	}
}

func TestMalformedIPCReleasesReceivedHandle(t *testing.T) {
	service := newScript()
	message, _ := vfs.EncodeReply(vfs.Reply{Operation: vfs.OperationStat}, 1)
	message.HandleCount, message.Handles[0] = 1, 123
	var wire [ipc.WireMaxSize]byte
	size, _ := ipc.Encode(message, wire[:])
	wire[15] = 1 // bounded framing, invalid IPC reserved field
	service.raw = wire[:size]
	s := newFiles(service)
	if _, ok := s.call(vfs.Request{Operation: vfs.OperationStat}, 1, 1); ok || s.bound() != 0 {
		t.Fatal("malformed IPC left the endpoint usable")
	}
	if len(service.closed) != 1 || service.closed[0] != 123 {
		t.Fatalf("malformed delivery handle ownership: %v", service.closed)
	}
}

func TestSplitProtocolRejectionQuarantinesAndReleasesHandles(t *testing.T) {
	for _, protocol := range []string{"tcp", "udp"} {
		t.Run(protocol, func(t *testing.T) {
			service := newScript()
			service.reply = func(ipc.Message) ipc.Message {
				return ipc.Message{ProtocolID: 1, Version: 1, Kind: ipc.KindReply, Correlation: 1, HandleCount: 1, Handles: [ipc.MaxAttachedHandles]handle.ID{999}}
			}
			var s session[scriptedIPC]
			s.io = scriptedIPC{service}
			s.bind(7)
			s.hold()
			var ok bool
			if protocol == "tcp" {
				reply := tcpapi.Reply{Endpoint: 5}
				_, ok = receiveProtocol(&s, 1, &reply, tcpapi.DecodeReply)
				if reply.Endpoint != 0 {
					t.Fatal("stale TCP output retained")
				}
			} else {
				reply := udpapi.Reply{Endpoint: 5}
				_, ok = receiveProtocol(&s, 1, &reply, udpapi.DecodeReply)
				if reply.Endpoint != 0 {
					t.Fatal("stale UDP output retained")
				}
			}
			if ok || s.bound() != 0 || len(service.closed) != 1 || service.closed[0] != 999 {
				t.Fatalf("protocol rejection ownership: ok %v bound %d closed %v", ok, s.bound(), service.closed)
			}
			if s.send(&ipc.Message{ProtocolID: 1, Version: 1, Kind: ipc.KindRequest, Correlation: 2}) {
				t.Fatal("split session sent after protocol rejection")
			}
			s.end()
		})
	}
}

func TestRebindQueuedBehindFailedExchange(t *testing.T) {
	service := newScript()
	service.status = kcall.StatusPeerDied
	service.entered, service.resume = make(chan handle.ID), make(chan struct{})
	s := newFiles(service)
	done := make(chan bool)
	go func() {
		_, ok := s.call(vfs.Request{Operation: vfs.OperationReadDir, NameReturn: true}, 1, 1)
		done <- ok
	}()
	<-service.entered
	rebound := make(chan struct{})
	go func() { s.bind(8); close(rebound) }()
	close(service.resume)
	if <-done {
		t.Fatal("dead exchange succeeded")
	}
	<-rebound
	if s.bound() != 8 || len(s.paths.retired) != 1 {
		t.Fatal("rebind lost failure quarantine")
	}
	s.bind(7)
	if s.bound() != 0 {
		t.Fatal("old dead endpoint revived")
	}
}

func TestVFSSentCancellationRetiresPageBeforeNextEndpoint(t *testing.T) {
	service := newScript()
	s := newFiles(service)
	request := vfs.Request{Operation: vfs.OperationReadDir, NameReturn: true}
	s.hold()
	s.paths.Hold()
	attached, _ := s.paths.Prepare(&request)
	message, _ := vfs.EncodeRequest(request, s.next())
	message.HandleCount, message.Handles[0] = 1, attached
	if !s.send(&message) {
		t.Fatal("send")
	}
	old := s.paths.memory
	// There is no public cancellation API. An owner which abandons a sent
	// transaction must perform BOTH operations before dropping its locks.
	s.abandon()
	s.paths.Abandon()
	s.paths.Release()
	s.end()
	s.bind(8)
	service.reply = func(m ipc.Message) ipc.Message { return replyTo(m, nil, nil) }
	if _, ok := s.call(request, 1, 1); !ok {
		t.Fatal("fresh exchange")
	}
	copy(old, "late cancelled request")
	if bytes.HasPrefix(s.paths.memory, []byte("late")) || len(service.closed) != 0 {
		t.Fatal("cancelled transaction reused memory or closed borrowed endpoint")
	}
}

func TestVFSConcurrentAttachedNamesStayWithCaller(t *testing.T) {
	service := newScript()
	s := newFiles(service)
	nameSize := vfs.MaxInlineName + 1
	service.reply = func(m ipc.Message) ipc.Message {
		reply, _ := vfs.EncodeReply(vfs.Reply{Operation: vfs.OperationReadDir, Status: vfs.StatusOK, AttachedName: uint16(nameSize)}, m.Correlation)
		return reply
	}
	var group sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		group.Add(1)
		go func(worker int) {
			defer group.Done()
			for round := 0; round < 80; round++ {
				path := bytes.Repeat([]byte{byte('a' + worker), byte(round)}, 150)
				reply, ok := s.call(vfs.Request{Operation: vfs.OperationReadDir, Path: path, NameReturn: true}, 1, 1)
				if !ok || !bytes.Equal(reply.Name, path[:nameSize]) {
					t.Errorf("worker %d round %d received another caller's name", worker, round)
					return
				}
			}
		}(worker)
	}
	group.Wait()
}
