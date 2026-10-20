package region

import "testing"

func TestStackBatchGuardsAndMutation(t *testing.T) {
	m := newMap(t)
	r := Region{Base: lower, Length: 32 * StackStride, Protection: Read | Write, Backing: Anonymous, Flags: StackBatch}
	if got := m.Insert(r); got != OK {
		t.Fatalf("insert: %v", got)
	}
	for slot := uint64(0); slot < 32; slot++ {
		base := r.Base + slot*StackStride
		for offset := uint64(0); offset < StackStride; offset += PageSize {
			if got := r.GuardedAt(base + offset); got != (offset < StackGuardBytes) {
				t.Fatalf("slot %d offset %#x: guard=%v", slot, offset, got)
			}
		}
	}
	if r.GuardedAt(r.Base-1) || r.GuardedAt(r.End()) {
		t.Fatal("guard outside reservation")
	}
	if got := m.Remove(lower+StackStride, StackStride, false); got != PermissionDenied {
		t.Fatalf("partial removal: %v", got)
	}
	if got := m.Protect(lower, r.Length, Read|Write); got != PermissionDenied {
		t.Fatalf("protect batch: %v", got)
	}
	if got := m.Restrict(lower, StackGuardBytes, 0); got != PermissionDenied {
		t.Fatalf("restrict batch: %v", got)
	}
	expect(t, m, r)
	if got := m.Remove(lower, r.Length, false); got != OK || m.Count() != 0 {
		t.Fatalf("whole removal: %v count=%d", got, m.Count())
	}
}

func TestStackBatchValidation(t *testing.T) {
	for _, r := range []Region{
		{Base: lower, Length: PageSize, Protection: Read | Write, Backing: Anonymous, Flags: StackBatch},
		{Base: lower, Length: StackStride, Protection: Read, Backing: Anonymous, Flags: StackBatch},
		{Base: lower, Length: StackStride, Protection: Read | Write, Backing: Shared, Flags: StackBatch},
	} {
		m := newMap(t)
		if got := m.Insert(r); got != InvalidArgument || m.Count() != 0 {
			t.Fatalf("accepted %+v: %v", r, got)
		}
	}
}
