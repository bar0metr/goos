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

// Queueing a VFS request and recording Sent are one cancellation boundary.
// If abandonment wins, the send callback is never entered. If send wins, the
// state returned by abandonment says that the open reached the peer.
func TestContinuationSendAndAbandonAreAtomic(t *testing.T) {
	lock.Install(lock.Hooks{})
	for round := 0; round < 200; round++ {
		var table ContinuationTable
		task, _ := sched.NewTaskID(6, uint32(round+1))
		if !table.Begin(task, Request{Number: abi.SysOpenat}, nil) || !table.Issue(task, OpenStageOpen, 73) {
			t.Fatal("setup")
		}
		var callback atomic.Bool
		var sendOK, active bool
		var abandoned Pending
		var start, done sync.WaitGroup
		start.Add(1)
		done.Add(2)
		go func() {
			defer done.Done()
			start.Wait()
			sendOK = table.Send(task, OpenStageOpen, 73, func() bool {
				callback.Store(true)
				return true
			})
		}()
		go func() {
			defer done.Done()
			start.Wait()
			abandoned, active = table.Abandon(task)
		}()
		start.Done()
		done.Wait()
		if !active {
			t.Fatalf("round %d: abandonment missed active continuation", round)
		}
		if callback.Load() != sendOK || abandoned.Sent != sendOK || abandoned.AbandonFailsVFS() != sendOK {
			t.Fatalf("round %d: callback=%v send=%v abandoned=%+v", round, callback.Load(), sendOK, abandoned)
		}
		if _, exists := table.Pending(task); exists {
			t.Fatalf("round %d: abandoned continuation remained active", round)
		}
	}
}

// A reader holds copied paths while another processor cancels and reuses the
// same continuation slot. Barriers force the overwrite before the read ends.
func TestContinuationSnapshotSurvivesCancelAndRebegin(t *testing.T) {
	lock.Install(lock.Hooks{})
	var table ContinuationTable
	task, _ := sched.NewTaskID(7, 1)
	request := Request{Number: abi.SysRename}
	if !table.Begin(task, request, []byte("/old/path")) || !table.Annotate(task, []byte("/old/target"), 17) {
		t.Fatal("setup")
	}
	var snapshot PathSnapshot
	ready, started, reuse, result := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan bool, 1)
	go func() {
		pending, ok := table.Snapshot(task, &snapshot)
		close(ready)
		close(started)
		for {
			select {
			case <-reuse:
				result <- ok && pending.Note == 17 && pending.Number == abi.SysRename &&
					string(snapshot.Path()) == "/old/path" && string(snapshot.Target()) == "/old/target"
				return
			default:
				if string(snapshot.Path()) != "/old/path" || string(snapshot.Target()) != "/old/target" {
					ok = false
				}
			}
		}
	}()
	<-ready
	<-started
	if !table.Cancel(task) || !table.Begin(task, Request{Number: abi.SysOpenat}, []byte("/new/path")) || !table.Annotate(task, []byte("/new/target"), 42) {
		t.Fatal("cancel and reuse")
	}
	close(reuse)
	if !<-result {
		t.Fatal("snapshot changed after cancel and reuse")
	}
	pending, ok := table.Snapshot(task, &snapshot)
	if !ok || pending.Number != abi.SysOpenat || pending.Note != 42 || string(snapshot.Path()) != "/new/path" || string(snapshot.Target()) != "/new/target" {
		t.Fatalf("reused snapshot: %+v %q %q", pending, snapshot.Path(), snapshot.Target())
	}
	if allocs := testing.AllocsPerRun(100, func() { table.Snapshot(task, &snapshot) }); allocs != 0 {
		t.Fatalf("snapshot allocated %v objects", allocs)
	}
}
