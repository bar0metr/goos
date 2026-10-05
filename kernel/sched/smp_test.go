package sched

import (
	"math/rand"
	"testing"
)

// newCore returns a core with the four kernel tasks, task 0 running on the
// housekeeping processor, and processors 1..cpus-1 online.
func newCore(t *testing.T, cpus int) *Core {
	t.Helper()
	core := &Core{}
	if !core.Bootstrap(4, 0x7) {
		t.Fatal("bootstrap refused")
	}
	for cpu := 1; cpu < cpus; cpu++ {
		if !core.Online(cpu) {
			t.Fatalf("processor %d refused", cpu)
		}
	}
	check(t, core)
	return core
}

func check(t *testing.T, core *Core) {
	t.Helper()
	if !core.Check() {
		t.Fatalf("invariants broken: %+v", core.Snapshot())
	}
}

func startTask(t *testing.T, core *Core, self int) int {
	t.Helper()
	slot, _, ok := core.Install()
	if !ok {
		t.Fatal("install refused")
	}
	if _, started := core.Start(slot, self); !started {
		t.Fatal("start refused")
	}
	check(t, core)
	return slot
}

// switchTo runs Switch and Finish the way the architecture layer does.
func switchTo(core *Core, cpu int, action Action) int {
	previous := core.Current(cpu)
	next := core.Switch(cpu, action)
	if next != previous {
		core.Finish(cpu, previous)
	}
	return next
}

func TestKernelTasksStayOnHousekeeping(t *testing.T) {
	core := newCore(t, 4)
	for cpu := 1; cpu < 4; cpu++ {
		if next := switchTo(core, cpu, Preempt); next != Idle {
			t.Fatalf("processor %d took kernel task %d", cpu, next)
		}
	}
	if next := switchTo(core, 0, Preempt); next != 1 {
		t.Fatalf("housekeeping picked %d after task 0, want 1", next)
	}
	check(t, core)
}

func TestNewTaskGoesToIdleProcessor(t *testing.T) {
	core := newCore(t, 4)
	slot := startTask(t, core, 0)
	home := core.homeOf(slot)
	if home == 0 || !core.idle(home, slot) {
		t.Fatalf("new task placed on %d, which is not an idle processor", home)
	}
	if next := switchTo(core, home, Preempt); next != slot {
		t.Fatalf("processor %d picked %d, want %d", home, next, slot)
	}
	check(t, core)
}

func TestTaskNeverRunsOnTwoProcessors(t *testing.T) {
	core := newCore(t, 4)
	slot := startTask(t, core, 0)
	home := core.homeOf(slot)
	switchTo(core, home, Preempt)
	// The task is preempted but its processor has not left its stack yet.
	core.Switch(home, Block)
	core.Wake(slot, 0)
	for cpu := 0; cpu < 4; cpu++ {
		if cpu == home {
			continue
		}
		if next := core.Switch(cpu, Preempt); next == slot {
			t.Fatalf("processor %d picked a task still on processor %d", cpu, home)
		}
	}
	if core.RunningOn(slot) != home && core.running&bit(slot) == 0 {
		t.Fatal("task left the running set before Finish")
	}
	check(t, core)
}

func TestWakeDuringSwitchOutRunsWhenFinished(t *testing.T) {
	core := newCore(t, 2)
	slot := startTask(t, core, 0)
	home := core.homeOf(slot)
	switchTo(core, home, Preempt)
	previous := core.Current(home)
	if next := core.Switch(home, Block); next != Idle {
		t.Fatalf("blocked task's processor picked %d", next)
	}
	if target, ok := core.Wake(slot, 0); !ok || target != Idle {
		t.Fatalf("wake of a task still on its processor = %d, %v", target, ok)
	}
	if kick := core.Finish(home, previous); kick != home {
		t.Fatalf("finish asked to interrupt %d, want the idle home %d", kick, home)
	}
	if next := switchTo(core, home, Preempt); next != slot {
		t.Fatalf("woken task not picked: %d", next)
	}
	check(t, core)
}

func TestBlockKeepsTaskRunnableAfterEarlierWake(t *testing.T) {
	core := newCore(t, 2)
	slot := startTask(t, core, 0)
	home := core.homeOf(slot)
	switchTo(core, home, Preempt)
	core.Wake(slot, 0)
	if next := switchTo(core, home, Block); next != slot {
		t.Fatalf("a wake before the block was lost: picked %d", next)
	}
	if next := switchTo(core, home, Block); next != Idle {
		t.Fatalf("a second block without a wake kept the task: %d", next)
	}
	check(t, core)
}

func TestWakeOnAnotherProcessor(t *testing.T) {
	core := newCore(t, 3)
	slot := startTask(t, core, 0)
	home := core.homeOf(slot)
	switchTo(core, home, Preempt)
	switchTo(core, home, Block)
	target, ok := core.Wake(slot, 0)
	if !ok || target == Idle || target == 0 {
		t.Fatalf("wake target = %d, %v", target, ok)
	}
	if next := switchTo(core, target, Preempt); next != slot {
		t.Fatalf("processor %d picked %d", target, next)
	}
	check(t, core)
}

func TestIdleProcessorStealsWork(t *testing.T) {
	core := newCore(t, 2)
	first := startTask(t, core, 0)
	second := startTask(t, core, 0)
	// Both tasks queue on processor 1; processor 0 runs task 0.
	core.move(first, 1)
	core.move(second, 1)
	switchTo(core, 1, Preempt)
	running := core.Current(1)
	queued := first + second - running
	// Housekeeping task 0 blocks, and the kernel tasks have nothing to do.
	core.runnable &^= 0x7
	next := switchTo(core, 0, Block)
	if next != queued {
		t.Fatalf("idle housekeeping processor picked %d, want stolen %d", next, queued)
	}
	if core.homeOf(queued) != 0 || core.Snapshot().Stats.Steals != 1 {
		t.Fatal("steal not recorded")
	}
	check(t, core)
}

func TestPreemptedTaskMovesToIdleProcessor(t *testing.T) {
	core := newCore(t, 3)
	first := startTask(t, core, 0)
	core.move(first, 1)
	second := startTask(t, core, 0)
	core.move(second, 1)
	switchTo(core, 1, Preempt)
	running := core.Current(1)
	previous := running
	next := core.Switch(1, Preempt)
	if next == previous || next == Idle {
		t.Fatalf("round robin kept %d", next)
	}
	if kick := core.Finish(1, previous); kick != 2 {
		t.Fatalf("finish kicked %d, want idle processor 2", kick)
	}
	if switchTo(core, 2, Preempt) != previous {
		t.Fatal("idle processor did not run the moved task")
	}
	// The next tick on processor 1 finds only its own task: no ping-pong.
	if switchTo(core, 1, Preempt) != next {
		t.Fatal("processor 1 lost its task")
	}
	check(t, core)
}

func TestExitPublishedOnlyAfterFinish(t *testing.T) {
	core := newCore(t, 2)
	slot := startTask(t, core, 0)
	home := core.homeOf(slot)
	switchTo(core, home, Preempt)
	core.Switch(home, Exit)
	if _, found := core.TakeExited(); found {
		t.Fatal("exit published while the task's stack is in use")
	}
	if core.Reclaimable(slot) {
		t.Fatal("a task still on its processor is reclaimable")
	}
	core.Finish(home, slot)
	taken, found := core.TakeExited()
	if !found || taken != slot {
		t.Fatalf("exit not published: %d %v", taken, found)
	}
	if !core.Vacate(slot) {
		t.Fatal("finished task not reclaimable")
	}
	check(t, core)
}

func TestTerminateRunningTaskKillsAtNextSwitch(t *testing.T) {
	core := newCore(t, 2)
	slot := startTask(t, core, 0)
	home := core.homeOf(slot)
	switchTo(core, home, Preempt)
	target, ok := core.Terminate(slot, 0)
	if !ok || target != home {
		t.Fatalf("terminate = %d, %v", target, ok)
	}
	if !core.Killed(home) {
		t.Fatal("kill not visible to the running processor")
	}
	if _, again := core.Terminate(slot, 0); again {
		t.Fatal("a killed task was terminated twice")
	}
	if next := switchTo(core, home, Preempt); next != Idle {
		t.Fatalf("killed task kept running: %d", next)
	}
	if taken, found := core.TakeExited(); !found || taken != slot {
		t.Fatal("killed task's exit not published")
	}
	check(t, core)
}

func TestTerminateRefusesCallersOwnTask(t *testing.T) {
	core := newCore(t, 2)
	slot := startTask(t, core, 0)
	home := core.homeOf(slot)
	switchTo(core, home, Preempt)
	if _, ok := core.Terminate(slot, home); ok {
		t.Fatal("a processor terminated its own current task")
	}
}

func TestExitWhileAnotherProcessorSelects(t *testing.T) {
	core := newCore(t, 2)
	a := startTask(t, core, 0)
	b := startTask(t, core, 0)
	core.move(a, 1)
	core.move(b, 1)
	switchTo(core, 1, Preempt)
	running := core.Current(1)
	core.Switch(1, Exit)
	// Processor 0 selects while processor 1 is still on the exiting stack.
	core.runnable &^= 0x7
	stolen := core.Switch(0, Block)
	if stolen == running {
		t.Fatal("an exiting task was stolen")
	}
	core.Finish(1, running)
	core.Finish(0, 0)
	check(t, core)
}

func TestGenerationReuse(t *testing.T) {
	core := newCore(t, 1)
	slot, first, _ := core.Install()
	core.Start(slot, 0)
	old := core.TaskID(slot)
	if found, ok := core.Terminate(slot, 0); !ok || found != Idle {
		t.Fatal("queued task not terminated at once")
	}
	core.TakeExited()
	core.Vacate(slot)
	again, second, _ := core.Install()
	if again != slot || second == first {
		t.Fatalf("slot %d generation %d reused as %d generation %d", slot, first, again, second)
	}
	if _, ok := core.Alive(old); ok {
		t.Fatal("stale task identity resolves")
	}
	if _, ok := core.Wake(slot, 0); ok {
		t.Fatal("unstarted task woken")
	}
	check(t, core)
}

func TestOfflineProcessorNeverReceivesWork(t *testing.T) {
	core := newCore(t, 2)
	for index := 0; index < 20; index++ {
		slot := startTask(t, core, 0)
		if home := core.homeOf(slot); home > 1 {
			t.Fatalf("task placed on offline processor %d", home)
		}
	}
	for cpu := 2; cpu < MaxCPUs; cpu++ {
		if core.home[cpu] != 0 {
			t.Fatalf("offline processor %d has a queue", cpu)
		}
	}
	check(t, core)
}

func TestTaskBoundIsExact(t *testing.T) {
	core := newCore(t, 1)
	for index := 4; index < MaxTasks; index++ {
		if _, _, ok := core.Install(); !ok {
			t.Fatalf("install %d refused", index)
		}
	}
	if _, _, ok := core.Install(); ok {
		t.Fatal("installed a 65th task")
	}
}

func TestAbandonFreesUnstartedSlot(t *testing.T) {
	core := newCore(t, 1)
	slot, _, _ := core.Install()
	if !core.Abandon(slot) {
		t.Fatal("abandon refused")
	}
	if again, _, _ := core.Install(); again != slot {
		t.Fatal("abandoned slot not reused")
	}
	check(t, core)
}

func TestEmptyQueuesIdleEveryProcessor(t *testing.T) {
	core := newCore(t, 4)
	core.runnable = 0
	for cpu := 0; cpu < 4; cpu++ {
		if next := switchTo(core, cpu, Block); next != Idle {
			t.Fatalf("processor %d picked %d from empty queues", cpu, next)
		}
	}
	check(t, core)
}

// TestRandomInterleavings drives every operation in random order across
// processors and checks the invariants after each step, including that no
// two processors ever run one task.
func TestRandomInterleavings(t *testing.T) {
	random := rand.New(rand.NewSource(1))
	for round := 0; round < 200; round++ {
		cpus := 1 + random.Intn(8)
		core := newCore(t, cpus)
		pending := map[int]int{}
		for step := 0; step < 400; step++ {
			cpu := random.Intn(cpus)
			switch random.Intn(9) {
			case 0:
				if slot, _, ok := core.Install(); ok {
					core.Start(slot, cpu)
				}
			case 1, 2:
				previous := core.Current(cpu)
				action := Action(random.Intn(3))
				if previous != Idle && previous < 4 && action == Exit {
					action = Block
				}
				next := core.Switch(cpu, action)
				if previous != Idle && previous < 4 && core.dying&bit(previous) != 0 {
					t.Fatal("a kernel task exited")
				}
				if next != previous {
					pending[cpu] = previous
				} else {
					delete(pending, cpu)
				}
			case 3:
				if previous, ok := pending[cpu]; ok {
					core.Finish(cpu, previous)
					delete(pending, cpu)
				}
			case 4:
				core.Wake(random.Intn(MaxTasks), cpu)
			case 5:
				if slot := 4 + random.Intn(MaxTasks-4); core.inRange(slot) {
					core.Terminate(slot, cpu)
				}
			case 6:
				if slot, ok := core.TakeExited(); ok {
					core.Vacate(slot)
				}
			case 7:
				core.Block(cpu)
			case 8:
				core.Tick()
			}
			if core.Current(cpu) >= 0 && core.Current(cpu) < 4 && cpu != Housekeeping {
				t.Fatal("kernel task ran away from the housekeeping processor")
			}
			for other, previous := range pending {
				if previous != Idle && core.Current(other) != previous && core.running&bit(previous) == 0 {
					t.Fatal("a task left the running set before its processor finished")
				}
			}
			check(t, core)
		}
	}
}

func TestHeldTaskLeavesAndStaysOffUntilRelease(t *testing.T) {
	core := newCore(t, 2)
	slot := startTask(t, core, 0)
	home := core.homeOf(slot)
	switchTo(core, home, Preempt)
	target, ok := core.Hold(slot, 0)
	if !ok || target != home {
		t.Fatalf("hold = %d, %v", target, ok)
	}
	if !core.MustLeave(home) {
		t.Fatal("held task's processor not told to leave it")
	}
	if next := switchTo(core, home, Preempt); next == slot {
		t.Fatal("a held task kept its processor")
	}
	core.Wake(slot, 0)
	for cpu := 0; cpu < 2; cpu++ {
		if next := switchTo(core, cpu, Preempt); next == slot {
			t.Fatalf("processor %d picked a held task", cpu)
		}
	}
	if target := core.Release(slot, 0); target == Idle && core.Current(home) == Idle {
		t.Fatal("release did not place the runnable task")
	}
	found := false
	for cpu := 0; cpu < 2; cpu++ {
		if switchTo(core, cpu, Preempt) == slot {
			found = true
		}
	}
	if !found {
		t.Fatal("released task never ran")
	}
	check(t, core)
}
