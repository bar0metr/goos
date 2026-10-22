//go:build goos_userspace

package main

import _ "unsafe"

// vectorSpin fills XMM0 to XMM14 with a pattern and loops without calls
// until *stop is nonzero. It returns how many times a register had lost the
// pattern: a preemption that does not keep the SSE state of the code it
// interrupts shows up here.
//
//go:linkname vectorSpin goos_probe_vector_spin
func vectorSpin(stop *uint32) uint64
