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

// The task quota binds before the table does, which is the point: the table
// size is a structural bound and the quota is the policy one.
func TestProcessTaskCapacityIsBounded(t *testing.T) {
	process, _ := New(2, AddressSpace{Root: 0x1000})
	_, limits := process.Quota()
	limit := int(limits[ResourceTasks])
	if limit >= MaxTasksPerProcess {
		t.Fatalf("task quota %d does not bind before the table of %d", limit, MaxTasksPerProcess)
	}
	for index := 0; index < limit; index++ {
		task, _ := sched.NewTaskID(uint32(index), 1)
		if !process.AddTask(task) {
			t.Fatalf("task %d rejected", index)
		}
	}
	overflow, _ := sched.NewTaskID(uint32(limit), 1)
	if process.AddTask(overflow) {
		t.Fatal("process task quota exceeded")
	}
	if used, _ := process.Quota(); used[ResourceTasks] != uint32(limit) {
		t.Fatalf("tasks charged %d, expected %d", used[ResourceTasks], limit)
	}
}

func TestInitPreparesSlotInPlace(t *testing.T) {
	var slot Process
	task, _ := sched.NewTaskID(0, 1)
	if !Init(&slot, 1, AddressSpace{Root: 0x1000}) || !slot.AddTask(task) {
		t.Fatal("valid process rejected")
	}
	if !Init(&slot, 2, AddressSpace{Root: 0x2000}) {
		t.Fatal("slot reuse rejected")
	}
	if slot.ID() != 2 || slot.AddressSpace().Root != 0x2000 || slot.TaskCount() != 0 {
		t.Fatalf("slot not reinitialized: id=%d root=%#x tasks=%d", slot.ID(), slot.AddressSpace().Root, slot.TaskCount())
	}
	if Init(nil, 1, AddressSpace{Root: 0x1000}) || Init(&slot, 0, AddressSpace{Root: 0x1000}) || Init(&slot, 1, AddressSpace{}) {
		t.Fatal("invalid identity accepted")
	}
}

// TestInitDoesNotAllocate guards the kernel, which leaks every allocation and
// cannot grow its heap after it leaves boot services.
func TestInitDoesNotAllocate(t *testing.T) {
	var slot Process
	if allocations := testing.AllocsPerRun(100, func() { Init(&slot, 1, AddressSpace{Root: 0x1000}) }); allocations != 0 {
		t.Fatalf("Init allocated %v times per run", allocations)
	}
}
