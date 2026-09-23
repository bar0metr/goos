package process

import (
	"goos/kernel/sched"
	"testing"
)

func TestQuotaRefusesPastItsLimitAndChangesNothing(t *testing.T) {
	process, _ := New(2, AddressSpace{Root: 0x1000})
	_, limits := process.Quota()
	limit := limits[ResourceStreams]
	if limit == 0 {
		t.Fatal("stream quota is zero")
	}
	if !process.Charge(ResourceStreams, limit) {
		t.Fatal("charge up to the limit refused")
	}
	if process.Charge(ResourceStreams, 1) {
		t.Fatal("charge past the limit accepted")
	}
	if used, _ := process.Quota(); used[ResourceStreams] != limit {
		t.Fatalf("a refused charge moved the counter to %d", used[ResourceStreams])
	}
	// A charge that would overflow the counter is refused, not wrapped.
	if process.Charge(ResourceStreams, ^uint32(0)) {
		t.Fatal("overflowing charge accepted")
	}
	if used, _ := process.Quota(); used[ResourceStreams] != limit {
		t.Fatalf("an overflowing charge moved the counter to %d", used[ResourceStreams])
	}
}

func TestQuotaReleaseNeitherWrapsNorHides(t *testing.T) {
	process, _ := New(2, AddressSpace{Root: 0x1000})
	if !process.Charge(ResourcePages, 3) || !process.Release(ResourcePages, 2) {
		t.Fatal("ordinary charge and release refused")
	}
	if used, _ := process.Quota(); used[ResourcePages] != 1 {
		t.Fatalf("pages held = %d", used[ResourcePages])
	}
	// Releasing more than is held is a kernel bug. The counter must stay
	// truthful and the caller must be told.
	if process.Release(ResourcePages, 5) {
		t.Fatal("over-release reported success")
	}
	if used, _ := process.Quota(); used[ResourcePages] != 0 {
		t.Fatalf("over-release left %d pages held", used[ResourcePages])
	}
	if process.Charge(ResourcePages, 0) || process.Release(ResourcePages, 0) {
		t.Fatal("zero amount accepted")
	}
	if process.Charge(resourceCount, 1) || process.Release(resourceCount, 1) {
		t.Fatal("unknown resource accepted")
	}
}

func TestSystemQuotaIsPID1Only(t *testing.T) {
	ordinary, _ := New(2, AddressSpace{Root: 0x2000})
	if ordinary.GrantSystemQuota() {
		t.Fatal("an ordinary process raised its own quota")
	}
	if _, limits := ordinary.Quota(); limits[ResourceStreams] != defaultStreamLimit {
		t.Fatalf("ordinary stream limit = %d", limits[ResourceStreams])
	}
	pid1, _ := New(1, AddressSpace{Root: 0x1000})
	if !pid1.GrantSystemQuota() {
		t.Fatal("PID 1 refused the system quota")
	}
	_, limits := pid1.Quota()
	if limits[ResourceStreams] != systemStreamLimit || limits[ResourceChannels] != systemChannelLimit {
		t.Fatalf("system limits = %+v", limits)
	}
	// The larger share is still a limit.
	if !pid1.Charge(ResourceStreams, systemStreamLimit) || pid1.Charge(ResourceStreams, 1) {
		t.Fatal("PID 1 charged past its own quota")
	}
}

func TestQuotaDoesNotSurviveSlotReuse(t *testing.T) {
	var slot Process
	if !Init(&slot, 1, AddressSpace{Root: 0x1000}) || !slot.GrantSystemQuota() || !slot.Charge(ResourcePages, 10) {
		t.Fatal("setup failed")
	}
	slot.AccountCPU(7)
	if !Init(&slot, 2, AddressSpace{Root: 0x2000}) {
		t.Fatal("slot reuse refused")
	}
	used, limits := slot.Quota()
	if used[ResourcePages] != 0 {
		t.Fatalf("reused slot still holds %d pages", used[ResourcePages])
	}
	if limits[ResourceStreams] != defaultStreamLimit {
		t.Fatalf("reused slot kept the system quota: %d", limits[ResourceStreams])
	}
	if slot.CPUTicks() != 0 {
		t.Fatalf("reused slot kept %d CPU ticks", slot.CPUTicks())
	}
}

func TestReleaseAllClearsEveryReservation(t *testing.T) {
	process, _ := New(2, AddressSpace{Root: 0x1000})
	task, _ := sched.NewTaskID(1, 1)
	if !process.Charge(ResourcePages, 5) || !process.Charge(ResourceStreams, 1) || !process.AddTask(task) {
		t.Fatal("setup failed")
	}
	process.ReleaseAll()
	used, _ := process.Quota()
	for resource := Resource(0); resource < resourceCount; resource++ {
		if used[resource] != 0 {
			t.Fatalf("resource %d still holds %d", resource, used[resource])
		}
	}
}

func TestRegistryChargesAndReleasesByProcessID(t *testing.T) {
	var registry Registry
	process, _ := New(3, AddressSpace{Root: 0x3000})
	if !registry.Add(&process) {
		t.Fatal("add failed")
	}
	if !registry.ChargeProcess(3, ResourceChannels, 1) {
		t.Fatal("charge by ID refused")
	}
	if registry.ChargeProcess(9, ResourceChannels, 1) {
		t.Fatal("charge to an unknown process accepted")
	}
	if !registry.ReleaseProcess(3, ResourceChannels, 1) {
		t.Fatal("release by ID refused")
	}
	// A resource outliving its creator must not charge whoever reuses the ID.
	if registry.ReleaseProcess(9, ResourceChannels, 1) {
		t.Fatal("release to an unknown process reported success")
	}
}

func TestCPUAccountingAccumulates(t *testing.T) {
	process, _ := New(2, AddressSpace{Root: 0x1000})
	if process.CPUTicks() != 0 {
		t.Fatal("a new process already used CPU")
	}
	process.AccountCPU(3)
	process.AccountCPU(4)
	if process.CPUTicks() != 7 {
		t.Fatalf("CPU ticks = %d", process.CPUTicks())
	}
}
