package ipc

import "goos/kernel/sched"

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

// WaitSet is a bounded allocation-free collection used while IPC and the
// bootstrap scheduler are protected by disabled local interrupts.
type WaitSet struct {
	waiters [MaxWaiters]waiter
}

// Block registers a deadline before removing the task from the runnable set.
// Registration is rolled back if the scheduler rejects the transition.
func (set *WaitSet) Block(scheduler *sched.Scheduler, task sched.TaskID, channel uint64, side uint8, deadline uint64) bool {
	if scheduler == nil || !set.Add(task, channel, side, deadline) {
		return false
	}
	if scheduler.Block(task) {
		return true
	}
	set.Cancel(task)
	return false
}

func (set *WaitSet) Add(task sched.TaskID, channel uint64, side uint8, deadline uint64) bool {
	if _, _, valid := task.Parts(); !valid || channel == 0 || side > 1 || deadline == 0 {
		return false
	}
	for index := range set.waiters {
		if set.waiters[index].active && set.waiters[index].task == task {
			return false
		}
	}
	for index := range set.waiters {
		if !set.waiters[index].active {
			set.waiters[index] = waiter{task: task, channel: channel, side: side, deadline: deadline, active: true}
			return true
		}
	}
	return false
}

func (set *WaitSet) Wake(channel uint64, side uint8, reason WaitReason, output []WaitResult) int {
	if channel == 0 || side > 1 || reason == 0 {
		return 0
	}
	count := 0
	for index := range set.waiters {
		entry := &set.waiters[index]
		if entry.active && entry.channel == channel && entry.side == side && count < len(output) {
			output[count] = WaitResult{Task: entry.task, Reason: reason}
			count++
			*entry = waiter{}
		}
	}
	return count
}

func (set *WaitSet) Expire(now uint64, output []WaitResult) int {
	count := 0
	for index := range set.waiters {
		entry := &set.waiters[index]
		if entry.active && now >= entry.deadline && count < len(output) {
			output[count] = WaitResult{Task: entry.task, Reason: WaitTimeout}
			count++
			*entry = waiter{}
		}
	}
	return count
}

func (set *WaitSet) Apply(scheduler *sched.Scheduler, results []WaitResult) int {
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

// Cancel removes a task's registration and reports whether one existed. A
// waiter registers before it re-checks its condition, so that an event arriving
// in between cannot be missed; a successful re-check then cancels instead of
// blocking.
// Count reports how many registrations the set holds.
func (set *WaitSet) Count() int {
	count := 0
	for index := range set.waiters {
		if set.waiters[index].active {
			count++
		}
	}
	return count
}

func (set *WaitSet) Cancel(task sched.TaskID) bool {
	for index := range set.waiters {
		if set.waiters[index].active && set.waiters[index].task == task {
			set.waiters[index] = waiter{}
			return true
		}
	}
	return false
}
