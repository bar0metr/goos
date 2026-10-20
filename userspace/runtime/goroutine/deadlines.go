package goroutine

import (
	"goos/kernel/clock"
	"goos/kernel/kcall"
)

// deadlineFromTicks converts one relative native timeout into the scheduler's
// absolute nanosecond deadline. Finite waits must fit below the never sentinel.
func deadlineFromTicks(now int64, ticks uint64) (int64, bool) {
	if ticks == kcall.WaitAnyInfiniteTimeout {
		return 0, true
	}
	const maxDeadline = int64(1<<63 - 2)
	const perTick = int64(1000000000 / clock.TicksPerSecond)
	if now < 0 || now > maxDeadline || ticks > uint64((maxDeadline-now)/perTick) {
		return 0, false
	}
	return now + int64(ticks)*perTick, true
}
