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
	ID      TaskID
	State   State
	Context Context
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
	if context.StackPointer == 0 || scheduler.count == len(scheduler.tasks) {
		return 0, false
	}
	scheduler.nextID++
	task := Task{ID: scheduler.nextID, State: TaskRunnable, Context: context}
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

func (scheduler *Scheduler) Count() int { return scheduler.count }
