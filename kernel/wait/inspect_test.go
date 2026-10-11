package wait

import "testing"

func TestInspectUsesCallerStorageAndTracksCursor(t *testing.T) {
	var table Table
	var view View
	id := task(0, 1)
	entries := []Entry{logEntry(10, 123), entry(KindChannel, 20, 1, Readable)}
	if !table.Begin(id, entries, 77) || !table.Inspect(0, &view) {
		t.Fatal("inspect active registration")
	}
	if view.Task != id || view.Deadline != 77 || !view.Active || view.Complete || view.Count != 2 || view.Entries[0].Cursor != 123 {
		t.Fatalf("view %+v", view)
	}
	entries[0].Cursor = 999
	if view.Entries[0].Cursor != 123 {
		t.Fatal("inspection aliases registration input")
	}
	var output [1]Completion
	table.PublishLog(10, 124, output[:])
	if !table.Inspect(0, &view) || !view.Complete || view.Active || view.Ready[0] != Readable {
		t.Fatal("inspect completed registration")
	}
	if _, ok := table.Take(id); !ok {
		t.Fatal("take")
	}
	if table.Inspect(0, &view) || table.Inspect(-1, &view) || table.Inspect(0, nil) {
		t.Fatal("inspect nonexistent slot")
	}
	if !table.Begin(id, []Entry{entry(KindChannel, 20, 0, PeerClosed)}, 0) || !table.Inspect(0, &view) {
		t.Fatal("reuse slot")
	}
	if view.Entries[0].Cursor != 0 || view.Ready[0] != 0 || view.TimedOut || view.Complete {
		t.Fatal("reset retained cursor/readiness")
	}
	if allocations := testing.AllocsPerRun(100, func() { table.Inspect(0, &view) }); allocations != 0 {
		t.Fatalf("inspect allocations %v", allocations)
	}
}
