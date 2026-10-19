//go:build goos_userspace

package tls

import _ "unsafe"

//go:linkname stackPointer goos_stack_pointer
func stackPointer() uintptr

// Slot returns the caller's thread slot.
func Slot() int { return SlotOf(stackPointer()) }
