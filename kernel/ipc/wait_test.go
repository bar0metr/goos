package ipc

import (
	"goos/kernel/sched"
	"testing"
)

func TestWaitSetWakeAndTimeout(t *testing.T) {
	var set WaitSet
	if !set.Add(1, 7, 0, 10) || !set.Add(2, 7, 1, 20) || set.Add(1, 8, 0, 30) {
		t.Fatal("failed to register bounded waiters")
	}
	results := make([]WaitResult, 2)
	if count := set.Wake(7, 0, WaitMessage, results); count != 1 || results[0] != (WaitResult{Task: 1, Reason: WaitMessage}) {
		t.Fatalf("unexpected message wake: %#v", results)
	}
	if count := set.Expire(19, results); count != 0 {
		t.Fatalf("waiter expired early: %d", count)
	}
	if count := set.Expire(20, results); count != 1 || results[0] != (WaitResult{Task: 2, Reason: WaitTimeout}) {
		t.Fatalf("unexpected timeout wake: %#v", results)
	}
}

func TestWaitSetIsBounded(t *testing.T) {
	var set WaitSet
	for index := 0; index < MaxWaiters; index++ {
		if !set.Add(sched.TaskID(index+1), uint64(index+1), 0, 1) {
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
