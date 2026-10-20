// Package tls finds the native thread the caller runs on (ADR 0203). A
// thread's stack pointer always lies in its own stack, and the kernel puts
// every thread stack at a fixed place by slot, so the slot follows from the
// stack pointer with no system call and no thread-local register.
package tls

import "goos/kernel/kcall"

// Slots is the number of thread slots of a process. Slot 0 is the main
// thread.
const Slots = kcall.ThreadSlots

// SlotOf returns the slot whose stack holds sp. A stack pointer below the
// thread area belongs to no thread; it stops the program.
func SlotOf(sp uintptr) int {
	slot := kcall.ThreadSlotOf(uint64(sp))
	if slot >= Slots {
		panic("tls: stack pointer outside every thread stack")
	}
	return int(slot)
}
