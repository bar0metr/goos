package process

import (
	"goos/kernel/handle"
	"goos/kernel/sched"
)

const (
	MaxTasksPerProcess = 32
	MaxProcesses       = 32
	MaxExitEvents      = 32
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
	name         [16]byte
	nameSize     uint8
}

func New(id ID, addressSpace AddressSpace) (Process, bool) {
	if id == 0 || addressSpace.Root == 0 {
		return Process{}, false
	}
	return Process{id: id, addressSpace: addressSpace}, true
}

func (process *Process) ID() ID { return process.id }

// SetName records a display name, truncated to 16 bytes.
func (process *Process) SetName(name string) {
	process.nameSize = uint8(copy(process.name[:], name))
}

func (process *Process) Name() []byte { return process.name[:process.nameSize] }

func (process *Process) AddressSpace() AddressSpace { return process.addressSpace }

func (process *Process) Handles() *handle.Table { return &process.handles }

func (process *Process) AddTask(id sched.TaskID) bool {
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
