package process

import (
	"goos/kernel/deadline"
	"goos/kernel/lock"
	"goos/kernel/sched"
)

// MaxWaiters is one registration per task slot: a task waits on one thing at
// a time, so the table can never need more (ADR 0111).
const MaxWaiters = sched.MaxTasks

type WaitResult struct {
	Task sched.TaskID
}

type waiter struct {
	task     sched.TaskID
	process  ID
	deadline uint64
	active   bool
}

// WaitSet is a bounded collection of tasks blocked on process completion,
// one registration per task slot. Its lock makes each method one step, safe
// on several processors at once. With a deadline index installed, every
// registration's deadline is in the index while the registration lasts.
type WaitSet struct {
	lock      lock.Spin
	waiters   [MaxWaiters]waiter
	deadlines *deadline.Index
	source    deadline.Source
}

// UseDeadlines keeps the set's deadlines in index under source.
func (set *WaitSet) UseDeadlines(index *deadline.Index, source deadline.Source) {
	set.deadlines, set.source = index, source
}

func (set *WaitSet) forget(entry *waiter) {
	if set.deadlines != nil {
		set.deadlines.Cancel(set.source, entry.task)
	}
	*entry = waiter{}
}

func (set *WaitSet) take() {
	set.lock.SetLevel(lock.LevelWait)
	set.lock.Lock()
}

func (set *WaitSet) addHeld(task sched.TaskID, process ID, at uint64) bool {
	slot, _, valid := task.Parts()
	if !valid || process == 0 || at == 0 {
		return false
	}
	entry := &set.waiters[slot]
	if entry.active && entry.task == task {
		return false
	}
	*entry = waiter{task: task, process: process, deadline: at, active: true}
	if set.deadlines != nil {
		set.deadlines.Arm(set.source, task, at)
	}
	return true
}

func (set *WaitSet) countHeld() int {
	count := 0
	for index := range set.waiters {
		if set.waiters[index].active {
			count++
		}
	}
	return count
}

func (set *WaitSet) cancelHeld(task sched.TaskID) bool {
	slot, _, valid := task.Parts()
	if !valid || !set.waiters[slot].active || set.waiters[slot].task != task {
		return false
	}
	set.forget(&set.waiters[slot])
	return true
}

func (set *WaitSet) wakeHeld(process ID, output []WaitResult) int {
	count := 0
	for index := range set.waiters {
		entry := &set.waiters[index]
		if entry.active && entry.process == process && count < len(output) {
			output[count] = WaitResult{Task: entry.task}
			count++
			set.forget(entry)
		}
	}
	return count
}

func (set *WaitSet) expireTaskHeld(task sched.TaskID, now uint64) (WaitResult, bool) {
	slot, _, valid := task.Parts()
	if !valid {
		return WaitResult{}, false
	}
	entry := &set.waiters[slot]
	if !entry.active || entry.task != task || now < entry.deadline {
		return WaitResult{}, false
	}
	*entry = waiter{}
	return WaitResult{Task: task}, true
}

func (set *WaitSet) Add(task sched.TaskID, process ID, deadline uint64) bool {
	set.take()
	result := set.addHeld(task, process, deadline)
	set.lock.Unlock()
	return result
}

// Count reports how many registrations the set holds.
func (set *WaitSet) Count() int {
	set.take()
	result := set.countHeld()
	set.lock.Unlock()
	return result
}

// Cancel removes a task's registration and reports whether one existed. A
// waiter registers before it re-checks completion, so that an exit delivered in
// between cannot be missed; a successful re-check then cancels instead of
// blocking.
func (set *WaitSet) Cancel(task sched.TaskID) bool {
	set.take()
	result := set.cancelHeld(task)
	set.lock.Unlock()
	return result
}

func (set *WaitSet) Wake(process ID, output []WaitResult) int {
	set.take()
	result := set.wakeHeld(process, output)
	set.lock.Unlock()
	return result
}

// ExpireTask times out task's registration when its deadline is at or before
// now, and leaves alone one a wake ended or the task made since.
func (set *WaitSet) ExpireTask(task sched.TaskID, now uint64) (WaitResult, bool) {
	set.take()
	result, expired := set.expireTaskHeld(task, now)
	set.lock.Unlock()
	return result, expired
}
