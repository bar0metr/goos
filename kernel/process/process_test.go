package process

import (
	"goos/kernel/sched"
	"testing"
)

func TestProcessOwnsAddressSpaceAndTasks(t *testing.T) {
	process, ok := New(1, AddressSpace{Root: 0x1000})
	if !ok || process.ID() != 1 || process.AddressSpace().Root != 0x1000 {
		t.Fatal("valid process rejected")
	}
	task, _ := sched.NewTaskID(0, 1)
	if !process.AddTask(task) || process.AddTask(task) {
		t.Fatal("duplicate task ownership accepted")
	}
	if process.TaskCount() != 1 {
		t.Fatalf("task count = %d", process.TaskCount())
	}
}

func TestProcessRejectsInvalidIdentity(t *testing.T) {
	if _, ok := New(0, AddressSpace{Root: 0x1000}); ok {
		t.Fatal("zero process ID accepted")
	}
	if _, ok := New(1, AddressSpace{}); ok {
		t.Fatal("zero address-space root accepted")
	}
}

func TestProcessTaskCapacityIsBounded(t *testing.T) {
	process, _ := New(1, AddressSpace{Root: 0x1000})
	for index := 0; index < MaxTasksPerProcess; index++ {
		task, _ := sched.NewTaskID(uint32(index), 1)
		if !process.AddTask(task) {
			t.Fatalf("task %d rejected", index)
		}
	}
	overflow, _ := sched.NewTaskID(MaxTasksPerProcess, 1)
	if process.AddTask(overflow) {
		t.Fatal("process task capacity exceeded")
	}
}
