package arena

import "testing"

// Host builds poison what sweep frees, as validation images do: a dead slot
// and the first page of a dead run read freedPoison until reused, a live
// object keeps its bytes, and a reused slot reads zero.
func TestSweepPoisonsFreedMemory(t *testing.T) {
	if !poisonFreed {
		t.Skip("poisoning is off in this build")
	}
	th := newTestHeap(t, 64, 64)
	var objects [8]uintptr
	for index := range objects {
		objects[index] = th.mustAlloc(t, 48)
		*wordAt(objects[index]) = uintptr(0x1000 + index)
		*wordAt(objects[index] + 40) = uintptr(0x2000 + index)
	}
	run := th.mustAlloc(t, 2*pageBytes)
	*wordAt(run) = 0x3000
	*wordAt(run + pageBytes - wordBytes) = 0x3001
	keep := roots{objects[1], objects[4]}
	before := PoisonedBytes()
	th.beginMark()
	keep.mark(&th.heap)
	th.finishMark()
	th.sweep()
	for index, address := range objects {
		live := index == 1 || index == 4
		first, last := *wordAt(address), *wordAt(address + 40)
		switch {
		case live && (first != uintptr(0x1000+index) || last != uintptr(0x2000+index)):
			t.Fatalf("live object %d changed: %#x %#x", index, first, last)
		case !live && (first != freedPoison || last != freedPoison):
			t.Fatalf("dead object %d not poisoned: %#x %#x", index, first, last)
		}
	}
	if *wordAt(run) != freedPoison || *wordAt(run + pageBytes - wordBytes) != freedPoison {
		t.Fatalf("dead run not poisoned: %#x", *wordAt(run))
	}
	if *wordAt(run + pageBytes) != 0 {
		t.Fatalf("poison reached past the first page of a run")
	}
	if got, want := PoisonedBytes()-before, uint64(6*48)+uint64(pageBytes); got != want {
		t.Fatalf("poisoned %d bytes, want %d", got, want)
	}
	reused := th.mustAlloc(t, 48)
	for offset := uintptr(0); offset < 48; offset += wordBytes {
		if *wordAt(reused + offset) != 0 {
			t.Fatalf("reused slot reads %#x at %d", *wordAt(reused + offset), offset)
		}
	}
	th.verify()
}
