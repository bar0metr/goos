package sched

const MaxTasks = 64

type TaskID uint64

type State uint8

const (
	TaskCreated State = iota
	TaskRunnable
	TaskRunning
	TaskBlocked
	TaskDead
)

type Context struct {
	StackPointer uint64
}

type Task struct {
	ID           TaskID
	ProcessID    uint64
	AddressSpace uint64
	State        State
	Context      Context
}

// Scheduler is a bounded single-CPU round-robin run queue. IRQ-level locking
// is provided by the architecture layer until SMP introduces per-CPU queues.
type Scheduler struct {
	tasks   [MaxTasks]Task
	count   int
	current int
	nextID  TaskID
}

func (scheduler *Scheduler) Add(context Context) (TaskID, bool) {
	return scheduler.AddOwned(context, 0, 0)
}

// AddOwned associates a userspace task with its process and address-space
// root. Kernel tasks use zero for both ownership fields.
func (scheduler *Scheduler) AddOwned(context Context, processID uint64, addressSpace uint64) (TaskID, bool) {
	if context.StackPointer == 0 || scheduler.count == len(scheduler.tasks) {
		return 0, false
	}
	if (processID == 0) != (addressSpace == 0) {
		return 0, false
	}
	scheduler.nextID++
	task := Task{ID: scheduler.nextID, ProcessID: processID, AddressSpace: addressSpace, State: TaskRunnable, Context: context}
	scheduler.tasks[scheduler.count] = task
	scheduler.count++
	return task.ID, true
}

func (scheduler *Scheduler) Start() (Task, bool) {
	if scheduler.count == 0 {
		return Task{}, false
	}
	scheduler.current = 0
	scheduler.tasks[0].State = TaskRunning
	return scheduler.tasks[0], true
}

func (scheduler *Scheduler) Next() (Task, bool) {
	if scheduler.count == 0 {
		return Task{}, false
	}
	if scheduler.tasks[scheduler.current].State == TaskRunning {
		scheduler.tasks[scheduler.current].State = TaskRunnable
	}
	for checked := 0; checked < scheduler.count; checked++ {
		candidate := (scheduler.current + checked + 1) % scheduler.count
		if scheduler.tasks[candidate].State == TaskRunnable {
			scheduler.current = candidate
			scheduler.tasks[candidate].State = TaskRunning
			return scheduler.tasks[candidate], true
		}
	}
	return Task{}, false
}

func (scheduler *Scheduler) SetState(id TaskID, state State) bool {
	for index := 0; index < scheduler.count; index++ {
		if scheduler.tasks[index].ID == id {
			scheduler.tasks[index].State = state
			return true
		}
	}
	return false
}

func (scheduler *Scheduler) Block(id TaskID) bool {
	return scheduler.transition(id, TaskBlocked, TaskRunnable, TaskRunning)
}

func (scheduler *Scheduler) Wake(id TaskID) bool {
	return scheduler.transition(id, TaskRunnable, TaskBlocked)
}

func (scheduler *Scheduler) Exit(id TaskID) bool {
	return scheduler.transition(id, TaskDead, TaskCreated, TaskRunnable, TaskRunning, TaskBlocked)
}

func (scheduler *Scheduler) transition(id TaskID, target State, allowed ...State) bool {
	for index := 0; index < scheduler.count; index++ {
		if scheduler.tasks[index].ID != id {
			continue
		}
		for _, state := range allowed {
			if scheduler.tasks[index].State == state {
				scheduler.tasks[index].State = target
				return true
			}
		}
		return false
	}
	return false
}

func (scheduler *Scheduler) Count() int { return scheduler.count }
