package utility

import (
	"bytes"
	"goos/kernel/handle"
	"goos/kernel/ipc"
	"goos/kernel/kcall"
	"goos/kernel/vfs"
	"testing"
)

type testClock struct{ ticks []uint64 }

func (c *testClock) now() (uint64, kcall.Status) {
	tick := c.ticks[0]
	if len(c.ticks) > 1 {
		c.ticks = c.ticks[1:]
	}
	return tick, kcall.StatusOK
}

type clockedIPC struct {
	scriptedIPC
	clock *testClock
}

func (c clockedIPC) now() (uint64, kcall.Status) { return c.clock.now() }
func newOwned(service *scriptedService, ticks ...uint64) *ownedVFS[clockedIPC, *testPaths] {
	var s ownedVFS[clockedIPC, *testPaths]
	s.io = clockedIPC{scriptedIPC{service}, &testClock{ticks}}
	s.paths = &testPaths{}
	return &s
}

func TestOwnedVFSAbsoluteDeadlineBeforeAndAfterSend(t *testing.T) {
	request := vfs.Request{Operation: vfs.OperationReadDir, NameReturn: true}
	for _, point := range []string{"before-send", "after-send", "wait-budget"} {
		t.Run(point, func(t *testing.T) {
			service := newScript()
			service.reply = func(m ipc.Message) ipc.Message { return replyTo(m, nil, nil) }
			s := newOwned(service, 10, 15)
			if point == "after-send" {
				s.io.clock.ticks = []uint64{10, 11, 15}
			}
			if point == "wait-budget" {
				s.io.clock.ticks = []uint64{10, 11, 14}
			}
			_, ok := s.callOn(7, request, 0, 123, 5)
			switch point {
			case "before-send":
				if ok || len(service.sent) != 0 || len(s.quarantined) != 0 || len(s.paths.retired) != 0 || len(service.closed) != 1 {
					t.Fatal("unsent expiry treated as abandoned transaction")
				}
			case "after-send":
				if ok || len(service.sent) != 1 || len(service.waits) != 0 || s.bound() != 0 || len(s.paths.retired) != 1 {
					t.Fatal("sent expiry did not quarantine and retire")
				}
			case "wait-budget":
				if !ok || len(service.waits) != 1 || service.waits[0] != 1 || service.last[7].Correlation != 123 {
					t.Fatalf("absolute budget/correlation lost: %v %v", ok, service.waits)
				}
			}
		})
	}
}

func TestOwnedVFSCompletedWorkWithheldReplyStillQuarantines(t *testing.T) {
	service := newScript()
	service.status = kcall.StatusTimeout
	s := newOwned(service, 10)
	request := vfs.Request{Operation: vfs.OperationReadDir, Path: bytes.Repeat([]byte("p"), 300), NameReturn: true}
	if _, ok := s.callOn(7, request, 0, 1, 5); ok {
		t.Fatal("withheld reply accepted")
	}
	old := s.paths.retired[0]
	// Work could already be complete, but without its reply no completion
	// fence was observed. A later valid reply must not revive this endpoint.
	service.queued[7] = []ipc.Message{replyTo(service.last[7], nil, nil)}
	service.status = kcall.StatusOK
	service.reply = func(m ipc.Message) ipc.Message { return replyTo(m, nil, nil) }
	if _, ok := s.callOn(7, request, 0, 2, 5); ok || len(service.sent) != 1 {
		t.Fatal("late reply revived old endpoint")
	}
	if _, ok := s.callOn(8, request, 0, 3, 5); !ok {
		t.Fatal("fresh supervisor endpoint refused")
	}
	copy(old, "late old mapping access")
	if bytes.HasPrefix(s.paths.memory, []byte("late")) || len(service.queued[7]) != 1 {
		t.Fatal("old mapping/reply reached fresh transaction")
	}
}

func TestOwnedAsyncCompletionVsMalformedReply(t *testing.T) {
	request := vfs.Request{Operation: vfs.OperationPrepareExecutable, Size: 5}
	for _, fault := range []string{"completed-error", "future", "operation", "handle", "size", "peer-death"} {
		t.Run(fault, func(t *testing.T) {
			service := newScript()
			s := newOwned(service, 10)
			message, _ := vfs.EncodeReply(vfs.Reply{Operation: request.Operation, Status: vfs.StatusOK, Size: 5}, 9)
			status := kcall.StatusOK
			switch fault {
			case "completed-error":
				message, _ = vfs.EncodeReply(vfs.Reply{Operation: request.Operation, Status: vfs.StatusBackendError}, 9)
			case "future":
				message.Correlation = 10
			case "operation":
				message.Payload[0] = byte(vfs.OperationClose)
			case "handle":
				message.HandleCount, message.Handles[0] = 1, 99
			case "size":
				message, _ = vfs.EncodeReply(vfs.Reply{Operation: request.Operation, Status: vfs.StatusOK, Size: 4}, 9)
			case "peer-death":
				status = kcall.StatusPeerDied
			}
			reply, valid := s.completeOn(7, request, 9, &message, status)
			if fault == "completed-error" {
				if !valid || reply.Status != vfs.StatusBackendError || s.bound() != 7 {
					t.Fatal("valid error reply confused with abandonment")
				}
			} else {
				if valid || s.bound() != 0 {
					t.Fatal("unsafe async reply left channel live")
				}
				if _, ok := s.callOn(7, vfs.Request{Operation: vfs.OperationClose}, 0, 10, 5); ok || len(service.sent) != 0 {
					t.Fatal("cleanup sent Close into unresolved exchange")
				}
			}
			if fault == "handle" && (len(service.closed) != 1 || service.closed[0] != 99) {
				t.Fatal("rejected async handle leaked")
			}
		})
	}
}

func TestVFSControlOwnershipAndCorrelations(t *testing.T) {
	for _, fault := range []string{"completed-error", "timeout", "future", "handle", "send-failure"} {
		t.Run(fault, func(t *testing.T) {
			service := newScript()
			s := newOwned(service, 10)
			request, _ := vfs.AddSessionRequest(55, 9)
			service.reply = func(m ipc.Message) ipc.Message {
				reply, _ := vfs.AddSessionReply(vfs.StatusOK, m.Correlation)
				if fault == "completed-error" {
					reply, _ = vfs.AddSessionReply(vfs.StatusBackendError, m.Correlation)
				}
				if fault == "future" {
					reply.Correlation++
				}
				if fault == "handle" {
					reply.HandleCount, reply.Handles[0] = 1, 99
				}
				return reply
			}
			if fault == "timeout" {
				service.status = kcall.StatusTimeout
			}
			if fault == "send-failure" {
				service.sendStatus = kcall.StatusResourceExhausted
			}
			result, status, sent := s.controlOn(7, &request, 5)
			if fault == "completed-error" {
				if status != kcall.StatusOK || !sent || result != vfs.StatusBackendError || s.bound() != 7 {
					t.Fatal("control error status is not an abandoned exchange")
				}
				return
			}
			if status == kcall.StatusOK || s.bound() != 0 || sent == (fault == "send-failure") {
				t.Fatalf("control failure ownership: %v %v", status, sent)
			}
			// Failed sends retain attachment 55 for the owner; successful sends
			// transfer it even if the status reply is late or malformed.
			for _, id := range service.closed {
				if id == 55 {
					t.Fatal("control helper closed caller/transferred attachment")
				}
			}
			if fault == "handle" && (len(service.closed) != 1 || service.closed[0] != 99) {
				t.Fatal("unexpected control reply handle leaked")
			}
			before := len(service.sent)
			if _, _, again := s.controlOn(7, &request, 5); again || len(service.sent) != before {
				t.Fatal("control endpoint reused after failure")
			}
		})
	}
}

func TestVFSControlAbsoluteBudgetAndUnsentOwnership(t *testing.T) {
	for _, point := range []string{"before-send", "after-send", "wait-budget"} {
		t.Run(point, func(t *testing.T) {
			service := newScript()
			service.reply = func(m ipc.Message) ipc.Message {
				reply, _ := vfs.AddSessionReply(vfs.StatusOK, m.Correlation)
				return reply
			}
			s := newOwned(service, 10, 15)
			if point == "after-send" {
				s.io.clock.ticks = []uint64{10, 11, 15}
			}
			if point == "wait-budget" {
				s.io.clock.ticks = []uint64{10, 11, 14}
			}
			request, _ := vfs.AddSessionRequest(55, 9)
			_, status, sent := s.controlOn(7, &request, 5)
			switch point {
			case "before-send":
				if sent || status != kcall.StatusTimeout || len(service.sent) != 0 || len(service.closed) != 0 || s.bound() != 7 {
					t.Fatal("unsent control expiry changed ownership/quarantine")
				}
			case "after-send":
				if !sent || status != kcall.StatusTimeout || len(service.waits) != 0 || s.bound() != 0 || len(service.closed) != 0 {
					t.Fatal("sent control expiry changed ownership or waited again")
				}
			case "wait-budget":
				if !sent || status != kcall.StatusOK || len(service.waits) != 1 || service.waits[0] != 1 {
					t.Fatalf("control wait reset budget: %v", service.waits)
				}
			}
		})
	}
}

func TestVFSControlKeepsFilesystemLostNotice(t *testing.T) {
	service := newScript()
	s := newOwned(service, 10)
	notice, _ := vfs.FilesystemLostNotice(4)
	service.queued[7] = []ipc.Message{notice}
	service.reply = func(m ipc.Message) ipc.Message {
		reply, _ := vfs.AddSessionReply(vfs.StatusOK, m.Correlation)
		return reply
	}
	request, _ := vfs.SyncRequest(9)
	result, status, sent := s.controlOn(7, &request, 5)
	if status != kcall.StatusOK || !sent || result != vfs.StatusOK || s.bound() != 7 {
		t.Fatalf("notice before the reply failed the exchange: %v %v %v", result, status, sent)
	}
	if generation, ok := s.takeLost(); !ok || generation != 4 {
		t.Fatalf("notice lost: %d %v", generation, ok)
	}
	if _, ok := s.takeLost(); ok {
		t.Fatal("notice delivered twice")
	}
	// Any other notification is still no reply.
	stray, _ := vfs.SessionNotice(55)
	service.queued[7] = []ipc.Message{stray}
	if _, status, _ := s.controlOn(7, &request, 5); status == kcall.StatusOK || s.bound() != 0 {
		t.Fatal("a stray notification was taken for a control reply")
	}
}

func TestVFSControlNoticeDoesNotExtendBudget(t *testing.T) {
	service := newScript()
	s := newOwned(service, 10, 11, 12, 15)
	notice, _ := vfs.FilesystemLostNotice(2)
	service.queued[7] = []ipc.Message{notice}
	service.reply = func(m ipc.Message) ipc.Message {
		reply, _ := vfs.AddSessionReply(vfs.StatusOK, m.Correlation)
		return reply
	}
	request, _ := vfs.SyncRequest(9)
	if _, status, sent := s.controlOn(7, &request, 5); status != kcall.StatusTimeout || !sent || s.bound() != 0 {
		t.Fatalf("a notice renewed the control budget: %v %v", status, sent)
	}
	if generation, ok := s.takeLost(); !ok || generation != 2 {
		t.Fatal("notice received before the expiry was dropped")
	}
}

func TestVFSControlPollKeepsNoticesAndDropsStrays(t *testing.T) {
	service := newScript()
	service.drained = kcall.StatusNotFound
	s := newOwned(service, 10)
	first, _ := vfs.FilesystemLostNotice(3)
	latest, _ := vfs.FilesystemLostNotice(5)
	stray, _ := vfs.SessionNotice(99)
	late, _ := vfs.AddSessionReply(vfs.StatusOK, 8)
	service.queued[7] = []ipc.Message{first, stray, late, latest}
	if !s.pollControlOn(7) || s.bound() != 7 {
		t.Fatal("polling discarded the endpoint")
	}
	if len(service.closed) != 1 || service.closed[0] != 99 {
		t.Fatalf("stray handles not closed: %v", service.closed)
	}
	if generation, ok := s.takeLost(); !ok || generation != 5 {
		t.Fatalf("latest notice = %d %v", generation, ok)
	}
	// A quarantined endpoint is neither read nor reported usable.
	s.abandon()
	service.queued[7] = []ipc.Message{first}
	if s.pollControlOn(7) || len(service.queued[7]) != 1 {
		t.Fatal("polled a quarantined control endpoint")
	}
}

// A busy persistent lane can outlive an ordinary control's budget. Its
// quarantine must not prevent the independent supervisor recovery channel
// from delivering the notice and acknowledging a drained control handoff.
func TestIndependentRecoveryAfterBusyControlTimeout(t *testing.T) {
	service := newScript()
	s := newOwned(service, 10)
	oldControl, recovery, freshControl := handle.ID(7), handle.ID(8), handle.ID(9)
	request, _ := vfs.AddSessionRequest(55, 20)
	service.status = kcall.StatusTimeout
	if _, status, sent := s.controlOn(oldControl, &request, 2); status != kcall.StatusTimeout || !sent {
		t.Fatal("busy accepted control did not time out")
	}
	// The old AddSession is ambiguous. Its client would be closed by PID 1;
	// the request is never repeated on either new endpoint.
	service.status = kcall.StatusOK
	service.drained = kcall.StatusNotFound
	lost, _ := vfs.FilesystemLostNotice(1)
	service.queued[recovery] = []ipc.Message{lost}
	if !s.pollControlOn(recovery) {
		t.Fatal("independent notice path unavailable")
	}
	if generation, ok := s.takeLost(); !ok || generation != 1 {
		t.Fatal("lost notice hidden by control quarantine")
	}
	oldWriterAlive := true
	exitObserved := false
	handedOff := false
	service.drained = kcall.StatusOK
	service.reply = func(m ipc.Message) ipc.Message {
		if _, ok := vfs.DecodeReplaceControl(m); ok {
			handedOff = true
		} else if attach, ok := vfs.DecodeSetFilesystem(m); ok {
			if oldWriterAlive || !exitObserved || !attach.Fenced || !handedOff {
				t.Fatal("reattach before both fences")
			}
		} else {
			t.Fatal("ambiguous control replayed")
		}
		reply, _ := vfs.AddSessionReply(vfs.StatusOK, m.Correlation)
		return reply
	}
	rotate, _ := vfs.ReplaceControlRequest(77, 21)
	if result, status, sent := s.controlOn(recovery, &rotate, 20); result != vfs.StatusOK || status != kcall.StatusOK || !sent {
		t.Fatal("handoff failed")
	}
	// PID 1 kills the old writer and waits for its kernel exit event before
	// constructing the new attachment. No unquarantine of oldControl occurs.
	oldWriterAlive = false
	exitObserved = true
	attach, _ := vfs.SetFilesystemRequest(vfs.FilesystemAttach{Client: 88, Generation: 2, Fenced: exitObserved}, 22)
	if result, status, sent := s.controlOn(freshControl, &attach, 20); result != vfs.StatusOK || status != kcall.StatusOK || !sent {
		t.Fatal("fenced reattach failed")
	}
	late, _ := vfs.AddSessionReply(vfs.StatusOK, 20)
	service.queued[oldControl] = []ipc.Message{late}
	if s.pollControlOn(oldControl) || len(service.queued[oldControl]) != 1 {
		t.Fatal("old control or its stale reply revived")
	}
	if len(service.sent) != 3 || service.sent[0] != oldControl || service.sent[1] != recovery || service.sent[2] != freshControl {
		t.Fatalf("unexpected replay or route: %v", service.sent)
	}
}
