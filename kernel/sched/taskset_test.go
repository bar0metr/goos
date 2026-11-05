package sched

import (
	"math/rand"
	"testing"
)

func setAffinity(core *Core, slot int, mask uint64) bool {
	_, ok := core.SetAffinity(slot, mask, 0)
	return ok
}

func snapshotOf(core *Core) Snapshot {
	var view Snapshot
	core.Snapshot(&view)
	return view
}

func TestTaskSetBoundaries(t *testing.T) {
	if MaxTasks%64 != 0 || TaskWords*64 != MaxTasks {
		t.Fatalf("MaxTasks %d is not a whole number of words", MaxTasks)
	}
	for _, slot := range []int{0, 1, 62, 63, 64, 65, 127, 128, 191, 192, MaxTasks - 1} {
		var set TaskSet
		if set.Has(slot) || !set.Empty() || set.First() != -1 || set.NextAfter(slot) != -1 {
			t.Fatalf("empty set answers for %d", slot)
		}
		set.Add(slot)
		if !set.Has(slot) || set.Empty() || set.Count() != 1 || set.First() != slot {
			t.Fatalf("slot %d: has %v count %d first %d", slot, set.Has(slot), set.Count(), set.First())
		}
		if set.Has(slot-1) || set.Has(slot+1) {
			t.Fatalf("slot %d leaks into a neighbour", slot)
		}
		if set[slot/64] != uint64(1)<<uint(slot%64) {
			t.Fatalf("slot %d in word %d is %#x", slot, slot/64, set[slot/64])
		}
		// The only member is found from anywhere, itself included.
		for _, after := range []int{-1, 0, slot - 1, slot, slot + 1, MaxTasks - 1, MaxTasks} {
			if next := set.NextAfter(after); next != slot {
				t.Fatalf("NextAfter(%d) of {%d} = %d", after, slot, next)
			}
		}
		set.Remove(slot)
		if !set.Empty() {
			t.Fatalf("slot %d not removed", slot)
		}
	}
	var set TaskSet
	for _, slot := range []int{-1, MaxTasks, MaxTasks + 64} {
		set.Add(slot)
		if !set.Empty() || set.Has(slot) {
			t.Fatalf("out-of-range slot %d changed the set", slot)
		}
	}
}

func TestTaskSetNextAfterWalksAcrossWords(t *testing.T) {
	members := []int{3, 63, 64, 127, 200, 255}
	var set TaskSet
	for _, slot := range members {
		set.Add(slot)
	}
	walk := -1
	for index := range members {
		walk = set.NextAfter(walk)
		if walk != members[index] {
			t.Fatalf("step %d reached %d, want %d", index, walk, members[index])
		}
	}
	if next := set.NextAfter(walk); next != members[0] {
		t.Fatalf("walk did not wrap: %d", next)
	}
	if set.Count() != len(members) {
		t.Fatalf("count %d", set.Count())
	}
}

func TestTaskSetOperationsMatchAModel(t *testing.T) {
	random := rand.New(rand.NewSource(7))
	for round := 0; round < 200; round++ {
		var a, b TaskSet
		var modelA, modelB [MaxTasks]bool
		for i := 0; i < 40; i++ {
			slot := random.Intn(MaxTasks)
			a.Add(slot)
			modelA[slot] = true
			slot = random.Intn(MaxTasks)
			b.Add(slot)
			modelB[slot] = true
		}
		and, andNot, or := a.And(b), a.AndNot(b), a.Or(b)
		intersects, subset := false, true
		for slot := 0; slot < MaxTasks; slot++ {
			if and.Has(slot) != (modelA[slot] && modelB[slot]) || andNot.Has(slot) != (modelA[slot] && !modelB[slot]) ||
				or.Has(slot) != (modelA[slot] || modelB[slot]) {
				t.Fatalf("round %d slot %d: set operations disagree with the model", round, slot)
			}
			intersects = intersects || modelA[slot] && modelB[slot]
			subset = subset && (!modelA[slot] || modelB[slot])
		}
		if a.Intersects(&b) != intersects || a.SubsetOf(&b) != subset || !and.SubsetOf(&a) || !a.SubsetOf(&or) {
			t.Fatalf("round %d: Intersects or SubsetOf disagree with the model", round)
		}
		union := a
		union.Union(&b)
		if union != or {
			t.Fatalf("round %d: Union differs from Or", round)
		}
	}
}

func TestLowTasks(t *testing.T) {
	for _, count := range []int{0, 1, 63, 64, 65, 128, MaxTasks} {
		set := lowTasks(count)
		if set.Count() != count {
			t.Fatalf("lowTasks(%d) holds %d", count, set.Count())
		}
		if count > 0 && (!set.Has(count-1) || set.Has(count)) {
			t.Fatalf("lowTasks(%d) has the wrong top", count)
		}
	}
}

// fillCore installs and starts every free slot, so the core holds MaxTasks
// tasks.
func fillCore(t *testing.T, core *Core, self int) []int {
	t.Helper()
	var slots []int
	for core.FreeSlots() > 0 {
		slots = append(slots, startTask(t, core, self))
	}
	return slots
}

func TestCoreHoldsMaxTasksAndRefusesOneMore(t *testing.T) {
	core := newCore(t, 4)
	slots := fillCore(t, core, 0)
	if len(slots) != MaxTasks-4 || snapshotOf(core).Count != MaxTasks {
		t.Fatalf("filled %d slots, count %d", len(slots), snapshotOf(core).Count)
	}
	// Install's refusal is the documented error: no slot, no generation.
	if slot, generation, ok := core.Install(); ok || slot != 0 || generation != 0 {
		t.Fatalf("task %d installed past MaxTasks (%d, %d)", MaxTasks+1, slot, generation)
	}
	last := slots[len(slots)-1]
	if last != MaxTasks-1 {
		t.Fatalf("last slot %d", last)
	}
	id := core.TaskID(last)
	if resolved, ok := core.Alive(id); !ok || resolved != last {
		t.Fatalf("slot %d does not resolve: %d %v", last, resolved, ok)
	}

	// Release one slot in each word and take them back.
	released := []int{63, 64, 127, 128, MaxTasks - 1}
	for _, slot := range released {
		if _, ok := core.Terminate(slot, 0); !ok {
			t.Fatalf("terminate %d refused", slot)
		}
	}
	for {
		slot, ok := core.TakeExited()
		if !ok {
			break
		}
		if !core.Vacate(slot) {
			t.Fatalf("vacate %d refused", slot)
		}
	}
	check(t, core)
	if core.FreeSlots() != len(released) {
		t.Fatalf("free slots %d, want %d", core.FreeSlots(), len(released))
	}
	if _, ok := core.Alive(id); ok {
		t.Fatal("a vacated slot's old task still resolves")
	}
	for _, want := range released {
		slot, generation, ok := core.Install()
		if !ok || slot != want || generation != 2 {
			t.Fatalf("reuse gave slot %d generation %d ok %v, want slot %d generation 2", slot, generation, ok, want)
		}
	}
	if _, _, ok := core.Install(); ok {
		t.Fatal("install past MaxTasks after reuse")
	}
}

func TestSelectionCoversEveryWord(t *testing.T) {
	core := newCore(t, 2)
	// Park the kernel tasks so processor 0 has only the tasks below.
	core.runnable[0] &^= 0x7
	for core.FreeSlots() > 0 {
		if _, _, ok := core.Install(); !ok {
			t.Fatal("install refused")
		}
	}
	// Start one task per word, all on processor 1.
	picked := []int{5, 70, 140, 250}
	for _, slot := range picked {
		if !setAffinity(core, slot, bit(1)) {
			t.Fatalf("affinity of %d refused", slot)
		}
		if _, ok := core.Start(slot, 1); !ok {
			t.Fatalf("start %d refused", slot)
		}
		if home := core.homeOf(slot); home != 1 {
			t.Fatalf("slot %d queued on %d", slot, home)
		}
	}
	check(t, core)
	seen := map[int]int{}
	for round := 0; round < 2*len(picked); round++ {
		next := switchTo(core, 1, Preempt)
		check(t, core)
		seen[next]++
	}
	for _, slot := range picked {
		if seen[slot] != 2 {
			t.Fatalf("slot %d ran %d times in two rotations: %v", slot, seen[slot], seen)
		}
	}
}

func TestStealReachesHighWords(t *testing.T) {
	core := newCore(t, 3)
	core.runnable[0] &^= 0x7
	for core.FreeSlots() > 0 {
		if _, _, ok := core.Install(); !ok {
			t.Fatal("install refused")
		}
	}
	// Two tasks in the top word on processor 1; processor 2 steals one.
	for _, slot := range []int{200, 255} {
		setAffinity(core, slot, bit(1)|bit(2))
		if _, ok := core.Start(slot, 1); !ok {
			t.Fatalf("start %d refused", slot)
		}
	}
	first := switchTo(core, 1, Preempt)
	if first != 200 && first != 255 {
		t.Fatalf("processor 1 picked %d", first)
	}
	stolen := switchTo(core, 2, Preempt)
	if stolen == first || (stolen != 200 && stolen != 255) || core.homeOf(stolen) != 2 {
		t.Fatalf("processor 2 stole %d (home %d), processor 1 runs %d", stolen, core.homeOf(stolen), first)
	}
	check(t, core)
}

func TestAffinityAcrossWords(t *testing.T) {
	core := newCore(t, 4)
	for core.FreeSlots() > 0 {
		if _, _, ok := core.Install(); !ok {
			t.Fatal("install refused")
		}
	}
	for _, slot := range []int{63, 64, 127, 128, 255} {
		target := 1 + slot%3
		if _, ok := core.Start(slot, 0); !ok {
			t.Fatalf("start %d refused", slot)
		}
		if _, ok := core.SetAffinity(slot, bit(target), 0); !ok {
			t.Fatalf("affinity of %d refused", slot)
		}
		if home := core.homeOf(slot); home != target {
			t.Fatalf("slot %d queued on %d, want %d", slot, home, target)
		}
		if core.Affinity(slot) != bit(target) {
			t.Fatalf("slot %d affinity %#x", slot, core.Affinity(slot))
		}
		for cpu := 1; cpu < 4; cpu++ {
			if core.allows(slot, cpu) != (cpu == target) || core.barred[cpu].Has(slot) == (cpu == target) {
				t.Fatalf("slot %d on processor %d: allows %v", slot, cpu, core.allows(slot, cpu))
			}
		}
		// Neighbouring slots in other words keep their freedom.
		for _, other := range []int{slot - 1, slot + 1} {
			if other < MaxTasks && other >= 4 && core.affinity[other] == 0 {
				for cpu := 1; cpu < 4; cpu++ {
					if core.barred[cpu].Has(other) {
						t.Fatalf("slot %d barred from %d by slot %d's affinity", other, cpu, slot)
					}
				}
			}
		}
		check(t, core)
	}
	// Every processor runs only what its affinity let it have.
	for round := 0; round < 8; round++ {
		for cpu := 1; cpu < 4; cpu++ {
			if next := switchTo(core, cpu, Preempt); next != Idle && core.Affinity(next)&bit(cpu) == 0 {
				t.Fatalf("processor %d ran %d against its affinity", cpu, next)
			}
			check(t, core)
		}
	}
}
