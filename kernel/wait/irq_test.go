package wait

import "testing"

func TestDiskAndNetworkLinesAreSeparateInterests(t *testing.T) {
	var table Table
	waiter := task(2, 1)
	const netLine, diskLine = 19, 11
	entries := []Entry{
		{Key: Key{Kind: KindIRQ, Object: netLine}, Events: Readable},
		{Key: Key{Kind: KindIRQ, Object: diskLine}, Events: Readable},
	}
	if !ValidEntries(entries) || !table.Begin(waiter, entries, 0) {
		t.Fatal("a wait on both lines was rejected")
	}
	var completed [1]Completion
	if table.Publish(Key{Kind: KindIRQ, Object: diskLine}, Readable, completed[:]) != 1 {
		t.Fatal("the disk line did not complete the wait")
	}
	result, ready := table.Take(waiter)
	if !ready || result.ReadyMask != 2 || result.Event != Readable {
		t.Fatalf("disk line result = %+v, %v", result, ready)
	}
	if ValidEntries([]Entry{entries[1], entries[1]}) {
		t.Fatal("one line named twice was accepted")
	}
}
