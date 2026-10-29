// Package wait provides the kernel's bounded, typed wait-any registration table.
package wait

import (
	"goos/internal/logfmt"
	"goos/kernel/deadline"
	"goos/kernel/lock"
	"goos/kernel/sched"
)

// MaxObjects is the most objects one wait names: kcall.WaitAnyMaxObjects,
// the width of the ready mask a waiter receives (ADR 0111).
const MaxObjects = 64

type Kind uint8

const (
	KindChannel Kind = iota + 1
	KindStream
	KindProcess
	KindIRQ
	// KindConsole is the system console's input. It has one object and no
	// sides, so a reader waits on it the way it waits on anything else instead
	// of polling the device.
	KindConsole
	KindLog
	// KindThread is a native thread's wake flag (ADR 0204). Its object is
	// the thread's task ID, so a wake never reaches a later thread in the
	// same slot.
	KindThread
	// KindThreadEnd is the end of a native thread of the waiter's process
	// (kcall.WaitAnyThreadEnd). Its object is the thread's task ID and its
	// event Exited.
	KindThreadEnd
	// KindLinuxTerminal is a Linux terminal's processed input or geometry.
	// Its object is the terminal's identity. Only Linux calls wait on it.
	KindLinuxTerminal

	// lastKind is the highest valid kind; validKey bounds keys by it.
	lastKind = KindLinuxTerminal
)

type Event uint8

const (
	Readable Event = 1 << iota
	Writable
	Exited
	PeerClosed
	Interrupt
)

const allEvents = Readable | Writable | Exited | PeerClosed | Interrupt

type Key struct {
	Kind   Kind
	Object uint64
	Side   uint8
}

type Entry struct {
	Key    Key
	Events Event
	Cursor uint64 // next absolute record sequence, only for KindLog
}

type Completion struct {
	Task       sched.TaskID
	ReadyMask  uint64
	Event      Event
	ReadyIndex uint8
	TimedOut   bool
}

type operation struct {
	task        sched.TaskID
	deadline    uint64
	entries     [MaxObjects]Entry
	readyEvents [MaxObjects]Event
	count       uint8
	active      bool
	complete    bool
	timedOut    bool
}

// Table stores at most one wait operation for each scheduler task slot.
// Completed registrations remain only until Take so readiness published in
// the same scheduling interval can be coalesced into one result. Its lock
// makes each method one step, safe on several processors at once.
//
// With a deadline index installed, an active operation's deadline is in the
// index until the operation completes or is cancelled.
type Table struct {
	lock       lock.Spin
	operations [sched.MaxTasks]operation
	active     int
	registered int
	deadlines  *deadline.Index
	source     deadline.Source
}

// UseDeadlines keeps the table's deadlines in index under source.
func (table *Table) UseDeadlines(index *deadline.Index, source deadline.Source) {
	table.deadlines, table.source = index, source
}

// deactivate ends an active operation's wait and drops its deadline.
func (table *Table) deactivate(op *operation) {
	op.active = false
	table.active--
	table.registered -= int(op.count)
	if table.deadlines != nil && op.deadline != 0 {
		table.deadlines.Cancel(table.source, op.task)
	}
}

func (set *Table) take() {
	set.lock.SetLevel(lock.LevelWait)
	set.lock.Lock()
}

func (table *Table) beginHeld(task sched.TaskID, entries []Entry, deadline uint64) bool {
	slot, _, valid := task.Parts()
	if !valid || !ValidEntries(entries) || len(entries) == 0 && deadline == 0 {
		return false
	}

	op := &table.operations[slot]
	if op.task == task && (op.active || op.complete) {
		return false
	}
	if op.active {
		table.deactivate(op)
	}
	// The fields are set in place: an operation literal is larger than
	// TinyGo keeps on the stack, and the kernel's heap never frees.
	op.task, op.deadline, op.count = task, deadline, uint8(len(entries))
	op.active, op.complete, op.timedOut = true, false, false
	clear(op.readyEvents[:])
	copy(op.entries[:], entries)
	table.active++
	table.registered += len(entries)
	if table.deadlines != nil && deadline != 0 {
		table.deadlines.Arm(table.source, task, deadline)
	}
	return true
}

func (table *Table) publishHeld(key Key, event Event, total uint64, output []Completion) int {
	if !validKey(key) || event == 0 || event&^allEvents != 0 {
		return 0
	}
	completed := 0
	for slot := range table.operations[:] {
		op := &table.operations[slot]
		if (!op.active && !op.complete) || op.timedOut {
			continue
		}
		matched := false
		for index := 0; index < int(op.count); index++ {
			entry := op.entries[index]
			ready := entry.Events & event
			if entry.Key == key && ready != 0 && (key.Kind != KindLog || logfmt.SequenceBefore(entry.Cursor, total)) {
				op.readyEvents[index] |= ready
				matched = true
			}
		}
		if matched && op.active {
			table.deactivate(op)
			op.complete = true
			if completed < len(output) {
				output[completed] = completion(op)
			}
			completed++
		}
	}
	return completed
}

// expireTaskHeld times out task's operation if its deadline has come. The
// index already let go of the deadline it reported.
func (table *Table) expireTaskHeld(task sched.TaskID, now uint64) (Completion, bool) {
	slot, _, valid := task.Parts()
	if !valid {
		return Completion{}, false
	}
	op := &table.operations[slot]
	if op.task != task || !op.active || op.deadline == 0 || now < op.deadline {
		return Completion{}, false
	}
	op.active = false
	table.active--
	table.registered -= int(op.count)
	op.complete = true
	op.timedOut = true
	return completion(op), true
}

func (table *Table) cancelHeld(task sched.TaskID) bool {
	slot, _, valid := task.Parts()
	if !valid {
		return false
	}
	op := &table.operations[slot]
	if op.task != task || (!op.active && !op.complete) {
		return false
	}
	if op.active {
		table.deactivate(op)
	}
	resetOperation(op)
	return true
}

func (table *Table) takeHeld(task sched.TaskID) (Completion, bool) {
	slot, _, valid := task.Parts()
	if !valid {
		return Completion{}, false
	}
	op := &table.operations[slot]
	if op.task != task || !op.complete {
		return Completion{}, false
	}
	result := completion(op)
	resetOperation(op)
	return result, true
}

func completion(op *operation) Completion {
	result := Completion{Task: op.task, ReadyIndex: MaxObjects, TimedOut: op.timedOut}
	for index := 0; index < int(op.count); index++ {
		if op.readyEvents[index] == 0 {
			continue
		}
		result.ReadyMask |= 1 << index
		if result.ReadyIndex == MaxObjects {
			result.ReadyIndex = uint8(index)
			result.Event = op.readyEvents[index]
		}
	}
	return result
}

// View is a read-only copy of one slot's operation for diagnostics.
type View struct {
	Task     sched.TaskID
	Deadline uint64
	Entries  [MaxObjects]Entry
	Ready    [MaxObjects]Event
	Count    int
	Active   bool
	Complete bool
	TimedOut bool
}

func (table *Table) atHeld(slot int) (View, bool) {
	if slot < 0 || slot >= len(table.operations) {
		return View{}, false
	}
	op := &table.operations[slot]
	if !op.active && !op.complete {
		return View{}, false
	}
	return View{Task: op.task, Deadline: op.deadline, Entries: op.entries, Ready: op.readyEvents, Count: int(op.count),
		Active: op.active, Complete: op.complete, TimedOut: op.timedOut}, true
}

func (table *Table) countHeld() int { return table.active }

func (table *Table) registrationCountHeld() int { return table.registered }

// ValidEntries checks the resolved object keys before either polling or
// registering a wait. Different handles to one object are duplicate keys.
// Empty entries are valid; the caller separately validates the deadline.
func ValidEntries(entries []Entry) bool {
	if len(entries) > MaxObjects {
		return false
	}
	for index, entry := range entries {
		if !validEntry(entry) {
			return false
		}
		for previous := 0; previous < index; previous++ {
			if entries[previous].Key == entry.Key {
				return false
			}
		}
	}
	return true
}

func validEntry(entry Entry) bool {
	return validKey(entry.Key) && entry.Events != 0 && entry.Events&^allEvents == 0 &&
		(entry.Key.Kind == KindLog && entry.Events == Readable || entry.Key.Kind != KindLog && entry.Cursor == 0)
}

func validKey(key Key) bool {
	if key.Object == 0 || key.Kind < KindChannel || key.Kind > lastKind {
		return false
	}
	if (key.Kind == KindChannel || key.Kind == KindStream) && key.Side > 1 {
		return false
	}
	return (key.Kind == KindChannel || key.Kind == KindStream) || key.Side == 0
}

// Begin registers a wait. An empty interest set is a timed sleep and needs a
// deadline, or the task would never run again.
func (table *Table) Begin(task sched.TaskID, entries []Entry, deadline uint64) bool {
	table.take()
	result := table.beginHeld(task, entries, deadline)
	table.lock.Unlock()
	return result
}

func (table *Table) Publish(key Key, event Event, output []Completion) int {
	table.take()
	result := table.publishHeld(key, event, 0, output)
	table.lock.Unlock()
	return result
}

// ExpireTask times out task's operation when its deadline is at or before
// now. An operation an event completed first, or one the task began since,
// is left alone, so exactly one of them completes a wait.
func (table *Table) ExpireTask(task sched.TaskID, now uint64) (Completion, bool) {
	table.take()
	result, expired := table.expireTaskHeld(task, now)
	table.lock.Unlock()
	return result, expired
}

func (table *Table) Cancel(task sched.TaskID) bool {
	table.take()
	result := table.cancelHeld(task)
	table.lock.Unlock()
	return result
}

func (table *Table) Take(task sched.TaskID) (Completion, bool) {
	table.take()
	completion, ready := table.takeHeld(task)
	table.lock.Unlock()
	return completion, ready
}

func (table *Table) At(slot int) (View, bool) {
	table.take()
	view, found := table.atHeld(slot)
	table.lock.Unlock()
	return view, found
}

func (table *Table) Count() int {
	table.take()
	result := table.countHeld()
	table.lock.Unlock()
	return result
}

func (table *Table) RegistrationCount() int {
	table.take()
	result := table.registrationCountHeld()
	table.lock.Unlock()
	return result
}

// PublishLog wakes only readers behind total. Call it after releasing the ring
// lock; the wait table never nests the log lock inside its own lock.
func (table *Table) PublishLog(object, total uint64, output []Completion) int {
	table.take()
	result := table.publishHeld(Key{Kind: KindLog, Object: object}, Readable, total, output)
	table.lock.Unlock()
	return result
}

// resetOperation clears in place rather than materializing a large literal.
func resetOperation(op *operation) {
	op.task, op.deadline, op.count = 0, 0, 0
	op.active, op.complete, op.timedOut = false, false, false
	clear(op.entries[:])
	clear(op.readyEvents[:])
}

// Inspect copies diagnostics into caller-owned storage, avoiding a large
// returned value/literal in the freestanding kernel. At remains compatible.
func (table *Table) Inspect(slot int, target *View) bool {
	if target == nil || slot < 0 || slot >= len(table.operations) {
		return false
	}
	table.take()
	op := &table.operations[slot]
	if !op.active && !op.complete {
		table.lock.Unlock()
		return false
	}
	target.Task, target.Deadline, target.Count = op.task, op.deadline, int(op.count)
	target.Active, target.Complete, target.TimedOut = op.active, op.complete, op.timedOut
	copy(target.Entries[:], op.entries[:])
	copy(target.Ready[:], op.readyEvents[:])
	table.lock.Unlock()
	return true
}
