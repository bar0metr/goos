// Package nativethread keeps the thread group of one native process: which
// task runs in each of its thread slots, how each ended, who waits to join
// it, the process's exit code once a thread set it, and the tasks a
// collection holds (ADR 0203). The kernel changes a group only under the
// registry's write lock; the package itself takes no lock and allocates
// nothing.
package nativethread

import (
	"goos/kernel/kcall"
	"goos/kernel/sched"
)

// Slots is the number of thread slots of a process. Slot 0 is the main
// task.
const Slots = kcall.ThreadSlots

// State is what a thread slot holds.
type State uint8

const (
	// Free slots hold nothing.
	Free State = iota
	// Reserved slots are claimed by a creation that has not started its
	// task yet.
	Reserved
	// Running slots hold a live thread.
	Running
	// Exited slots hold a thread that ended and was not joined.
	Exited
)

// Thread is one slot.
type Thread struct {
	Task  sched.TaskID
	State State
	// Leaving marks a thread that called ThreadExit: its end is its own and
	// does not take the process with it.
	Leaving bool
	Code    uint64
	// Joiner is the task waiting to join the thread, or zero.
	Joiner sched.TaskID
	// JoinToken reserves the join for a goroutine independently of its M.
	// The reservation remains until collection, release or process teardown.
	JoinToken uint64
}

// Group is a process's thread slots and the state they share.
type Group struct {
	threads      [Slots]Thread
	joinSequence uint64
	exitCode     uint64
	exitSet      bool
	// suspender is the task whose ThreadSuspend holds held[:heldCount].
	suspender sched.TaskID
	held      [Slots]sched.TaskID
	heldCount int
}

// Reset empties the group in place; a composite literal this size would be
// built on the kernel's leaking heap.
func (group *Group) Reset() {
	for index := range group.threads {
		group.threads[index] = Thread{}
	}
	group.exitCode, group.exitSet, group.joinSequence = 0, false, 0
	group.suspender, group.heldCount = 0, 0
	for index := range group.held {
		group.held[index] = 0
	}
}

// Adopt records main as the process's main thread when the group is still
// empty: the first thread call of a process names its first task.
func (group *Group) Adopt(main sched.TaskID) {
	if group.threads[0].State == Free && main != 0 {
		group.threads[0] = Thread{Task: main, State: Running}
	}
}

// Reserve claims a free slot for a thread being created.
func (group *Group) Reserve() (uint64, bool) {
	for slot := 1; slot < Slots; slot++ {
		if group.threads[slot].State == Free {
			group.threads[slot] = Thread{State: Reserved}
			return uint64(slot), true
		}
	}
	return 0, false
}

// Commit binds the task of a reserved slot. It runs before the task starts.
func (group *Group) Commit(slot uint64, task sched.TaskID) bool {
	if slot == 0 || slot >= Slots || group.threads[slot].State != Reserved || task == 0 {
		return false
	}
	group.threads[slot] = Thread{Task: task, State: Running}
	return true
}

// Cancel returns a reserved slot whose creation failed.
func (group *Group) Cancel(slot uint64) {
	if slot > 0 && slot < Slots && group.threads[slot].State == Reserved {
		group.threads[slot] = Thread{}
	}
}

// SlotOf returns the slot of a live thread.
func (group *Group) SlotOf(task sched.TaskID) (uint64, bool) {
	if task == 0 {
		return 0, false
	}
	for slot := range group.threads {
		if group.threads[slot].Task == task && group.threads[slot].State == Running {
			return uint64(slot), true
		}
	}
	return 0, false
}

// Live returns the number of live threads.
func (group *Group) Live() int {
	count := 0
	for slot := range group.threads {
		if group.threads[slot].State == Running {
			count++
		}
	}
	return count
}

// Leave marks the calling thread as ending by its own ThreadExit.
func (group *Group) Leave(task sched.TaskID, code uint64) bool {
	slot, ok := group.SlotOf(task)
	if !ok {
		return false
	}
	group.threads[slot].Leaving, group.threads[slot].Code = true, code
	return true
}

// Ending is what Ended found.
type Ending struct {
	Slot uint64
	// Leaving reports a thread that called ThreadExit; anything else ends
	// the process.
	Leaving bool
	Code    uint64
	// Joiner is the task to wake.
	Joiner sched.TaskID
}

// Ended records that a live thread's task is gone. A thread that left by
// ThreadExit keeps its code for a joiner; any other end keeps code.
func (group *Group) Ended(task sched.TaskID, code uint64) (Ending, bool) {
	slot, ok := group.SlotOf(task)
	if !ok {
		return Ending{}, false
	}
	thread := &group.threads[slot]
	if !thread.Leaving {
		thread.Code = code
	}
	ending := Ending{Slot: slot, Leaving: thread.Leaving, Code: thread.Code, Joiner: thread.Joiner}
	thread.State = Exited
	if slot == 0 {
		// Nobody joins the main thread; its slot keeps it from being
		// adopted again.
		thread.Task = 0
	}
	return ending, true
}

// JoinStatus is what Join found.
type JoinStatus uint8

const (
	// Joined: the thread had ended; its code is returned and its slot freed.
	Joined JoinStatus = iota
	// Waiting: the thread runs and the caller is now its joiner.
	Waiting
	// Busy: another task is already its joiner.
	Busy
	// NotFound: no thread of the group has that ID, or it was joined.
	NotFound
	// Self: a thread cannot join itself.
	Self
	// Exhausted: the group has used its last join token.
	Exhausted
)

// Join claims the end of thread for caller. A caller that registers as the
// joiner calls Join again when woken.
func (group *Group) Join(caller, thread sched.TaskID, register bool) (uint64, JoinStatus) {
	if thread == 0 {
		return 0, NotFound
	}
	if thread == caller {
		return 0, Self
	}
	for slot := 1; slot < Slots; slot++ {
		entry := &group.threads[slot]
		if entry.Task != thread {
			continue
		}
		if entry.JoinToken != 0 || entry.Joiner != 0 && entry.Joiner != caller {
			return 0, Busy
		}
		switch entry.State {
		case Exited:
			code := entry.Code
			*entry = Thread{}
			return code, Joined
		case Running:
			if register {
				entry.Joiner = caller
			}
			return 0, Waiting
		}
	}
	return 0, NotFound
}

// Find returns the state of a thread that Join can name: a running thread or
// one that ended and was not joined. The main thread is never found.
func (group *Group) Find(thread sched.TaskID) (State, bool) {
	if entry := group.joinTarget(thread); entry != nil {
		return entry.State, true
	}
	return Free, false
}

// Forget drops caller as joiner wherever it waits, when it ends.
func (group *Group) Forget(caller sched.TaskID) {
	for slot := range group.threads {
		if group.threads[slot].Joiner == caller {
			group.threads[slot].Joiner = 0
		}
	}
}

// Free returns the slot of a thread whose task ended and that nobody may
// join any more: the process is ending.
func (group *Group) Free(slot uint64) {
	if slot > 0 && slot < Slots && group.threads[slot].State == Exited {
		group.threads[slot] = Thread{}
	}
}

// SetExitCode records the process's exit code unless one is set: the first
// Exit, fault, or kill of a thread decides it.
func (group *Group) SetExitCode(code uint64) bool {
	if group.exitSet {
		return false
	}
	group.exitCode, group.exitSet = code, true
	return true
}

// ExitCode returns the process's recorded exit code.
func (group *Group) ExitCode() (uint64, bool) { return group.exitCode, group.exitSet }

// Others copies into into the tasks of every live thread but self and
// returns how many.
func (group *Group) Others(self sched.TaskID, into []sched.TaskID) int {
	count := 0
	for slot := range group.threads {
		thread := &group.threads[slot]
		if thread.State == Running && thread.Task != self && count < len(into) {
			into[count] = thread.Task
			count++
		}
	}
	return count
}

// BeginSuspend records the tasks suspender holds. Only one suspension holds
// tasks at a time.
func (group *Group) BeginSuspend(suspender sched.TaskID, tasks []sched.TaskID) bool {
	if group.suspender != 0 || suspender == 0 || len(tasks) > len(group.held) {
		return false
	}
	group.suspender = suspender
	group.heldCount = copy(group.held[:], tasks)
	return true
}

// Suspender returns the task that holds the group's other threads, or zero.
func (group *Group) Suspender() sched.TaskID { return group.suspender }

// EndSuspend clears the suspension of suspender and returns the tasks it
// held, which the caller releases.
func (group *Group) EndSuspend(suspender sched.TaskID, into []sched.TaskID) (int, bool) {
	if suspender == 0 || group.suspender != suspender {
		return 0, false
	}
	count := copy(into, group.held[:group.heldCount])
	group.suspender, group.heldCount = 0, 0
	for index := range group.held {
		group.held[index] = 0
	}
	return count, true
}

// ClaimJoin reserves the join of thread for a goroutine. The group issues
// each token once; an old token cannot collect or release a later claim.
// No native task owns the token, so ending an M does not revoke a migrated
// goroutine's reservation. The runtime releases it when that G stops waiting;
// Reset clears reservations when the process ends.
func (group *Group) ClaimJoin(caller, thread sched.TaskID) (uint64, JoinStatus) {
	if thread == caller && thread != 0 {
		return 0, Self
	}
	entry := group.joinTarget(thread)
	if entry == nil {
		return 0, NotFound
	}
	if entry.JoinToken != 0 || entry.Joiner != 0 {
		return 0, Busy
	}
	if group.joinSequence == ^uint64(0) {
		return 0, Exhausted
	}
	group.joinSequence++
	entry.JoinToken = group.joinSequence
	return entry.JoinToken, Waiting
}

// CollectJoin polls a claimed join without registering a task waiter.
func (group *Group) CollectJoin(thread sched.TaskID, token uint64) (uint64, JoinStatus) {
	entry := group.joinTarget(thread)
	if entry == nil || token == 0 || entry.JoinToken != token {
		return 0, NotFound
	}
	if entry.State == Running {
		return 0, Waiting
	}
	code := entry.Code
	*entry = Thread{}
	return code, Joined
}

// ReleaseJoin relinquishes a claim, including one whose target has ended.
// The caller may then join the target through either native or token calls.
func (group *Group) ReleaseJoin(thread sched.TaskID, token uint64) bool {
	entry := group.joinTarget(thread)
	if entry == nil || token == 0 || entry.JoinToken != token {
		return false
	}
	entry.JoinToken = 0
	return true
}

func (group *Group) joinTarget(thread sched.TaskID) *Thread {
	if thread == 0 {
		return nil
	}
	for slot := 1; slot < Slots; slot++ {
		entry := &group.threads[slot]
		if entry.Task == thread && (entry.State == Running || entry.State == Exited) {
			return entry
		}
	}
	return nil
}
