package goroutine

import "testing"

func TestSleepHeapRemoveAndOrdering(t *testing.T) {
	for remove := uint32(0); remove < 47; remove++ {
		h := new(sleepHeap)
		for i := uint32(0); i < 47; i++ {
			if !h.push(sleepEntry{when: int64((i * 13) % 47), index: i, gen: 3, seq: 9, kind: sleepWaitTimeout}) {
				t.Fatal("heap full")
			}
		}
		if h.remove(remove, 2, 9) || h.remove(remove, 3, 8) {
			t.Fatal("removed a different generation or wait")
		}
		if !h.remove(remove, 3, 9) || h.remove(remove, 3, 9) {
			t.Fatalf("remove %d", remove)
		}
		last := int64(-1)
		for h.count != 0 {
			e := h.pop()
			if e.when < last || e.index == remove {
				t.Fatalf("after removing %d: %+v after %d", remove, e, last)
			}
			last = e.when
		}
	}
}

func TestSleepHeapCanceledWaitReleasesCapacity(t *testing.T) {
	h := new(sleepHeap)
	for seq := uint32(0); seq < 10*MaxGoroutines; seq++ {
		if !h.push(sleepEntry{when: 1 << 60, index: 0, gen: 1, seq: seq, kind: sleepWaitTimeout}) || !h.remove(0, 1, seq) {
			t.Fatalf("wait %d leaked its timer", seq)
		}
	}
	if h.count != 0 {
		t.Fatalf("%d timers remain", h.count)
	}
}
