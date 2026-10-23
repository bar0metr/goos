//go:build goos_userspace

package arena

import (
	"sync/atomic"

	"goos/internal/validation"
)

// A validation image can stop a thread inside a region transition, after it
// stored its new cursor and before it stored its new innermost region, so a
// test can make another thread's collection hold it exactly there
// (threadprobe region-window). Enter and Leave reach holdTransition only
// when validation.Enabled, a constant, so other builds compile the call out.

var (
	// holdSlot is the slot plus one of the thread whose next region
	// transition stops, or zero.
	holdSlot atomic.Int32
	// holding is set while that thread waits inside its transition.
	holding atomic.Uint32
)

// HoldRegionTransition makes the calling thread's next Enter or Leave stop
// between its cursor and innermost stores until ReleaseRegionTransition. It
// takes effect only in validation builds.
func HoldRegionTransition() {
	if !validation.Enabled {
		panic("arena: region transition holds need a validation build")
	}
	holdSlot.Store(int32(selfSlot() + 1))
}

// RegionTransitionHeld reports whether a thread waits inside a region
// transition.
func RegionTransitionHeld() bool { return holding.Load() != 0 }

// ReleaseRegionTransition lets a waiting thread finish its transition and
// reports whether one waited.
func ReleaseRegionTransition() bool { return holding.CompareAndSwap(1, 0) }

// holdTransition waits while the calling thread's transition is held. The
// caller is inside its region-transition window and pinned to its thread.
// It allocates nothing: a thread inside a region never collects.
func holdTransition(t *threadState) {
	slot := int32(selfSlot() + 1)
	if holdSlot.Load() != slot || !holdSlot.CompareAndSwap(slot, 0) {
		return
	}
	if t.regionChanging.Load() == 0 {
		panic("arena: region transition held outside its window")
	}
	holding.Store(1)
	for holding.Load() != 0 {
	}
}
