// Package thread runs Go functions on native threads of the calling process
// (ADR 0203). Every thread shares the process's heap, globals, handles, and
// capabilities, and has its own kernel-made stack. Threads run in parallel
// on as many processors as the process's tasks get.
//
// A thread runs a plain function with one uintptr argument. The function
// value is kept in a package table, which the collector scans, until the
// thread has read it. TinyGo goroutines, go statements, channels between
// threads, and the sync package are not supported: share state through
// atomics and the locks the runtime provides.
//
// The process ends when its last thread ends, or at once when any thread
// calls Exit or faults. A thread's own end is ThreadExit, which its function
// returning calls; it does not end the process. Every started thread must
// be joined once, which frees its slot.
package thread

import (
	"goos/kernel/kcall"
	"goos/userspace/runtime/tls"
)

// ID names a thread of the calling process. A thread that ended never
// shares an ID with a later one.
type ID uint64

// Slots is the number of threads a process can have, the main one
// included.
const Slots = tls.Slots

// Slot returns the calling thread's slot, 0 for the main thread.
func Slot() int { return tls.Slot() }

// start is a function waiting for its thread to read it.
type start struct {
	fn       func(uintptr)
	argument uintptr
	used     bool
}

// starts holds the functions of threads being started. starting is its
// spin lock; it is never held across a system call or a heap operation.
var (
	starts   [Slots]start
	starting spin
)

// claim records fn for a new thread and returns its index.
func claim(fn func(uintptr), argument uintptr) (int, bool) {
	starting.lock()
	for index := range starts {
		if !starts[index].used {
			starts[index] = start{fn: fn, argument: argument, used: true}
			starting.unlock()
			return index, true
		}
	}
	starting.unlock()
	return 0, false
}

// take returns the function at index and frees the entry.
func take(index int) (func(uintptr), uintptr) {
	starting.lock()
	entry := starts[index]
	starts[index] = start{}
	starting.unlock()
	return entry.fn, entry.argument
}

// Status is a thread call's outcome.
type Status = kcall.Status
