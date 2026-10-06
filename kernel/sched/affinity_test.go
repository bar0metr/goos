package sched

import "testing"

// runOn starts slot and runs it on cpu: its home processor picks it.
func runOn(t *testing.T, core *Core, slot int, cpu int) {
	t.Helper()
	if next := switchTo(core, cpu, Preempt); next != slot {
		t.Fatalf("processor %d picked %d, want %d", cpu, next, slot)
	}
}

func TestDefaultAffinityIsEveryOnlineProcessor(t *testing.T) {
	core := newCore(t, 4)
	slot := startTask(t, core, 0)
	if mask := core.Affinity(slot); mask != 0xf {
		t.Fatalf("default affinity %#x, want 0xf", mask)
	}
	if mask := core.Affinity(0); mask != 1 {
		t.Fatalf("kernel task affinity %#x, want 1", mask)
	}
}

func TestPlacementRespectsSingleProcessorMask(t *testing.T) {
	core := newCore(t, 4)
	slot, _, _ := core.Install()
	if _, ok := core.SetAffinity(slot, 1<<2, 0); !ok {
		t.Fatal("affinity refused")
	}
	target, _ := core.Start(slot, 0)
	if target != 2 || core.homeOf(slot) != 2 {
		t.Fatalf("task placed on %d, home %d, want 2", target, core.homeOf(slot))
	}
	runOn(t, core, slot, 2)
	switchTo(core, 2, Block)
	for round := 0; round < 8; round++ {
		if target, _ := core.Wake(slot, round%4); target != 2 {
			t.Fatalf("wake placed the task on %d", target)
		}
		runOn(t, core, slot, 2)
		switchTo(core, 2, Block)
		check(t, core)
	}
}

func TestStealingSkipsTasksThatBarTheThief(t *testing.T) {
	core := newCore(t, 3)
	busy := startTask(t, core, 0)
	runOn(t, core, busy, core.homeOf(busy))
	home := core.homeOf(busy)
	slot, _, _ := core.Install()
	core.SetAffinity(slot, bit(home), 0)
	core.Start(slot, 0)
	if core.homeOf(slot) != home {
		t.Fatalf("pinned task queued on %d, want %d", core.homeOf(slot), home)
	}
	for cpu := 1; cpu < 3; cpu++ {
		if cpu == home {
			continue
		}
		if next := switchTo(core, cpu, Preempt); next == slot {
			t.Fatalf("processor %d stole a task its affinity leaves out", cpu)
		}
	}
	check(t, core)
}

func TestRestrictionMovesQueuedAndRunningTasks(t *testing.T) {
	core := newCore(t, 4)
	slot := startTask(t, core, 0)
	home := core.homeOf(slot)
	runOn(t, core, slot, home)
	other := 1 + home%3
	kick, ok := core.SetAffinity(slot, bit(other), 0)
	if !ok || kick != home {
		t.Fatalf("restriction kicked %d, ok %v; want the running processor %d", kick, ok, home)
	}
	check(t, core)
	previous := core.Current(home)
	if next := core.Switch(home, Preempt); next == slot {
		t.Fatal("the processor kept a task its affinity now leaves out")
	}
	if kick := core.Finish(home, previous); kick != other {
		t.Fatalf("finish kicked %d, want the idle new home %d", kick, other)
	}
	runOn(t, core, slot, other)
	check(t, core)
}

func TestWideningPermitsMigrationAgain(t *testing.T) {
	core := newCore(t, 4)
	slot, _, _ := core.Install()
	core.SetAffinity(slot, bit(1), 0)
	core.Start(slot, 0)
	runOn(t, core, slot, 1)
	if _, ok := core.SetAffinity(slot, 0xe, 1); !ok {
		t.Fatal("widening refused")
	}
	// Another task needs processor 1; the preempted one may now go to an
	// idle processor.
	second := startTask(t, core, 0)
	core.move(second, 1)
	for round := 0; round < 2 && core.Current(1) == slot; round++ {
		previous := core.Current(1)
		if next := core.Switch(1, Preempt); next != previous {
			core.Finish(1, previous)
		}
	}
	if core.Current(1) == slot {
		t.Fatal("processor 1 never switched to the other task")
	}
	if home := core.homeOf(slot); home != 2 && home != 3 {
		t.Fatalf("widened task moved to %d, want an idle allowed processor", home)
	}
	check(t, core)
}

func TestAffinityRejectsEmptyAndOfflineMasks(t *testing.T) {
	core := newCore(t, 2)
	slot := startTask(t, core, 0)
	for _, mask := range []uint64{0, 1 << 5, 1 << 63} {
		if _, ok := core.SetAffinity(slot, mask, 0); ok {
			t.Fatalf("mask %#x accepted", mask)
		}
	}
	if mask := core.Affinity(slot); mask != 3 {
		t.Fatalf("a refused mask changed the affinity to %#x", mask)
	}
}

func TestKernelTasksStayPinned(t *testing.T) {
	core := newCore(t, 4)
	if _, ok := core.SetAffinity(1, 1<<2, 0); ok {
		t.Fatal("a kernel task moved away from the housekeeping processor")
	}
	if _, ok := core.SetAffinity(1, 0xf, 0); !ok {
		t.Fatal("a mask with the housekeeping processor was refused")
	}
	if mask := core.Affinity(1); mask != 1 {
		t.Fatalf("kernel task affinity %#x", mask)
	}
}

func TestAffinityWithHeldAndKilledTasks(t *testing.T) {
	core := newCore(t, 4)
	slot := startTask(t, core, 0)
	home := core.homeOf(slot)
	core.Hold(slot, 0)
	if kick, ok := core.SetAffinity(slot, bit(1+home%3), 0); !ok || kick != Idle {
		t.Fatalf("held task: kick %d, ok %v; a held task must not be kicked to run", kick, ok)
	}
	if target := core.Release(slot, 0); target != 1+home%3 {
		t.Fatalf("released task placed on %d", target)
	}
	core.Terminate(slot, 0)
	if _, ok := core.SetAffinity(slot, 0xf, 0); ok {
		t.Fatal("a dead task's affinity changed")
	}
	check(t, core)
}

func TestInstallResetsAffinity(t *testing.T) {
	core := newCore(t, 4)
	slot := startTask(t, core, 0)
	core.SetAffinity(slot, bit(3), 0)
	core.Terminate(slot, 0)
	reused, ok := core.TakeExited()
	if !ok || reused != slot || !core.Vacate(slot) {
		t.Fatal("slot not reclaimed")
	}
	again, _, _ := core.Install()
	if again != slot || core.Affinity(again) != 0xf {
		t.Fatalf("reused slot %d kept affinity %#x", again, core.Affinity(again))
	}
	check(t, core)
}

func TestPerProcessorStats(t *testing.T) {
	core := newCore(t, 2)
	slot := startTask(t, core, 0)
	home := core.homeOf(slot)
	runOn(t, core, slot, home)
	if core.CPUStats(home).Switches == 0 || core.Snapshot().Stats.Switches < core.CPUStats(home).Switches {
		t.Fatalf("switch counts %+v %+v", core.CPUStats(home), core.Snapshot().Stats)
	}
	if core.CPUStats(-1) != (Stats{}) || core.CPUStats(MaxCPUs) != (Stats{}) {
		t.Fatal("an out-of-range processor has counters")
	}
}
