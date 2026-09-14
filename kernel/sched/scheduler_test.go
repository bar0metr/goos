package sched

import "testing"

func TestRoundRobinSelection(t *testing.T) {
	var scheduler Scheduler
	first, ok := scheduler.Add(Context{StackPointer: 0x1000})
	if !ok {
		t.Fatal("first task rejected")
	}
	second, ok := scheduler.Add(Context{StackPointer: 0x2000})
	if !ok {
		t.Fatal("second task rejected")
	}
	started, ok := scheduler.Start()
	if !ok || started.ID != first {
		t.Fatalf("unexpected first task: %#v", started)
	}
	next, ok := scheduler.Next()
	if !ok || next.ID != second {
		t.Fatalf("unexpected second task: %#v", next)
	}
	next, ok = scheduler.Next()
	if !ok || next.ID != first {
		t.Fatalf("round robin did not wrap: %#v", next)
	}
}

func TestBlockedTaskIsSkipped(t *testing.T) {
	var scheduler Scheduler
	first, _ := scheduler.Add(Context{StackPointer: 0x1000})
	second, _ := scheduler.Add(Context{StackPointer: 0x2000})
	third, _ := scheduler.Add(Context{StackPointer: 0x3000})
	scheduler.Start()
	if !scheduler.SetState(second, TaskBlocked) {
		t.Fatal("task state update failed")
	}
	next, ok := scheduler.Next()
	if !ok || next.ID != third || next.ID == first {
		t.Fatalf("blocked task was not skipped: %#v", next)
	}
}

func TestSchedulerRejectsInvalidAndExcessTasks(t *testing.T) {
	var scheduler Scheduler
	if _, ok := scheduler.Add(Context{}); ok {
		t.Fatal("accepted zero stack pointer")
	}
	for index := 0; index < MaxTasks; index++ {
		if _, ok := scheduler.Add(Context{StackPointer: uint64(index+1) * 0x1000}); !ok {
			t.Fatalf("task %d rejected", index)
		}
	}
	if _, ok := scheduler.Add(Context{StackPointer: 0x100000}); ok {
		t.Fatal("accepted task beyond fixed capacity")
	}
}
