// Package wait exposes the native capability-safe multi-object readiness wait.
package wait

import (
	"goos/kernel/kcall"
	"goos/userspace/runtime/sys"
)

const MaxObjects = kcall.WaitAnyMaxObjects

// Interest is one object to watch and the events that make it ready.
type Interest = sys.Interest

type Result struct {
	ReadyMask uint64
}

// Any blocks until at least one interest is ready or timeout ticks elapse.
// It observes readiness only; callers retain responsibility for the typed
// receive/read/process operation that consumes the ready state.
func Any(interests *[MaxObjects]Interest, count int, timeout uint64) (Result, kcall.Status) {
	mask, status := sys.WaitAny(interests, count, timeout)
	return Result{ReadyMask: mask}, status
}

// Sleep blocks the caller for timeout ticks. It is Any with no interests, so
// the kernel needs no separate sleep operation.
func Sleep(timeout uint64) kcall.Status {
	var interests [MaxObjects]Interest
	_, status := Any(&interests, 0, timeout)
	return status
}
