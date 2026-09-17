package process

import (
	"goos/kernel/sched"
	"testing"
)

func TestRegistryDeliversOwnedTaskExit(t *testing.T) {
	first, _ := New(1, AddressSpace{Root: 0x1000})
	second, _ := New(2, AddressSpace{Root: 0x2000})
	task, _ := sched.NewTaskID(6, 1)
	var registry Registry
	if !registry.Add(&first) || !registry.Add(&second) || registry.Add(&first) ||
		!registry.BindTask(2, task) || registry.BindTask(1, task) ||
		!registry.ReportTaskExit(task, 9) {
		t.Fatal("registry setup failed")
	}
	event, ok := registry.NextExit()
	if !ok || event.Process != 2 || event.Task != task || event.Code != 9 {
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
	first, _ := sched.NewTaskID(3, 1)
	last, _ := sched.NewTaskID(4, 1)
	registry.BindTask(1, first)
	registry.BindTask(1, last)
	if !registry.ReportTaskExit(first, 0) {
		t.Fatal("first task exit rejected")
	}
	if _, ok := registry.NextExit(); ok {
		t.Fatal("process exited while a task remained")
	}
	if !registry.ReportTaskExit(last, 7) {
		t.Fatal("last task exit rejected")
	}
	event, ok := registry.NextExit()
	if !ok || event.Process != 1 || event.Task != last || event.Code != 7 {
		t.Fatalf("process exit = %#v", event)
	}
}

func TestRegistryWaitSurvivesExitDeliveryAndRemoval(t *testing.T) {
	var registry Registry
	process, _ := New(9, AddressSpace{Root: 0x9000})
	registry.Add(&process)
	task, _ := sched.NewTaskID(3, 5)
	registry.BindTask(9, task)
	if _, status := registry.Wait(9); status != WaitRunning {
		t.Fatalf("running wait status = %d", status)
	}
	if !registry.ReportTaskExit(task, 27) {
		t.Fatal("task exit rejected")
	}
	event, status := registry.Wait(9)
	if status != WaitExited || event.Process != 9 || event.Task != task || event.Code != 27 {
		t.Fatalf("completed wait = %#v, %d", event, status)
	}
	if _, ok := registry.NextExit(); !ok || !registry.Remove(9) {
		t.Fatal("exit delivery or removal failed")
	}
	event, status = registry.Wait(9)
	if status != WaitExited || event.Code != 27 {
		t.Fatalf("completion was not retained: %#v, %d", event, status)
	}
	if _, status = registry.Wait(99); status != WaitNotFound {
		t.Fatalf("unknown wait status = %d", status)
	}
}

func TestRegistryRemovesProcessSlot(t *testing.T) {
	first, _ := New(10, AddressSpace{Root: 0x1000})
	second, _ := New(11, AddressSpace{Root: 0x2000})
	var registry Registry
	if !registry.Add(&first) || !registry.Remove(10) || !registry.Add(&second) {
		t.Fatal("registry slot was not reusable")
	}
	if _, found := registry.ByID(10); found {
		t.Fatal("removed process remains registered")
	}
	if process, found := registry.ByID(11); !found || process != &second {
		t.Fatal("replacement process missing")
	}
}

func TestStartupCanOnlyBeFinalizedBeforeTaskBinding(t *testing.T) {
	owner, _ := New(12, AddressSpace{Root: 0x12000})
	if !owner.SetStartup([]byte("startup")) {
		t.Fatal("startup finalization failed")
	}
	var data [MaxStartupData]byte
	size, ok := owner.Startup(data[:])
	if !ok || string(data[:size]) != "startup" {
		t.Fatalf("startup = %q, %v", data[:size], ok)
	}
	task, _ := sched.NewTaskID(1, 1)
	if !owner.AddTask(task) || owner.SetStartup([]byte("changed")) {
		t.Fatal("startup changed after task binding")
	}
}
