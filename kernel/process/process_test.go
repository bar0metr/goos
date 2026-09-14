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
	if !process.AddTask(sched.TaskID(1)) || process.AddTask(sched.TaskID(1)) {
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
		if !process.AddTask(sched.TaskID(index + 1)) {
			t.Fatalf("task %d rejected", index)
		}
	}
	if process.AddTask(sched.TaskID(MaxTasksPerProcess + 1)) {
		t.Fatal("process task capacity exceeded")
	}
}
