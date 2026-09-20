package wait

import (
	"testing"

	"goos/kernel/sched"
)

func task(slot, generation uint32) sched.TaskID {
	id, _ := sched.NewTaskID(slot, generation)
	return id
}

func entry(kind Kind, object uint64, side uint8, events Event) Entry {
	return Entry{Key: Key{Kind: kind, Object: object, Side: side}, Events: events}
}

func TestBeginValidatesBoundsAndDuplicates(t *testing.T) {
	var table Table
	id := task(0, 1)
	if table.Begin(0, []Entry{entry(KindChannel, 1, 0, Readable)}, 0) ||
		table.Begin(id, nil, 0) ||
		table.Begin(id, make([]Entry, MaxObjects+1), 0) ||
		table.Begin(id, []Entry{entry(0, 1, 0, Readable)}, 0) ||
		table.Begin(id, []Entry{entry(KindIRQ, 1, 1, Interrupt)}, 0) ||
		table.Begin(id, []Entry{entry(KindChannel, 1, 0, Readable), entry(KindChannel, 1, 0, Writable)}, 0) {
		t.Fatal("invalid wait accepted")
	}
	entries := make([]Entry, MaxObjects)
	for index := range entries {
		entries[index] = entry(KindProcess, uint64(index+1), 0, Exited)
	}
	if !table.Begin(id, entries, 0) || table.Count() != 1 || table.RegistrationCount() != MaxObjects {
		t.Fatal("bounded wait was not registered")
	}
	if table.Begin(id, entries[:1], 0) {
		t.Fatal("second operation for task accepted")
	}
}

func TestPublishCompletesEveryMatchingTaskAndCleansSiblings(t *testing.T) {
	var table Table
	key := Key{Kind: KindChannel, Object: 7, Side: 0}
	for slot := uint32(0); slot < 2; slot++ {
		if !table.Begin(task(slot, 1), []Entry{{Key: key, Events: Readable}, entry(KindProcess, uint64(slot+1), 0, Exited)}, 0) {
			t.Fatal("begin failed")
		}
	}
	completed := make([]Completion, 2)
	if got := table.Publish(key, Readable, completed); got != 2 || table.Count() != 0 || table.RegistrationCount() != 0 {
		t.Fatalf("publish=%d active=%d registrations=%d", got, table.Count(), table.RegistrationCount())
	}
	if completed[0].Task != task(0, 1) || completed[1].Task != task(1, 1) {
		t.Fatalf("publish order is not deterministic: %+v", completed)
	}
	for slot := uint32(0); slot < 2; slot++ {
		result, ok := table.Take(task(slot, 1))
		if !ok || result.ReadyMask != 1 || result.ReadyIndex != 0 || result.Event != Readable {
			t.Fatalf("unexpected completion: %+v %v", result, ok)
		}
	}
}

func TestCancelRemovesOperation(t *testing.T) {
	var table Table
	id := task(3, 1)
	table.Begin(id, []Entry{entry(KindStream, 2, 1, Readable|PeerClosed)}, 10)
	if !table.Cancel(id) || table.Cancel(id) || table.Count() != 0 || table.RegistrationCount() != 0 {
		t.Fatal("cancel did not remove exactly one operation")
	}
	if _, ok := table.Take(id); ok {
		t.Fatal("cancelled result remained")
	}
}

func TestEventWinsTimeoutRaceOnce(t *testing.T) {
	var table Table
	id := task(4, 1)
	key := Key{Kind: KindIRQ, Object: 9}
	table.Begin(id, []Entry{{Key: key, Events: Interrupt}}, 20)
	completed := make([]Completion, sched.MaxTasks)
	if table.Publish(key, Interrupt, completed) != 1 || table.Expire(20, completed) != 0 || table.Publish(key, Interrupt, completed) != 0 {
		t.Fatal("event did not win exactly once")
	}
	result, ok := table.Take(id)
	if !ok || result.TimedOut || result.Event != Interrupt {
		t.Fatalf("unexpected result: %+v %v", result, ok)
	}

	id = task(5, 1)
	table.Begin(id, []Entry{{Key: key, Events: Interrupt}}, 20)
	if table.Expire(20, completed) != 1 || table.Publish(key, Interrupt, completed) != 0 {
		t.Fatal("timeout did not win exactly once")
	}
	result, ok = table.Take(id)
	if !ok || !result.TimedOut || result.ReadyMask != 0 {
		t.Fatalf("unexpected timeout: %+v %v", result, ok)
	}
}

func TestGenerationIsolationAndStalePublish(t *testing.T) {
	var table Table
	old := task(6, 1)
	current := task(6, 2)
	oldKey := Key{Kind: KindProcess, Object: 11}
	newKey := Key{Kind: KindProcess, Object: 12}
	table.Begin(old, []Entry{{Key: oldKey, Events: Exited}}, 0)
	if !table.Begin(current, []Entry{{Key: newKey, Events: Exited}}, 0) {
		t.Fatal("new generation could not replace stale operation")
	}
	if table.Publish(oldKey, Exited, nil) != 0 || table.Cancel(old) {
		t.Fatal("stale generation affected current operation")
	}
	if table.Publish(newKey, Exited, nil) != 1 {
		t.Fatal("current operation was not completed")
	}
	if _, ok := table.Take(old); ok {
		t.Fatal("stale task took current completion")
	}
	if _, ok := table.Take(current); !ok {
		t.Fatal("current task could not take completion")
	}
}

func TestSimultaneousReadinessUsesMaskAndLowestIndex(t *testing.T) {
	var table Table
	id := task(7, 1)
	first := entry(KindStream, 1, 0, Readable|PeerClosed)
	second := entry(KindChannel, 2, 1, Writable)
	third := entry(KindProcess, 3, 0, Exited)
	table.Begin(id, []Entry{first, second, third}, 0)
	completed := make([]Completion, 1)
	if table.Publish(third.Key, Exited, completed) != 1 || table.Publish(first.Key, PeerClosed|Readable, completed) != 0 {
		t.Fatal("simultaneous publication completed more than once")
	}
	result, ok := table.Take(id)
	if !ok || result.ReadyMask != 0b101 || result.ReadyIndex != 0 || result.Event != Readable|PeerClosed {
		t.Fatalf("unexpected simultaneous result: %+v %v", result, ok)
	}
}
