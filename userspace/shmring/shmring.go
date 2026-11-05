// Package shmring holds the single-producer, single-consumer ring that two
// address spaces share through mapped memory, and the doorbell rule that
// wakes an idle consumer. linuxsockapi lays its family rings on it, and
// GTCP 4 its session rings.
//
// Heads and tails run free modulo 2^32. The producer writes an entry, then
// stores the tail; the consumer loads the tail, reads the entry, then stores
// the head. A ring's head and tail may sit on different pages, so a layout
// can give each word exactly one writer.
package shmring

import (
	"sync/atomic"
	"unsafe"
)

// Status is the outcome of a ring operation.
type Status uint8

const (
	OK Status = iota
	// Empty: nothing to consume.
	Empty
	// Full: no room to produce.
	Full
	// Broken: the indices claim more entries than the ring holds.
	Broken
)

// Word returns the 32-bit word at offset in page. offset must be 4-aligned.
func Word(page []byte, offset int) *uint32 {
	return (*uint32)(unsafe.Pointer(&page[offset]))
}

// Ring is a view of one ring: its two indices and count entries of size
// bytes each.
type Ring struct {
	head, tail *uint32
	entries    []byte
	count      uint32
	size       uint32
}

// New makes a view. entries must hold count*size bytes.
func New(head, tail *uint32, entries []byte, count, size int) Ring {
	return Ring{head: head, tail: tail, entries: entries[:count*size], count: uint32(count), size: uint32(size)}
}

func (r *Ring) entry(index uint32) []byte {
	start := (index % r.count) * r.size
	return r.entries[start : start+r.size]
}

// Len reports the entries waiting, or Broken when the indices are corrupt.
func (r *Ring) Len() (uint32, Status) {
	used := atomic.LoadUint32(r.tail) - atomic.LoadUint32(r.head)
	if used > r.count {
		return 0, Broken
	}
	return used, OK
}

// Room reports how many entries the producer can add now.
func (r *Ring) Room() uint32 {
	used, status := r.Len()
	if status != OK {
		return 0
	}
	return r.count - used
}

// Reserve hands the producer the entry at the tail, cleared. Publish makes
// it visible.
func (r *Ring) Reserve() ([]byte, Status) {
	used, status := r.Len()
	if status != OK {
		return nil, status
	}
	if used == r.count {
		return nil, Full
	}
	p := r.entry(atomic.LoadUint32(r.tail))
	clear(p)
	return p, OK
}

// Publish moves the tail past the entry Reserve returned.
func (r *Ring) Publish() { atomic.StoreUint32(r.tail, atomic.LoadUint32(r.tail)+1) }

// Front hands the consumer the entry at the head. Consume releases it.
func (r *Ring) Front() ([]byte, Status) {
	used, status := r.Len()
	if status != OK {
		return nil, status
	}
	if used == 0 {
		return nil, Empty
	}
	return r.entry(atomic.LoadUint32(r.head)), OK
}

// Consume moves the head past the entry Front returned.
func (r *Ring) Consume() { atomic.StoreUint32(r.head, atomic.LoadUint32(r.head)+1) }

// Pending reports whether entries wait.
func (r *Ring) Pending() bool { return atomic.LoadUint32(r.tail) != atomic.LoadUint32(r.head) }

// The doorbell rule for a consumer that sleeps. The consumer owns an armed
// counter and the producer a rung counter, each in memory only its owner
// writes. Before it sleeps the consumer calls Arm and then checks its rings
// once more. After it publishes, the producer calls ShouldRing and sends one
// notification when that reports true. Both sides use sequentially
// consistent atomics, so either the consumer's last check sees the entry or
// the producer sees the arm. One arm draws at most one notification, however
// many entries or rings follow it.

// Arm records that the consumer is about to sleep.
func Arm(armed *uint32) { atomic.StoreUint32(armed, atomic.LoadUint32(armed)+1) }

// ShouldRing reports whether the consumer armed since the producer last
// rang, and marks the arm answered.
func ShouldRing(armed, rung *uint32) bool {
	current := atomic.LoadUint32(armed)
	if current == atomic.LoadUint32(rung) {
		return false
	}
	atomic.StoreUint32(rung, current)
	return true
}
