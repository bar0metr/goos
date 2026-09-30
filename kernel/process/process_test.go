package process

import (
	"goos/kernel/mm/region"
	"goos/kernel/sched"
	"testing"
)

func TestProcessOwnsAddressSpaceAndTasks(t *testing.T) {
	process, ok := New(1, ABINative, AddressSpace{Root: 0x1000})
	if !ok || process.ID() != 1 || process.ABI() != ABINative || process.AddressSpace().Root != 0x1000 {
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

func TestReplaceAddressSpacePreservesProcessIdentity(t *testing.T) {
	value, ok := New(7, ABILinuxX8664, AddressSpace{Root: 0x1000})
	if !ok {
		t.Fatal("process creation failed")
	}
	value.SetName([]byte("old"))
	var regions region.Map
	regions.Init(0x1000, 0x9000)
	if !value.ReplaceAddressSpace(AddressSpace{Root: 0x2000}, &regions) {
		t.Fatal("replacement failed")
	}
	if value.ID() != 7 || value.ABI() != ABILinuxX8664 || value.AddressSpace().Root != 0x2000 || string(value.Name()) != "old" {
		t.Fatalf("process changed: id=%d abi=%d root=%#x name=%q", value.ID(), value.ABI(), value.AddressSpace().Root, value.Name())
	}
}

func TestInitChildCopiesLimitsRegionsAndName(t *testing.T) {
	var parent, child Process
	if !InitWithProfile(&parent, 3, ABILinuxX8664, AddressSpace{Root: 0x1000}, ProfileLarge) {
		t.Fatal("parent init failed")
	}
	parent.SetName([]byte("busybox"))
	if parent.Regions().Insert(region.Region{Base: 0x400000, Length: 0x2000, Protection: region.Read | region.Write, Backing: region.Anonymous}) != region.OK ||
		!parent.Charge(ResourcePages, 5) {
		t.Fatal("parent setup failed")
	}
	task, _ := sched.NewTaskID(0, 1)
	parent.AddTask(task)
	if InitChild(&child, 4, &parent, AddressSpace{}) || InitChild(&parent, 4, &parent, AddressSpace{Root: 0x2000}) {
		t.Fatal("invalid child accepted")
	}
	if !InitChild(&child, 4, &parent, AddressSpace{Root: 0x2000}) {
		t.Fatal("child init failed")
	}
	used, limits := child.Quota()
	_, parentLimits := parent.Quota()
	if child.ID() != 4 || child.ABI() != ABILinuxX8664 || child.AddressSpace().Root != 0x2000 || string(child.Name()) != "busybox" ||
		child.Regions().Count() != 1 || child.TaskCount() != 0 || used != [4]uint32{} || limits != parentLimits {
		t.Fatalf("child = id %d abi %d root %#x name %q regions %d tasks %d used %v limits %v",
			child.ID(), child.ABI(), child.AddressSpace().Root, child.Name(), child.Regions().Count(), child.TaskCount(), used, limits)
	}
}

func TestProcessRejectsInvalidIdentity(t *testing.T) {
	if _, ok := New(0, ABINative, AddressSpace{Root: 0x1000}); ok {
		t.Fatal("zero process ID accepted")
	}
	if _, ok := New(1, ABINative, AddressSpace{}); ok {
		t.Fatal("zero address-space root accepted")
	}
	if _, ok := New(1, ABIInvalid, AddressSpace{Root: 0x1000}); ok {
		t.Fatal("invalid process ABI accepted")
	}
	if _, ok := New(1, ABI(255), AddressSpace{Root: 0x1000}); ok {
		t.Fatal("unknown process ABI accepted")
	}
}

func TestProcessABIIsFixedAtInitialization(t *testing.T) {
	native, nativeOK := New(1, ABINative, AddressSpace{Root: 0x1000})
	linux, linuxOK := New(2, ABILinuxX8664, AddressSpace{Root: 0x2000})
	if !nativeOK || !linuxOK {
		t.Fatal("valid process ABI rejected")
	}
	if native.ABI() != ABINative || linux.ABI() != ABILinuxX8664 {
		t.Fatalf("process ABIs = %d, %d", native.ABI(), linux.ABI())
	}
}

// The task quota binds before the table does, which is the point: the table
// size is a structural bound and the quota is the policy one.
func TestProcessTaskCapacityIsBounded(t *testing.T) {
	process, _ := New(2, ABINative, AddressSpace{Root: 0x1000})
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
	if !Init(&slot, 1, ABINative, AddressSpace{Root: 0x1000}) || !slot.AddTask(task) {
		t.Fatal("valid process rejected")
	}
	if !Init(&slot, 2, ABILinuxX8664, AddressSpace{Root: 0x2000}) {
		t.Fatal("slot reuse rejected")
	}
	if slot.ID() != 2 || slot.ABI() != ABILinuxX8664 || slot.AddressSpace().Root != 0x2000 || slot.TaskCount() != 0 {
		t.Fatalf("slot not reinitialized: id=%d root=%#x tasks=%d", slot.ID(), slot.AddressSpace().Root, slot.TaskCount())
	}
	if Init(nil, 1, ABINative, AddressSpace{Root: 0x1000}) || Init(&slot, 0, ABINative, AddressSpace{Root: 0x1000}) || Init(&slot, 1, ABIInvalid, AddressSpace{Root: 0x1000}) || Init(&slot, 1, ABINative, AddressSpace{}) {
		t.Fatal("invalid identity accepted")
	}
}

// TestInitDoesNotAllocate guards the kernel, which leaks every allocation and
// cannot grow its heap after it leaves boot services.
func TestInitDoesNotAllocate(t *testing.T) {
	var slot Process
	if allocations := testing.AllocsPerRun(100, func() { Init(&slot, 1, ABINative, AddressSpace{Root: 0x1000}) }); allocations != 0 {
		t.Fatalf("Init allocated %v times per run", allocations)
	}
}
