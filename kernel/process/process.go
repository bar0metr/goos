package process

import (
	"goos/kernel/handle"
	"goos/kernel/sched"
)

const MaxTasksPerProcess = 32

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
