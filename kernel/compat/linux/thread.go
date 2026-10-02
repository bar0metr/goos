package linux

import (
	"goos/kernel/handle"
	"goos/kernel/sched"
)

// Thread is a Linux thread other than the first of its process: a task that
// shares the process's address space, descriptors, and signal handlers and
// has a TID of its own. The first thread's TID is the PID, and its
// clear-child-TID and robust list live in ProcessState.
type Thread struct {
	Task sched.TaskID
	Root uint64
	TID  int32
	// ClearChildTID is the address the kernel zeroes and wakes when the
	// thread ends, so a joiner sleeping on it returns.
	ClearChildTID uint64
	RobustList    uint64
	// IOBuffer is the thread's own I/O object, created on its first file
	// read or write.
	IOBuffer handle.ID
	// Mask is the thread's blocked signal mask while another thread's is
	// in force.
	Mask ThreadMask
}

var threads [sched.MaxTasks]Thread

// AllocateTID takes a TID from the PID space, distinct from every live PID
// and TID. The caller records it with AddThread before allocating another.
func AllocateTID() int32 { return allocatePID() }

// ThreadSlotFree reports whether the table can take another thread.
func ThreadSlotFree() bool {
	for index := range threads {
		if threads[index].Task == 0 {
			return true
		}
	}
	return false
}

// AddThread records a new thread of the process at root with a TID from
// AllocateTID.
func AddThread(task sched.TaskID, root uint64, tid int32) (*Thread, bool) {
	if task == 0 || root == 0 || tid < firstPID {
		return nil, false
	}
	if _, found := FindProcess(root); !found {
		return nil, false
	}
	free := -1
	for index := range threads {
		if threads[index].Task == task {
			return nil, false
		}
		if free == -1 && threads[index].Task == 0 {
			free = index
		}
	}
	if free == -1 {
		return nil, false
	}
	threads[free] = Thread{Task: task, Root: root, TID: tid}
	return &threads[free], true
}

// FindThread returns the thread record of task, if task is a thread
// created by clone.
func FindThread(task sched.TaskID) (*Thread, bool) {
	if task == 0 {
		return nil, false
	}
	for index := range threads {
		if threads[index].Task == task {
			return &threads[index], true
		}
	}
	return nil, false
}

// RemoveThread forgets the thread of task and returns what it recorded.
func RemoveThread(task sched.TaskID) (Thread, bool) {
	record, found := FindThread(task)
	if !found {
		return Thread{}, false
	}
	removed := *record
	*record = Thread{}
	return removed, true
}

// ThreadCount reports how many clone-created threads the process at root
// has.
func ThreadCount(root uint64) int {
	count := 0
	for index := range threads {
		if threads[index].Task != 0 && threads[index].Root == root {
			count++
		}
	}
	return count
}

// TaskTID is the TID task sees from gettid: its thread's, or the PID for
// the first thread.
func TaskTID(task sched.TaskID, state *ProcessState) int32 {
	if thread, found := FindThread(task); found {
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
		if threads[index].Task != 0 && threads[index].TID == tid {
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

// ForgetThreadsOf is forgetThreads for the kernel's exec path: the old
// image's threads have been ended and must not touch the new one.
func ForgetThreadsOf(root uint64) { forgetThreads(root) }

// ThreadGroupOf returns the PID of the process whose thread holds tid. A
// signal aimed at a thread goes to its process, whose signal state all its
// threads share.
func ThreadGroupOf(tid int32) (int32, bool) {
	for index := range threads {
		if threads[index].Task == 0 || threads[index].TID != tid {
			continue
		}
		if state, found := FindProcess(threads[index].Root); found {
			return state.PID, true
		}
	}
	return 0, false
}

// SelectThreadSignals puts the signal mask of task in force in its process's
// state, saving the mask of the thread that had it. The kernel calls it as
// each Linux system call begins, so a call reads and changes its own
// thread's mask and a signal is delivered at its return only if that
// thread's mask allows. A thread that has ended gives its mask up.
func SelectThreadSignals(state *ProcessState, task sched.TaskID) {
	if state == nil {
		return
	}
	tid := TaskTID(task, state)
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
	if thread, found := FindThread(task); found {
		state.Signals.LoadThreadMask(thread.Mask)
		state.maskTID = tid
	}
}

// AdoptSignalsAsFirst makes the mask in force the first thread's, for a
// thread exec makes the process's only one.
func AdoptSignalsAsFirst(state *ProcessState) {
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
