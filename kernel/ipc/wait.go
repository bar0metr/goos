package ipc

import (
	"goos/kernel/deadline"
	"goos/kernel/lock"
	"goos/kernel/sched"
)

// MaxWaiters is one registration per task slot: a task waits on one thing at
// a time, so the table can never need more (ADR 0111).
const MaxWaiters = sched.MaxTasks

type WaitReason uint8

const (
	WaitMessage WaitReason = iota + 1
	WaitTimeout
	WaitPeerDied
)

type WaitResult struct {
	Task   sched.TaskID
	Reason WaitReason
}

type waiter struct {
	task     sched.TaskID
	channel  uint64
	side     uint8
	deadline uint64
	active   bool
}

// WaitSet is a bounded allocation-free collection of tasks blocked on a
// channel or stream side, one registration per task slot. Its lock makes
// each method one step, safe on several processors at once. With a deadline
// index installed, every registration's deadline is in the index while the
// registration lasts, so expiry visits only the tasks that are due.
type WaitSet struct {
	lock      lock.Spin
	waiters   [MaxWaiters]waiter
	deadlines *deadline.Index
	source    deadline.Source
}

// UseDeadlines keeps the set's deadlines in index under source. The kernel
// installs it before a task waits.
func (set *WaitSet) UseDeadlines(index *deadline.Index, source deadline.Source) {
	set.deadlines, set.source = index, source
}

// forget removes an entry's deadline from the index.
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

func (set *WaitSet) blockHeld(scheduler *sched.Scheduler, task sched.TaskID, channel uint64, side uint8, deadline uint64) bool {
	if scheduler == nil || !set.addHeld(task, channel, side, deadline) {
		return false
	}
	if scheduler.Block(task) {
		return true
	}
	set.cancelHeld(task)
	return false
}

func (set *WaitSet) addHeld(task sched.TaskID, channel uint64, side uint8, at uint64) bool {
	slot, _, valid := task.Parts()
	if !valid || channel == 0 || side > 1 || at == 0 {
		return false
	}
	entry := &set.waiters[slot]
	if entry.active && entry.task == task {
		return false
	}
	// A registration a dead task left in the slot goes with it.
	*entry = waiter{task: task, channel: channel, side: side, deadline: at, active: true}
	if set.deadlines != nil {
		set.deadlines.Arm(set.source, task, at)
	}
	return true
}

func (set *WaitSet) wakeHeld(channel uint64, side uint8, reason WaitReason, output []WaitResult) int {
	if channel == 0 || side > 1 || reason == 0 {
		return 0
	}
	count := 0
	for index := range set.waiters {
		entry := &set.waiters[index]
		if entry.active && entry.channel == channel && entry.side == side && count < len(output) {
			output[count] = WaitResult{Task: entry.task, Reason: reason}
			count++
			set.forget(entry)
		}
	}
	return count
}

// expireTaskHeld times out task's registration if its deadline has come.
// The index already let go of the deadline it reported.
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
	return WaitResult{Task: task, Reason: WaitTimeout}, true
}

func (set *WaitSet) applyHeld(scheduler *sched.Scheduler, results []WaitResult) int {
	if scheduler == nil {
		return 0
	}
	woken := 0
	for _, result := range results {
		if result.Task != 0 && result.Reason != 0 && scheduler.Wake(result.Task) {
			woken++
		}
	}
	return woken
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

// Block registers a deadline before removing the task from the runnable set.
// Registration is rolled back if the scheduler rejects the transition.
func (set *WaitSet) Block(scheduler *sched.Scheduler, task sched.TaskID, channel uint64, side uint8, deadline uint64) bool {
	set.take()
	result := set.blockHeld(scheduler, task, channel, side, deadline)
	set.lock.Unlock()
	return result
}

func (set *WaitSet) Add(task sched.TaskID, channel uint64, side uint8, deadline uint64) bool {
	set.take()
	result := set.addHeld(task, channel, side, deadline)
	set.lock.Unlock()
	return result
}

func (set *WaitSet) Wake(channel uint64, side uint8, reason WaitReason, output []WaitResult) int {
	set.take()
	result := set.wakeHeld(channel, side, reason, output)
	set.lock.Unlock()
	return result
}

// ExpireTask times out task's registration when its deadline is at or before
// now. A registration a wake already ended, or one the task made since, is
// left alone, so a timeout and a wake never both complete one wait.
func (set *WaitSet) ExpireTask(task sched.TaskID, now uint64) (WaitResult, bool) {
	set.take()
	result, expired := set.expireTaskHeld(task, now)
	set.lock.Unlock()
	return result, expired
}

func (set *WaitSet) Apply(scheduler *sched.Scheduler, results []WaitResult) int {
	set.take()
	result := set.applyHeld(scheduler, results)
	set.lock.Unlock()
	return result
}

// Cancel removes a task's registration and reports whether one existed. A
// waiter registers before it re-checks its condition, so that an event arriving
// in between cannot be missed; a successful re-check then cancels instead of
// blocking.
// Count reports how many registrations the set holds.
func (set *WaitSet) Count() int {
	set.take()
	result := set.countHeld()
	set.lock.Unlock()
	return result
}

func (set *WaitSet) Cancel(task sched.TaskID) bool {
	set.take()
	result := set.cancelHeld(task)
	set.lock.Unlock()
	return result
}
