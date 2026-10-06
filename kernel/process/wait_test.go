package process

import (
	"goos/kernel/sched"
	"testing"
)

func TestProcessWaitWakeAndTimeout(t *testing.T) {
	var set WaitSet
	first, _ := sched.NewTaskID(1, 3)
	second, _ := sched.NewTaskID(2, 4)
	if !set.Add(first, 7, 10) || !set.Add(second, 8, 20) || set.Add(first, 9, 30) {
		t.Fatal("wait registration failed")
	}
	results := make([]WaitResult, 2)
	if count := set.Wake(7, results); count != 1 || results[0].Task != first {
		t.Fatalf("wake result = %#v", results)
	}
	if _, expired := set.ExpireTask(second, 19); expired {
		t.Fatal("wait expired early")
	}
	if result, expired := set.ExpireTask(second, 20); !expired || result.Task != second {
		t.Fatalf("timeout result = %#v", result)
	}
}

// TestWaitSetCancelReleasesRegistration covers the register-then-re-check
// sequence the kernel uses to close the lost-wakeup window: a waiter that finds
// its condition already satisfied must be able to withdraw.
func TestWaitSetCancelReleasesRegistration(t *testing.T) {
	var set WaitSet
	first, _ := sched.NewTaskID(1, 1)
	second, _ := sched.NewTaskID(2, 1)
	if !set.Add(first, 7, 100) || !set.Add(second, 7, 100) {
		t.Fatal("registration failed")
	}
	if !set.Cancel(first) || set.Cancel(first) {
		t.Fatal("cancel must remove exactly one registration")
	}
	var results [MaxWaiters]WaitResult
	if count := set.Wake(7, results[:]); count != 1 || results[0].Task != second {
		t.Fatalf("wake after cancel = %d, %v", count, results[0])
	}
	if !set.Add(first, 7, 100) {
		t.Fatal("cancelled slot was not reusable")
	}
}
