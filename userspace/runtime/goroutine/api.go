//go:build goos_userspace && goos_goroutines

package goroutine

import (
	"goos/userspace/runtime/arena"
)

// Init prepares a program for goroutines: it sets the heap up, adopts the
// main function as the main goroutine, and then runs the package
// initializers the compiler left for run time, which may use channels. A
// program built with the goroutine toolchain calls it first.
func Init() {
	arena.Init()
	ensureStarted()
	installThreadHooks()
	installWaitHooks()
	arena.InitPackages()
}

func init() {
	installThreadHooks()
	installWaitHooks()
}

// SetLimit sets how many goroutines may exist at once, up to
// MaxGoroutines. It reports false for a limit out of range or below the
// goroutines that exist.
func SetLimit(n int) bool {
	ensureStarted()
	if n < 1 || n > MaxGoroutines {
		return false
	}
	sched.lock.lock()
	ok := int32(n) >= sched.live.Load()
	if ok {
		sched.limit = int32(n)
	}
	sched.lock.unlock()
	return ok
}

// Limit returns how many goroutines may exist at once.
func Limit() int {
	ensureStarted()
	sched.lock.lock()
	limit := sched.limit
	sched.lock.unlock()
	return int(limit)
}

// MaxProcs is runtime.GOMAXPROCS.
func MaxProcs(n int) int { return maxProcs(n) }

// Sleep parks the goroutine for at least nanoseconds.
func Sleep(nanoseconds int64) {
	if nanoseconds > 0 {
		sleep(nanoseconds)
	}
}

// Now returns the monotonic clock in nanoseconds, the clock Wait's
// deadlines use.
func Now() int64 { return nanotime() }

// Thread returns the slot of the thread the calling goroutine runs on at
// the moment of the call.
func Thread() int {
	pin()
	slot := int(currentM().slot)
	unpin()
	return slot
}
