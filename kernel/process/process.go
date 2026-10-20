package process

import (
	"goos/kernel/handle"
	"goos/kernel/lock"
	"goos/kernel/mm/region"
	"goos/kernel/mm/vm"
	"goos/kernel/nativethread"
	"goos/kernel/sched"
)

// MaxProcesses is the system-wide process limit. A process needs at least one
// of the scheduler's sched.MaxTasks task slots, so more processes than slots
// could never run; the kernel allocates a process record's pages the first
// time a slot is used (ADR 0111). MaxExitEvents keeps one exit report per
// process until PID 1 collects it.
const (
	MaxTasksPerProcess = 32
	MaxProcesses       = sched.MaxTasks
	MaxExitEvents      = MaxProcesses
)

type ID uint64

// ABI identifies the userspace contract a process is allowed to enter. It is
// kernel-owned and fixed when the process slot is initialized from a validated
// executable.
type ABI uint8

const (
	ABIInvalid ABI = iota
	ABINative
	ABILinuxX8664
)

type AddressSpace struct {
	Root uint64
}

type Process struct {
	id           ID
	abi          ABI
	addressSpace AddressSpace
	handles      handle.Table
	tasks        [MaxTasksPerProcess]sched.TaskID
	taskCount    int
	taskReserved uint8
	quota        Quota
	// regions is the process's virtual-memory region map (ADR 0104). space
	// guards it and the page tables under the root.
	regions region.Map
	space   lock.RW
	// executable is the executable object the process was started from and
	// reads its pages from, or zero (ADR 0106).
	executable uint64
	cpuTicks   uint64
	name       [16]byte
	nameSize   uint8
	// threads is the thread group of a native process (ADR 0203). The
	// registry's write lock guards it.
	threads nativethread.Group
}

// Init prepares a process slot in place; see stream.Stream.Reset. Returning a
// Process by value instead would build the whole handle table as a temporary,
// which the kernel's leaking heap never frees.
func Init(target *Process, id ID, abi ABI, addressSpace AddressSpace) bool {
	return InitWithProfile(target, id, abi, addressSpace, ProfileNormal)
}

// InitWithProfile prepares a process with one of the kernel's bounded quota
// sets. Callers select a profile, never individual limits.
func InitWithProfile(target *Process, id ID, abi ABI, addressSpace AddressSpace, profile ResourceProfile) bool {
	if target == nil || id == 0 || !abi.Valid() || addressSpace.Root == 0 {
		return false
	}
	quota, ok := quotaForProfile(profile)
	if !ok {
		return false
	}
	target.Reset()
	target.id = id
	target.abi = abi
	target.addressSpace = addressSpace
	target.quota = quota
	target.space.SetLevel(lock.LevelAddressSpace)
	target.handles.SetOwner(uint64(id))
	target.regions.Init(vm.UserAddressStart, vm.UserAddressLimit)
	return true
}

// InitChild prepares target as a fork of parent. The child keeps the parent's
// ABI, quota limits, region map, and name. It gets its own identity and
// address space, an empty handle table, and an account with nothing charged.
func InitChild(target *Process, id ID, parent *Process, addressSpace AddressSpace) bool {
	if target == nil || parent == nil || target == parent || id == 0 || parent.id == 0 || addressSpace.Root == 0 {
		return false
	}
	target.Reset()
	target.id = id
	target.abi = parent.abi
	target.addressSpace = addressSpace
	target.quota.limits = parent.quota.limits
	target.regions.CopyFrom(&parent.regions)
	target.space.SetLevel(lock.LevelAddressSpace)
	target.handles.SetOwner(uint64(id))
	target.name, target.nameSize = parent.name, parent.nameSize
	return true
}

// New allocates and prepares a process; host tests use it, the kernel
// prepares slots in place with Init.
func New(id ID, abi ABI, addressSpace AddressSpace) (*Process, bool) {
	created := new(Process)
	ok := Init(created, id, abi, addressSpace)
	return created, ok
}

func (process *Process) ID() ID { return process.id }

// ABI reports the immutable userspace ABI selected at validated launch.
func (process *Process) ABI() ABI { return process.abi }

func (abi ABI) Valid() bool { return abi == ABINative || abi == ABILinuxX8664 }

// SetName records a display name, truncated to 16 bytes. It takes bytes
// because the launch path names a process from the requested path, which the
// kernel must not turn into a string.
func (process *Process) SetName(name []byte) {
	process.nameSize = uint8(copy(process.name[:], name))
}

func (process *Process) Name() []byte { return process.name[:process.nameSize] }

func (process *Process) AddressSpace() AddressSpace { return process.addressSpace }

// ReplaceAddressSpace commits a prepared address space and region map without
// changing process identity, ABI, handles, tasks, or resource profile.
func (process *Process) ReplaceAddressSpace(addressSpace AddressSpace, regions *region.Map) bool {
	if process == nil || addressSpace.Root == 0 || regions == nil {
		return false
	}
	process.addressSpace = addressSpace
	process.regions.CopyFrom(regions)
	return true
}

func (process *Process) Handles() *handle.Table { return &process.handles }

// Executable is the executable object the process reads its image from.
func (process *Process) Executable() uint64 { return process.executable }

// SetExecutable records the executable object the process was started from.
func (process *Process) SetExecutable(id uint64) { process.executable = id }

// Space is the lock of the process's address space: its region map and
// page tables. Copies and faults read; mapping changes write.
func (process *Process) Space() *lock.RW { return &process.space }

// Regions is the process's region map.
func (process *Process) Regions() *region.Map { return &process.regions }

// Threads is the process's native thread group.
func (process *Process) Threads() *nativethread.Group { return &process.threads }

func (process *Process) AddTask(id sched.TaskID) bool {
	guard.CheckWriter()
	if !process.ReserveTask() {
		return false
	}
	if process.CommitReservedTask(id) {
		return true
	}
	process.CancelReservedTask()
	return false
}

// ReserveTask charges one task slot before architecture state becomes
// runnable. The caller either commits the returned slot or cancels it.
func (process *Process) ReserveTask() bool {
	guard.CheckWriter()
	if process == nil || process.taskCount+int(process.taskReserved) == len(process.tasks) || !process.Charge(ResourceTasks, 1) {
		return false
	}
	process.taskReserved++
	return true
}

// CommitReservedTask binds a prepared architecture task without another
// allocation or quota decision.
func (process *Process) CommitReservedTask(id sched.TaskID) bool {
	guard.CheckWriter()
	if process == nil || process.taskReserved == 0 {
		return false
	}
	if _, _, valid := id.Parts(); !valid || process.taskCount == len(process.tasks) {
		return false
	}
	for index := 0; index < process.taskCount; index++ {
		if process.tasks[index] == id {
			return false
		}
	}
	process.tasks[process.taskCount] = id
	process.taskCount++
	process.taskReserved--
	return true
}

// CancelReservedTask returns one uncommitted task charge.
func (process *Process) CancelReservedTask() bool {
	guard.CheckWriter()
	if process == nil || process.taskReserved == 0 {
		return false
	}
	process.taskReserved--
	process.Release(ResourceTasks, 1)
	return true
}

func (process *Process) TaskCount() int { return process.taskCount }

func (process *Process) Tasks() []sched.TaskID { return process.tasks[:process.taskCount] }

func (process *Process) OwnsTask(id sched.TaskID) bool {
	for index := 0; index < process.taskCount; index++ {
		if process.tasks[index] == id {
			return true
		}
	}
	return false
}

// DetachTask removes a task that no longer belongs to the program the
// process runs, as exec ends the other threads of the image it replaced. The
// task's exit then neither ends the process nor gives it an exit code.
func (process *Process) DetachTask(id sched.TaskID) bool { return process.removeTask(id) }

func (process *Process) removeTask(id sched.TaskID) bool {
	guard.CheckWriter()
	for index := 0; index < process.taskCount; index++ {
		if process.tasks[index] != id {
			continue
		}
		process.taskCount--
		process.tasks[index] = process.tasks[process.taskCount]
		process.tasks[process.taskCount] = 0
		process.Release(ResourceTasks, 1)
		return true
	}
	return false
}

type ExitEvent struct {
	Process ID
	Task    sched.TaskID
	Code    uint64
}

type WaitStatus uint8

const (
	WaitNotFound WaitStatus = iota
	WaitRunning
	WaitExited
)

// ExitReport states what ReportTaskExit did with one exited task.
type ExitReport uint8

const (
	// ExitUnknownTask means no live process owned the task.
	ExitUnknownTask ExitReport = iota
	// ExitTaskRemoved means the owning process still has other tasks.
	ExitTaskRemoved
	// ExitProcessEnded means the last task exited and the returned event was
	// recorded as the process completion.
	ExitProcessEnded
)

type Registry struct {
	processes     [MaxProcesses]*Process
	count         uint8
	completed     [MaxExitEvents]ExitEvent
	completedNext uint8
	// owners maps each task slot to the process its task runs in. Entries
	// change under the registry's lock, and a slot's entry only while its
	// task is not running, so a task reads its own entry with no lock.
	owners [sched.MaxTasks]owner
}

type owner struct {
	task    sched.TaskID
	process *Process
}

// NoteOwner records the process a new task runs in. BindTask records its
// own; a thread a process creates for itself is recorded here.
func (registry *Registry) NoteOwner(task sched.TaskID, process *Process) bool {
	guard.CheckWriter()
	slot, _, valid := task.Parts()
	if !valid || int(slot) >= len(registry.owners) || process == nil {
		return false
	}
	registry.owners[slot] = owner{task: task, process: process}
	return true
}

// ForgetOwner clears the entry of a task that has exited.
func (registry *Registry) ForgetOwner(task sched.TaskID) {
	guard.CheckWriter()
	if slot, _, valid := task.Parts(); valid && int(slot) < len(registry.owners) && registry.owners[slot].task == task {
		registry.owners[slot] = owner{}
	}
}

// Owner returns the process task runs in. The calling task may ask for
// itself without the registry's lock: nothing changes its entry while it
// runs.
func (registry *Registry) Owner(task sched.TaskID) (*Process, bool) {
	slot, _, valid := task.Parts()
	if !valid || int(slot) >= len(registry.owners) {
		return nil, false
	}
	entry := registry.owners[slot]
	if entry.task != task || entry.process == nil {
		return nil, false
	}
	return entry.process, true
}

func (registry *Registry) Add(process *Process) bool {
	guard.CheckWriter()
	if process == nil || process.ID() == 0 || registry.count == MaxProcesses {
		return false
	}
	for index := 0; index < int(registry.count); index++ {
		if registry.processes[index] == process || registry.processes[index].ID() == process.ID() {
			return false
		}
	}
	registry.processes[registry.count] = process
	registry.count++
	// Until now only its creator could reach the process. From here other
	// processors can, so its region map checks for its lock.
	if guard.Installed() {
		process.regions.Guard(&process.space)
	}
	return true
}

// GuardSpaces makes the region map of every registered process check for its
// lock. Boot registers processes before a second processor runs, and calls
// it once the locks are in force.
func (registry *Registry) GuardSpaces() {
	for index := 0; index < int(registry.count); index++ {
		registry.processes[index].regions.Guard(&registry.processes[index].space)
		registry.processes[index].handles.Guard()
	}
}

func (registry *Registry) BindTask(id ID, task sched.TaskID) bool {
	guard.CheckWriter()
	if task == 0 {
		return false
	}
	for index := 0; index < int(registry.count); index++ {
		if registry.processes[index].OwnsTask(task) {
			return false
		}
	}
	for index := 0; index < int(registry.count); index++ {
		if registry.processes[index].ID() == id {
			if !registry.processes[index].AddTask(task) {
				return false
			}
			// Until now only the call creating the process, which holds the
			// registry for writing, could reach its handle table; its task
			// will, so the table checks for its lock from here.
			if guard.Installed() {
				registry.processes[index].handles.Guard()
			}
			return registry.NoteOwner(task, registry.processes[index])
		}
	}
	return false
}

// ReportTaskExit removes an exited task from its owning process and, when that
// was the process's last task, records the completion and returns it. The
// record is written unconditionally: callers perform teardown that can fail,
// and a waiter must still be able to observe that the process ended.
func (registry *Registry) ReportTaskExit(task sched.TaskID, code uint64) (ExitEvent, ExitReport) {
	guard.CheckWriter()
	for index := 0; index < int(registry.count); index++ {
		owner := registry.processes[index]
		if !owner.OwnsTask(task) {
			continue
		}
		if !owner.removeTask(task) {
			return ExitEvent{}, ExitUnknownTask
		}
		if owner.TaskCount() != 0 {
			return ExitEvent{}, ExitTaskRemoved
		}
		event := ExitEvent{Process: owner.ID(), Task: task, Code: code}
		registry.completed[registry.completedNext] = event
		registry.completedNext = (registry.completedNext + 1) % MaxExitEvents
		return event, ExitProcessEnded
	}
	return ExitEvent{}, ExitUnknownTask
}

// Wait observes bounded process completion state without consuming lifecycle
// delivery. Recent exit records remain queryable after registry removal.
func (registry *Registry) Wait(id ID) (ExitEvent, WaitStatus) {
	guard.Check()
	if id == 0 {
		return ExitEvent{}, WaitNotFound
	}
	for index := range registry.completed {
		if registry.completed[index].Process == id {
			return registry.completed[index], WaitExited
		}
	}
	if process, found := registry.ByID(id); found && process.TaskCount() != 0 {
		return ExitEvent{}, WaitRunning
	}
	return ExitEvent{}, WaitNotFound
}

// ByTask finds the process owning a task, which teardown needs after the task
// has stopped running but before it is removed from its process.
func (registry *Registry) ByTask(task sched.TaskID) (*Process, bool) {
	guard.Check()
	for index := 0; index < int(registry.count); index++ {
		if registry.processes[index].OwnsTask(task) {
			return registry.processes[index], true
		}
	}
	return nil, false
}

func (registry *Registry) ByRoot(root uint64) (*Process, bool) {
	guard.Check()
	if root == 0 {
		return nil, false
	}
	for index := 0; index < int(registry.count); index++ {
		if registry.processes[index].AddressSpace().Root == root {
			return registry.processes[index], true
		}
	}
	return nil, false
}

// AllowsABI reports whether root belongs to a live process with the requested
// userspace ABI. Entry gates use it before decoding any ABI-specific request.
func (registry *Registry) AllowsABI(root uint64, abi ABI) bool {
	guard.Check()
	process, found := registry.ByRoot(root)
	return found && abi.Valid() && process.ABI() == abi
}

// EndpointHolder returns a process that holds a handle to the channel
// endpoint object, from the handle tables' holder index.
func (registry *Registry) EndpointHolder(object uint64) (*Process, bool) {
	guard.Check()
	holder, held := handle.Holder(object)
	if !held {
		return nil, false
	}
	return registry.ByID(ID(holder))
}

func (registry *Registry) ByID(id ID) (*Process, bool) {
	guard.Check()
	for index := 0; index < int(registry.count); index++ {
		if registry.processes[index].ID() == id {
			return registry.processes[index], true
		}
	}
	return nil, false
}

func (registry *Registry) Count() int { return int(registry.count) }

func (registry *Registry) At(index int) (*Process, bool) {
	guard.Check()
	if index < 0 || index >= int(registry.count) {
		return nil, false
	}
	return registry.processes[index], true
}

func (registry *Registry) Remove(id ID) bool {
	guard.CheckWriter()
	for index := 0; index < int(registry.count); index++ {
		if registry.processes[index].ID() != id {
			continue
		}
		registry.count--
		registry.processes[index] = registry.processes[registry.count]
		registry.processes[registry.count] = nil
		return true
	}
	return false
}

func (process *Process) Reset() { *process = Process{} }
