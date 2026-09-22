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
		!registry.BindTask(2, task) || registry.BindTask(1, task) {
		t.Fatal("registry setup failed")
	}
	event, report := registry.ReportTaskExit(task, 9)
	if report != ExitProcessEnded || event.Process != 2 || event.Task != task || event.Code != 9 {
		t.Fatalf("exit event = %#v, report = %d", event, report)
	}
	if _, report := registry.ReportTaskExit(99, 1); report != ExitUnknownTask {
		t.Fatalf("unknown task report = %d", report)
	}
	if _, report := registry.ReportTaskExit(task, 1); report != ExitUnknownTask {
		t.Fatalf("repeated exit report = %d", report)
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
	if _, report := registry.ReportTaskExit(first, 0); report != ExitTaskRemoved {
		t.Fatalf("first task report = %d", report)
	}
	if _, status := registry.Wait(1); status != WaitRunning {
		t.Fatal("process completed while a task remained")
	}
	event, report := registry.ReportTaskExit(last, 7)
	if report != ExitProcessEnded || event.Process != 1 || event.Task != last || event.Code != 7 {
		t.Fatalf("process exit = %#v, report = %d", event, report)
	}
}

// TestCompletionRecordedForEveryProcess covers the teardown invariant: a waiter
// must be able to observe an exit even when many processes end in a burst, so
// recording a completion may not depend on any bounded queue having room.
func TestCompletionRecordedForEveryProcess(t *testing.T) {
	var registry Registry
	owners := make([]*Process, MaxExitEvents)
	for index := range owners {
		owner, _ := New(ID(index+1), AddressSpace{Root: uint64(index+1) * 0x1000})
		owners[index] = &owner
		if !registry.Add(&owner) {
			t.Fatalf("registry rejected process %d", index+1)
		}
		task, _ := sched.NewTaskID(uint32(index+1), 1)
		if !registry.BindTask(ID(index+1), task) {
			t.Fatalf("bind %d failed", index+1)
		}
	}
	for index := range owners {
		task, _ := sched.NewTaskID(uint32(index+1), 1)
		if _, report := registry.ReportTaskExit(task, uint64(index)); report != ExitProcessEnded {
			t.Fatalf("process %d report = %d", index+1, report)
		}
	}
	for index := range owners {
		event, status := registry.Wait(ID(index + 1))
		if status != WaitExited || event.Code != uint64(index) {
			t.Fatalf("process %d wait = %#v, %d", index+1, event, status)
		}
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
	if _, report := registry.ReportTaskExit(task, 27); report != ExitProcessEnded {
		t.Fatalf("task exit report = %d", report)
	}
	event, status := registry.Wait(9)
	if status != WaitExited || event.Process != 9 || event.Task != task || event.Code != 27 {
		t.Fatalf("completed wait = %#v, %d", event, status)
	}
	if !registry.Remove(9) {
		t.Fatal("removal failed")
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

func TestRegistryAtListsProcessesAndTasks(t *testing.T) {
	var registry Registry
	first, _ := New(1, AddressSpace{Root: 0x1000})
	registry.Add(&first)
	task, _ := sched.NewTaskID(3, 1)
	registry.BindTask(1, task)
	found, ok := registry.At(0)
	if !ok || found.ID() != 1 || len(found.Tasks()) != 1 || found.Tasks()[0] != task {
		t.Fatal("process or task not listed")
	}
	if _, ok := registry.At(1); ok {
		t.Fatal("empty index listed")
	}
}

func TestProcessNameIsBounded(t *testing.T) {
	process, _ := New(4, AddressSpace{Root: 0x1000})
	process.SetName("a-very-long-executable-name")
	if string(process.Name()) != "a-very-long-exec" {
		t.Fatalf("name %q", process.Name())
	}
}
