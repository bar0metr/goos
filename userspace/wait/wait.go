// Package wait exposes the native capability-safe multi-object readiness wait.
package wait

import (
	"goos/kernel/handle"
	"goos/kernel/kcall"
	"unsafe"
)

const MaxObjects = kcall.WaitAnyMaxObjects

type Interest struct {
	Handle handle.ID
	Events kcall.WaitEvent
}

type Result struct {
	ReadyMask uint16
}

// Any blocks until at least one interest is ready or timeout ticks elapse.
// It observes readiness only; callers retain responsibility for the typed
// receive/read/process operation that consumes the ready state.
func Any(interests *[MaxObjects]Interest, count int, timeout uint64) (Result, kcall.Status) {
	if interests == nil || count <= 0 || count > MaxObjects {
		return Result{}, kcall.StatusInvalidArgument
	}
	descriptor := [2]uint64{timeout, 0}
	status := goosWaitAny(
		unsafe.Pointer(&interests[0]),
		uintptr(count),
		unsafe.Pointer(&descriptor[0]),
	)
	return Result{ReadyMask: uint16(descriptor[1])}, kcall.Status(status)
}

//go:linkname goosWaitAny goos_wait_any
func goosWaitAny(interests unsafe.Pointer, count uintptr, result unsafe.Pointer) uint64
