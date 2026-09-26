// Package wait provides the kernel's bounded, typed wait-any registration table.
package wait

import "goos/kernel/sched"

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
// the same scheduling interval can be coalesced into one result.
type Table struct {
	operations [sched.MaxTasks]operation
	active     int
	registered int
}

// Begin registers a wait. An empty interest set is a timed sleep and needs a
// deadline, or the task would never run again.
func (table *Table) Begin(task sched.TaskID, entries []Entry, deadline uint64) bool {
	slot, _, valid := task.Parts()
	if !valid || len(entries) > MaxObjects || len(entries) == 0 && deadline == 0 {
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

	op := &table.operations[slot]
	if op.task == task && (op.active || op.complete) {
		return false
	}
	if op.active {
		table.active--
		table.registered -= int(op.count)
	}
	// The fields are set in place: an operation literal is larger than
	// TinyGo keeps on the stack, and the kernel's heap never frees.
	op.task, op.deadline, op.count = task, deadline, uint8(len(entries))
	op.active, op.complete, op.timedOut = true, false, false
	clear(op.readyEvents[:])
	copy(op.entries[:], entries)
	table.active++
	table.registered += len(entries)
	return true
}

func (table *Table) Publish(key Key, event Event, output []Completion) int {
	if !validKey(key) || event == 0 || event&^allEvents != 0 {
		return 0
	}
	completed := 0
	for slot := range table.operations {
		op := &table.operations[slot]
		if (!op.active && !op.complete) || op.timedOut {
			continue
		}
		matched := false
		for index := 0; index < int(op.count); index++ {
			entry := op.entries[index]
			ready := entry.Events & event
			if entry.Key == key && ready != 0 {
				op.readyEvents[index] |= ready
				matched = true
			}
		}
		if matched && op.active {
			op.active = false
			op.complete = true
			table.active--
			table.registered -= int(op.count)
			if completed < len(output) {
				output[completed] = completion(op)
			}
			completed++
		}
	}
	return completed
}

func (table *Table) Expire(now uint64, output []Completion) int {
	completed := 0
	for slot := range table.operations {
		op := &table.operations[slot]
		if op.active && op.deadline != 0 && now >= op.deadline {
			op.active = false
			op.complete = true
			op.timedOut = true
			table.active--
			table.registered -= int(op.count)
			if completed < len(output) {
				output[completed] = completion(op)
			}
			completed++
		}
	}
	return completed
}

func (table *Table) Cancel(task sched.TaskID) bool {
	slot, _, valid := task.Parts()
	if !valid {
		return false
	}
	op := &table.operations[slot]
	if op.task != task || (!op.active && !op.complete) {
		return false
	}
	if op.active {
		table.active--
		table.registered -= int(op.count)
	}
	*op = operation{}
	return true
}

func (table *Table) Take(task sched.TaskID) (Completion, bool) {
	slot, _, valid := task.Parts()
	if !valid {
		return Completion{}, false
	}
	op := &table.operations[slot]
	if op.task != task || !op.complete {
		return Completion{}, false
	}
	result := completion(op)
	*op = operation{}
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

func (table *Table) At(slot int) (View, bool) {
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

func (table *Table) Count() int { return table.active }

func (table *Table) RegistrationCount() int { return table.registered }

func validEntry(entry Entry) bool {
	return validKey(entry.Key) && entry.Events != 0 && entry.Events&^allEvents == 0
}

func validKey(key Key) bool {
	if key.Object == 0 || key.Kind < KindChannel || key.Kind > KindConsole {
		return false
	}
	if (key.Kind == KindChannel || key.Kind == KindStream) && key.Side > 1 {
		return false
	}
	return (key.Kind == KindChannel || key.Kind == KindStream) || key.Side == 0
}
