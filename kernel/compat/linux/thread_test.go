package linux

import (
	"testing"

	"goos/kernel/handle"
	"goos/kernel/process"
	"goos/kernel/sched"
)

func TestThreadTIDsShareThePIDSpace(t *testing.T) {
	resetCompatibilityProcesses()
	data := linuxStartup(t, "/", [4]handle.ID{1, 2, 3, 4})
	streams := StandardStreams{Input: 1, Output: 2, Error: 3}
	if !InstallProcess(0x1000, 30, data, streams, 0x600000, 0x4600000) {
		t.Fatal("install failed")
	}
	state, _ := FindProcess(0x1000)
	main, _ := sched.NewTaskID(1, 1)
	worker, _ := sched.NewTaskID(2, 1)
	third, _ := sched.NewTaskID(3, 1)
	thread, added := AddThread(worker, 0x1000, AllocateTID())
	if !added || thread.TID == state.PID || thread.TID < firstPID {
		t.Fatalf("thread %+v beside PID %d", thread, state.PID)
	}
	if TaskTID(worker, state) != thread.TID || TaskTID(main, state) != state.PID || ThreadCount(0x1000) != 1 {
		t.Fatal("TID lookup wrong")
	}
	if _, again := AddThread(worker, 0x1000, AllocateTID()); again {
		t.Fatal("a task became two threads")
	}
	if _, orphan := AddThread(third, 0x7000, AllocateTID()); orphan {
		t.Fatal("a thread joined a process that does not exist")
	}
	// A process installed while the thread lives never gets its TID.
	nextPID = thread.TID
	if !InstallProcess(0x2000, 31, data, streams, 0x600000, 0x4600000) {
		t.Fatal("install failed")
	}
	if other, _ := FindProcess(0x2000); other.PID == thread.TID {
		t.Fatalf("PID %d reused a live TID", other.PID)
	}
	if group, found := ThreadGroupOf(thread.TID); !found || group != state.PID {
		t.Fatalf("thread %d belongs to %d, want %d", thread.TID, group, state.PID)
	}
	if _, found := ThreadGroupOf(state.PID); found {
		t.Fatal("a PID taken for a thread TID")
	}
	thread.ClearChildTID = 0x5000
	removed, found := RemoveThread(worker)
	if !found || removed.ClearChildTID != 0x5000 || ThreadCount(0x1000) != 0 {
		t.Fatalf("removed %+v", removed)
	}
	AddThread(worker, 0x1000, AllocateTID())
	if !ExitProcess(0x1000, Termination{}, nil) {
		t.Fatal("exit failed")
	}
	if _, left := FindThread(worker); left {
		t.Fatal("a thread outlived its process")
	}
	resetCompatibilityProcesses()
}

func TestThreadsHaveTheirOwnSignalMasks(t *testing.T) {
	resetCompatibilityProcesses()
	data := linuxStartup(t, "/", [4]handle.ID{1, 2, 3, 4})
	if !InstallProcess(0x1000, 30, data, StandardStreams{Input: 1, Output: 2, Error: 3}, 0x600000, 0x4600000) {
		t.Fatal("install failed")
	}
	state, _ := FindProcess(0x1000)
	first, _ := sched.NewTaskID(1, 1)
	worker, _ := sched.NewTaskID(2, 1)
	SelectThreadSignals(state, first)
	if _, errno := state.Signals.Mask(0, 1<<9, true); errno != 0 { // SIG_BLOCK SIGUSR1
		t.Fatal(errno)
	}
	thread, _ := AddThread(worker, 0x1000, AllocateTID())
	thread.Mask = state.Signals.SaveThreadMask()
	// The worker blocks everything, as a C library does before a thread
	// exits. The first thread's mask must not change.
	SelectThreadSignals(state, worker)
	if _, errno := state.Signals.Mask(2, ^uint64(0), true); errno != 0 { // SIG_SETMASK all
		t.Fatal(errno)
	}
	SelectThreadSignals(state, first)
	if mask, _ := state.Signals.Mask(0, 0, false); mask != 1<<9 {
		t.Fatalf("first thread's mask %#x, want only SIGUSR1", mask)
	}
	SelectThreadSignals(state, worker)
	if mask, _ := state.Signals.Mask(0, 0, false); mask == 1<<9 {
		t.Fatal("the worker lost its own mask")
	}
	// A thread that ended gives its mask up.
	RemoveThread(worker)
	SelectThreadSignals(state, first)
	if mask, _ := state.Signals.Mask(0, 0, false); mask != 1<<9 {
		t.Fatalf("after the worker ended the first thread has %#x", mask)
	}
	resetCompatibilityProcesses()
}

func TestThreadCreationRollsBackStageFailures(t *testing.T) {
	resetCompatibilityProcesses()
	data := linuxStartup(t, "/", [4]handle.ID{1, 2, 3, 4})
	streams := StandardStreams{Input: 1, Output: 2, Error: 3}
	if !InstallProcess(0x1000, 30, data, streams, 0x600000, 0x4600000) {
		t.Fatal("install failed")
	}
	exhausted, _ := process.New(30, process.ABILinuxX8664, process.AddressSpace{Root: 0x1000})
	limit := exhausted.Available(process.ResourceTasks)
	for index := uint32(0); index < limit; index++ {
		task, _ := sched.NewTaskID(index, 1)
		if !exhausted.AddTask(task) {
			t.Fatal("quota setup failed")
		}
	}
	if _, ok := PrepareThreadCreation(&exhausted, 0x1000); ok {
		t.Fatal("creation passed an exhausted task quota")
	}

	owner, _ := process.New(31, process.ABILinuxX8664, process.AddressSpace{Root: 0x1000})
	available := owner.Available(process.ResourceTasks)
	for index := range threads {
		threads[index] = Thread{Root: 0x2000, TID: firstPID + int32(index)}
	}
	if _, ok := PrepareThreadCreation(&owner, 0x1000); ok || owner.Available(process.ResourceTasks) != available {
		t.Fatal("full thread table leaked the task charge")
	}
	threads = [len(threads)]Thread{}

	creation, ok := PrepareThreadCreation(&owner, 0x1000)
	prepared, found := creation.PreparedThread()
	if !ok || !found || prepared.TID < firstPID || !tidInUse(prepared.TID) {
		t.Fatal("thread preparation failed")
	}
	prepared.ClearChildTID = 0x5000
	// Cancel clears the record prepared points at, TID included.
	reservedTID := prepared.TID
	if !creation.Cancel() || owner.Available(process.ResourceTasks) != available || tidInUse(reservedTID) ||
		ThreadCount(0x1000) != 0 || ThreadRecords(0x1000) != 0 {
		t.Fatal("task creation failure left prepared state")
	}

	creation, ok = PrepareThreadCreation(&owner, 0x1000)
	if _, committed := creation.Commit(0); !ok || committed || owner.Available(process.ResourceTasks) != available || ThreadCount(0x1000) != 0 {
		t.Fatal("invalid task commit left prepared state")
	}

	creation, ok = PrepareThreadCreation(&owner, 0x1000)
	task, _ := sched.NewTaskID(2, 1)
	thread, committed := creation.Commit(task)
	if !ok || !committed || thread.Task != task || ThreadCount(0x1000) != 1 || owner.TaskCount() != 1 {
		t.Fatal("prepared thread did not commit")
	}
	if _, found := FindThread(task); !found {
		t.Fatal("committed thread missing")
	}
	resetCompatibilityProcesses()
}
