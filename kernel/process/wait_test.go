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
	if count := set.Expire(19, results); count != 0 {
		t.Fatalf("wait expired early: %d", count)
	}
	if count := set.Expire(20, results); count != 1 || results[0].Task != second {
		t.Fatalf("timeout result = %#v", results)
	}
}
