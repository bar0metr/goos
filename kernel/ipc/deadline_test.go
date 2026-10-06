package ipc

import (
	"testing"

	"goos/kernel/deadline"
	"goos/kernel/sched"
)

func taskID(t *testing.T, slot, generation uint32) sched.TaskID {
	t.Helper()
	id, ok := sched.NewTaskID(slot, generation)
	if !ok {
		t.Fatal("task identity")
	}
	return id
}

// due returns the tasks whose deadlines the index reports at now.
func due(index *deadline.Index, now uint64) []sched.TaskID {
	var output [deadline.Capacity]deadline.Entry
	count := index.Expired(now, output[:])
	tasks := make([]sched.TaskID, count)
	for position := range tasks {
		tasks[position] = output[position].Task
	}
	return tasks
}

// The index holds a deadline exactly while its registration lasts: a wake
// or a cancel takes it out, and expiry reports only registrations still
// waiting.
func TestDeadlinesFollowRegistrations(t *testing.T) {
	var index deadline.Index
	var set WaitSet
	set.UseDeadlines(&index, deadline.SourceChannel)
	woken, cancelled, waiting := taskID(t, 1, 1), taskID(t, 2, 1), taskID(t, 3, 1)
	set.Add(woken, 7, 0, 10)
	set.Add(cancelled, 8, 0, 10)
	set.Add(waiting, 9, 0, 30)
	set.Add(taskID(t, 4, 1), 9, 1, ^uint64(0))
	if index.Len() != 3 {
		t.Fatalf("%d deadlines indexed, want 3: a wait without one is not indexed", index.Len())
	}
	var results [MaxWaiters]WaitResult
	set.Wake(7, 0, WaitMessage, results[:])
	set.Cancel(cancelled)
	if got := due(&index, 20); len(got) != 0 {
		t.Fatalf("ended registrations still expire: %v", got)
	}
	got := due(&index, 30)
	if len(got) != 1 || got[0] != waiting {
		t.Fatalf("due %v, want %v", got, waiting)
	}
	if result, expired := set.ExpireTask(waiting, 30); !expired || result.Reason != WaitTimeout {
		t.Fatal("the due registration did not time out")
	}
}

// Readiness and the timeout race for one wait: whichever comes first
// completes it, and the other finds nothing.
func TestReadinessAndTimeoutCompleteOnce(t *testing.T) {
	var index deadline.Index
	var set WaitSet
	set.UseDeadlines(&index, deadline.SourceStream)
	var results [MaxWaiters]WaitResult
	ready := taskID(t, 5, 1)
	set.Add(ready, 7, 0, 10)
	if set.Wake(7, 0, WaitMessage, results[:]) != 1 {
		t.Fatal("readiness lost")
	}
	if _, expired := set.ExpireTask(ready, 10); expired {
		t.Fatal("a wait readiness completed also timed out")
	}
	late := taskID(t, 6, 1)
	set.Add(late, 8, 0, 10)
	for _, task := range due(&index, 10) {
		if _, expired := set.ExpireTask(task, 10); !expired {
			t.Fatal("the timeout lost to nothing")
		}
	}
	if set.Wake(8, 0, WaitMessage, results[:]) != 0 {
		t.Fatal("a wait the timeout completed was woken again")
	}
}

// A deadline a task left behind never fires for the next task in its slot.
func TestDeadlineOfReusedSlot(t *testing.T) {
	var index deadline.Index
	var set WaitSet
	set.UseDeadlines(&index, deadline.SourceChannel)
	old, next := taskID(t, 9, 1), taskID(t, 9, 2)
	set.Add(old, 7, 0, 10)
	set.Cancel(old)
	set.Add(next, 7, 0, 50)
	if got := due(&index, 10); len(got) != 0 {
		t.Fatalf("the old task's deadline fired: %v", got)
	}
	if _, expired := set.ExpireTask(old, 60); expired {
		t.Fatal("a stale identity expired the slot's new wait")
	}
	if got := due(&index, 50); len(got) != 1 || got[0] != next {
		t.Fatalf("due %v, want the new task", got)
	}
}

// Many waits with deadlines spread over time expire in order, each once.
func TestManyDeadlinesExpire(t *testing.T) {
	var index deadline.Index
	var set WaitSet
	set.UseDeadlines(&index, deadline.SourceChannel)
	for slot := uint32(0); slot < MaxWaiters; slot++ {
		set.Add(taskID(t, slot, 1), uint64(slot+1), 0, uint64(100-slot))
	}
	expired := 0
	for now := uint64(0); now <= 100; now++ {
		for _, task := range due(&index, now) {
			if _, ok := set.ExpireTask(task, now); !ok {
				t.Fatalf("task %v reported due at %d but did not expire", task, now)
			}
			expired++
		}
	}
	if expired != MaxWaiters || set.Count() != 0 || index.Len() != 0 {
		t.Fatalf("expired %d, left %d registrations and %d deadlines", expired, set.Count(), index.Len())
	}
}
