// Package klog retains the most recent structured log records in a bounded
// ring. The kernel is the early backend of the system log (spec §27): records
// arrive from the kernel itself and from services through a native call.
package klog

import "goos/internal/logfmt"

const Capacity = 64

type entry struct {
	time          uint64
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
	entries [Capacity]entry
	next    int
	total   uint64
}

// Append copies a validated record, overwriting the oldest when full.
func (ring *Ring) Append(record logfmt.Record) {
	slot := &ring.entries[ring.next]
	slot.time, slot.process, slot.level = record.Time, record.Process, record.Level
	slot.componentSize = uint8(copy(slot.component[:], record.Component))
	slot.messageSize = uint8(copy(slot.message[:], record.Message))
	slot.fieldsSize = uint8(copy(slot.fields[:], record.Fields))
	ring.next = (ring.next + 1) % Capacity
	ring.total++
}

func (ring *Ring) Len() int {
	if ring.total < Capacity {
		return int(ring.total)
	}
	return Capacity
}

func (ring *Ring) Total() uint64 { return ring.total }

// At returns the index-th retained record, oldest first. The slices alias the
// ring and stay valid until that slot is overwritten.
func (ring *Ring) At(index int) (logfmt.Record, bool) {
	if index < 0 || index >= ring.Len() {
		return logfmt.Record{}, false
	}
	slot := &ring.entries[(ring.next-ring.Len()+index+Capacity)%Capacity]
	return logfmt.Record{Time: slot.time, Process: slot.process, Level: slot.level,
		Component: slot.component[:slot.componentSize], Message: slot.message[:slot.messageSize], Fields: slot.fields[:slot.fieldsSize]}, true
}
