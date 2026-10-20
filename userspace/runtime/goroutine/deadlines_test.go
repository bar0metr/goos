package goroutine

import (
	"testing"

	"goos/kernel/clock"
	"goos/kernel/kcall"
)

func TestDeadlineFromTicks(t *testing.T) {
	const now = int64(1234567)
	const perTick = int64(1000000000 / clock.TicksPerSecond)
	for _, ticks := range []uint64{0, 1, 5, 100} {
		got, valid := deadlineFromTicks(now, ticks)
		if !valid || got != now+int64(ticks)*perTick {
			t.Fatalf("timeout %d: deadline=%d valid=%v", ticks, got, valid)
		}
	}
	if got, valid := deadlineFromTicks(now, kcall.WaitAnyInfiniteTimeout); !valid || got != 0 {
		t.Fatalf("infinite timeout: deadline=%d valid=%v", got, valid)
	}
	for _, sample := range []struct {
		now   int64
		ticks uint64
	}{
		{-1, 1},
		{1<<63 - 1, 0},
		{1<<63 - 2, 1},
		{now, kcall.WaitAnyInfiniteTimeout - 1},
	} {
		if got, valid := deadlineFromTicks(sample.now, sample.ticks); valid {
			t.Fatalf("overflow %+v accepted as %d", sample, got)
		}
	}
}
