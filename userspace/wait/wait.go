// Package wait exposes the native capability-safe multi-object readiness wait.
package wait

import (
	"goos/kernel/clock"
	"goos/kernel/kcall"
	"goos/userspace/runtime/arena"
	"goos/userspace/runtime/sys"
)

const MaxObjects = kcall.WaitAnyMaxObjects

// idleTicks is how long a wait runs with nothing ready before the caller's
// garbage is collected (ADR 0201).
const idleTicks = clock.TicksPerSecond / 10

// Interest is one object to watch and the events that make it ready.
type Interest = sys.Interest

type Result struct {
	ReadyMask uint64
}

// Any blocks until at least one interest is ready or timeout ticks elapse.
// It observes readiness only; callers retain responsibility for the typed
// receive/read/process operation that consumes the ready state.
//
// A caller that allocated since its last collection and waits longer than
// idleTicks is idle: after idleTicks with nothing ready, Any collects and then
// waits out the rest of the timeout. In a process with several threads the
// collection runs only if every other thread waits too, so the caller is
// marked idle for the whole wait.
func Any(interests *[MaxObjects]Interest, count int, timeout uint64) (Result, kcall.Status) {
	if timeout == 0 {
		mask, status := sys.WaitAny(interests, count, timeout)
		return Result{ReadyMask: mask}, status
	}
	arena.IdleEnter()
	mask, status := waitIdle(interests, count, timeout)
	arena.IdleLeave()
	return Result{ReadyMask: mask}, status
}

func waitIdle(interests *[MaxObjects]Interest, count int, timeout uint64) (uint64, kcall.Status) {
	if timeout > idleTicks && arena.IdlePending() {
		mask, status := sys.WaitAny(interests, count, idleTicks)
		if status != kcall.StatusTimeout {
			return mask, status
		}
		arena.CollectIdle()
		if timeout != kcall.WaitAnyInfiniteTimeout {
			timeout -= idleTicks
		}
	}
	return sys.WaitAny(interests, count, timeout)
}

// CursorInterest is an Interest that can name a log cursor.
type CursorInterest = sys.CursorInterest

// AnyCursor is Any over cursor interests: a log interest is ready once the
// log holds a record at or after its cursor. It collects garbage when idle
// as Any does.
func AnyCursor(interests *[MaxObjects]CursorInterest, count int, timeout uint64) (Result, kcall.Status) {
	if timeout == 0 {
		mask, status := sys.WaitAnyCursor(interests, count, timeout)
		return Result{ReadyMask: mask}, status
	}
	arena.IdleEnter()
	if timeout > idleTicks && arena.IdlePending() {
		mask, status := sys.WaitAnyCursor(interests, count, idleTicks)
		if status != kcall.StatusTimeout {
			arena.IdleLeave()
			return Result{ReadyMask: mask}, status
		}
		arena.CollectIdle()
		if timeout != kcall.WaitAnyInfiniteTimeout {
			timeout -= idleTicks
		}
	}
	mask, status := sys.WaitAnyCursor(interests, count, timeout)
	arena.IdleLeave()
	return Result{ReadyMask: mask}, status
}

// Sleep blocks the caller for timeout ticks. It is Any with no interests, so
// the kernel needs no separate sleep operation.
func Sleep(timeout uint64) kcall.Status {
	var interests [MaxObjects]Interest
	_, status := Any(&interests, 0, timeout)
	return status
}
