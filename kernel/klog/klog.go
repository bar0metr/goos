// Package klog retains the most recent structured log records in a bounded
// ring. The kernel is the early backend of the system log (spec §27): records
// arrive from the kernel itself and from services through a native call.
// Processors append and read at once; the ring's lock serializes them.
package klog

import (
	"goos/internal/logfmt"
	"goos/internal/timekeeping"
	"goos/kernel/lock"
)

const Capacity = 64

type entry struct {
	time          timekeeping.Time
	process       uint64
	level         logfmt.Level
	componentSize uint8
	messageSize   uint8
	fieldsSize    uint8
	component     [logfmt.MaxComponent]byte
	message       [logfmt.MaxMessage]byte
	fields        [logfmt.MaxFields]byte
}

type Ring struct {
	lock    lock.Spin
	entries [Capacity]entry
	next    int
	used    int
	total   uint64
}

// Copy holds one record read out of the ring, so the caller can use it after
// the ring's lock is released.
type Copy struct{ entry entry }

// Record borrows its fields from the copy until the copy is reused.
func (target *Copy) Record() logfmt.Record {
	slot := &target.entry
	return logfmt.Record{Time: slot.time, Process: slot.process, Level: slot.level,
		Component: slot.component[:slot.componentSize], Message: slot.message[:slot.messageSize], Fields: slot.fields[:slot.fieldsSize]}
}

// Read snapshots records starting at an absolute next-record cursor. First
// advances to Oldest if records were overwritten. Bounds and copies come from
// the same lock acquisition; appends cannot shift the snapshot's indices.
func (ring *Ring) Read(cursor uint64, target []Copy) (oldest, total, first uint64, count int) {
	ring.take()
	length := ring.length()
	oldest, total = ring.total-uint64(length), ring.total
	first = cursor
	if logfmt.SequenceBefore(first, oldest) {
		first = oldest
	}
	for count < len(target) && count < Capacity && logfmt.SequenceBefore(first, total) && uint64(count) < total-first {
		index := int(first-oldest) + count
		target[count].entry = ring.entries[(ring.next-length+index+Capacity)%Capacity]
		count++
	}
	ring.lock.Unlock()
	return
}

func (ring *Ring) take() {
	ring.lock.SetLevel(lock.LevelLog)
	ring.lock.Lock()
}

// Append copies a validated record, overwriting the oldest when full.
func (ring *Ring) Append(record logfmt.Record) uint64 {
	ring.take()
	slot := &ring.entries[ring.next]
	slot.time, slot.process, slot.level = record.Time, record.Process, record.Level
	slot.componentSize = uint8(copy(slot.component[:], record.Component))
	slot.messageSize = uint8(copy(slot.message[:], record.Message))
	slot.fieldsSize = uint8(copy(slot.fields[:], record.Fields))
	ring.next = (ring.next + 1) % Capacity
	if ring.used < Capacity {
		ring.used++
	}
	ring.total++
	total := ring.total
	ring.lock.Unlock()
	return total
}

func (ring *Ring) Len() int {
	ring.take()
	length := ring.length()
	ring.lock.Unlock()
	return length
}

func (ring *Ring) length() int {
	return ring.used
}

func (ring *Ring) Total() uint64 {
	_, total := ring.Bounds()
	return total
}

// Oldest is the sequence number of the oldest retained record. Records before
// it were overwritten, so a reader whose cursor is below it has lost some and
// can say how many rather than silently skipping them.
func (ring *Ring) Oldest() uint64 {
	oldest, _ := ring.Bounds()
	return oldest
}

// Bounds returns Oldest and Total from one moment.
func (ring *Ring) Bounds() (oldest uint64, total uint64) {
	ring.take()
	oldest, total = ring.total-uint64(ring.length()), ring.total
	ring.lock.Unlock()
	return oldest, total
}

// At copies the index-th retained record, oldest first, into target and
// returns it. The slices alias target.
func (ring *Ring) At(index int, target *Copy) (logfmt.Record, bool) {
	ring.take()
	if index < 0 || index >= ring.length() {
		ring.lock.Unlock()
		return logfmt.Record{}, false
	}
	target.entry = ring.entries[(ring.next-ring.length()+index+Capacity)%Capacity]
	ring.lock.Unlock()
	slot := &target.entry
	return logfmt.Record{Time: slot.time, Process: slot.process, Level: slot.level,
		Component: slot.component[:slot.componentSize], Message: slot.message[:slot.messageSize], Fields: slot.fields[:slot.fieldsSize]}, true
}
