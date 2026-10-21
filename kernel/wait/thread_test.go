package wait

import "testing"

func TestThreadWakeRegistrationAndGeneration(t *testing.T) {
	var table Table
	waiter := task(0, 1)
	oldThread, nextThread := task(3, 1), task(3, 2)
	key := Key{Kind: KindThread, Object: uint64(oldThread)}
	if !table.Begin(waiter, []Entry{{Key: key, Events: Readable}}, 0) {
		t.Fatal("native thread wake wait was rejected")
	}
	if _, ready := table.Take(waiter); ready || table.Count() != 1 || table.RegistrationCount() != 1 {
		t.Fatal("thread wait completed before its wake")
	}
	var completed [1]Completion
	if table.Publish(Key{Kind: KindThread, Object: uint64(nextThread)}, Readable, completed[:]) != 0 {
		t.Fatal("another thread generation woke the waiter")
	}
	if table.Publish(key, Readable, completed[:]) != 1 || completed[0].Task != waiter {
		t.Fatal("thread wake did not publish its completion")
	}
	result, ready := table.Take(waiter)
	if !ready || result.ReadyMask != 1 || result.Event != Readable || result.TimedOut {
		t.Fatalf("thread wake result = %+v, %v", result, ready)
	}
	if _, ready := table.Take(waiter); ready || table.Count() != 0 || table.RegistrationCount() != 0 {
		t.Fatal("thread wake did not retire exactly once")
	}
}

func TestThreadEndKeyIsValidAndGenerationSafe(t *testing.T) {
	var table Table
	waiter := task(0, 1)
	ended, later := task(4, 1), task(4, 2)
	key := Key{Kind: KindThreadEnd, Object: uint64(ended)}
	if !ValidEntries([]Entry{{Key: key, Events: Exited}}) {
		t.Fatal("validation rejects the thread-end kind")
	}
	if ValidEntries([]Entry{{Key: Key{Kind: lastKind + 1, Object: 1}, Events: Exited}}) {
		t.Fatal("validation admits a kind past the last")
	}
	// A wake flag and an end of the same thread are different objects.
	both := []Entry{{Key: key, Events: Exited}, {Key: Key{Kind: KindThread, Object: uint64(ended)}, Events: Readable}}
	if !table.Begin(waiter, both, 0) {
		t.Fatal("thread-end wait was rejected")
	}
	var completed [1]Completion
	if table.Publish(Key{Kind: KindThreadEnd, Object: uint64(later)}, Exited, completed[:]) != 0 {
		t.Fatal("a later thread in the slot completed the wait")
	}
	if table.Publish(key, Readable, completed[:]) != 0 {
		t.Fatal("an event the interest does not name completed the wait")
	}
	if table.Publish(key, Exited, completed[:]) != 1 || completed[0].Task != waiter {
		t.Fatal("thread end did not complete the wait")
	}
	result, ready := table.Take(waiter)
	if !ready || result.ReadyMask != 1 || result.Event != Exited || result.TimedOut {
		t.Fatalf("thread end result = %+v, %v", result, ready)
	}
}
