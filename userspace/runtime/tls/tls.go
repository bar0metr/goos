// Package tls finds the native thread the caller runs on (ADR 0203, ADR
// 0204). Every thread's FS base points at its control block, which the
// runtime's entry code fills in before any Go code runs on the thread, so
// the slot is one FS-relative load whatever stack the caller runs on: the
// thread's own stack or a goroutine's.
package tls

import "goos/kernel/kcall"

// Slots is the number of thread slots of a process. Slot 0 is the main
// thread.
const Slots = kcall.ThreadSlots

// Magic is the value every installed control block holds at offset 16.
const Magic = uint64(0x31424354534f4f47)

// Block is a thread's control block. The layout is fixed: the runtime's
// assembly reads and writes it at these offsets (userspace/runtime/amd64.S).
type Block struct {
	// Self is the block's own address, the thread's FS base.
	Self uintptr
	// Slot is the thread's slot.
	Slot uint64
	// Magic is Magic once the block is installed.
	Magic uint64
	// M is the goroutine runtime's record of the thread, or zero.
	M uintptr
	// Pins counts the reasons the thread must not be preempted onto
	// another goroutine: it uses state that belongs to the thread, such as
	// its system-call descriptors or allocator caches.
	Pins uint64
	// G is the goroutine the thread runs, or zero.
	G uintptr
	// ID is the thread's ID once the goroutine runtime has read it.
	ID uint64
	// Task is the TinyGo task of the code the thread runs, in a program
	// built with the goroutine runtime.
	Task uintptr
}

// SlotOf returns the slot whose fixed stack holds sp: 0 above the thread
// area, and Slots for an address below every thread stack. Only code that
// knows sp lies on a thread's own stack may use it.
func SlotOf(sp uintptr) int { return int(kcall.ThreadSlotOf(uint64(sp))) }
