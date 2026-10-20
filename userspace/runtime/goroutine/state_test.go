package goroutine

import (
	"sync"
	"sync/atomic"
	"testing"
)

func TestWakeCreditSurvivesPreemptionBeforePause(t *testing.T) {
	for _, at := range []string{"before-preempt", "while-runnable", "after-dispatch", "after-park"} {
		t.Run(at, func(t *testing.T) {
			var g G
			g.status.Store(gRunning)
			enqueues := 0
			wake := func() {
				if g.readyState() {
					enqueues++
				}
			}
			if at == "before-preempt" {
				wake()
			}
			g.rescheduleState()
			if at == "while-runnable" {
				wake()
			}
			g.dispatchState()
			if at == "after-dispatch" {
				wake()
			}
			if g.parkState() {
				enqueues++
			}
			if at == "after-park" {
				wake()
			}
			if enqueues != 1 || g.status.Load() != gRunnable {
				t.Fatalf("enqueues=%d state=%d", enqueues, g.status.Load())
			}
			g.dispatchState()
			if g.parkState() || g.status.Load() != gParked {
				t.Fatal("one wake satisfied two parks")
			}
		})
	}
}

func TestWakeCreditCoalescesAcrossRepeatedMigration(t *testing.T) {
	var g G
	g.status.Store(gRunning)
	if g.readyState() {
		t.Fatal("running goroutine enqueued twice")
	}
	for range 100 {
		g.rescheduleState()
		if g.status.Load() != gRunnableWaking || g.readyState() {
			t.Fatal("runnable wake credit lost or enqueued twice")
		}
		g.dispatchState()
		if g.status.Load() != gWaking || g.readyState() {
			t.Fatal("dispatch consumed the wake credit")
		}
	}
	if !g.parkState() {
		t.Fatal("park did not consume its wake credit")
	}
	g.dispatchState()
	if g.parkState() {
		t.Fatal("coalesced wake credited the next park")
	}
}

func TestReadyRacesParkWithExactlyOneEnqueue(t *testing.T) {
	for round := 0; round < 1000; round++ {
		var g G
		g.status.Store(gRunning)
		var enqueues atomic.Uint32
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			if g.readyState() {
				enqueues.Add(1)
			}
		}()
		go func() {
			defer wg.Done()
			if g.parkState() {
				enqueues.Add(1)
			}
		}()
		wg.Wait()
		if enqueues.Load() != 1 || g.status.Load() != gRunnable {
			t.Fatalf("round %d: enqueues=%d state=%d", round, enqueues.Load(), g.status.Load())
		}
		g.dispatchState()
		if g.parkState() {
			t.Fatal("completed park retained a wake credit")
		}
	}
}

func TestReadyRacesRescheduleAndDispatch(t *testing.T) {
	for round := 0; round < 1000; round++ {
		var g G
		g.status.Store(gRunning)
		var enqueues atomic.Uint32
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			if g.readyState() {
				enqueues.Add(1)
			}
		}()
		go func() {
			defer wg.Done()
			g.rescheduleState()
			g.dispatchState()
		}()
		wg.Wait()
		if enqueues.Load() != 0 || g.status.Load() != gWaking || !g.parkState() {
			t.Fatalf("round %d: wake lost or duplicated", round)
		}
	}
}
