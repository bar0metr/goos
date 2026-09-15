package process

import (
	"goos/kernel/sched"
	"testing"
)

func TestRegistryDeliversOwnedTaskExit(t *testing.T) {
	first, _ := New(1, AddressSpace{Root: 0x1000})
	second, _ := New(2, AddressSpace{Root: 0x2000})
	var registry Registry
	if !registry.Add(&first) || !registry.Add(&second) || registry.Add(&first) ||
		!registry.BindTask(2, sched.TaskID(7)) || registry.BindTask(1, sched.TaskID(7)) ||
		!registry.ReportTaskExit(sched.TaskID(7), 9) {
		t.Fatal("registry setup failed")
	}
	event, ok := registry.NextExit()
	if !ok || event.Process != 2 || event.Task != 7 || event.Code != 9 {
		t.Fatalf("exit event = %#v", event)
	}
	if _, ok := registry.NextExit(); ok || registry.ReportTaskExit(99, 1) {
		t.Fatal("invalid exit event accepted")
	}
}

func TestRegistryWaitsForLastTask(t *testing.T) {
	owner, _ := New(1, AddressSpace{Root: 0x1000})
	var registry Registry
	registry.Add(&owner)
	registry.BindTask(1, 4)
	registry.BindTask(1, 5)
	if !registry.ReportTaskExit(4, 0) {
		t.Fatal("first task exit rejected")
	}
	if _, ok := registry.NextExit(); ok {
		t.Fatal("process exited while a task remained")
	}
	if !registry.ReportTaskExit(5, 7) {
		t.Fatal("last task exit rejected")
	}
	event, ok := registry.NextExit()
	if !ok || event.Process != 1 || event.Task != 5 || event.Code != 7 {
		t.Fatalf("process exit = %#v", event)
	}
}
