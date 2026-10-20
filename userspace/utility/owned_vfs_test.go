package utility

import (
	"bytes"
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
