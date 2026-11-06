package process

import (
	"testing"

	"goos/kernel/sched"
)

func taskID(t *testing.T, slot int) sched.TaskID {
	t.Helper()
	id, ok := sched.NewTaskID(uint32(slot), 1)
	if !ok {
		t.Fatalf("no task ID for slot %d", slot)
	}
	return id
}

// A large process runs MaxTasksPerProcess tasks and the next one is refused
// with nothing charged; each task past the first holds TaskPages.
func TestLargeProfileRunsSixtyFourTasks(t *testing.T) {
	var owner Process
	if !InitWithProfile(&owner, 2, ABINative, AddressSpace{Root: 0x1000}, ProfileLarge) {
		t.Fatal("large profile refused")
	}
	for slot := 0; slot < MaxTasksPerProcess; slot++ {
		if !owner.AddTask(taskID(t, slot)) {
			t.Fatalf("task %d refused", slot)
		}
	}
	used, _ := owner.Quota()
	if used[ResourceTasks] != MaxTasksPerProcess || used[ResourcePages] != (MaxTasksPerProcess-1)*TaskPages {
		t.Fatalf("charged %d tasks and %d pages", used[ResourceTasks], used[ResourcePages])
	}
	if owner.ReserveTask() || owner.AddTask(taskID(t, MaxTasksPerProcess)) {
		t.Fatal("task 65 accepted")
	}
	if after, _ := owner.Quota(); after != used {
		t.Fatalf("refused task changed the account: %v -> %v", used, after)
	}
	for slot := MaxTasksPerProcess - 1; slot >= 0; slot-- {
		if !owner.removeTask(taskID(t, slot)) {
			t.Fatalf("task %d not removed", slot)
		}
	}
	if used, _ := owner.Quota(); used[ResourceTasks] != 0 || used[ResourcePages] != 0 {
		t.Fatalf("removed tasks left %d tasks and %d pages charged", used[ResourceTasks], used[ResourcePages])
	}
	for slot := 0; slot < MaxTasksPerProcess; slot++ {
		if !owner.AddTask(taskID(t, slot)) {
			t.Fatalf("task %d refused after release", slot)
		}
	}
}

func TestProfileTaskQuotas(t *testing.T) {
	for _, test := range []struct {
		profile ResourceProfile
		tasks   int
	}{{ProfileTiny, 1}, {ProfileNormal, 16}, {ProfileLarge, 64}} {
		var owner Process
		if !InitWithProfile(&owner, 2, ABINative, AddressSpace{Root: 0x1000}, test.profile) {
			t.Fatalf("profile %d refused", test.profile)
		}
		for slot := 0; slot < test.tasks; slot++ {
			if !owner.AddTask(taskID(t, slot)) {
				t.Fatalf("profile %d refused task %d", test.profile, slot)
			}
		}
		if owner.AddTask(taskID(t, test.tasks)) {
			t.Fatalf("profile %d accepted task %d", test.profile, test.tasks+1)
		}
	}
}

// A thread the page quota cannot cover is refused before anything changes,
// and a cancelled reservation returns its pages.
func TestThreadPagesComeFromThePageQuota(t *testing.T) {
	var owner Process
	if !InitWithProfile(&owner, 2, ABINative, AddressSpace{Root: 0x1000}, ProfileLarge) || !owner.AddTask(taskID(t, 0)) {
		t.Fatal("setup failed")
	}
	if used, _ := owner.Quota(); used[ResourcePages] != 0 {
		t.Fatalf("the first task charged %d pages", used[ResourcePages])
	}
	if !owner.Charge(ResourcePages, owner.Available(ResourcePages)-TaskPages+1) {
		t.Fatal("could not fill the page quota")
	}
	tasks := owner.Available(ResourceTasks)
	if owner.ReserveTask() || owner.Available(ResourceTasks) != tasks {
		t.Fatal("a thread past the page quota was reserved")
	}
	owner.Release(ResourcePages, 1)
	pages := owner.Available(ResourcePages)
	if !owner.ReserveTask() || owner.Available(ResourcePages) != pages-TaskPages {
		t.Fatal("a thread within the page quota was refused")
	}
	if !owner.CancelReservedTask() || owner.Available(ResourcePages) != pages || owner.Available(ResourceTasks) != tasks {
		t.Fatal("a cancelled thread kept its charge")
	}
}

// The registry holds MaxProcesses processes, refuses the next, and reuses a
// removed one's place.
func TestRegistryHoldsMaxProcesses(t *testing.T) {
	if MaxProcesses < 256 {
		t.Fatalf("MaxProcesses is %d", MaxProcesses)
	}
	var registry Registry
	owners := make([]Process, MaxProcesses+1)
	for index := range owners {
		if !Init(&owners[index], ID(index+1), ABINative, AddressSpace{Root: uint64(index+1) * 0x1000}) {
			t.Fatalf("init %d failed", index)
		}
	}
	for index := 0; index < MaxProcesses; index++ {
		if !registry.Add(&owners[index]) {
			t.Fatalf("process %d refused", index+1)
		}
	}
	if registry.Count() != MaxProcesses || registry.Add(&owners[MaxProcesses]) {
		t.Fatal("process 257 accepted")
	}
	if !registry.Remove(ID(MaxProcesses / 2)) {
		t.Fatal("remove failed")
	}
	if !registry.Add(&owners[MaxProcesses]) || registry.Count() != MaxProcesses {
		t.Fatal("a removed process's place was not reused")
	}
	if _, found := registry.ByID(ID(MaxProcesses + 1)); !found {
		t.Fatal("reused place not found")
	}
}
