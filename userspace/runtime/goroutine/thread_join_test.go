package goroutine

import (
	"strconv"
	"testing"

	"goos/kernel/handle"
	"goos/kernel/kcall"
	"goos/userspace/runtime/sys"
)

func TestThreadJoinImmediatePollKeepsNativeStatus(t *testing.T) {
	for _, status := range []kcall.Status{kcall.StatusOK, kcall.StatusInvalidArgument, kcall.StatusAlreadyExists, kcall.StatusNotFound, kcall.StatusPermissionDenied} {
		t.Run(strconv.Itoa(int(status)), func(t *testing.T) {
			ops := threadJoinOps{
				probe: func(thread uint64) (uint64, kcall.Status) {
					if thread != 9 {
						t.Fatalf("probe(%d)", thread)
					}
					return 42, status
				},
				wait: func(*[kcall.WaitAnyMaxObjects]sys.Interest, int, int64) (uint64, kcall.Status) {
					t.Fatal("an immediate result parked")
					return 0, kcall.StatusOK
				},
			}
			if code, got := joinThread(9, ops); code != 42 || got != status {
				t.Fatalf("join = %d, %v; want 42, %v", code, got, status)
			}
			if claimJoin(9) != kcall.StatusOK {
				t.Fatal("an immediate result left a claim")
			}
			releaseJoin(9)
		})
	}
}

func TestThreadJoinParksOnTheEndThenCollects(t *testing.T) {
	probes, waits := 0, 0
	ops := threadJoinOps{
		probe: func(thread uint64) (uint64, kcall.Status) {
			probes++
			if probes == 3 {
				return 42, kcall.StatusOK
			}
			return 0, kcall.StatusTimeout
		},
		wait: func(interests *[kcall.WaitAnyMaxObjects]sys.Interest, count int, deadline int64) (uint64, kcall.Status) {
			waits++
			want := sys.Interest{Handle: handle.ID(kcall.ThreadEndInterest(9)), Events: kcall.WaitCompleted}
			if count != 1 || interests[0] != want || deadline != 0 {
				t.Fatalf("wait interest=%v count=%d deadline=%d", interests[0], count, deadline)
			}
			// While this joiner parks, a second goroutine joiner is refused
			// as a second native joiner is.
			if code, status := joinThread(9, threadJoinOps{probe: func(uint64) (uint64, kcall.Status) { return 0, kcall.StatusTimeout }}); code != 0 || status != kcall.StatusAlreadyExists {
				t.Fatalf("second joiner = %d, %v", code, status)
			}
			return 1, kcall.StatusOK
		},
	}
	if code, status := joinThread(9, ops); code != 42 || status != kcall.StatusOK || probes != 3 || waits != 2 {
		t.Fatalf("join = %d, %v; probes=%d waits=%d", code, status, probes, waits)
	}
	if claimJoin(9) != kcall.StatusOK {
		t.Fatal("the claim outlived the join")
	}
	releaseJoin(9)
}

func TestThreadJoinReportsWaitAndLateJoinFailures(t *testing.T) {
	for _, tc := range []struct {
		name       string
		waitStatus kcall.Status
		after      kcall.Status
		want       kcall.Status
	}{
		{"wait refused", kcall.StatusNotFound, kcall.StatusOK, kcall.StatusNotFound},
		{"native joiner first", kcall.StatusOK, kcall.StatusNotFound, kcall.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			probes := 0
			ops := threadJoinOps{
				probe: func(uint64) (uint64, kcall.Status) {
					probes++
					if probes == 1 {
						return 0, kcall.StatusTimeout
					}
					return 0, tc.after
				},
				wait: func(*[kcall.WaitAnyMaxObjects]sys.Interest, int, int64) (uint64, kcall.Status) {
					return 0, tc.waitStatus
				},
			}
			if _, status := joinThread(11, ops); status != tc.want {
				t.Fatalf("join = %v, want %v", status, tc.want)
			}
			if claimJoin(11) != kcall.StatusOK {
				t.Fatal("a failed join left its claim")
			}
			releaseJoin(11)
		})
	}
}

func TestThreadJoinClaimsAreBounded(t *testing.T) {
	for thread := uint64(1); thread <= kcall.ThreadSlots; thread++ {
		if claimJoin(thread) != kcall.StatusOK {
			t.Fatalf("claim %d", thread)
		}
	}
	if claimJoin(kcall.ThreadSlots+1) != kcall.StatusResourceExhausted {
		t.Fatal("a full claim table accepted another thread")
	}
	for thread := uint64(1); thread <= kcall.ThreadSlots; thread++ {
		releaseJoin(thread)
	}
}
