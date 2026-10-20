//go:build goos_userspace

package tls

import "unsafe"

//go:linkname slot goos_tls_slot
func slot() uint64

//go:linkname pin goos_tls_pin
func pin() uint64

//go:linkname unpin goos_tls_unpin
func unpin()

//go:linkname block goos_tls_block
func block() uintptr

//go:extern goos_tcbs
var blocks [Slots]Block

// Slot returns the caller's thread slot.
func Slot() int { return int(slot()) }

// Pin keeps the calling goroutine on its thread until Unpin and returns the
// thread's slot. Pins nest.
func Pin() int { return int(pin()) }

// Unpin undoes one Pin.
func Unpin() { unpin() }

// Current returns the calling thread's control block.
func Current() *Block { return (*Block)(unsafe.Pointer(block())) }

// Of returns the control block of slot.
func Of(slot int) *Block { return &blocks[slot] }
