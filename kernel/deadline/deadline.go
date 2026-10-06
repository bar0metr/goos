// Package deadline keeps the kernel's wait deadlines in order, so the
// housekeeping task finds the next one without looking at every waiter
// (ADR 0189).
package deadline

import (
	"sync/atomic"

	"goos/kernel/lock"
	"goos/kernel/sched"
)

// Source names the table a deadline belongs to.
type Source uint8

const (
	// SourceChannel and SourceStream are the channel and stream wait sets,
	// SourceProcess the process wait set, SourceWaitAny the multi-object
	// waits, and SourcePaging the retries of executable page requests.
	SourceChannel Source = iota + 1
	SourceStream
	SourceProcess
	SourceWaitAny
	SourcePaging
	sourceCount = int(SourcePaging)
)

// Capacity bounds the index: a task waits in each table at most once, so
// one entry per source and task slot is all it can need.
const Capacity = sourceCount * sched.MaxTasks

// Never is the deadline of a wait without one; it is not indexed.
const Never = ^uint64(0)

// Entry is one deadline. Task carries its slot's generation, so an entry
// left behind by a task that ended never names the slot's next task.
type Entry struct {
	At     uint64
	Source Source
	Task   sched.TaskID
}

// Index is a fixed-capacity min-heap of deadlines ordered by time, with each
// entry's heap position kept per source and slot. Inserting, replacing, and
// cancelling cost O(log N); the earliest deadline is O(1) and readable
// without the lock. It allocates nothing. Its lock is taken inside the wait
// tables' locks.
type Index struct {
	lock     lock.Spin
	heap     [Capacity]Entry
	count    int
	position [sourceCount][sched.MaxTasks]int16
	// earliest is the first deadline, or zero when there is none: no
	// indexed deadline is zero.
	earliest atomic.Uint64
}

func (index *Index) take() {
	index.lock.SetLevel(lock.LevelDeadline)
	index.lock.Lock()
}

func (index *Index) release() {
	if index.count == 0 {
		index.earliest.Store(0)
	} else {
		index.earliest.Store(index.heap[0].At)
	}
	index.lock.Unlock()
}

// key returns where an entry's position lives, or false for an invalid
// source or task.
func key(source Source, task sched.TaskID) (int, int, bool) {
	slot, _, valid := task.Parts()
	if !valid || source < SourceChannel || int(source) > sourceCount {
		return 0, 0, false
	}
	return int(source) - 1, int(slot), true
}

// Arm records task's deadline in source, replacing the one it had there.
// A deadline of zero or Never cancels instead.
func (index *Index) Arm(source Source, task sched.TaskID, at uint64) bool {
	table, slot, valid := key(source, task)
	if !valid {
		return false
	}
	if at == 0 || at == Never {
		index.Cancel(source, task)
		return true
	}
	index.take()
	entry := Entry{At: at, Source: source, Task: task}
	if position := int(index.position[table][slot]) - 1; position >= 0 {
		index.heap[position] = entry
		index.fix(position)
	} else {
		position = index.count
		index.count++
		index.heap[position] = entry
		index.position[table][slot] = int16(position + 1)
		index.up(position)
	}
	index.release()
	return true
}

// Cancel removes task's deadline in source. An entry a later task of the
// same slot armed stays.
func (index *Index) Cancel(source Source, task sched.TaskID) bool {
	table, slot, valid := key(source, task)
	if !valid {
		return false
	}
	index.take()
	position := int(index.position[table][slot]) - 1
	removed := position >= 0 && index.heap[position].Task == task
	if removed {
		index.remove(position)
	}
	index.release()
	return removed
}

// Earliest returns the next deadline, or Never when there is none. It takes
// no lock, so a caller can learn that nothing is due at the cost of one load.
func (index *Index) Earliest() uint64 {
	if at := index.earliest.Load(); at != 0 {
		return at
	}
	return Never
}

// Expired removes and returns, earliest first, the deadlines at or before
// now, up to len(output). A caller that fills output calls again.
func (index *Index) Expired(now uint64, output []Entry) int {
	if index.Earliest() > now {
		return 0
	}
	index.take()
	count := 0
	for count < len(output) && index.count != 0 && index.heap[0].At <= now {
		output[count] = index.heap[0]
		count++
		index.remove(0)
	}
	index.release()
	return count
}

// Len reports how many deadlines the index holds.
func (index *Index) Len() int {
	index.take()
	count := index.count
	index.release()
	return count
}

func (index *Index) remove(position int) {
	table, slot, _ := key(index.heap[position].Source, index.heap[position].Task)
	index.position[table][slot] = 0
	index.count--
	if position == index.count {
		index.heap[position] = Entry{}
		return
	}
	index.heap[position] = index.heap[index.count]
	index.heap[index.count] = Entry{}
	index.place(position)
	index.fix(position)
}

// fix restores the heap order around position.
func (index *Index) fix(position int) {
	if position > 0 && index.less(position, (position-1)/2) {
		index.up(position)
		return
	}
	index.down(position)
}

func (index *Index) up(position int) {
	for position > 0 {
		parent := (position - 1) / 2
		if !index.less(position, parent) {
			return
		}
		index.swap(position, parent)
		position = parent
	}
}

func (index *Index) down(position int) {
	for {
		smallest := position
		for _, child := range [2]int{2*position + 1, 2*position + 2} {
			if child < index.count && index.less(child, smallest) {
				smallest = child
			}
		}
		if smallest == position {
			return
		}
		index.swap(position, smallest)
		position = smallest
	}
}

// less orders by time, then by source and task, so equal deadlines come out
// in one fixed order.
func (index *Index) less(left, right int) bool {
	a, b := &index.heap[left], &index.heap[right]
	if a.At != b.At {
		return a.At < b.At
	}
	if a.Source != b.Source {
		return a.Source < b.Source
	}
	return a.Task < b.Task
}

func (index *Index) swap(left, right int) {
	index.heap[left], index.heap[right] = index.heap[right], index.heap[left]
	index.place(left)
	index.place(right)
}

func (index *Index) place(position int) {
	table, slot, _ := key(index.heap[position].Source, index.heap[position].Task)
	index.position[table][slot] = int16(position + 1)
}
