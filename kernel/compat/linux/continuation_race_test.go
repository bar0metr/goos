//go:build !goos_validation

package linux

import (
	"sync"
	"sync/atomic"
	"testing"

	"goos/kernel/compat/linux/abi"
	"goos/kernel/lock"
	"goos/kernel/sched"
)

// A blocked call accepts one wake, whichever processor sends it first: a
// message, a timeout, and a signal race for it, and every other wake fails.
func TestContinuationResumesOnceUnderRacingWakers(t *testing.T) {
	lock.Install(lock.Hooks{})
	for round := 0; round < 200; round++ {
		var table ContinuationTable
		task, _ := sched.NewTaskID(5, uint32(round+1))
		if !table.Begin(task, Request{Number: abi.SysFutex}, nil) || !table.Issue(task, 1, 7) || !table.Block(task, 1) {
			t.Fatal("setup")
		}
		var woken atomic.Int32
		var start, done sync.WaitGroup
		start.Add(1)
		for waker := 0; waker < 8; waker++ {
			done.Add(1)
			go func(reason WakeReason) {
				defer done.Done()
				start.Wait()
				if table.Resume(task, 7, reason) {
					woken.Add(1)
				}
			}([]WakeReason{WakeMessage, WakeTimeout, WakeInterrupted}[waker%3])
		}
		start.Done()
		done.Wait()
		if woken.Load() != 1 {
			t.Fatalf("round %d: %d wakes accepted", round, woken.Load())
		}
		if pending, active := table.Pending(task); !active || pending.Phase != ContinuationReady {
			t.Fatalf("round %d: continuation %+v after the wake", round, pending)
		}
	}
}
