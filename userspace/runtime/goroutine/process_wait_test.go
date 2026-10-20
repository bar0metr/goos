package goroutine

import (
	"strconv"
	"testing"

	"goos/kernel/clock"
	"goos/kernel/handle"
	"goos/kernel/kcall"
	"goos/userspace/runtime/sys"
)

func TestProcessWaitImmediateProbe(t *testing.T) {
	for _, status := range []kcall.Status{kcall.StatusOK, kcall.StatusPermissionDenied, kcall.StatusNotFound, kcall.StatusUnsupported} {
		t.Run(strconv.Itoa(int(status)), func(t *testing.T) {
			ops := processWaitOps{
				probe: func(process handle.ID, timeout uint64) (uint64, kcall.Status) {
					if process != 7 || timeout != 0 {
						t.Fatalf("probe(%d, %d), want (7, 0)", process, timeout)
					}
					return 42, status
				},
				now: func() int64 { t.Fatal("immediate result read clock"); return 0 },
				wait: func(*[kcall.WaitAnyMaxObjects]sys.Interest, int, int64) (uint64, kcall.Status) {
					t.Fatal("immediate result parked")
					return 0, kcall.StatusOK
				},
			}
			// Even an unrepresentable timeout must not hide a completed exit
			// or a native capability/object error.
			code, got := waitForProcess(7, kcall.WaitAnyInfiniteTimeout, ops)
			if code != 42 || got != status {
				t.Fatalf("wait = %d, %v; want 42, %v", code, got, status)
			}
		})
	}
}

func TestProcessWaitAbsoluteBudgetAcrossReadinessRaces(t *testing.T) {
	const perTick = int64(1000000000 / clock.TicksPerSecond)
	now := int64(19)
	deadline := now + 3*perTick
	probes, waits := 0, 0
	ops := processWaitOps{
		probe: func(process handle.ID, timeout uint64) (uint64, kcall.Status) {
			probes++
			if process != 7 || timeout != 0 {
				t.Fatalf("probe(%d, %d)", process, timeout)
			}
			if probes == 3 {
				return 42, kcall.StatusOK
			}
			return 0, kcall.StatusTimeout
		},
		now: func() int64 { return now },
		wait: func(interests *[kcall.WaitAnyMaxObjects]sys.Interest, count int, got int64) (uint64, kcall.Status) {
			waits++
			if count != 1 || interests[0] != (sys.Interest{Handle: 7, Events: kcall.WaitCompleted}) || got != deadline {
				t.Fatalf("wait interests=%v count=%d deadline=%d, want %d", interests[0], count, got, deadline)
			}
			now += perTick
			return 1, kcall.StatusOK
		},
	}
	code, status := waitForProcess(7, 3, ops)
	if code != 42 || status != kcall.StatusOK || probes != 3 || waits != 2 {
		t.Fatalf("wait = %d, %v; probes=%d waits=%d", code, status, probes, waits)
	}
}

func TestProcessWaitReadinessRaceExhaustsBudget(t *testing.T) {
	const perTick = int64(1000000000 / clock.TicksPerSecond)
	now := int64(1)
	waits := 0
	ops := processWaitOps{
		probe: func(handle.ID, uint64) (uint64, kcall.Status) { return 0, kcall.StatusTimeout },
		now:   func() int64 { return now },
		wait: func(*[kcall.WaitAnyMaxObjects]sys.Interest, int, int64) (uint64, kcall.Status) {
			waits++
			now += perTick
			return 1, kcall.StatusOK
		},
	}
	code, status := waitForProcess(7, 1, ops)
	if code != 0 || status != kcall.StatusTimeout || waits != 1 {
		t.Fatalf("wait = %d, %v; waits=%d", code, status, waits)
	}
}

func TestProcessWaitPendingDeadlineEdges(t *testing.T) {
	const perTick = int64(1000000000 / clock.TicksPerSecond)
	const maximum = int64(1<<63 - 2)
	for _, sample := range []struct {
		name  string
		now   int64
		ticks uint64
		want  kcall.Status
		park  bool
	}{
		{"poll", 0, 0, kcall.StatusTimeout, false},
		{"one tick", 1, 1, kcall.StatusTimeout, true},
		{"last deadline", maximum - perTick, 1, kcall.StatusTimeout, true},
		{"overflow", maximum - perTick + 1, 1, kcall.StatusResourceExhausted, false},
		{"negative clock", -1, 1, kcall.StatusResourceExhausted, false},
		{"past maximum", 1<<63 - 1, 1, kcall.StatusResourceExhausted, false},
		{"large finite", 1, kcall.WaitAnyInfiniteTimeout - 1, kcall.StatusResourceExhausted, false},
		{"not an infinite wait", 1, kcall.WaitAnyInfiniteTimeout, kcall.StatusResourceExhausted, false},
	} {
		t.Run(sample.name, func(t *testing.T) {
			parked := false
			ops := processWaitOps{
				probe: func(handle.ID, uint64) (uint64, kcall.Status) { return 0, kcall.StatusTimeout },
				now:   func() int64 { return sample.now },
				wait: func(_ *[kcall.WaitAnyMaxObjects]sys.Interest, _ int, deadline int64) (uint64, kcall.Status) {
					parked = true
					if deadline != sample.now+int64(sample.ticks)*perTick || deadline == 0 {
						t.Fatalf("deadline=%d", deadline)
					}
					return 0, kcall.StatusTimeout
				},
			}
			code, status := waitForProcess(7, sample.ticks, ops)
			if code != 0 || status != sample.want || parked != sample.park {
				t.Fatalf("wait = %d, %v parked=%v; want %v parked=%v", code, status, parked, sample.want, sample.park)
			}
		})
	}
}

func TestProcessWaitPropagatesWaitAndReprobeErrors(t *testing.T) {
	for _, fromWait := range []bool{false, true} {
		for _, want := range []kcall.Status{kcall.StatusTimeout, kcall.StatusPermissionDenied, kcall.StatusNotFound, kcall.StatusResourceExhausted} {
			probes := 0
			ops := processWaitOps{
				probe: func(handle.ID, uint64) (uint64, kcall.Status) {
					probes++
					if probes > 1 {
						return 0, want
					}
					return 0, kcall.StatusTimeout
				},
				now: func() int64 { return 1 },
				wait: func(*[kcall.WaitAnyMaxObjects]sys.Interest, int, int64) (uint64, kcall.Status) {
					if fromWait {
						return 0, want
					}
					return 1, kcall.StatusOK
				},
			}
			// Timeout after a readiness race is pending, not a terminal
			// native error. The clock reaches the same absolute deadline.
			if !fromWait && want == kcall.StatusTimeout {
				reads := 0
				ops.now = func() int64 {
					reads++
					if reads > 1 {
						return 1 << 62
					}
					return 1
				}
			}
			if code, status := waitForProcess(7, 1, ops); code != 0 || status != want {
				t.Fatalf("fromWait=%v wait=%d, %v; want %v", fromWait, code, status, want)
			}
		}
	}
}
