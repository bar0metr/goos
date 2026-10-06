package linux

import (
	"goos/kernel/handle"
	"goos/kernel/process"
	"goos/kernel/sched"
)

// Thread is a Linux thread other than the first of its process: a task that
// shares the process's address space, descriptors, and signal handlers and
// has a TID of its own. The first thread's TID is the PID, and its
// clear-child-TID lives in ProcessState.
type Thread struct {
	Task sched.TaskID
	Root uint64
	TID  int32
	// ClearChildTID is the address the kernel zeroes and wakes when the
	// thread ends, so a joiner sleeping on it returns.
	ClearChildTID uint64
	// IOBuffer is the thread's own I/O object, created on its first file
	// read or write.
	IOBuffer handle.ID
	// Mask is the thread's blocked signal mask while another thread's is
	// in force.
	Mask     ThreadMask
	reserved bool
}

// threadReservation owns one compatibility thread slot and TID until the
// scheduler task is ready to publish.
type threadReservation struct {
	slot uint8
	tid  int32
	root uint64
}

// ThreadCreation holds the Process charge, TID, and compatibility slot until
// the architecture task exists.
type ThreadCreation struct {
	owner       *process.Process
	reservation threadReservation
	active      bool
}

var threads [sched.MaxTasks]Thread

func allocateTIDHeld() int32 { return allocatePID() }

func threadSlotFreeHeld() bool {
	guard.Check()
	for index := range threads {
		if threads[index].Root == 0 {
			return true
		}
	}
	return false
}

func addThreadHeld(task sched.TaskID, root uint64, tid int32) (*Thread, bool) {
	guard.Check()
	if task == 0 || root == 0 || tid < firstPID {
		return nil, false
	}
	if _, found := findProcessHeld(root); !found {
		return nil, false
	}
	free := -1
	for index := range threads {
		if threads[index].Task == task {
			return nil, false
		}
		if free == -1 && threads[index].Root == 0 {
			free = index
		}
	}
	if free == -1 {
		return nil, false
	}
	threads[free] = Thread{Task: task, Root: root, TID: tid}
	return &threads[free], true
}

// reserveThread takes a TID and compatibility slot before task creation. A
// failed creator must cancel the reservation.
func reserveThread(root uint64) (threadReservation, bool) {
	if root == 0 {
		return threadReservation{}, false
	}
	if _, found := findProcessHeld(root); !found {
		return threadReservation{}, false
	}
	for index := range threads {
		if threads[index].Root != 0 {
			continue
		}
		tid := allocateTIDHeld()
		threads[index] = Thread{Root: root, TID: tid, reserved: true}
		return threadReservation{slot: uint8(index + 1), tid: tid, root: root}, true
	}
	return threadReservation{}, false
}

// preparedThread returns the reserved record so the creator can fill fields
// that cannot fail before it publishes the scheduler task.
func preparedThread(reservation threadReservation) (*Thread, bool) {
	index := int(reservation.slot) - 1
	if index < 0 || index >= len(threads) {
		return nil, false
	}
	thread := &threads[index]
	if !thread.reserved || thread.Root != reservation.root || thread.TID != reservation.tid {
		return nil, false
	}
	return thread, true
}

// commitThread attaches a prepared task to its reserved record.
func commitThread(reservation threadReservation, task sched.TaskID) (*Thread, bool) {
	thread, found := preparedThread(reservation)
	if !found || task == 0 {
		return nil, false
	}
	if _, _, valid := task.Parts(); !valid {
		return nil, false
	}
	for index := range threads {
		if threads[index].Task == task {
			return nil, false
		}
	}
	thread.Task = task
	thread.reserved = false
	return thread, true
}

// cancelThread releases a reservation whose scheduler task was not created.
func cancelThread(reservation threadReservation) bool {
	thread, found := preparedThread(reservation)
	if !found {
		return false
	}
	*thread = Thread{}
	return true
}

func prepareThreadCreationHeld(owner *process.Process, root uint64) (ThreadCreation, bool) {
	guard.Check()
	if owner == nil || !owner.ReserveTask() {
		return ThreadCreation{}, false
	}
	reservation, reserved := reserveThread(root)
	if !reserved {
		owner.CancelReservedTask()
		return ThreadCreation{}, false
	}
	return ThreadCreation{owner: owner, reservation: reservation, active: true}, true
}

func (creation *ThreadCreation) preparedThreadHeld() (*Thread, bool) {
	guard.Check()
	if creation == nil || !creation.active {
		return nil, false
	}
	return preparedThread(creation.reservation)
}

func (creation *ThreadCreation) cancelHeld() bool {
	guard.Check()
	if creation == nil || !creation.active {
		return false
	}
	threadCanceled := cancelThread(creation.reservation)
	taskCanceled := creation.owner.CancelReservedTask()
	creation.active = false
	return threadCanceled && taskCanceled
}

func (creation *ThreadCreation) commitHeld(task sched.TaskID) (*Thread, bool) {
	guard.Check()
	if creation == nil || !creation.active {
		return nil, false
	}
	thread, committed := commitThread(creation.reservation, task)
	if !committed {
		creation.cancelHeld()
		return nil, false
	}
	if !creation.owner.CommitReservedTask(task) {
		removeThreadHeld(task)
		creation.owner.CancelReservedTask()
		creation.active = false
		return nil, false
	}
	creation.active = false
	return thread, true
}

func findThreadHeld(task sched.TaskID) (*Thread, bool) {
	guard.Check()
	if task == 0 {
		return nil, false
	}
	for index := range threads {
		if !threads[index].reserved && threads[index].Task == task {
			return &threads[index], true
		}
	}
	return nil, false
}

func removeThreadHeld(task sched.TaskID) (Thread, bool) {
	guard.Check()
	record, found := findThreadHeld(task)
	if !found {
		return Thread{}, false
	}
	removed := *record
	*record = Thread{}
	return removed, true
}

func threadCountHeld(root uint64) int {
	guard.Check()
	count := 0
	for index := range threads {
		if threads[index].Task != 0 && threads[index].Root == root {
			count++
		}
	}
	return count
}

func threadRecordsHeld(root uint64) int {
	guard.Check()
	count := 0
	for index := range threads {
		if root != 0 && threads[index].Root == root {
			count++
		}
	}
	return count
}

func taskTIDHeld(task sched.TaskID, state *ProcessState) int32 {
	guard.Check()
	if thread, found := findThreadHeld(task); found {
		return thread.TID
	}
	if state == nil {
		return 0
	}
	return state.PID
}

// tidInUse reports whether a live thread holds tid.
func tidInUse(tid int32) bool {
	for index := range threads {
		if threads[index].Root != 0 && threads[index].TID == tid {
			return true
		}
	}
	return false
}

// forgetThreads drops every thread record of the process at root, which has
// ended.
func forgetThreads(root uint64) {
	for index := range threads {
		if threads[index].Root == root {
			threads[index] = Thread{}
		}
	}
}

func forgetThreadsOfHeld(root uint64) { forgetThreads(root) }

func threadGroupOfHeld(tid int32) (int32, bool) {
	guard.Check()
	for index := range threads {
		if threads[index].Task == 0 || threads[index].TID != tid {
			continue
		}
		if state, found := findProcessHeld(threads[index].Root); found {
			return state.PID, true
		}
	}
	return 0, false
}

func selectThreadSignalsHeld(state *ProcessState, task sched.TaskID) {
	guard.Check()
	if state == nil {
		return
	}
	tid := taskTIDHeld(task, state)
	current := state.maskTID
	if current == 0 {
		current = state.PID
	}
	if current == tid {
		return
	}
	saved := state.Signals.SaveThreadMask()
	if current == state.PID {
		state.firstMask = saved
	} else if owner, found := threadByTID(current); found {
		owner.Mask = saved
	}
	if tid == state.PID {
		state.Signals.LoadThreadMask(state.firstMask)
		state.maskTID = 0
		return
	}
	if thread, found := findThreadHeld(task); found {
		state.Signals.LoadThreadMask(thread.Mask)
		state.maskTID = tid
	}
}

func adoptSignalsAsFirstHeld(state *ProcessState) {
	guard.Check()
	if state != nil {
		state.maskTID = 0
	}
}

func threadByTID(tid int32) (*Thread, bool) {
	for index := range threads {
		if threads[index].Task != 0 && threads[index].TID == tid {
			return &threads[index], true
		}
	}
	return nil, false
}

// AllocateTID takes a TID from the PID space, distinct from every live PID
// and TID. The caller records it with AddThread before allocating another.
func AllocateTID() int32 {
	tablesLock.Lock()
	result := allocateTIDHeld()
	tablesLock.Unlock()
	return result
}

// ThreadSlotFree reports whether the table can take another thread.
func ThreadSlotFree() bool {
	tablesLock.Lock()
	result := threadSlotFreeHeld()
	tablesLock.Unlock()
	return result
}

// AddThread records a new thread of the process at root with a TID from
// AllocateTID.
func AddThread(task sched.TaskID, root uint64, tid int32) (*Thread, bool) {
	tablesLock.Lock()
	result0, result1 := addThreadHeld(task, root, tid)
	tablesLock.Unlock()
	return result0, result1
}

// PrepareThreadCreation reserves every non-architecture resource that clone
// needs. Failure returns the Process quota to its previous value.
func PrepareThreadCreation(owner *process.Process, root uint64) (ThreadCreation, bool) {
	tablesLock.Lock()
	result0, result1 := prepareThreadCreationHeld(owner, root)
	tablesLock.Unlock()
	return result0, result1
}

func (creation *ThreadCreation) PreparedThread() (*Thread, bool) {
	tablesLock.Lock()
	result0, result1 := creation.preparedThreadHeld()
	tablesLock.Unlock()
	return result0, result1
}

// Cancel releases a prepared creation after architecture setup fails.
func (creation *ThreadCreation) Cancel() bool {
	tablesLock.Lock()
	result := creation.cancelHeld()
	tablesLock.Unlock()
	return result
}

// Commit binds the scheduler task to both reservations. A rejected task
// returns the transaction to its initial state.
func (creation *ThreadCreation) Commit(task sched.TaskID) (*Thread, bool) {
	tablesLock.Lock()
	result0, result1 := creation.commitHeld(task)
	tablesLock.Unlock()
	return result0, result1
}

// FindThread returns the thread record of task, if task is a thread
// created by clone.
func FindThread(task sched.TaskID) (*Thread, bool) {
	tablesLock.Lock()
	result0, result1 := findThreadHeld(task)
	tablesLock.Unlock()
	return result0, result1
}

// RemoveThread forgets the thread of task and returns what it recorded.
func RemoveThread(task sched.TaskID) (Thread, bool) {
	tablesLock.Lock()
	result0, result1 := removeThreadHeld(task)
	tablesLock.Unlock()
	return result0, result1
}

// ThreadCount reports how many clone-created threads the process at root
// has.
func ThreadCount(root uint64) int {
	tablesLock.Lock()
	result := threadCountHeld(root)
	tablesLock.Unlock()
	return result
}

// ThreadRecords counts the records the process at root holds, reserved or
// committed.
func ThreadRecords(root uint64) int {
	tablesLock.Lock()
	result := threadRecordsHeld(root)
	tablesLock.Unlock()
	return result
}

// TaskTID is the TID task sees from gettid: its thread's, or the PID for
// the first thread.
func TaskTID(task sched.TaskID, state *ProcessState) int32 {
	tablesLock.Lock()
	result := taskTIDHeld(task, state)
	tablesLock.Unlock()
	return result
}

// ForgetThreadsOf is forgetThreads for the kernel's exec path: the old
// image's threads have been ended and must not touch the new one.
func ForgetThreadsOf(root uint64) {
	tablesLock.Lock()
	forgetThreadsOfHeld(root)
	tablesLock.Unlock()
}

// ThreadGroupOf returns the PID of the process whose thread holds tid. A
// signal aimed at a thread goes to its process, whose signal state all its
// threads share.
func ThreadGroupOf(tid int32) (int32, bool) {
	tablesLock.Lock()
	result0, result1 := threadGroupOfHeld(tid)
	tablesLock.Unlock()
	return result0, result1
}

// SelectThreadSignals puts the signal mask of task in force in its process's
// state, saving the mask of the thread that had it. The kernel calls it as
// each Linux system call begins, so a call reads and changes its own
// thread's mask and a signal is delivered at its return only if that
// thread's mask allows. A thread that has ended gives its mask up.
func SelectThreadSignals(state *ProcessState, task sched.TaskID) {
	tablesLock.Lock()
	selectThreadSignalsHeld(state, task)
	tablesLock.Unlock()
}

// AdoptSignalsAsFirst makes the mask in force the first thread's, for a
// thread exec makes the process's only one.
func AdoptSignalsAsFirst(state *ProcessState) {
	tablesLock.Lock()
	adoptSignalsAsFirstHeld(state)
	tablesLock.Unlock()
}
