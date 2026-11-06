package linux

import (
	"testing"

	"goos/kernel/handle"
	"goos/kernel/process"
	"goos/kernel/sched"
)

// A Linux process launched with the large profile runs 64 threads, its main
// one included. The 65th clone finds the task quota spent, which the kernel
// answers with EAGAIN, and leaves no record behind.
func TestLargeLinuxProcessRunsSixtyFourThreads(t *testing.T) {
	resetCompatibilityProcesses()
	defer resetCompatibilityProcesses()
	data := linuxStartup(t, "/", [4]handle.ID{1, 2, 3, 4})
	if !InstallProcess(0x1000, 30, data, StandardStreams{Input: 1, Output: 2, Error: 3}, 0x600000, 0x4600000) {
		t.Fatal("install failed")
	}
	var owner process.Process
	if !process.InitWithProfile(&owner, 30, process.ABILinuxX8664, process.AddressSpace{Root: 0x1000}, process.ProfileLarge) {
		t.Fatal("large profile refused")
	}
	main, _ := sched.NewTaskID(0, 1)
	if !owner.AddTask(main) {
		t.Fatal("main task refused")
	}
	for slot := 1; slot < process.MaxTasksPerProcess; slot++ {
		creation, ok := PrepareThreadCreation(&owner, 0x1000)
		task, _ := sched.NewTaskID(uint32(slot), 1)
		if !ok {
			t.Fatalf("thread %d refused", slot+1)
		}
		if _, committed := creation.Commit(task); !committed {
			t.Fatalf("thread %d not committed", slot+1)
		}
	}
	if owner.TaskCount() != 64 || ThreadCount(0x1000) != 63 {
		t.Fatalf("tasks %d, thread records %d", owner.TaskCount(), ThreadCount(0x1000))
	}
	records := ThreadRecords(0x1000)
	if _, ok := PrepareThreadCreation(&owner, 0x1000); ok || ThreadRecords(0x1000) != records {
		t.Fatal("thread 65 was prepared")
	}
}

// Process states take pages only for slots a process used, and a slot keeps
// its pages for the next process: installing after a removal takes none.
func TestProcessStatesArePagedByUse(t *testing.T) {
	resetCompatibilityProcesses()
	defer resetCompatibilityProcesses()
	data := linuxStartup(t, "/", [4]handle.ID{1, 2, 3, 4})
	streams := StandardStreams{Input: 1, Output: 2, Error: 3}
	if ProcessStatePages() != 0 {
		t.Fatal("an empty table holds pages")
	}
	for index := 0; index < 3; index++ {
		if !InstallProcess(uint64(index+1)*0x1000, process.ID(index+10), data, streams, 0x600000, 0x4600000) {
			t.Fatalf("install %d failed", index)
		}
	}
	if ProcessStatePages() != 3 {
		t.Fatalf("three processes hold %d states", ProcessStatePages())
	}
	state, _ := FindProcess(0x2000)
	if !RemoveProcess(0x2000, nil) || ProcessStatePages() != 3 {
		t.Fatal("removal changed the pages held")
	}
	if !InstallProcess(0x9000, 20, data, streams, 0x600000, 0x4600000) || ProcessStatePages() != 3 {
		t.Fatal("a reused slot took new pages")
	}
	if reused, _ := FindProcess(0x9000); reused != state {
		t.Fatal("the new process did not reuse the freed slot")
	}
}

// Every one of the Linux share of the registry's slots can hold a process.
func TestLinuxProcessSharesScale(t *testing.T) {
	if process.MaxProcesses != 256 || LinuxGlobalMaxProcesses != 224 || LinuxFamilyMaxProcesses != 64 {
		t.Fatalf("limits %d, %d, %d", process.MaxProcesses, LinuxGlobalMaxProcesses, LinuxFamilyMaxProcesses)
	}
	if !LinuxProcessCreationAllowed(LinuxGlobalMaxProcesses-1) || LinuxProcessCreationAllowed(LinuxGlobalMaxProcesses) {
		t.Fatal("the Linux process share boundary is wrong")
	}
}
