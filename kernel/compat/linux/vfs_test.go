package linux

import (
	"testing"

	"goos/kernel/ipc"
	"goos/kernel/process"
	"goos/kernel/sched"
	"goos/kernel/vfs"
)

func vfsReplyMessage(t *testing.T, reply vfs.Reply, correlation uint64) ipc.Message {
	t.Helper()
	message, ok := vfs.EncodeReply(reply, correlation)
	if !ok {
		t.Fatal("reply did not encode")
	}
	return message
}

func TestVFSSessionCorrelationsRiseAndStopAfterFailure(t *testing.T) {
	session := VFSSession{}
	first, ok := session.Next()
	second, again := session.Next()
	if !ok || !again || first != 1 || second != 2 || session.Failed() {
		t.Fatalf("correlations = %d,%d ok=%v,%v", first, second, ok, again)
	}
	session.Fail()
	if _, ok := session.Next(); ok || !session.Failed() {
		t.Fatal("failed session issued a correlation")
	}
	var absent *VFSSession
	if _, ok := absent.Next(); ok || !absent.Failed() {
		t.Fatal("absent session is usable")
	}
}

func TestClassifyVFSReply(t *testing.T) {
	open := vfs.Reply{Operation: vfs.OperationOpen, Status: vfs.StatusOK, Handle: 0x10001}
	message := vfsReplyMessage(t, open, 7)
	reply, verdict := ClassifyVFSReply(&message, 7, vfs.OperationOpen)
	if verdict != VFSReplyAccepted || reply.Handle != 0x10001 {
		t.Fatalf("matching reply: verdict=%d reply=%#v", verdict, reply)
	}
	// A reply to a request the caller gave up on is dropped, not failed.
	if _, verdict := ClassifyVFSReply(&message, 8, vfs.OperationOpen); verdict != VFSReplyStale {
		t.Fatalf("older correlation: verdict=%d", verdict)
	}
	// No request with a later correlation was ever sent.
	if _, verdict := ClassifyVFSReply(&message, 6, vfs.OperationOpen); verdict != VFSReplyMalformed {
		t.Fatalf("future correlation: verdict=%d", verdict)
	}
	if _, verdict := ClassifyVFSReply(&message, 7, vfs.OperationClose); verdict != VFSReplyMalformed {
		t.Fatalf("other operation: verdict=%d", verdict)
	}
	if _, verdict := ClassifyVFSReply(&message, 0, vfs.OperationOpen); verdict != VFSReplyMalformed {
		t.Fatalf("zero correlation: verdict=%d", verdict)
	}
	attached := message
	attached.HandleCount = 1
	if _, verdict := ClassifyVFSReply(&attached, 7, vfs.OperationOpen); verdict != VFSReplyMalformed {
		t.Fatalf("attached handle: verdict=%d", verdict)
	}
	request, ok := vfs.EncodeRequest(vfs.Request{Operation: vfs.OperationOpen, Rights: vfs.Read, Path: []byte("/x")}, 7)
	if !ok {
		t.Fatal("request did not encode")
	}
	if _, verdict := ClassifyVFSReply(&request, 7, vfs.OperationOpen); verdict != VFSReplyMalformed {
		t.Fatalf("request as reply: verdict=%d", verdict)
	}
	foreign := message
	foreign.ProtocolID++
	if _, verdict := ClassifyVFSReply(&foreign, 7, vfs.OperationOpen); verdict != VFSReplyMalformed {
		t.Fatalf("foreign protocol: verdict=%d", verdict)
	}
	if _, verdict := ClassifyVFSReply(nil, 7, vfs.OperationOpen); verdict != VFSReplyMalformed {
		t.Fatalf("nil message: verdict=%d", verdict)
	}
}

func TestVFSCountersSaturate(t *testing.T) {
	vfsDiagnostics = VFSDiagnostics{Requests: ^uint64(0) - 1}
	NoteVFSRequest()
	NoteVFSRequest()
	NoteVFSFailure()
	NoteVFSStale()
	if got := VFSStatistics(); got.Requests != ^uint64(0) || got.Failures != 1 || got.Stale != 1 {
		t.Fatalf("statistics = %#v", got)
	}
	vfsDiagnostics = VFSDiagnostics{}
}

func TestVFSContextSerializesAndCancelsWaiters(t *testing.T) {
	vfsContexts = [process.MaxProcesses]LinuxVFSContext{}
	if !installVFSContext(41) || !RetainVFSContext(41) {
		t.Fatal("context setup failed")
	}
	first, _ := sched.NewTaskID(1, 1)
	second, _ := sched.NewTaskID(2, 1)
	third, _ := sched.NewTaskID(3, 1)
	if AcquireVFS(41, first) != VFSAccessAcquired || AcquireVFS(41, second) != VFSAccessQueued || AcquireVFS(41, third) != VFSAccessQueued {
		t.Fatal("context did not serialize callers")
	}
	if next, released := ReleaseVFS(41, first); !released || next != second {
		t.Fatalf("first release = next %d released=%v", next, released)
	}
	if next := CancelVFSTask(second); next != third {
		t.Fatalf("owner cancellation woke %d", next)
	}
	if next, released := ReleaseVFS(41, third); !released || next != 0 {
		t.Fatalf("final release = next %d released=%v", next, released)
	}
	if final, released := ReleaseVFSContext(41); final || !released {
		t.Fatalf("first context release = final %v released %v", final, released)
	}
	if final, released := ReleaseVFSContext(41); !final || !released {
		t.Fatalf("final context release = final %v released %v", final, released)
	}
	if _, found := VFSContext(41); found {
		t.Fatal("released context remained visible")
	}
}

func TestVFSContextFailureAppliesToFamily(t *testing.T) {
	vfsContexts = [process.MaxProcesses]LinuxVFSContext{}
	if !installVFSContext(42) {
		t.Fatal("context setup failed")
	}
	context, _ := VFSContext(42)
	context.Session.Fail()
	task, _ := sched.NewTaskID(1, 1)
	if AcquireVFS(42, task) != VFSAccessInvalid {
		t.Fatal("failed family context accepted a request")
	}
}

func vfsTasks(t *testing.T, count int) []sched.TaskID {
	t.Helper()
	tasks := make([]sched.TaskID, count)
	for index := range tasks {
		tasks[index], _ = sched.NewTaskID(uint32(index+1), 1)
	}
	return tasks
}

func vfsWaiters(context *LinuxVFSContext) []sched.TaskID {
	return append([]sched.TaskID(nil), context.waiters[:context.waitCount]...)
}

func TestVFSContextHandsOwnershipOnInFIFOOrder(t *testing.T) {
	vfsContexts = [process.MaxProcesses]LinuxVFSContext{}
	if !installVFSContext(51) {
		t.Fatal("context setup failed")
	}
	context, _ := VFSContext(51)
	task := vfsTasks(t, 3)
	if AcquireVFS(51, task[0]) != VFSAccessAcquired || AcquireVFS(51, task[1]) != VFSAccessQueued ||
		AcquireVFS(51, task[2]) != VFSAccessQueued {
		t.Fatal("the context did not serialize three callers")
	}
	// A retried call that is already queued keeps its place once.
	if AcquireVFS(51, task[1]) != VFSAccessQueued || len(vfsWaiters(context)) != 2 {
		t.Fatalf("waiters = %v", vfsWaiters(context))
	}
	if next, _ := ReleaseVFS(51, task[0]); next != task[1] || context.Owner != task[1] {
		t.Fatalf("first release handed the context to %d", next)
	}
	if ReleaseVFS(51, task[0]); context.Owner != task[1] {
		t.Fatal("a release by a task that no longer owns the context took effect")
	}
	if next, _ := ReleaseVFS(51, task[1]); next != task[2] || context.Owner != task[2] || len(vfsWaiters(context)) != 0 {
		t.Fatalf("second release handed the context to %d, waiters %v", next, vfsWaiters(context))
	}
	if next, released := ReleaseVFS(51, task[2]); next != 0 || !released || context.Owner != 0 {
		t.Fatal("the last release left an owner")
	}
}

func TestVFSContextCancelsAQueuedWaiter(t *testing.T) {
	vfsContexts = [process.MaxProcesses]LinuxVFSContext{}
	installVFSContext(52)
	context, _ := VFSContext(52)
	task := vfsTasks(t, 3)
	AcquireVFS(52, task[0])
	AcquireVFS(52, task[1])
	AcquireVFS(52, task[2])
	if next := CancelVFSTask(task[1]); next != 0 {
		t.Fatalf("cancelling a waiter handed the context to %d", next)
	}
	if waiters := vfsWaiters(context); len(waiters) != 1 || waiters[0] != task[2] {
		t.Fatalf("waiters after cancel = %v", waiters)
	}
	if next, _ := ReleaseVFS(52, task[0]); next != task[2] {
		t.Fatalf("release after cancel handed the context to %d", next)
	}
	if CancelVFSTask(task[1]) != 0 || context.Owner != task[2] || len(vfsWaiters(context)) != 0 {
		t.Fatal("a second cancel of the removed waiter changed the context")
	}
}

func TestVFSContextCancelsItsOwner(t *testing.T) {
	vfsContexts = [process.MaxProcesses]LinuxVFSContext{}
	installVFSContext(53)
	context, _ := VFSContext(53)
	task := vfsTasks(t, 3)
	AcquireVFS(53, task[0])
	AcquireVFS(53, task[1])
	AcquireVFS(53, task[2])
	if next := CancelVFSTask(task[0]); next != task[1] || context.Owner != task[1] {
		t.Fatalf("owner cancellation handed the context to %d", next)
	}
	if waiters := vfsWaiters(context); len(waiters) != 1 || waiters[0] != task[2] {
		t.Fatalf("waiters after owner cancel = %v", waiters)
	}
}

func TestVFSContextPeerDeathDrainsWaiters(t *testing.T) {
	vfsContexts = [process.MaxProcesses]LinuxVFSContext{}
	if !installVFSContext(54) || !RetainVFSContext(54) || !RetainVFSContext(54) {
		t.Fatal("context setup failed")
	}
	context, _ := VFSContext(54)
	task := vfsTasks(t, 4)
	AcquireVFS(54, task[0])
	AcquireVFS(54, task[1])
	AcquireVFS(54, task[2])
	if correlation, ok := context.Session.Next(); !ok || correlation != 1 {
		t.Fatal("the owner could not issue its request")
	}
	// The VFS dies under the owner's exchange.
	context.Session.Fail()
	if _, ok := context.Session.Next(); ok {
		t.Fatal("a failed session issued another correlation")
	}
	// No caller joins a failed context, and none reconnects it.
	if AcquireVFS(54, task[3]) != VFSAccessInvalid || len(vfsWaiters(context)) != 2 {
		t.Fatal("a new caller joined a failed context")
	}
	// Each waiter in turn owns the context only long enough to fail.
	for index := 0; index < 3; index++ {
		next, released := ReleaseVFS(54, task[index])
		if !released || !context.Session.Failed() {
			t.Fatalf("release %d failed or reconnected the session", index)
		}
		if index < 2 && next != task[index+1] || index == 2 && next != 0 {
			t.Fatalf("release %d handed the context to %d", index, next)
		}
	}
	if context.Owner != 0 || len(vfsWaiters(context)) != 0 {
		t.Fatal("waiter state survived the drain")
	}
	for index := 0; index < 3; index++ {
		final, released := ReleaseVFSContext(54)
		if !released || final != (index == 2) {
			t.Fatalf("context release %d = final %v released %v", index, final, released)
		}
	}
	if _, found := VFSContext(54); found {
		t.Fatal("the last member's exit left the context")
	}
}

func TestVFSReplyFromAnAbandonedRequestCannotAnswerANewerOne(t *testing.T) {
	var session VFSSession
	abandoned, _ := session.Next()
	current, _ := session.Next()
	stale := vfsReplyMessage(t, vfs.Reply{Operation: vfs.OperationOpen, Status: vfs.StatusOK}, abandoned)
	if _, verdict := ClassifyVFSReply(&stale, current, vfs.OperationOpen); verdict != VFSReplyStale {
		t.Fatalf("an abandoned reply was %d", verdict)
	}
	future := vfsReplyMessage(t, vfs.Reply{Operation: vfs.OperationOpen, Status: vfs.StatusOK}, current+1)
	if _, verdict := ClassifyVFSReply(&future, current, vfs.OperationOpen); verdict != VFSReplyMalformed {
		t.Fatalf("a reply from the future was %d", verdict)
	}
	other := vfsReplyMessage(t, vfs.Reply{Operation: vfs.OperationStat, Status: vfs.StatusOK}, current)
	if _, verdict := ClassifyVFSReply(&other, current, vfs.OperationOpen); verdict != VFSReplyMalformed {
		t.Fatalf("a reply to another operation was %d", verdict)
	}
	right := vfsReplyMessage(t, vfs.Reply{Operation: vfs.OperationOpen, Status: vfs.StatusOK}, current)
	if _, verdict := ClassifyVFSReply(&right, current, vfs.OperationOpen); verdict != VFSReplyAccepted {
		t.Fatalf("the reply in flight was %d", verdict)
	}
}
