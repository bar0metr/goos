package wait

import (
	"testing"

	"goos/kernel/sched"
)

// A publication reaches waits in every word of the task set, in slot order,
// and a slot whose wait was taken or cancelled stops being visited.
func TestPublishReachesEverySlotWord(t *testing.T) {
	var table Table
	slots := []uint32{0, 63, 64, 127, 128, sched.MaxTasks - 1}
	for _, slot := range slots {
		if !table.Begin(task(slot, 1), []Entry{entry(KindChannel, 9, 0, Readable)}, 0) {
			t.Fatalf("slot %d refused", slot)
		}
	}
	output := make([]Completion, len(slots))
	if completed := table.Publish(Key{Kind: KindChannel, Object: 9}, Readable, output); completed != len(slots) {
		t.Fatalf("published to %d waits, want %d", completed, len(slots))
	}
	for index, slot := range slots {
		if output[index].Task != task(slot, 1) {
			t.Fatalf("completion %d names %v, want slot %d", index, output[index].Task, slot)
		}
	}
	for index, slot := range slots {
		id := task(slot, 1)
		if index%2 == 0 {
			if _, ok := table.Take(id); !ok {
				t.Fatalf("take %d failed", slot)
			}
		} else if !table.Cancel(id) {
			t.Fatalf("cancel %d failed", slot)
		}
	}
	if !table.inUse.Empty() || table.Count() != 0 {
		t.Fatalf("slots left in use: %d active", table.Count())
	}
	if !table.Begin(task(sched.MaxTasks-1, 2), []Entry{entry(KindChannel, 9, 0, Readable)}, 0) {
		t.Fatal("reused slot refused")
	}
	if completed := table.Publish(Key{Kind: KindChannel, Object: 9}, Readable, output); completed != 1 || output[0].Task != task(sched.MaxTasks-1, 2) {
		t.Fatalf("publication after reuse completed %d: %v", completed, output[0].Task)
	}
}
