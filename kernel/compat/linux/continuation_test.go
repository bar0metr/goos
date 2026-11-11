package linux

import (
	"testing"

	"goos/kernel/compat/linux/abi"
	"goos/kernel/ramfs"
	"goos/kernel/sched"
	"goos/kernel/vfs"
)

func TestContinuationBlocksResumesAndAdvancesExactlyOnce(t *testing.T) {
	var table ContinuationTable
	task, _ := sched.NewTaskID(3, 7)
	request := Request{Number: abi.SysOpenat, Arguments: [6]uint64{1, 2, 3, 4, 5, 6}}
	if !table.Begin(task, request, []byte("/tmp/file")) || table.Begin(task, request, nil) {
		t.Fatal("one-active-syscall invariant failed")
	}
	frame := abi.Frame{}
	if !table.Issue(task, 1, 91) || table.Issue(task, 1, 91) || !table.BlockFrame(&frame, task, 1) || frame.Action != abi.ActionBlock {
		t.Fatal("stage was not issued and blocked exactly once")
	}
	if table.Resume(task, 90, WakeMessage) || !table.Resume(task, 91, WakeMessage) || table.Resume(task, 91, WakeMessage) {
		t.Fatal("correlation matching failed")
	}
	if !table.Advance(task, 1, 2, 17) || table.Advance(task, 1, 2, 17) {
		t.Fatal("stage advanced more than once")
	}
	state, found := table.Lookup(task)
	if !found || state.Number != abi.SysOpenat || state.Arguments != request.Arguments || state.Stage != 2 || state.Partial != 17 || string(state.Path()) != "/tmp/file" || state.Issued || state.Correlation != 0 {
		t.Fatalf("state = %#v path=%q", state, state.Path())
	}
	if !table.Finish(task) {
		t.Fatal("ready continuation did not finish")
	}
}

type cleanupObjectBackend struct {
	*ramfs.FileSystem
	acquired int
	released int
}

func (backend *cleanupObjectBackend) AcquireObject(id ramfs.NodeID) (ramfs.NodeID, ramfs.Status) {
	backend.acquired++
	return id, ramfs.StatusOK
}

func (backend *cleanupObjectBackend) ReleaseObject(id ramfs.NodeID) (ramfs.NodeID, ramfs.Status) {
	backend.released++
	return id, ramfs.StatusOK
}

func TestOpenCleanupContinuationAfterDescriptionExhaustion(t *testing.T) {
	resetFDTest()
	defer resetFDTest()
	var fds FDTable
	if !fds.Initialize(1, 1, 2, 3) || fds.Reserve() != 0 {
		t.Fatal("FD table setup failed")
	}
	backend := &cleanupObjectBackend{FileSystem: &ramfs.FileSystem{}}
	root := backend.Init()
	var namespace vfs.Namespace
	if status := namespace.Mount([]byte("/"), backend, root); status != vfs.StatusOK {
		t.Fatal(status)
	}
	handle, kind, status := namespace.OpenIntent(1, []byte("/file"), vfs.Write, vfs.OpenCreate)
	if status != vfs.StatusOK || kind != ramfs.KindFile || handle == 0 || backend.acquired != 1 {
		t.Fatalf("open = %d, %d, %d, acquired=%d", handle, kind, status, backend.acquired)
	}
	for i := range openDescriptions {
		if openDescriptions[i].familyID == 0 {
			openDescriptions[i].familyID = 2
		}
	}
	fd, errno := fds.Allocate(FileDescription{Kind: FDVFSFile, Object: uint64(handle)}, false)
	if fd != -1 || errno != abi.ENFILE {
		t.Fatalf("allocation = %d, %d", fd, errno)
	}
	if _, found := fds.Lookup(3); found {
		t.Fatal("failed allocation installed an FD")
	}
	var table ContinuationTable
	task, _ := sched.NewTaskID(8, 1)
	request := Request{Number: abi.SysOpenat}
	if !table.Begin(task, request, []byte("/file")) || !table.Issue(task, OpenStageOpen, 1) || !table.Block(task, OpenStageOpen) || !table.Resume(task, 1, WakeMessage) {
		t.Fatal("open continuation setup failed")
	}
	if !table.AdvanceOpenCleanup(task, uint64(handle), errno) || table.AdvanceOpenCleanup(task, uint64(handle), errno) {
		t.Fatal("cleanup transition failed")
	}
	if !table.Issue(task, OpenStageCleanup, 2) || !table.Block(task, OpenStageCleanup) || !table.Resume(task, 2, WakeTimeout) || !table.Block(task, OpenStageCleanup) || !table.Resume(task, 2, WakeMessage) {
		t.Fatal("close continuation failed")
	}
	pending, found := table.Pending(task)
	if !found || pending.Stage != OpenStageCleanup || pending.Partial != uint64(handle) || pending.Note != uint64(abi.ENFILE) {
		t.Fatalf("cleanup state = %#v", pending)
	}
	if status := namespace.Close(1, vfs.Handle(pending.Partial)); status != vfs.StatusOK || backend.released != 1 {
		t.Fatalf("close = %d, released=%d", status, backend.released)
	}
	if status := namespace.Close(1, handle); status == vfs.StatusOK || backend.released != 1 {
		t.Fatalf("duplicate close = %d, released=%d", status, backend.released)
	}
	if _, status := namespace.Stat([]byte("/file")); status != vfs.StatusOK || !table.Finish(task) {
		t.Fatalf("created file missing or continuation unfinished: %d", status)
	}
}

func TestContinuationRejectsStaleGenerationAndBadPath(t *testing.T) {
	var table ContinuationTable
	first, _ := sched.NewTaskID(4, 1)
	stale, _ := sched.NewTaskID(4, 2)
	request := Request{Number: abi.SysRead}
	if !table.Begin(first, request, nil) {
		t.Fatal("begin failed")
	}
	if _, found := table.Lookup(stale); found || table.Cancel(stale) {
		t.Fatal("stale generation reached continuation")
	}
	if !table.Cancel(first) {
		t.Fatal("cancel failed")
	}
	if table.Begin(first, request, []byte{'a', 0, 'b'}) {
		t.Fatal("path containing NUL accepted")
	}
}

func TestContinuationPeerDeathCleanupRejectsLateReply(t *testing.T) {
	var table ContinuationTable
	task, _ := sched.NewTaskID(5, 9)
	request := Request{Number: abi.SysRead}
	if !table.Begin(task, request, nil) || !table.Issue(task, 1, 44) || !table.Block(task, 1) {
		t.Fatal("failed to establish waiting continuation")
	}
	if table.Finish(task) {
		t.Fatal("waiting continuation finished without cleanup")
	}
	if !table.Resume(task, 44, WakePeerDied) {
		t.Fatal("peer death did not resume exact continuation")
	}
	state, found := table.Lookup(task)
	if !found || state.Wake != WakePeerDied || state.Phase != ContinuationReady {
		t.Fatalf("peer-death state = %#v found=%v", state, found)
	}
	if !table.Finish(task) || table.Resume(task, 44, WakePeerDied) {
		t.Fatal("late peer-death reply reached cancelled continuation")
	}
}

func TestContinuationTableIsBoundedBySchedulerSlots(t *testing.T) {
	var table ContinuationTable
	request := Request{Number: abi.SysPpoll}
	for slot := uint32(0); slot < sched.MaxTasks; slot++ {
		task, _ := sched.NewTaskID(slot, 1)
		if !table.Begin(task, request, nil) {
			t.Fatalf("slot %d refused", slot)
		}
	}
	reused, _ := sched.NewTaskID(0, 2)
	if table.Begin(reused, request, nil) {
		t.Fatal("reused occupied slot accepted")
	}
}

func TestContinuationBlocksBeforeVFSIssue(t *testing.T) {
	var table ContinuationTable
	task, _ := sched.NewTaskID(4, 1)
	request := Request{Number: abi.SysOpen, Arguments: [6]uint64{1, 2, 3}}
	if !table.Begin(task, request, nil) || !table.BlockContext(task, 1) {
		t.Fatal("context wait did not start")
	}
	pending, _ := table.Pending(task)
	if pending.Phase != ContinuationWaiting || pending.Issued || pending.Correlation != 0 {
		t.Fatalf("blocked state = %#v", pending)
	}
	if !table.ResumeContext(task) {
		t.Fatal("context wait did not resume")
	}
	pending, _ = table.Pending(task)
	if pending.Phase != ContinuationReady || pending.Wake != WakeContext {
		t.Fatalf("resumed state = %#v", pending)
	}
	if !table.Issue(task, 1, 9) {
		t.Fatal("resumed call did not issue")
	}
	pending, _ = table.Pending(task)
	if pending.Wake != 0 || pending.Correlation != 9 || !pending.Issued {
		t.Fatalf("issued state = %#v", pending)
	}
}

func TestContinuationTracksSuccessfulSend(t *testing.T) {
	var table ContinuationTable
	task, _ := sched.NewTaskID(3, 1)
	if !table.Begin(task, Request{Number: abi.SysOpenat}, nil) || !table.Issue(task, OpenStageOpen, 41) {
		t.Fatal("could not issue open")
	}
	called := 0
	pending, _ := table.Pending(task)
	if !pending.Issued || pending.Sent || table.Send(task, OpenStageOpen, 42, func() bool { called++; return true }) || called != 0 {
		t.Fatal("unsent request marked sent")
	}
	if !table.Send(task, OpenStageOpen, 41, func() bool { called++; return true }) ||
		table.Send(task, OpenStageOpen, 41, func() bool { called++; return true }) || called != 1 {
		t.Fatal("send not recorded exactly once")
	}
	pending, _ = table.Pending(task)
	if !pending.Sent || !table.Block(task, OpenStageOpen) || !table.Resume(task, 41, WakeMessage) || !table.Advance(task, OpenStageOpen, OpenStageCleanup, 0) {
		t.Fatal("sent open did not advance")
	}
	pending, _ = table.Pending(task)
	if pending.Sent || pending.Issued {
		t.Fatal("send state carried into next stage")
	}
}

func TestContinuationAbandonReturnsSentOpenState(t *testing.T) {
	var table ContinuationTable
	task, _ := sched.NewTaskID(3, 1)
	if !table.Begin(task, Request{Number: abi.SysOpen}, nil) || !table.Issue(task, OpenStageOpen, 51) ||
		!table.Send(task, OpenStageOpen, 51, func() bool { return true }) {
		t.Fatal("could not send open")
	}
	pending, active := table.Abandon(task)
	if !active || !pending.AbandonFailsVFS() || !pending.Sent {
		t.Fatalf("abandoned open = %+v active=%v", pending, active)
	}
	if _, active = table.Pending(task); active {
		t.Fatal("abandoned continuation remained active")
	}
}

func TestCancelKnownKeepsOnlySentOpens(t *testing.T) {
	var table ContinuationTable
	issued, _ := sched.NewTaskID(4, 1)
	if !table.Begin(issued, Request{Number: abi.SysOpenat}, nil) || !table.Issue(issued, OpenStageOpen, 61) {
		t.Fatal("could not issue open")
	}
	if table.CancelKnown(issued) {
		t.Fatal("unsent open kept for exit teardown")
	}
	if _, active := table.Pending(issued); active {
		t.Fatal("unsent open stayed active")
	}
	if table.Send(issued, OpenStageOpen, 61, func() bool { return true }) {
		t.Fatal("cancelled open was sent")
	}
	sent, _ := sched.NewTaskID(5, 1)
	if !table.Begin(sent, Request{Number: abi.SysOpen}, nil) || !table.Issue(sent, OpenStageOpen, 62) ||
		!table.Send(sent, OpenStageOpen, 62, func() bool { return true }) {
		t.Fatal("could not send open")
	}
	if !table.CancelKnown(sent) {
		t.Fatal("sent open was not kept")
	}
	pending, active := table.Abandon(sent)
	if !active || !pending.Sent || !pending.AbandonFailsVFS() {
		t.Fatalf("exit teardown saw %+v active=%v", pending, active)
	}
	stat, _ := sched.NewTaskID(6, 1)
	if !table.Begin(stat, Request{Number: abi.SysStat}, nil) || table.CancelKnown(stat) {
		t.Fatal("stat kept for exit teardown")
	}
}

func TestExecOpenAbandonFailsOnlyAfterSend(t *testing.T) {
	pending := Pending{Number: abi.SysExecve, Stage: ExecStageOpen, Issued: true}
	if pending.AbandonFailsVFS() {
		t.Fatal("unsent exec open failed the session")
	}
	pending.Sent = true
	if !pending.AbandonFailsVFS() {
		t.Fatal("sent exec open did not fail the session")
	}
	pending.Stage = ExecStageStat
	if pending.AbandonFailsVFS() {
		t.Fatal("exec stat failed the session")
	}
}

func TestContinuationPendingAndTimeoutWake(t *testing.T) {
	var table ContinuationTable
	task, _ := sched.NewTaskID(3, 1)
	request := Request{Number: abi.SysOpenat, Arguments: [6]uint64{1, 2, 3, 4, 5, 6}}
	if _, found := table.Pending(task); found {
		t.Fatal("empty slot reported a pending call")
	}
	if !table.Begin(task, request, nil) || !table.Issue(task, 1, 41) || !table.Block(task, 1) {
		t.Fatal("continuation did not block")
	}
	pending, found := table.Pending(task)
	if !found || pending.Number != abi.SysOpenat || pending.Arguments != request.Arguments || pending.Stage != 1 ||
		pending.Phase != ContinuationWaiting || pending.Correlation != 41 || pending.Wake != 0 || !pending.Issued {
		t.Fatalf("pending = %#v found=%v", pending, found)
	}
	if table.Resume(task, 41, WakeTimeout+1) || !table.Resume(task, 41, WakeTimeout) {
		t.Fatal("timeout wake was not accepted exactly")
	}
	// A reply to an abandoned request leaves the call waiting again.
	if pending, _ = table.Pending(task); pending.Wake != WakeTimeout || pending.Phase != ContinuationReady || !table.Block(task, 1) {
		t.Fatalf("resumed call could not wait again: %#v", pending)
	}
	if !table.Resume(task, 41, WakeMessage) || !table.Finish(task) {
		t.Fatal("second wake did not finish")
	}
}

// Lookup copies a whole record, path included, for assertions. Kernel code
// reads the scalars through Pending instead: the record is over 4 KiB.
func (table *ContinuationTable) Lookup(task sched.TaskID) (Continuation, bool) {
	entry, ok := table.entry(task)
	if !ok {
		return Continuation{}, false
	}
	return *entry, true
}
