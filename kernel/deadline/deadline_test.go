package deadline

import (
	"math/rand"
	"sort"
	"testing"

	"goos/kernel/sched"
)

func task(t *testing.T, slot, generation uint32) sched.TaskID {
	t.Helper()
	id, ok := sched.NewTaskID(slot, generation)
	if !ok {
		t.Fatal("task identity")
	}
	return id
}

func drain(index *Index, now uint64) []Entry {
	var all []Entry
	var batch [7]Entry
	for {
		count := index.Expired(now, batch[:])
		all = append(all, batch[:count]...)
		if count < len(batch) {
			return all
		}
	}
}

func TestExpiresInOrderAndOnlyWhatIsDue(t *testing.T) {
	var index Index
	if index.Earliest() != Never || index.Expired(1<<60, make([]Entry, 4)) != 0 {
		t.Fatal("an empty index has a deadline")
	}
	times := []uint64{50, 10, 40, 30, 20}
	for slot, at := range times {
		index.Arm(SourceChannel, task(t, uint32(slot), 1), at)
	}
	if index.Earliest() != 10 {
		t.Fatalf("earliest %d, want 10", index.Earliest())
	}
	got := drain(&index, 35)
	if len(got) != 3 || got[0].At != 10 || got[1].At != 20 || got[2].At != 30 {
		t.Fatalf("expired %+v", got)
	}
	if index.Len() != 2 || index.Earliest() != 40 {
		t.Fatalf("left %d, earliest %d", index.Len(), index.Earliest())
	}
}

func TestEqualDeadlinesAllExpire(t *testing.T) {
	var index Index
	for slot := uint32(0); slot < 10; slot++ {
		index.Arm(Source(1+slot%5), task(t, slot, 3), 100)
	}
	got := drain(&index, 100)
	if len(got) != 10 {
		t.Fatalf("%d of 10 equal deadlines expired", len(got))
	}
	for position := 1; position < len(got); position++ {
		if got[position-1].Source > got[position].Source {
			t.Fatal("equal deadlines came out of their fixed order")
		}
	}
}

func TestCancelAndRearm(t *testing.T) {
	var index Index
	first, second := task(t, 1, 1), task(t, 2, 1)
	index.Arm(SourceStream, first, 10)
	index.Arm(SourceStream, second, 20)
	if !index.Cancel(SourceStream, first) || index.Cancel(SourceStream, first) {
		t.Fatal("cancel did not remove exactly once")
	}
	index.Arm(SourceStream, second, 5)
	if index.Len() != 1 || index.Earliest() != 5 {
		t.Fatalf("rearm left %d entries, earliest %d", index.Len(), index.Earliest())
	}
	index.Arm(SourceStream, second, Never)
	if index.Len() != 0 || index.Earliest() != Never {
		t.Fatal("arming Never did not cancel")
	}
}

// An entry a task left behind never touches the slot's next task: the
// older identity can neither cancel the new entry nor be reported for it.
func TestStaleIdentityAfterSlotReuse(t *testing.T) {
	var index Index
	old, reused := task(t, 4, 1), task(t, 4, 2)
	index.Arm(SourceProcess, old, 10)
	index.Arm(SourceProcess, reused, 30)
	if index.Len() != 1 {
		t.Fatal("one slot holds two deadlines in one source")
	}
	if index.Cancel(SourceProcess, old) {
		t.Fatal("a stale identity cancelled the slot's new deadline")
	}
	got := drain(&index, 30)
	if len(got) != 1 || got[0].Task != reused || got[0].At != 30 {
		t.Fatalf("expired %+v, want the new task at 30", got)
	}
}

func TestFullCapacity(t *testing.T) {
	var index Index
	for source := SourceChannel; source <= SourcePaging; source++ {
		for slot := uint32(0); slot < sched.MaxTasks; slot++ {
			if !index.Arm(source, task(t, slot, 1), uint64(1000-slot)) {
				t.Fatal("arm refused below capacity")
			}
		}
	}
	if index.Len() != Capacity {
		t.Fatalf("%d entries, want %d", index.Len(), Capacity)
	}
	if index.Arm(0, task(t, 0, 1), 5) || index.Arm(SourcePaging+1, task(t, 0, 1), 5) || index.Arm(SourceChannel, 0, 5) {
		t.Fatal("an invalid source or task was indexed")
	}
	if got := drain(&index, 1000); len(got) != Capacity {
		t.Fatalf("%d expired, want %d", len(got), Capacity)
	}
}

// A random sequence of arms, cancels, and expiries agrees with a plain map.
func TestRandomAgainstModel(t *testing.T) {
	random := rand.New(rand.NewSource(7))
	var index Index
	type slotKey struct {
		source Source
		slot   uint32
	}
	model := map[slotKey]Entry{}
	now := uint64(0)
	for step := 0; step < 20000; step++ {
		source := Source(1 + random.Intn(sourceCount))
		slot := uint32(random.Intn(sched.MaxTasks))
		id := task(t, slot, uint32(1+random.Intn(3)))
		switch random.Intn(4) {
		case 0, 1:
			at := now + 1 + uint64(random.Intn(50))
			index.Arm(source, id, at)
			model[slotKey{source, slot}] = Entry{At: at, Source: source, Task: id}
		case 2:
			if index.Cancel(source, id) {
				if model[slotKey{source, slot}].Task != id {
					t.Fatal("cancel removed another task's entry")
				}
				delete(model, slotKey{source, slot})
			}
		case 3:
			now += uint64(random.Intn(20))
			var want []Entry
			for key, entry := range model {
				if entry.At <= now {
					want = append(want, entry)
					delete(model, key)
				}
			}
			sort.Slice(want, func(i, j int) bool { return want[i].At < want[j].At })
			got := drain(&index, now)
			if len(got) != len(want) {
				t.Fatalf("step %d: %d expired, want %d", step, len(got), len(want))
			}
			for position := range got {
				if got[position].At != want[position].At {
					t.Fatalf("step %d: expiry %d at %d, want %d", step, position, got[position].At, want[position].At)
				}
			}
		}
		if index.Len() != len(model) {
			t.Fatalf("step %d: %d entries, model %d", step, index.Len(), len(model))
		}
	}
}

// BenchmarkNothingDue is the housekeeping tick with a full index and nothing
// due: one atomic load, however many waiters there are.
func BenchmarkNothingDue(b *testing.B) {
	var index Index
	for source := SourceChannel; source <= SourcePaging; source++ {
		for slot := uint32(0); slot < sched.MaxTasks; slot++ {
			id, _ := sched.NewTaskID(slot, 1)
			index.Arm(source, id, 1<<40)
		}
	}
	var output [8]Entry
	b.ResetTimer()
	for round := 0; round < b.N; round++ {
		if index.Expired(uint64(round), output[:]) != 0 {
			b.Fatal("a deadline was due")
		}
	}
}

// BenchmarkScanAll is what the tick cost before the index: a look at every
// registration of every table.
func BenchmarkScanAll(b *testing.B) {
	var deadlines [Capacity]uint64
	for position := range deadlines {
		deadlines[position] = 1 << 40
	}
	b.ResetTimer()
	for round := 0; round < b.N; round++ {
		for _, at := range deadlines {
			if at <= uint64(round) {
				b.Fatal("a deadline was due")
			}
		}
	}
}
