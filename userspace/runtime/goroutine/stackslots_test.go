package goroutine

import "testing"

func TestStackBatchExhaustionAndReuse(t *testing.T) {
	b := stackBatch{base: 0x10000000}
	var slots [stacksPerBatch]uintptr
	for i := range slots {
		base, ok := b.take()
		if !ok || base != b.base+uintptr(i)*(StackSize+GuardSize) {
			t.Fatalf("slot %d: %#x %v", i, base, ok)
		}
		slots[i] = base
	}
	if _, ok := b.take(); ok {
		t.Fatal("accepted a slot past the batch")
	}
	for i := range slots {
		if !b.release(slots[i]) || b.release(slots[i]) {
			t.Fatalf("release slot %d", i)
		}
		base, ok := b.take()
		if !ok || base != slots[i] {
			t.Fatalf("reuse slot %d: %#x %v", i, base, ok)
		}
	}
	for _, bad := range []uintptr{b.base - 1, b.base + 1, b.base + stacksPerBatch*(StackSize+GuardSize)} {
		if b.release(bad) {
			t.Fatalf("released invalid slot %#x", bad)
		}
	}
}
