package wait

import "testing"

func TestValidEntriesMatchesRegistrationContract(t *testing.T) {
	first := entry(KindStream, 7, 0, Readable)
	second := entry(KindStream, 8, 0, Readable|PeerClosed)
	for _, entries := range [][]Entry{nil, {first}, {first, second}} {
		if !ValidEntries(entries) {
			t.Fatalf("valid entries rejected: %+v", entries)
		}
	}
	// Handles with different rights or IDs can resolve to the same Key.
	alias := first
	alias.Events = PeerClosed
	for _, entries := range [][]Entry{
		{first, first}, {first, alias}, {entry(0, 1, 0, Readable)},
		{entry(KindThread, 1, 1, Readable)}, make([]Entry, MaxObjects+1),
	} {
		if ValidEntries(entries) {
			t.Fatalf("invalid entries accepted: %+v", entries)
		}
		var table Table
		if table.Begin(task(0, 1), entries, 10) {
			t.Fatal("Begin disagrees with the shared validation")
		}
	}
	var table Table
	if !ValidEntries(nil) || table.Begin(task(0, 1), nil, 0) || !table.Begin(task(0, 1), nil, 10) {
		t.Fatal("entry validation incorrectly includes the empty-wait deadline rule")
	}
}
