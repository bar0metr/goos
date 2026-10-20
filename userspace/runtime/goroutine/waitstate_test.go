package goroutine

import (
	"sync"
	"sync/atomic"
	"testing"

	"goos/kernel/kcall"
)

func TestWaitIdentityRejectsDelayedCompletion(t *testing.T) {
	var w waitState
	old := w.begin(1, nil)
	if !w.claim(1, old, 1, kcall.StatusOK) {
		t.Fatal("first completion failed")
	}
	next := w.begin(1, nil)
	if w.claim(1, old, 9, kcall.StatusPeerDied) {
		t.Fatal("previous wait completed the next wait")
	}
	if !w.claim(1, next, 2, kcall.StatusOK) {
		t.Fatal("next completion failed")
	}
	reused := w.begin(2, nil)
	if w.claim(1, reused, 9, kcall.StatusPeerDied) || !w.claim(2, reused, 4, kcall.StatusOK) {
		t.Fatal("generation identity failed")
	}
	if w.mask != 4 || w.status != kcall.StatusOK {
		t.Fatal("stale completion overwrote result")
	}
}

func TestWaitCompletionCommitsOnce(t *testing.T) {
	var w waitState
	for round := uint32(0); round < 100; round++ {
		seq := w.begin(3, nil)
		var winners atomic.Uint32
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				if w.claim(3, seq, uint64(1)<<i, kcall.StatusOK) {
					winners.Add(1)
				}
			}(i)
		}
		wg.Wait()
		if winners.Load() != 1 || w.mask == 0 || w.mask&(w.mask-1) != 0 {
			t.Fatalf("round %d: winners=%d mask=%x", round, winners.Load(), w.mask)
		}
	}
}
