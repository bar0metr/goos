package ipc

import (
	"goos/kernel/sched"
	"testing"
)

func TestWaitSetWakeAndTimeout(t *testing.T) {
	var set WaitSet
	first, _ := sched.NewTaskID(0, 1)
	second, _ := sched.NewTaskID(1, 1)
	if !set.Add(first, 7, 0, 10) || !set.Add(second, 7, 1, 20) || set.Add(first, 8, 0, 30) {
		t.Fatal("failed to register bounded waiters")
	}
	results := make([]WaitResult, 2)
	if count := set.Wake(7, 0, WaitMessage, results); count != 1 || results[0] != (WaitResult{Task: first, Reason: WaitMessage}) {
		t.Fatalf("unexpected message wake: %#v", results)
	}
	if count := set.Expire(19, results); count != 0 {
		t.Fatalf("waiter expired early: %d", count)
	}
	if count := set.Expire(20, results); count != 1 || results[0] != (WaitResult{Task: second, Reason: WaitTimeout}) {
		t.Fatalf("unexpected timeout wake: %#v", results)
	}
}

func TestWaitSetIsBounded(t *testing.T) {
	var set WaitSet
	for index := 0; index < MaxWaiters; index++ {
		task, _ := sched.NewTaskID(uint32(index), 1)
		if !set.Add(task, uint64(index+1), 0, 1) {
			t.Fatalf("wait set filled at %d", index)
		}
	}
	if set.Add(MaxWaiters+1, MaxWaiters+1, 0, 1) || set.Add(100, 1, 2, 1) {
		t.Fatal("invalid or overflowing waiter accepted")
	}
}

func TestWaitSetBlocksAndWakesSchedulerTask(t *testing.T) {
	var scheduler sched.Scheduler
	task, ok := scheduler.Add(sched.Context{StackPointer: 0x1000})
	if !ok {
		t.Fatal("task setup failed")
	}
	if _, ok := scheduler.Start(); !ok {
		t.Fatal("scheduler start failed")
	}
	var set WaitSet
	if !set.Block(&scheduler, task, 11, 0, 50) {
		t.Fatal("IPC wait did not block running task")
	}
	results := make([]WaitResult, 1)
	count := set.Wake(11, 0, WaitPeerDied, results)
	if count != 1 || set.Apply(&scheduler, results[:count]) != 1 {
		t.Fatal("IPC event did not wake blocked task")
	}
	if _, ok := scheduler.Next(); !ok {
		t.Fatal("woken task did not become runnable")
	}
}

// TestWaitSetCancelReleasesRegistration covers the register-then-re-check
// sequence the kernel uses to close the lost-wakeup window: a waiter whose
// condition is already satisfied must be able to withdraw before blocking.
func TestWaitSetCancelReleasesRegistration(t *testing.T) {
	var set WaitSet
	first, _ := sched.NewTaskID(1, 1)
	second, _ := sched.NewTaskID(2, 1)
	if !set.Add(first, 5, 0, 100) || !set.Add(second, 5, 0, 100) {
		t.Fatal("registration failed")
	}
	if !set.Cancel(first) || set.Cancel(first) {
		t.Fatal("cancel must remove exactly one registration")
	}
	var results [MaxWaiters]WaitResult
	if count := set.Wake(5, 0, WaitMessage, results[:]); count != 1 || results[0].Task != second {
		t.Fatalf("wake after cancel = %d, %v", count, results[0])
	}
	if !set.Add(first, 5, 0, 100) {
		t.Fatal("cancelled slot was not reusable")
	}
}

func TestWaitSetCountTracksRegistrations(t *testing.T) {
	var set WaitSet
	task, _ := sched.NewTaskID(1, 1)
	if set.Count() != 0 {
		t.Fatal("empty set counted registrations")
	}
	set.Add(task, 3, 0, 50)
	if set.Count() != 1 {
		t.Fatalf("count = %d", set.Count())
	}
	set.Cancel(task)
	if set.Count() != 0 {
		t.Fatalf("count after cancel = %d", set.Count())
	}
}
