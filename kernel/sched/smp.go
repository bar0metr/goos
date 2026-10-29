package sched

import "math/bits"

// MaxCPUs bounds the processors the scheduler runs on. Run sets are 64-bit
// masks indexed by task slot, and processor sets are 64-bit masks indexed by
// processor, so both bounds are one machine word.
const MaxCPUs = 64

// Housekeeping is the processor that runs the kernel's own tasks: deadline
// expiry, lifecycle delivery, and exit reaping run there and nowhere else, so
// each global side effect happens once.
const Housekeeping = 0

// Idle is the slot value of a processor that runs its idle loop.
const Idle = -1

// Action says why a processor leaves the task it runs.
type Action uint8

const (
	// Preempt ends a time slice or answers a reschedule request. The task
	// stays runnable.
	Preempt Action = iota
	// Block parks the task until a wake. A wake that arrived since the task
	// last ran keeps it runnable, so a wake can never fall between a task's
	// last check and its sleep.
	Block
	// Exit ends the task.
	Exit
)

// Core is the run state of every task and processor. It holds no lock: the
// architecture layer calls every method with the scheduler lock held.
//
// A task slot is vacant, or holds a task that is live or dead. A live task
// is runnable or not, and belongs to exactly one processor's run queue, its
// home. At most one processor runs it: the running mask marks a task from the
// moment a processor picks it until that processor has left its stack, so no
// other processor can pick a task whose registers are still being saved.
//
// A task runs only on the processors its affinity allows. Its home is always
// one of them; a processor whose task's affinity changed away from it leaves
// the task at its next scheduling point, and no processor steals a task it
// may not run.
type Core struct {
	generations [MaxTasks]uint32
	count       int
	vacant      uint64
	live        uint64
	started     uint64
	runnable    uint64
	dead        uint64
	dying       uint64
	exitPending uint64
	wakePending uint64
	killed      uint64
	held        uint64
	// stopped are the tasks a job-control stop keeps off every processor.
	// It is apart from held, which the kernel's own holds set and clear.
	stopped     uint64
	pinned      uint64
	running     uint64
	home        [MaxCPUs]uint64
	online      uint64
	current     [MaxCPUs]int8
	rotor       [MaxCPUs]int8
	// affinity is each task's requested processor mask; zero means every
	// processor. barred has, per processor, the tasks whose affinity leaves
	// it out, so a steal or a placement tests a task with one AND.
	affinity [MaxTasks]uint64
	barred   [MaxCPUs]uint64
	stats    [MaxCPUs]Stats
}

// Stats counts scheduler events. Per processor, Switches are the switches it
// made, Steals the tasks it took from other queues, Migrations the tasks
// moved to its queue, and Kills the tasks it terminated while they ran.
type Stats struct {
	Switches   uint64
	Steals     uint64
	Migrations uint64
	Kills      uint64
}

// Bootstrap installs the kernel's own tasks in slots [0, kernelTasks), pinned
// to the housekeeping processor, with runnable naming those that may run.
// The caller is task 0, already running on the housekeeping processor.
func (core *Core) Bootstrap(kernelTasks int, runnable uint64) bool {
	if kernelTasks < 1 || kernelTasks > MaxTasks || runnable&1 == 0 {
		return false
	}
	*core = Core{}
	for cpu := range core.current {
		core.current[cpu] = Idle
		core.rotor[cpu] = -1
	}
	mask := lowMask(kernelTasks)
	for slot := 0; slot < kernelTasks; slot++ {
		core.generations[slot] = 1
	}
	core.count = kernelTasks
	core.live, core.started, core.pinned = mask, mask, mask
	core.runnable = runnable & mask
	core.home[Housekeeping] = mask
	core.online = 1 << Housekeeping
	core.current[Housekeeping] = 0
	core.rotor[Housekeeping] = 0
	core.running = 1
	return true
}

// Online adds a processor, idle, to the set that runs tasks.
func (core *Core) Online(cpu int) bool {
	if cpu <= 0 || cpu >= MaxCPUs || core.online&bit(cpu) != 0 {
		return false
	}
	core.current[cpu] = Idle
	core.online |= bit(cpu)
	core.barred[cpu] = 0
	for slot := 0; slot < core.count; slot++ {
		if core.affinity[slot] != 0 && core.affinity[slot]&bit(cpu) == 0 {
			core.barred[cpu] |= bit(slot)
		}
	}
	return true
}

// OnlineMask returns the processors that run tasks.
func (core *Core) OnlineMask() uint64 { return core.online }

// Install claims a slot for a new task, live but not yet started, and
// returns it with its new generation. A dead slot is reused only once its
// exit has been delivered and its stack reclaimed.
func (core *Core) Install() (int, uint32, bool) {
	slot := -1
	if core.vacant != 0 {
		slot = bits.TrailingZeros64(core.vacant)
		core.vacant &^= bit(slot)
	} else if core.count < MaxTasks {
		slot = core.count
		core.count++
	} else {
		return 0, 0, false
	}
	core.generations[slot]++
	if core.generations[slot] == 0 {
		core.generations[slot] = 1
	}
	core.live |= bit(slot)
	core.started &^= bit(slot)
	core.runnable &^= bit(slot)
	core.dead &^= bit(slot)
	core.wakePending &^= bit(slot)
	core.killed &^= bit(slot)
	core.held &^= bit(slot)
	core.stopped &^= bit(slot)
	core.pinned &^= bit(slot)
	core.affinity[slot] = 0
	for cpu := range core.barred {
		core.barred[cpu] &^= bit(slot)
	}
	return slot, core.generations[slot], true
}

// Abandon returns a slot Install claimed whose task never started.
func (core *Core) Abandon(slot int) bool {
	if !core.inRange(slot) || core.live&bit(slot) == 0 || core.started&bit(slot) != 0 {
		return false
	}
	core.live &^= bit(slot)
	core.vacant |= bit(slot)
	return true
}

// Start makes an installed task runnable for the first time. It returns the
// processor that should run it.
func (core *Core) Start(slot int, self int) (int, bool) {
	if !core.inRange(slot) || core.live&bit(slot) == 0 || core.started&bit(slot) != 0 {
		return 0, false
	}
	core.started |= bit(slot)
	core.runnable |= bit(slot)
	return core.place(slot, self), true
}

// Wake makes a started, live task runnable and records the wake, so a task
// that is about to block keeps running instead. It returns the processor
// that should look at its run queue, or Idle when none needs to: a task
// still on a processor is picked again there.
func (core *Core) Wake(slot int, self int) (int, bool) {
	if !core.inRange(slot) || core.live&bit(slot) == 0 || core.started&bit(slot) == 0 || core.dead&bit(slot) != 0 {
		return Idle, false
	}
	core.runnable |= bit(slot)
	core.wakePending |= bit(slot)
	if core.running&bit(slot) != 0 {
		return Idle, true
	}
	return core.place(slot, self), true
}

// ForgetWake drops a wake recorded for the task a processor runs. A call
// clears it when it enters the kernel: a wait it registers there may only be
// kept from sleeping by a wake that came after, for that wait, and not by a
// wake meant for one that already ended.
func (core *Core) ForgetWake(cpu int) {
	if slot := int(core.current[cpu]); slot != Idle {
		core.wakePending &^= bit(slot)
	}
}

// Block parks the task a processor runs unless a wake is pending for it. The
// processor switches away at its next scheduling point.
func (core *Core) Block(cpu int) {
	slot := int(core.current[cpu])
	if slot == Idle {
		return
	}
	if core.wakePending&bit(slot) != 0 {
		core.wakePending &^= bit(slot)
		return
	}
	core.runnable &^= bit(slot)
}

// Terminate ends a task the calling processor does not run. A task off every
// processor is dead at once and its exit is pending. A task another
// processor runs is marked killed; that processor ends it at its next
// scheduling point, and the returned processor is the one to interrupt.
func (core *Core) Terminate(slot int, self int) (int, bool) {
	if !core.inRange(slot) || core.live&bit(slot) == 0 || core.dead&bit(slot) != 0 || core.killed&bit(slot) != 0 {
		return Idle, false
	}
	if int(core.current[self]) == slot {
		return Idle, false
	}
	if core.running&bit(slot) != 0 {
		core.killed |= bit(slot)
		core.stats[self].Kills++
		return core.runningOn(slot), true
	}
	core.retire(slot)
	core.exitPending |= bit(slot)
	return Idle, true
}

// Runnable reports whether a task wants a processor.
func (core *Core) Runnable(slot int) bool {
	return core.inRange(slot) && core.runnable&bit(slot) != 0
}

// Killed reports whether the task a processor runs has been terminated.
func (core *Core) Killed(cpu int) bool {
	slot := int(core.current[cpu])
	return slot != Idle && core.killed&bit(slot) != 0
}

// MustLeave reports whether the task a processor runs has been terminated or
// held, so that it should leave the processor at once.
func (core *Core) MustLeave(cpu int) bool {
	slot := int(core.current[cpu])
	return slot != Idle && (core.killed|core.held|core.stopped)&bit(slot) != 0
}

// Hold keeps a live task off every processor until Release: no processor
// picks it, whether it is runnable or not. A processor that runs it leaves
// it at its next scheduling point; the returned processor is the one to
// interrupt, or Idle. Kernel tasks cannot be held.
func (core *Core) Hold(slot int, self int) (int, bool) {
	if !core.inRange(slot) || core.live&bit(slot) == 0 || core.dead&bit(slot) != 0 ||
		core.pinned&bit(slot) != 0 || int(core.current[self]) == slot {
		return Idle, false
	}
	core.held |= bit(slot)
	if core.running&bit(slot) != 0 {
		return core.runningOn(slot), true
	}
	return Idle, true
}

// Release lets a held task run again. A runnable task is placed as a wake
// would place it; the returned processor is the one to interrupt.
func (core *Core) Release(slot int, self int) int {
	if !core.inRange(slot) || core.held&bit(slot) == 0 {
		return Idle
	}
	core.held &^= bit(slot)
	if core.runnable&bit(slot) == 0 || core.dead&bit(slot) != 0 || core.running&bit(slot) != 0 || core.stopped&bit(slot) != 0 {
		return Idle
	}
	return core.place(slot, self)
}

// Stop keeps a live task off every processor until Continue, as Hold does,
// for a job-control stop. Unlike Hold it takes the calling processor's own
// task, which leaves at its next scheduling point; the returned processor is
// the one to interrupt.
func (core *Core) Stop(slot int, self int) (int, bool) {
	if !core.inRange(slot) || core.live&bit(slot) == 0 || core.dead&bit(slot) != 0 || core.pinned&bit(slot) != 0 {
		return Idle, false
	}
	core.stopped |= bit(slot)
	if core.running&bit(slot) != 0 {
		return core.runningOn(slot), true
	}
	return Idle, true
}

// Continue ends a stop. A runnable task is placed as a wake would place it;
// a wait that completed while it was stopped is kept.
func (core *Core) Continue(slot int, self int) int {
	if !core.inRange(slot) || core.stopped&bit(slot) == 0 {
		return Idle
	}
	core.stopped &^= bit(slot)
	if core.runnable&bit(slot) == 0 || core.dead&bit(slot) != 0 || core.running&bit(slot) != 0 || core.held&bit(slot) != 0 {
		return Idle
	}
	return core.place(slot, self)
}

// Stopped reports whether a job-control stop holds a task.
func (core *Core) Stopped(slot int) bool {
	return core.inRange(slot) && core.stopped&bit(slot) != 0
}

// Switch leaves the task cpu runs for the given reason and picks the next one,
// which may be the same task. A killed task exits whatever the reason.
func (core *Core) Switch(cpu int, action Action) int {
	previous := int(core.current[cpu])
	if previous != Idle {
		if core.killed&bit(previous) != 0 {
			action = Exit
		}
		switch action {
		case Exit:
			core.retire(previous)
			core.dying |= bit(previous)
		case Block:
			if core.wakePending&bit(previous) != 0 {
				core.wakePending &^= bit(previous)
			} else {
				core.runnable &^= bit(previous)
			}
		}
	}
	next := core.pick(cpu, previous)
	if next != previous {
		core.stats[cpu].Switches++
		if next != Idle {
			core.running |= bit(next)
			core.wakePending &^= bit(next)
		}
		core.current[cpu] = int8(next)
	}
	return next
}

// Finish runs on cpu once it has left previous's stack. The task is now off
// every processor and may be picked elsewhere. A task that ended has its exit
// published. When previous is still runnable and some processor idles, the
// task moves there. It returns the processor to interrupt, or Idle.
func (core *Core) Finish(cpu int, previous int) int {
	if previous == Idle || !core.inRange(previous) || int(core.current[cpu]) == previous {
		return Idle
	}
	core.running &^= bit(previous)
	if core.dying&bit(previous) != 0 {
		core.dying &^= bit(previous)
		core.exitPending |= bit(previous)
		core.runnable |= 1
		core.wakePending |= 1
		if core.current[Housekeeping] == 0 {
			return Idle
		}
		return Housekeeping
	}
	if core.runnable&bit(previous) == 0 || (core.pinned|core.held|core.stopped)&bit(previous) != 0 {
		return Idle
	}
	if home := core.homeOf(previous); home != cpu && home >= 0 {
		// Its affinity moved it while it ran. The new home may be idle and
		// not know it has work.
		if core.current[home] == Idle {
			return home
		}
		return Idle
	}
	target := core.idleCPUFor(cpu, previous)
	if target == Idle || target == cpu {
		if core.current[cpu] == Idle {
			// It was woken while this processor left it for the idle loop.
			return cpu
		}
		return Idle
	}
	core.move(previous, target)
	return target
}

// Tick is the housekeeping processor's timer tick: it wakes task 0, which
// expires deadlines and delivers exits.
func (core *Core) Tick() {
	core.runnable |= 1
	core.wakePending |= 1
}

// TakeExited returns the lowest task whose exit is pending and clears it.
func (core *Core) TakeExited() (int, bool) {
	if core.exitPending == 0 {
		return 0, false
	}
	slot := bits.TrailingZeros64(core.exitPending)
	core.exitPending &^= bit(slot)
	return slot, true
}

// Reclaimable reports whether a dead task has left every processor and had
// its exit delivered, so its stack can be freed.
func (core *Core) Reclaimable(slot int) bool {
	return core.inRange(slot) && core.live&bit(slot) != 0 && core.dead&bit(slot) != 0 &&
		(core.exitPending|core.dying|core.running)&bit(slot) == 0
}

// Vacate frees a reclaimed slot for Install.
func (core *Core) Vacate(slot int) bool {
	if !core.Reclaimable(slot) {
		return false
	}
	core.live &^= bit(slot)
	core.vacant |= bit(slot)
	return true
}

// Current returns the slot cpu runs, or Idle.
func (core *Core) Current(cpu int) int {
	if cpu < 0 || cpu >= MaxCPUs {
		return Idle
	}
	return int(core.current[cpu])
}

// RunningOn returns the processor that runs slot, or Idle.
func (core *Core) RunningOn(slot int) int {
	if !core.inRange(slot) || core.running&bit(slot) == 0 {
		return Idle
	}
	return core.runningOn(slot)
}

// TaskID returns the identity of the task in slot, or zero for a slot that
// never held one.
func (core *Core) TaskID(slot int) TaskID {
	if !core.inRange(slot) || core.generations[slot] == 0 {
		return 0
	}
	id, _ := NewTaskID(uint32(slot), core.generations[slot])
	return id
}

// Resolve returns the slot of a task that is still installed, dead or alive.
func (core *Core) Resolve(id TaskID) (int, bool) {
	slot, generation, valid := id.Parts()
	if !valid || int(slot) >= core.count || core.generations[slot] != generation || core.live&bit(int(slot)) == 0 {
		return 0, false
	}
	return int(slot), true
}

// Alive returns the slot of a task that is installed and not dead.
func (core *Core) Alive(id TaskID) (int, bool) {
	slot, ok := core.Resolve(id)
	if !ok || core.dead&bit(slot) != 0 {
		return 0, false
	}
	return slot, true
}

// SetAffinity sets the processors a task may run on. The mask must name an
// online processor; a kernel task keeps the housekeeping processor and
// accepts only a mask that includes it. A task queued on a processor the mask
// leaves out moves to one it allows. It returns the processor to interrupt:
// the one that runs the task and must now leave it, or the idle one the task
// moved to, or Idle.
func (core *Core) SetAffinity(slot int, mask uint64, self int) (int, bool) {
	if !core.inRange(slot) || core.live&bit(slot) == 0 || core.dead&bit(slot) != 0 || mask&core.online == 0 {
		return Idle, false
	}
	if core.pinned&bit(slot) != 0 {
		return Idle, mask&bit(Housekeeping) != 0
	}
	core.affinity[slot] = mask
	for cpu := 0; cpu < MaxCPUs; cpu++ {
		if mask&bit(cpu) == 0 {
			core.barred[cpu] |= bit(slot)
		} else {
			core.barred[cpu] &^= bit(slot)
		}
	}
	kick := Idle
	if home := core.homeOf(slot); home >= 0 && !core.allows(slot, home) {
		target := core.idleCPUFor(home, slot)
		if target == Idle {
			target = core.leastLoaded(slot)
		}
		core.move(slot, target)
		if core.runnable&bit(slot) != 0 && (core.held|core.stopped)&bit(slot) == 0 {
			kick = target
		}
	}
	if core.running&bit(slot) != 0 {
		if cpu := core.runningOn(slot); cpu != Idle && !core.allows(slot, cpu) {
			return cpu, true
		}
		return Idle, true
	}
	return kick, true
}

// Affinity returns the online processors a task may run on, or zero for a
// slot that holds no live task.
func (core *Core) Affinity(slot int) uint64 {
	if !core.inRange(slot) || core.live&bit(slot) == 0 {
		return 0
	}
	if core.pinned&bit(slot) != 0 {
		return bit(Housekeeping)
	}
	mask := core.affinity[slot]
	if mask == 0 {
		mask = ^uint64(0)
	}
	return mask & core.online
}

// Snapshot is a view of the run state for diagnostics. Its Stats sum every
// processor's.
type Snapshot struct {
	Runnable, Dead, Running, Vacant, Online uint64
	Count                                   int
	Stats                                   Stats
}

func (core *Core) Snapshot() Snapshot {
	var total Stats
	for cpu := range core.stats {
		total.Switches += core.stats[cpu].Switches
		total.Steals += core.stats[cpu].Steals
		total.Migrations += core.stats[cpu].Migrations
		total.Kills += core.stats[cpu].Kills
	}
	return Snapshot{Runnable: core.runnable, Dead: core.dead, Running: core.running, Vacant: core.vacant,
		Online: core.online, Count: core.count, Stats: total}
}

// CPUStats returns one processor's counters.
func (core *Core) CPUStats(cpu int) Stats {
	if cpu < 0 || cpu >= MaxCPUs {
		return Stats{}
	}
	return core.stats[cpu]
}

// Check verifies the invariants that make two processors running one task
// impossible. Validation builds call it after every switch.
func (core *Core) Check() bool {
	var homes, running uint64
	for cpu := 0; cpu < MaxCPUs; cpu++ {
		if core.home[cpu]&homes != 0 || core.home[cpu]&core.barred[cpu] != 0 {
			return false
		}
		homes |= core.home[cpu]
		if core.online&bit(cpu) == 0 {
			if core.home[cpu] != 0 || core.current[cpu] != Idle {
				return false
			}
			continue
		}
		if slot := int(core.current[cpu]); slot != Idle {
			if running&bit(slot) != 0 || core.running&bit(slot) == 0 {
				return false
			}
			running |= bit(slot)
		}
	}
	live := core.live &^ core.dead
	return homes&^live == 0 && core.runnable&^homes == 0 && core.running&^core.live == 0 &&
		core.vacant&core.live == 0 && core.pinned&^core.home[Housekeeping]&live == 0
}

func (core *Core) pick(cpu int, previous int) int {
	queue := core.runnable & core.home[cpu] &^ core.running
	if previous != Idle {
		queue |= core.runnable & core.home[cpu] & bit(previous)
	}
	queue &^= core.held | core.stopped
	if queue != 0 {
		next := nextAfter(queue, int(core.rotor[cpu]))
		core.rotor[cpu] = int8(next)
		return next
	}
	// Nothing at home: take one task from another processor's queue. A
	// pinned task never moves, and a task still on a processor cannot.
	for step := 1; step < MaxCPUs; step++ {
		other := (cpu + step) % MaxCPUs
		if core.online&bit(other) == 0 {
			continue
		}
		candidates := core.runnable & core.home[other] &^ core.running &^ core.pinned &^ core.held &^ core.stopped &^ core.barred[cpu]
		if candidates == 0 {
			continue
		}
		next := nextAfter(candidates, int(core.rotor[other]))
		core.move(next, cpu)
		core.stats[cpu].Steals++
		core.rotor[cpu] = int8(next)
		return next
	}
	return Idle
}

// place chooses the run queue of a task that became runnable. A pinned task
// goes home to the housekeeping processor. Otherwise the task stays where it
// last ran when that processor idles, then goes to an idle processor, then
// to the least loaded one, each among the processors its affinity allows.
func (core *Core) place(slot int, self int) int {
	if core.pinned&bit(slot) != 0 {
		return Housekeeping
	}
	home := core.homeOf(slot)
	if home < 0 {
		home = self
		if !core.allows(slot, home) {
			home = core.leastLoaded(slot)
		}
		core.home[home] |= bit(slot)
	}
	if core.idle(home, slot) {
		return home
	}
	if target := core.idleCPUFor(home, slot); target != Idle {
		core.move(slot, target)
		return target
	}
	target, load := home, core.load(home, slot)
	for cpu := 0; cpu < MaxCPUs; cpu++ {
		if !core.allows(slot, cpu) {
			continue
		}
		if candidate := core.load(cpu, slot); candidate+1 < load {
			target, load = cpu, candidate
		}
	}
	if target != home {
		core.move(slot, target)
	}
	return target
}

// allows reports whether slot may run on cpu.
func (core *Core) allows(slot int, cpu int) bool {
	if core.pinned&bit(slot) != 0 {
		return cpu == Housekeeping
	}
	return cpu >= 0 && cpu < MaxCPUs && core.online&bit(cpu) != 0 && core.barred[cpu]&bit(slot) == 0
}

// leastLoaded returns the allowed processor with the least to run, the
// lowest on a tie. A task's affinity always names an online processor.
func (core *Core) leastLoaded(slot int) int {
	target, load := Housekeeping, -1
	for cpu := 0; cpu < MaxCPUs; cpu++ {
		if !core.allows(slot, cpu) {
			continue
		}
		if candidate := core.load(cpu, slot); load < 0 || candidate < load {
			target, load = cpu, candidate
		}
	}
	return target
}

// idle reports whether cpu runs its idle loop with nothing else queued.
func (core *Core) idle(cpu int, except int) bool {
	return core.online&bit(cpu) != 0 && core.current[cpu] == Idle &&
		core.runnable&core.home[cpu]&^core.held&^core.stopped&^bit(except) == 0
}

// idleCPUFor returns an idle processor slot may run on, searching from
// after start, or Idle.
func (core *Core) idleCPUFor(start int, slot int) int {
	for step := 1; step <= MaxCPUs; step++ {
		cpu := (start + step) % MaxCPUs
		if core.idle(cpu, -1) && core.allows(slot, cpu) {
			return cpu
		}
	}
	return Idle
}

// load counts what cpu has to run besides slot: its queue and its current
// task.
func (core *Core) load(cpu int, slot int) int {
	load := bits.OnesCount64(core.runnable & core.home[cpu] &^ bit(slot))
	if current := int(core.current[cpu]); current != Idle && current != slot && core.runnable&bit(current) == 0 {
		load++
	}
	return load
}

func (core *Core) retire(slot int) {
	core.runnable &^= bit(slot)
	core.wakePending &^= bit(slot)
	core.killed &^= bit(slot)
	core.held &^= bit(slot)
	core.stopped &^= bit(slot)
	core.dead |= bit(slot)
	if home := core.homeOf(slot); home >= 0 {
		core.home[home] &^= bit(slot)
	}
}

func (core *Core) move(slot int, target int) {
	if home := core.homeOf(slot); home >= 0 {
		if home == target {
			return
		}
		core.home[home] &^= bit(slot)
	}
	core.home[target] |= bit(slot)
	core.stats[target].Migrations++
}

func (core *Core) homeOf(slot int) int {
	for cpu := 0; cpu < MaxCPUs; cpu++ {
		if core.home[cpu]&bit(slot) != 0 {
			return cpu
		}
	}
	return -1
}

func (core *Core) runningOn(slot int) int {
	for cpu := 0; cpu < MaxCPUs; cpu++ {
		if int(core.current[cpu]) == slot {
			return cpu
		}
	}
	return Idle
}

func (core *Core) inRange(slot int) bool { return slot >= 0 && slot < core.count }

// nextAfter returns the lowest set bit of mask above position after,
// wrapping to the lowest set bit.
func nextAfter(mask uint64, after int) int {
	if after >= 0 && after < 63 {
		if above := mask &^ lowMask(after+1); above != 0 {
			return bits.TrailingZeros64(above)
		}
	}
	return bits.TrailingZeros64(mask)
}

func bit(slot int) uint64 { return uint64(1) << uint(slot) }

func lowMask(count int) uint64 {
	if count >= 64 {
		return ^uint64(0)
	}
	return bit(count) - 1
}
