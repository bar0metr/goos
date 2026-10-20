//go:build uefi && amd64

package amd64

import (
	"goos/internal/validation"
	"goos/kernel/lock"
	"goos/kernel/sched"
	"math/bits"
	"sync/atomic"
)

// MaxCPUs bounds the processors the kernel runs on.
const MaxCPUs = sched.MaxCPUs

// Slots 0-63 are tasks; slot idleBase+n is processor n's idle loop.
const (
	idleBase  = sched.MaxTasks
	slotCount = idleBase + MaxCPUs
	// kernelTasks are the kernel's own slots: the boot and housekeeping
	// task, the two scheduling demonstration tasks, and a reserved slot.
	kernelTasks = 4
)

// The words of a processor's data block, in goos_cpu_data.
const (
	cpuCurrent = iota
	cpuNeedResched
	cpuTSSRSP0
	cpuActiveRoot
	cpuEntrySeq
	cpuEnabled
	cpuKill
	cpuPrevious
	cpuReschedIPIs
	cpuTLBIPIs
	cpuTicks
	cpuCallArg2
	cpuKernelRoot
	cpuWords = 32
)

// The reasons the entry code passes goos_sched_switch.
const (
	switchPreempt = 0
	switchBlock   = 1
	switchExit    = 2
	switchTick    = 3
)

//go:extern goos_cpu_data
var cpuData [MaxCPUs][cpuWords]uint64

//go:extern goos_task_contexts
var taskContexts [slotCount]uint64

//go:extern goos_task_roots
var taskRoots [slotCount]uint64

//go:extern goos_task_fs_bases
var taskFSBases [slotCount]uint64

//go:extern goos_task_rsp0
var taskRSP0 [slotCount]uint64

//go:extern goos_task_exit_codes
var taskExitCodes [sched.MaxTasks]uint64

//go:extern goos_linux_entry_data
var linuxEntryData [MaxCPUs][4]uint64

var (
	// runState is the scheduler's state; schedulerLock guards it.
	runState      sched.Core
	schedulerLock = lock.New(lock.LevelScheduler)
	// cpuAPIC is each processor's APIC ID, written before it starts.
	cpuAPIC [MaxCPUs]uint32
	// killCodes are the exit codes of tasks killed while they ran.
	killCodes [sched.MaxTasks]uint64
	// kernelGeneration counts changes to kernel-root mappings; a processor
	// that has seen fewer flushes its TLB at its next switch.
	kernelGeneration atomic.Uint64
	cpuGeneration    [MaxCPUs]uint64
	// online mirrors runState's online mask for readers outside the lock.
	online atomic.Uint64
	// parallelPeak is the most tasks of one address space that ran on
	// distinct processors at once.
	parallelPeak atomic.Uint64
	// inKernel counts the processors between kernel entry and leave for a
	// call or a fault; kernelPeak is the most at once.
	inKernel   atomic.Int64
	kernelPeak atomic.Uint64
	// threadPeak is the most tasks of one address space inside the kernel
	// at once, which only a program with several threads raises.
	threadPeak atomic.Uint64
)

// CPUIndex returns the calling processor's index; processor 0 is the
// bootstrap processor.
func CPUIndex() int { return int(cpuIndex()) }

//export goos_cpu_index
func cpuIndex() uint64

//export goos_interrupts_enable
func enableInterrupts()

//export goos_wait_for_interrupt
func waitForInterrupt()

//export goos_flush_tlb
func flushTLB()

//export goos_write_fs_base
func writeFSBase(base uint64)

//export goos_yield
func yield()

// DisableInterrupts disables interrupts on the calling processor and returns
// the flags RestoreInterrupts needs. It excludes nothing on other
// processors; shared state needs its lock as well.
//
//export goos_interrupts_disable
func DisableInterrupts() uint64

//export goos_interrupts_restore
func RestoreInterrupts(flags uint64)

func onlineMask() uint64 { return online.Load() }

// OnlineMask returns the processors that run tasks.
func OnlineMask() uint64 { return onlineMask() }

func asmSlot(slot int, cpu int) uint64 {
	if slot == sched.Idle {
		return uint64(idleBase + cpu)
	}
	return uint64(slot)
}

// lockScheduler disables local interrupts, so no tick on this processor
// tries to take the lock again, and takes the scheduler lock.
func lockScheduler() uint64 {
	flags := DisableInterrupts()
	schedulerLock.Lock()
	return flags
}

func unlockScheduler(flags uint64) {
	schedulerLock.Unlock()
	RestoreInterrupts(flags)
}

// schedSwitch leaves the current task for the given reason and returns the
// slot to run next. The entry code calls it with the outgoing context saved.
//
//export goos_sched_switch
func schedSwitch(action uint64) uint64 {
	cpu := CPUIndex()
	schedulerLock.Lock()
	tick := action == switchTick
	if tick {
		if cpu == sched.Housekeeping {
			runState.Tick()
		}
		action = switchPreempt
	}
	previous := runState.Current(cpu)
	if tick && previous != sched.Idle {
		notePreemptTick(uint64(previous))
	}
	if runState.Killed(cpu) && previous != sched.Idle {
		taskExitCodes[previous] = killCodes[previous]
	}
	next := runState.Switch(cpu, sched.Action(action))
	if next != sched.Idle && next >= kernelTasks {
		noteParallel(taskRoots[next])
	}
	atomic.StoreUint64(&cpuData[cpu][cpuNeedResched], 0)
	leave := uint64(0)
	if runState.MustLeave(cpu) {
		leave = 1
	}
	atomic.StoreUint64(&cpuData[cpu][cpuKill], leave)
	if validation.Enabled && !runState.Check() {
		schedulerLock.Unlock()
		fatal("scheduler invariant broken")
	}
	schedulerLock.Unlock()
	if generation := kernelGeneration.Load(); cpuGeneration[cpu] != generation {
		cpuGeneration[cpu] = generation
		flushTLB()
	}
	return asmSlot(next, cpu)
}

// noteParallel counts the processors that run a task of root, the caller's
// next task included, and raises the peak. The scheduler lock is held.
func noteParallel(root uint64) {
	count := uint64(0)
	for mask := runState.OnlineMask(); mask != 0; mask &= mask - 1 {
		cpu := bits.TrailingZeros64(mask)
		if slot := runState.Current(cpu); slot != sched.Idle && taskRoots[slot] == root {
			count++
		}
	}
	if count > parallelPeak.Load() {
		parallelPeak.Store(count)
	}
}

// ParallelPeak returns the most tasks of one address space that ran on
// distinct processors at the same time since boot.
func ParallelPeak() uint64 { return parallelPeak.Load() }

// schedFinish runs on the incoming task's stack once the outgoing slot's
// stack is no longer in use.
//
//export goos_sched_finish
func schedFinish(previous uint64) {
	if previous >= sched.MaxTasks {
		return
	}
	cpu := CPUIndex()
	schedulerLock.Lock()
	kick := runState.Finish(cpu, int(previous))
	schedulerLock.Unlock()
	if kick != sched.Idle {
		kickCPU(kick, cpu)
	}
}

// kickCPU asks a processor to reschedule. A pending request covers later
// ones, so a processor is interrupted once per batch of wakes.
func kickCPU(target int, self int) {
	if target < 0 || target >= MaxCPUs {
		return
	}
	if atomic.SwapUint64(&cpuData[target][cpuNeedResched], 1) != 0 {
		return
	}
	if target == self {
		// The native and Linux returns check the request; an interrupt
		// return or the idle loop needs the interrupt.
		sendSelfIPI(rescheduleVector)
		return
	}
	sendIPI(target, rescheduleVector)
}

// kernelEnter starts a system call or user fault on the calling processor.
// It refuses, returning zero, when the calling task is being killed or held:
// a stopped task must not start a call whose waits nobody would cancel. The
// call itself takes the locks of the state it touches (ADR 0184) and gives
// them up the same way when the task is stopped while it waits for one.
//
//export goos_kernel_enter
func kernelEnter() uint64 {
	cpu := CPUIndex()
	if atomic.LoadUint64(&cpuData[cpu][cpuKill]) != 0 {
		return 0
	}
	schedulerLock.Lock()
	runState.ForgetWake(cpu)
	schedulerLock.Unlock()
	noteKernelEntry()
	return 1
}

//export goos_kernel_leave
func kernelLeave() {
	atomic.StoreUint64(&cpuData[CPUIndex()][cpuKernelRoot], 0)
	inKernel.Add(-1)
}

// noteKernelEntry counts the calling processor in the kernel and raises the
// peak.
func noteKernelEntry() {
	count := uint64(inKernel.Add(1))
	raisePeak(&kernelPeak, count)
	cpu := CPUIndex()
	slot := atomic.LoadUint64(&cpuData[cpu][cpuCurrent])
	if slot >= slotCount {
		return
	}
	root := taskRoots[slot]
	atomic.StoreUint64(&cpuData[cpu][cpuKernelRoot], root)
	if root == 0 {
		return
	}
	same := uint64(0)
	for mask := online.Load(); mask != 0; mask &= mask - 1 {
		if atomic.LoadUint64(&cpuData[bits.TrailingZeros64(mask)][cpuKernelRoot]) == root {
			same++
		}
	}
	raisePeak(&threadPeak, same)
}

func raisePeak(peak *atomic.Uint64, count uint64) {
	for old := peak.Load(); count > old && !peak.CompareAndSwap(old, count); old = peak.Load() {
	}
}

// StopWord is the calling processor's request to stop its task. It turns
// nonzero when another processor kills or holds the task, so a call that
// waits for a lock can give up instead of waiting on the processor that
// stopped it.
func StopWord() *uint64 { return &cpuData[CPUIndex()][cpuKill] }

// KernelParallelPeak returns the most processors that were inside system
// calls or fault resolution at the same time since boot.
func KernelParallelPeak() uint64 { return kernelPeak.Load() }

// KernelThreadPeak returns the most tasks of one address space that were
// inside system calls or fault resolution at the same time since boot.
func KernelThreadPeak() uint64 { return threadPeak.Load() }

// currentSlot returns the task the calling processor runs, or sched.Idle.
func currentSlot() int {
	slot := atomic.LoadUint64(&cpuData[CPUIndex()][cpuCurrent])
	if slot >= sched.MaxTasks {
		return sched.Idle
	}
	return int(slot)
}

// CurrentTask returns the task the calling processor runs, or zero.
func CurrentTask() sched.TaskID {
	slot := currentSlot()
	if slot == sched.Idle {
		return 0
	}
	return sched.TaskID(TaskIDForSlot(uint64(slot)))
}

// WakeTask makes a kernel task slot runnable.
func WakeTask(slot uint64) bool {
	if slot >= sched.MaxTasks {
		return false
	}
	return wakeSlot(int(slot))
}

//export goos_wake_slot
func wakeSlotFromInterrupt(slot uint64) {
	if slot < sched.MaxTasks {
		wakeSlot(int(slot))
	}
}

func wakeSlot(slot int) bool {
	cpu := CPUIndex()
	flags := lockScheduler()
	target, ok := runState.Wake(slot, cpu)
	schedulerLock.Unlock()
	if target != sched.Idle {
		kickCPU(target, cpu)
	}
	RestoreInterrupts(flags)
	return ok
}

// WakeTaskID makes a task runnable when its full identity still names it.
func WakeTaskID(task uint64) bool {
	cpu := CPUIndex()
	flags := lockScheduler()
	slot, ok := runState.Alive(sched.TaskID(task))
	target := sched.Idle
	if ok {
		target, ok = runState.Wake(slot, cpu)
	}
	schedulerLock.Unlock()
	if target != sched.Idle {
		kickCPU(target, cpu)
	}
	RestoreInterrupts(flags)
	return ok
}

// StartTask makes a task installed by AddUserTask runnable. The creator
// calls it once every piece of the task's state exists, so no processor can
// run a half-made task.
func StartTask(task sched.TaskID) bool {
	cpu := CPUIndex()
	flags := lockScheduler()
	slot, ok := runState.Alive(task)
	target := sched.Idle
	if ok {
		target, ok = runState.Start(slot, cpu)
	}
	schedulerLock.Unlock()
	if ok && target != cpu {
		kickCPU(target, cpu)
	}
	RestoreInterrupts(flags)
	return ok
}

// BlockCurrentTask parks the calling task unless a wake is pending for it;
// the processor leaves it at its next scheduling point, which a fault return
// takes at once.
func BlockCurrentTask() {
	cpu := CPUIndex()
	flags := lockScheduler()
	runState.Block(cpu)
	if slot := runState.Current(cpu); slot != sched.Idle && !runState.Runnable(slot) {
		atomic.StoreUint64(&cpuData[cpu][cpuNeedResched], 1)
	}
	unlockScheduler(flags)
}

// KernelSleep blocks the calling kernel task and switches away at once
// instead of halting until the next tick. A wake since the task last ran
// keeps it running, so no wake falls between its last check and the sleep.
// It returns after a wake, with the caller's interrupt state restored.
func KernelSleep() {
	flags := DisableInterrupts()
	yield()
	RestoreInterrupts(flags)
}

// SetCurrentTaskExitCode records the exit code of the calling task.
func SetCurrentTaskExitCode(code uint64) {
	if slot := currentSlot(); slot != sched.Idle {
		taskExitCodes[slot] = code
	}
}

// TerminateTask retires another task with an exit code, as a fault does. The
// caller cancels the task's wait registrations first. A task running on
// another processor is killed there: that processor stops it at its next
// scheduling point, and its exit is published once it is off the processor.
// The kernel's own tasks and the calling task cannot be terminated.
func TerminateTask(task uint64, code uint64) bool {
	cpu := CPUIndex()
	flags := lockScheduler()
	slot, ok := runState.Alive(sched.TaskID(task))
	target := sched.Idle
	if ok && slot >= kernelTasks {
		killCodes[slot] = code
		target, ok = runState.Terminate(slot, cpu)
		if ok && target == sched.Idle {
			taskExitCodes[slot] = code
		}
	} else {
		ok = false
	}
	if target != sched.Idle {
		atomic.StoreUint64(&cpuData[target][cpuKill], 1)
	}
	schedulerLock.Unlock()
	if target != sched.Idle {
		kickCPU(target, cpu)
	}
	RestoreInterrupts(flags)
	return ok
}

// HoldTasks keeps tasks off every processor until ReleaseTasks, and asks the
// processors that run them to leave them. The caller then waits with
// QuiesceTasks. Tasks that are dead, the kernel's own, or the caller's are
// skipped.
func HoldTasks(tasks []sched.TaskID) {
	cpu := CPUIndex()
	for index := range tasks {
		flags := lockScheduler()
		target := sched.Idle
		if slot, ok := runState.Alive(tasks[index]); ok && slot >= kernelTasks {
			target, _ = runState.Hold(slot, cpu)
		}
		if target != sched.Idle {
			atomic.StoreUint64(&cpuData[target][cpuKill], 1)
		}
		schedulerLock.Unlock()
		if target != sched.Idle {
			kickCPU(target, cpu)
		}
		RestoreInterrupts(flags)
	}
}

// ReleaseTasks lets tasks HoldTasks held run again.
func ReleaseTasks(tasks []sched.TaskID) {
	cpu := CPUIndex()
	for index := range tasks {
		flags := lockScheduler()
		target := sched.Idle
		if slot, ok := runState.Resolve(tasks[index]); ok {
			target = runState.Release(slot, cpu)
		}
		schedulerLock.Unlock()
		if target != sched.Idle && target != cpu {
			kickCPU(target, cpu)
		}
		RestoreInterrupts(flags)
	}
}

// SetTaskAffinity limits a live task to the processors in mask. A processor
// that runs it outside the mask is asked to reschedule and leaves it at its
// next scheduling point. It refuses a mask with no online processor, a dead
// task, and a kernel task moved away from the housekeeping processor.
func SetTaskAffinity(task sched.TaskID, mask uint64) bool {
	cpu := CPUIndex()
	flags := lockScheduler()
	slot, ok := runState.Alive(task)
	target := sched.Idle
	if ok {
		target, ok = runState.SetAffinity(slot, mask, cpu)
	}
	schedulerLock.Unlock()
	if target != sched.Idle {
		kickCPU(target, cpu)
	}
	RestoreInterrupts(flags)
	return ok
}

// TaskAffinity returns the online processors a live task may run on.
func TaskAffinity(task sched.TaskID) (uint64, bool) {
	flags := lockScheduler()
	slot, ok := runState.Alive(task)
	mask := uint64(0)
	if ok {
		mask = runState.Affinity(slot)
	}
	unlockScheduler(flags)
	return mask, ok && mask != 0
}

// TaskLive reports whether a task's identity still names a live task.
func TaskLive(task sched.TaskID) bool {
	flags := lockScheduler()
	_, ok := runState.Alive(task)
	unlockScheduler(flags)
	return ok
}

// TaskOffProcessor reports whether a task is on no processor: never started,
// dead and switched away from, or waiting in a run queue.
func TaskOffProcessor(task sched.TaskID) bool {
	flags := lockScheduler()
	slot, ok := runState.Resolve(task)
	off := !ok || runState.RunningOn(slot) == sched.Idle
	unlockScheduler(flags)
	return off
}

// QuiesceTasks waits until every task in tasks is off its processor. The
// caller has terminated them; each one's processor stops it at its next
// scheduling point, which a reschedule interrupt brings at once, and a task
// waiting for a lock gives up the wait. The caller holds the locks of its
// own call, which the stopped tasks never need to finish theirs. It reports
// false when a task does not stop within the bound.
func QuiesceTasks(tasks []sched.TaskID) bool {
	for spins := 0; spins < quiesceSpins; spins++ {
		quiet := true
		for index := range tasks {
			if tasks[index] != 0 && !TaskOffProcessor(tasks[index]) {
				quiet = false
				break
			}
		}
		if quiet {
			return true
		}
		cpuPause()
	}
	return false
}

// quiesceSpins bounds QuiesceTasks: far longer than an interrupt takes to
// arrive, short enough that a stuck processor is reported.
const quiesceSpins = 1 << 26

// TakeExitedTask returns the lowest task whose exit is pending, or zero.
func TakeExitedTask() uint64 {
	flags := lockScheduler()
	slot, found := runState.TakeExited()
	id := sched.TaskID(0)
	if found {
		id = runState.TaskID(slot)
	}
	unlockScheduler(flags)
	return uint64(id)
}

// TaskExitCode returns and clears the exit code of a task.
func TaskExitCode(task uint64) uint64 {
	flags := lockScheduler()
	slot, ok := runState.Resolve(sched.TaskID(task))
	code := uint64(0)
	if ok {
		code, taskExitCodes[slot] = taskExitCodes[slot], 0
	}
	unlockScheduler(flags)
	return code
}

// TaskIDForSlot returns the identity of the task in a slot, or zero.
func TaskIDForSlot(slot uint64) uint64 {
	if slot >= sched.MaxTasks {
		return 0
	}
	flags := lockScheduler()
	id := runState.TaskID(int(slot))
	unlockScheduler(flags)
	return uint64(id)
}

// SetTaskFSBase changes the saved FS base of one live task and updates the
// processor immediately when the task runs on it.
func SetTaskFSBase(task sched.TaskID, base uint64) bool {
	flags := lockScheduler()
	slot, ok := runState.Alive(task)
	if ok {
		taskFSBases[slot] = base
		if runState.Current(CPUIndex()) == slot {
			writeFSBase(base)
		}
	}
	unlockScheduler(flags)
	return ok
}

// TaskFSBase reads the saved FS base of one live task.
func TaskFSBase(task sched.TaskID) (uint64, bool) {
	flags := lockScheduler()
	slot, ok := runState.Alive(task)
	base := uint64(0)
	if ok {
		base = taskFSBases[slot]
	}
	unlockScheduler(flags)
	return base, ok
}

// ReplaceTaskAddressSpace changes the saved root of the calling task. The
// prepared root supplies the next return to ring 3 and later entries.
func ReplaceTaskAddressSpace(task sched.TaskID, pageTable uint64) bool {
	if pageTable == 0 {
		return false
	}
	cpu := CPUIndex()
	flags := lockScheduler()
	slot, ok := runState.Alive(task)
	ok = ok && runState.Current(cpu) == slot
	if ok {
		taskRoots[slot] = pageTable
		linuxEntryData[cpu][0] = pageTable
	}
	unlockScheduler(flags)
	return ok
}

// SchedulerState is a snapshot of the run state. Current is the calling
// processor's slot.
type SchedulerState struct {
	Runnable uint64
	Dead     uint64
	Count    uint64
	Current  uint64
	Running  uint64
	Online   uint64
	Stats    sched.Stats
}

func ReadSchedulerState() SchedulerState {
	cpu := CPUIndex()
	flags := lockScheduler()
	snapshot := runState.Snapshot()
	current := asmSlot(runState.Current(cpu), cpu)
	unlockScheduler(flags)
	return SchedulerState{Runnable: snapshot.Runnable, Dead: snapshot.Dead, Count: uint64(snapshot.Count),
		Current: current, Running: snapshot.Running, Online: snapshot.Online, Stats: snapshot.Stats}
}

// TaskStackTop returns the kernel-entry stack top of a user task slot, or zero
// for kernel tasks and unused slots.
func TaskStackTop(slot uint64) uint64 {
	if slot >= sched.MaxTasks {
		return 0
	}
	return atomic.LoadUint64(&taskRSP0[slot])
}

// FreeTaskSlots counts the slots a new task can take.
func FreeTaskSlots() int {
	snapshot := ReadSchedulerState()
	flags := lockScheduler()
	vacant := runState.Snapshot().Vacant
	unlockScheduler(flags)
	free := sched.MaxTasks - int(snapshot.Count)
	for ; vacant != 0; vacant &= vacant - 1 {
		free++
	}
	return free
}

// KernelMappingsChanged records a change to the kernel root's mappings that
// other processors may hold stale translations of. Each flushes before it
// next switches tasks; the only such changes are guard pages around new
// stacks, which only a task's own processor touches, and it switches to the
// task before it runs it.
func KernelMappingsChanged() { kernelGeneration.Add(1) }

// CPURuntime is one processor's counters at one moment. Each processor
// updates its own, so a set read across processors is not one instant.
type CPURuntime struct {
	Online      bool
	Current     sched.TaskID
	Ticks       uint64
	IdleTicks   uint64
	Scheduler   sched.Stats
	Reschedules uint64
	Shootdowns  uint64
}

// ReadCPURuntime returns processor cpu's counters. It stops no processor.
func ReadCPURuntime(cpu int) CPURuntime {
	if cpu < 0 || cpu >= MaxCPUs {
		return CPURuntime{}
	}
	flags := lockScheduler()
	runtime := CPURuntime{Online: runState.OnlineMask()&(uint64(1)<<uint(cpu)) != 0, Scheduler: runState.CPUStats(cpu)}
	if slot := runState.Current(cpu); slot != sched.Idle {
		runtime.Current = runState.TaskID(slot)
	}
	unlockScheduler(flags)
	counters := ProcessorCounters(cpu)
	runtime.Ticks, runtime.Reschedules, runtime.Shootdowns = counters.Ticks, counters.RescheduleIPIs, counters.ShootdownIPIs
	runtime.IdleTicks = IdleTicks(cpu)
	return runtime
}

// SMPCounters reports per-processor event counts for diagnostics.
type SMPCounters struct {
	Ticks, RescheduleIPIs, ShootdownIPIs uint64
}

func ProcessorCounters(cpu int) SMPCounters {
	if cpu < 0 || cpu >= MaxCPUs {
		return SMPCounters{}
	}
	return SMPCounters{
		Ticks:          atomic.LoadUint64(&cpuData[cpu][cpuTicks]),
		RescheduleIPIs: atomic.LoadUint64(&cpuData[cpu][cpuReschedIPIs]),
		ShootdownIPIs:  atomic.LoadUint64(&cpuData[cpu][cpuTLBIPIs]),
	}
}

// IdleTicks returns the ticks processor cpu spent in its idle loop.
func IdleTicks(cpu int) uint64 {
	if cpu < 0 || cpu >= MaxCPUs {
		return 0
	}
	return TaskTicks(uint64(idleBase + cpu))
}

var fatalHandler func(message string)

// SetFatalHandler names the function that stops the machine on a broken
// processor invariant.
func SetFatalHandler(handler func(message string)) {
	fatalHandler = handler
	lock.Install(lock.Hooks{Pause: func() { cpuPause() }, CPU: func() uint32 { return uint32(cpuIndex()) }, Fatal: handler})
}

// SetLockWatchdog arms the lock watchdog before a second processor starts:
// a spin that lasts longer than seconds on the TSC, which runs at hz, is
// reported through stuck.
func SetLockWatchdog(hz uint64, seconds uint64, stuck func(level lock.Level, holder uint32)) {
	lock.Install(lock.Hooks{
		Pause:      func() { cpuPause() },
		CPU:        func() uint32 { return uint32(cpuIndex()) },
		Fatal:      fatalHandler,
		Now:        func() uint64 { return ReadTSC() },
		StuckAfter: hz * seconds,
		Stuck:      stuck,
	})
}

func fatal(message string) {
	if fatalHandler != nil {
		fatalHandler(message)
	}
	for {
		Halt()
	}
}

// Pause hints to the processor that the caller spins.
func Pause() { cpuPause() }

// RequestReschedule makes the calling task's return from the kernel a
// scheduling point, so other runnable tasks on this processor run first.
func RequestReschedule() {
	atomic.StoreUint64(&cpuData[CPUIndex()][cpuNeedResched], 1)
}
