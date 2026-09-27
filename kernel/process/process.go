package process

import (
	"goos/kernel/handle"
	"goos/kernel/mm/region"
	"goos/kernel/mm/vm"
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

type AddressSpace struct {
	Root uint64
}

type Process struct {
	id           ID
	addressSpace AddressSpace
	handles      handle.Table
	tasks        [MaxTasksPerProcess]sched.TaskID
	taskCount    int
	quota        Quota
	// regions is the process's virtual-memory region map (ADR 0104).
	regions region.Map
	// executable is the executable object the process was started from and
	// reads its pages from, or zero (ADR 0106).
	executable uint64
	cpuTicks   uint64
	name       [16]byte
	nameSize   uint8
}

// Init prepares a process slot in place; see stream.Stream.Reset. Returning a
// Process by value instead would build the whole handle table as a temporary,
// which the kernel's leaking heap never frees.
func Init(target *Process, id ID, addressSpace AddressSpace) bool {
	if target == nil || id == 0 || addressSpace.Root == 0 {
		return false
	}
	target.Reset()
	target.id = id
	target.addressSpace = addressSpace
	target.quota = defaultQuota()
	target.regions.Init(vm.UserAddressStart, vm.UserAddressLimit)
	return true
}

func New(id ID, addressSpace AddressSpace) (Process, bool) {
	var created Process
	ok := Init(&created, id, addressSpace)
	return created, ok
}

func (process *Process) ID() ID { return process.id }

// SetName records a display name, truncated to 16 bytes. It takes bytes
// because the launch path names a process from the requested path, which the
// kernel must not turn into a string.
func (process *Process) SetName(name []byte) {
	process.nameSize = uint8(copy(process.name[:], name))
}

func (process *Process) Name() []byte { return process.name[:process.nameSize] }

func (process *Process) AddressSpace() AddressSpace { return process.addressSpace }

func (process *Process) Handles() *handle.Table { return &process.handles }

// Executable is the executable object the process reads its image from.
func (process *Process) Executable() uint64 { return process.executable }

// SetExecutable records the executable object the process was started from.
func (process *Process) SetExecutable(id uint64) { process.executable = id }

// Regions is the process's region map.
func (process *Process) Regions() *region.Map { return &process.regions }

func (process *Process) AddTask(id sched.TaskID) bool {
	if _, _, valid := id.Parts(); !valid || process.taskCount == len(process.tasks) {
		return false
	}
	for index := 0; index < process.taskCount; index++ {
		if process.tasks[index] == id {
			return false
		}
	}
	if !process.Charge(ResourceTasks, 1) {
		return false
	}
	process.tasks[process.taskCount] = id
	process.taskCount++
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

func (process *Process) removeTask(id sched.TaskID) bool {
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
}

func (registry *Registry) Add(process *Process) bool {
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
	return true
}

func (registry *Registry) BindTask(id ID, task sched.TaskID) bool {
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
			return registry.processes[index].AddTask(task)
		}
	}
	return false
}

// ReportTaskExit removes an exited task from its owning process and, when that
// was the process's last task, records the completion and returns it. The
// record is written unconditionally: callers perform teardown that can fail,
// and a waiter must still be able to observe that the process ended.
func (registry *Registry) ReportTaskExit(task sched.TaskID, code uint64) (ExitEvent, ExitReport) {
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
	for index := 0; index < int(registry.count); index++ {
		if registry.processes[index].OwnsTask(task) {
			return registry.processes[index], true
		}
	}
	return nil, false
}

func (registry *Registry) ByRoot(root uint64) (*Process, bool) {
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

func (registry *Registry) ByCapabilityObject(typeID handle.Type, object uint64) (*Process, bool) {
	for index := 0; index < int(registry.count); index++ {
		if registry.processes[index].Handles().HasObject(typeID, object) {
			return registry.processes[index], true
		}
	}
	return nil, false
}

func (registry *Registry) ByID(id ID) (*Process, bool) {
	for index := 0; index < int(registry.count); index++ {
		if registry.processes[index].ID() == id {
			return registry.processes[index], true
		}
	}
	return nil, false
}

func (registry *Registry) Count() int { return int(registry.count) }

func (registry *Registry) At(index int) (*Process, bool) {
	if index < 0 || index >= int(registry.count) {
		return nil, false
	}
	return registry.processes[index], true
}

func (registry *Registry) Remove(id ID) bool {
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
