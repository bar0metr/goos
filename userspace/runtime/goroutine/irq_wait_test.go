package goroutine

import (
	"testing"

	"goos/kernel/handle"
	"goos/kernel/kcall"
	"goos/userspace/runtime/sys"
)

func TestIRQWaitTakesPendingWithoutParking(t *testing.T) {
	for _, want := range []kcall.Status{kcall.StatusOK, kcall.StatusPermissionDenied} {
		ops := irqWaitOps{
			take: func(irq handle.ID) kcall.Status {
				if irq != 5 {
					t.Fatalf("take(%d)", irq)
				}
				return want
			},
			wait: func(*[kcall.WaitAnyMaxObjects]sys.Interest, int, int64) (uint64, kcall.Status) {
				t.Fatal("a pending or refused line parked")
				return 0, kcall.StatusOK
			},
		}
		if got := waitForIRQ(5, ops); got != want {
			t.Fatalf("IRQ wait = %v, want %v", got, want)
		}
	}
}

func TestIRQWaitParksAndRetriesLostRaces(t *testing.T) {
	takes, waits := 0, 0
	ops := irqWaitOps{
		take: func(handle.ID) kcall.Status {
			takes++
			// The first wake loses the event to another taker.
			if takes == 3 {
				return kcall.StatusOK
			}
			return kcall.StatusNotFound
		},
		wait: func(interests *[kcall.WaitAnyMaxObjects]sys.Interest, count int, deadline int64) (uint64, kcall.Status) {
			waits++
			if count != 1 || interests[0] != (sys.Interest{Handle: 5, Events: kcall.WaitReadable}) || deadline != 0 {
				t.Fatalf("wait interest=%v count=%d deadline=%d", interests[0], count, deadline)
			}
			return 1, kcall.StatusOK
		},
	}
	if status := waitForIRQ(5, ops); status != kcall.StatusOK || takes != 3 || waits != 2 {
		t.Fatalf("IRQ wait = %v; takes=%d waits=%d", status, takes, waits)
	}
}

func TestIRQWaitReportsWaitFailure(t *testing.T) {
	ops := irqWaitOps{
		take: func(handle.ID) kcall.Status { return kcall.StatusNotFound },
		wait: func(*[kcall.WaitAnyMaxObjects]sys.Interest, int, int64) (uint64, kcall.Status) {
			return 0, kcall.StatusUnsupported
		},
	}
	if status := waitForIRQ(5, ops); status != kcall.StatusUnsupported {
		t.Fatalf("IRQ wait = %v", status)
	}
}
