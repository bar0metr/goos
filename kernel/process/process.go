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
}

func New(id ID, addressSpace AddressSpace) (Process, bool) {
	if id == 0 || addressSpace.Root == 0 {
		return Process{}, false
	}
	return Process{id: id, addressSpace: addressSpace}, true
}

func (process *Process) ID() ID { return process.id }

func (process *Process) AddressSpace() AddressSpace { return process.addressSpace }

func (process *Process) Handles() *handle.Table { return &process.handles }

func (process *Process) AddTask(id sched.TaskID) bool {
	if id == 0 || process.taskCount == len(process.tasks) {
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

type Registry struct {
	processes [MaxProcesses]*Process
	count     uint8
	exits     [MaxExitEvents]ExitEvent
	exitHead  uint8
	exitCount uint8
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
			registry.exitCount++
			return true
		}
	}
	return false
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
