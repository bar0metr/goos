package process

import "goos/kernel/sched"

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

// WaitSet is a bounded collection of tasks blocked on process completion.
type WaitSet struct {
	waiters [MaxWaiters]waiter
}

func (set *WaitSet) Add(task sched.TaskID, process ID, deadline uint64) bool {
	if _, _, valid := task.Parts(); !valid || process == 0 || deadline == 0 {
		return false
	}
	for index := range set.waiters {
		if set.waiters[index].active && set.waiters[index].task == task {
			return false
		}
	}
	for index := range set.waiters {
		if !set.waiters[index].active {
			set.waiters[index] = waiter{task: task, process: process, deadline: deadline, active: true}
			return true
		}
	}
	return false
}

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

// Cancel removes a task's registration and reports whether one existed. A
// waiter registers before it re-checks completion, so that an exit delivered in
// between cannot be missed; a successful re-check then cancels instead of
// blocking.
func (set *WaitSet) Cancel(task sched.TaskID) bool {
	for index := range set.waiters {
		if set.waiters[index].active && set.waiters[index].task == task {
			set.waiters[index] = waiter{}
			return true
		}
	}
	return false
}

func (set *WaitSet) Wake(process ID, output []WaitResult) int {
	count := 0
	for index := range set.waiters {
		entry := &set.waiters[index]
		if entry.active && entry.process == process && count < len(output) {
			output[count] = WaitResult{Task: entry.task}
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
			output[count] = WaitResult{Task: entry.task}
			count++
			*entry = waiter{}
		}
	}
	return count
}
