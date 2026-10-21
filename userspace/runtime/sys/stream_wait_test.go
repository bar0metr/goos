package sys

import (
	"testing"

	"goos/kernel/handle"
	"goos/kernel/kcall"
)

func TestDirectionalStreamWaitDispatch(t *testing.T) {
	defer InstallWaitHandler(nil)
	for _, direction := range []struct {
		name   string
		wait   func(handle.ID, uint64) kcall.Status
		events kcall.WaitEvent
	}{
		{"read", StreamWaitRead, kcall.WaitReadable | kcall.WaitPeerClosed},
		{"write", StreamWaitWrite, kcall.WaitWritable | kcall.WaitPeerClosed},
	} {
		t.Run(direction.name, func(t *testing.T) {
			for _, timeout := range []uint64{17, kcall.WaitAnyInfiniteTimeout} {
				for _, want := range []kcall.Status{kcall.StatusOK, kcall.StatusTimeout, kcall.StatusPermissionDenied, kcall.StatusNotFound, kcall.StatusResourceExhausted} {
					calls := 0
					handler := WaitHandler(func(interests *[kcall.WaitAnyMaxObjects]Interest, count int, ticks uint64) (uint64, kcall.Status, bool) {
						calls++
						if count != 1 || ticks != timeout || interests[0] != (Interest{Handle: 7, Events: direction.events}) {
							t.Fatalf("wait count=%d ticks=%d interest=%v", count, ticks, interests[0])
						}
						// A ready mask does not distinguish data/space from peer
						// closure. In either case helpers must preserve OK and
						// leave the consuming operation to report PeerDied.
						return 1, want, true
					})
					InstallWaitHandler(&handler)
					if status := direction.wait(7, timeout); status != want || calls != 1 {
						t.Fatalf("wait = %v calls=%d, want %v", status, calls, want)
					}
				}
			}
		})
	}
}

func TestDirectionalStreamWaitPollingAndNativeFallback(t *testing.T) {
	defer InstallWaitHandler(nil)
	calls := 0
	handler := WaitHandler(func(*[kcall.WaitAnyMaxObjects]Interest, int, uint64) (uint64, kcall.Status, bool) {
		calls++
		return 1, kcall.StatusOK, false
	})
	InstallWaitHandler(&handler)
	for _, wait := range []func(handle.ID, uint64) kcall.Status{StreamWaitRead, StreamWaitWrite} {
		if status := wait(7, 0); status != kcall.StatusUnsupported {
			t.Fatalf("host poll = %v, want native Unsupported", status)
		}
	}
	if calls != 0 {
		t.Fatal("zero-time readiness polls entered scheduler")
	}
	for _, wait := range []func(handle.ID, uint64) kcall.Status{StreamWaitRead, StreamWaitWrite} {
		if status := wait(7, 17); status != kcall.StatusUnsupported {
			t.Fatalf("declined readiness wait = %v, want native Unsupported", status)
		}
	}
	if calls != 2 {
		t.Fatalf("declined waits called handler %d times, want 2", calls)
	}
}

func TestLegacyStreamWaitStaysNative(t *testing.T) {
	defer InstallWaitHandler(nil)
	handler := WaitHandler(func(*[kcall.WaitAnyMaxObjects]Interest, int, uint64) (uint64, kcall.Status, bool) {
		t.Fatal("legacy Wait-only stream wait entered directional readiness adapter")
		return 1, kcall.StatusOK, true
	})
	InstallWaitHandler(&handler)
	for _, timeout := range []uint64{0, 17, kcall.WaitAnyInfiniteTimeout} {
		// Host stubs always return Unsupported, including timeout zero.
		// The kernel retains its existing InvalidArgument for timeout zero.
		if status := StreamWait(7, timeout); status != kcall.StatusUnsupported {
			t.Fatalf("legacy native wait(%d) = %v", timeout, status)
		}
	}
}
