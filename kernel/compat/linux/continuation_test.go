package linux

import (
	"testing"

	"goos/kernel/compat/linux/abi"
	"goos/kernel/sched"
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
