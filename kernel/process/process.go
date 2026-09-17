package process

import (
	"goos/kernel/handle"
	"goos/kernel/sched"
)

const (
	MaxTasksPerProcess = 32
	MaxProcesses       = 32
	MaxExitEvents      = 32
	MaxStartupData     = 128
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
	startup      [MaxStartupData]byte
	startupSize  uint8
}

func New(id ID, addressSpace AddressSpace) (Process, bool) {
	return NewWithStartup(id, addressSpace, nil)
}

func NewWithStartup(id ID, addressSpace AddressSpace, startup []byte) (Process, bool) {
	if id == 0 || addressSpace.Root == 0 {
		return Process{}, false
	}
	if len(startup) > MaxStartupData {
		return Process{}, false
	}
	result := Process{id: id, addressSpace: addressSpace, startupSize: uint8(len(startup))}
	copy(result.startup[:], startup)
	return result, true
}

func (process *Process) ID() ID { return process.id }

func (process *Process) AddressSpace() AddressSpace { return process.addressSpace }

func (process *Process) Handles() *handle.Table { return &process.handles }

func (process *Process) Startup(target []byte) (int, bool) {
	if len(target) < MaxStartupData {
		return 0, false
	}
	for index := range target[:MaxStartupData] {
		target[index] = 0
	}
	copy(target, process.startup[:process.startupSize])
	return int(process.startupSize), true
}

// SetStartup completes process construction before its first task is runnable.
func (process *Process) SetStartup(startup []byte) bool {
	if process == nil || process.id == 0 || process.taskCount != 0 || len(startup) > MaxStartupData {
		return false
	}
	for index := range process.startup {
		process.startup[index] = 0
	}
	copy(process.startup[:], startup)
	process.startupSize = uint8(len(startup))
	return true
}

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

type Registry struct {
	processes     [MaxProcesses]*Process
	count         uint8
	exits         [MaxExitEvents]ExitEvent
	exitHead      uint8
	exitCount     uint8
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

func (registry *Registry) ReportTaskExit(task sched.TaskID, code uint64) bool {
	for index := 0; index < int(registry.count); index++ {
		owner := registry.processes[index]
		if owner.OwnsTask(task) {
			if !owner.removeTask(task) {
				return false
			}
			if owner.TaskCount() != 0 {
				return true
			}
			if registry.exitCount == MaxExitEvents {
				return false
			}
			tail := (int(registry.exitHead) + int(registry.exitCount)) % MaxExitEvents
			registry.exits[tail] = ExitEvent{Process: owner.ID(), Task: task, Code: code}
			registry.completed[registry.completedNext] = registry.exits[tail]
			registry.completedNext = (registry.completedNext + 1) % MaxExitEvents
			registry.exitCount++
			return true
		}
	}
	return false
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

func (registry *Registry) NextExit() (ExitEvent, bool) {
	if registry.exitCount == 0 {
		return ExitEvent{}, false
	}
	event := registry.exits[registry.exitHead]
	registry.exits[registry.exitHead] = ExitEvent{}
	registry.exitHead = (registry.exitHead + 1) % MaxExitEvents
	registry.exitCount--
	return event, true
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
