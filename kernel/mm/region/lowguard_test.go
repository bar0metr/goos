package region

import "testing"

func lowGuarded(base, length uint64) Region {
	return Region{Base: base, Length: length, Protection: Read | Write, Backing: Anonymous, Flags: Pinned | LowGuard}
}

func TestLowGuardInsertValidates(t *testing.T) {
	m := newMap(t)
	stride := StackGuardBytes + 512<<10
	cases := []struct {
		name   string
		region Region
	}{
		{"guard only", lowGuarded(lower, StackGuardBytes)},
		{"read-only", Region{Base: lower, Length: stride, Protection: Read, Backing: Anonymous, Flags: LowGuard}},
		{"image", Region{Base: lower, Length: stride, Protection: Read | Write, Backing: Image, Flags: LowGuard}},
		{"with a stack batch", Region{Base: lower, Length: StackStride, Protection: Read | Write, Backing: Anonymous, Flags: LowGuard | StackBatch}},
	}
	for _, c := range cases {
		if got := m.Insert(c.region); got != InvalidArgument {
			t.Fatalf("%s: Insert = %d, want InvalidArgument", c.name, got)
		}
	}
	if got := m.Insert(lowGuarded(lower, stride)); got != OK {
		t.Fatalf("Insert = %d", got)
	}
}

// The guard is the region's lowest 64 KiB: a demand fault there is a guard
// fault, and VMQuery describes it apart from the stack above it.
func TestLowGuardGuardsItsBase(t *testing.T) {
	stride := StackGuardBytes + 512<<10
	r := lowGuarded(lower, stride)
	for _, c := range []struct {
		address uint64
		guarded bool
	}{
		{lower - 1, false},
		{lower, true},
		{lower + StackGuardBytes - 1, true},
		{lower + StackGuardBytes, false},
		{lower + stride - 1, false},
		{lower + stride, false},
	} {
		if got := r.GuardedAt(c.address); got != c.guarded {
			t.Fatalf("GuardedAt(%#x) = %v, want %v", c.address, got, c.guarded)
		}
	}
	guard := r.PartAt(lower + PageSize)
	if guard.Base != lower || guard.Length != StackGuardBytes || guard.Backing != Guard || guard.Protection != 0 || guard.Flags&Pinned == 0 {
		t.Fatalf("guard part %+v", guard)
	}
	stack := r.PartAt(lower + StackGuardBytes)
	if stack.Base != lower+StackGuardBytes || stack.Length != 512<<10 || stack.Backing != Anonymous || stack.Protection != Read|Write {
		t.Fatalf("stack part %+v", stack)
	}
	plain := anonymous(lower, 4, Read)
	if plain.PartAt(lower+PageSize) != plain {
		t.Fatal("a region without a low guard was cut")
	}
}

// A low-guarded region goes whole or not at all, and keeps its protection,
// even for the kernel.
func TestLowGuardStaysWhole(t *testing.T) {
	m := newMap(t)
	stride := StackGuardBytes + 512<<10
	r := lowGuarded(lower, stride)
	if m.Insert(r) != OK {
		t.Fatal("insert")
	}
	if got := m.Remove(lower, StackGuardBytes, true); got != PermissionDenied {
		t.Fatalf("removing the guard alone = %d", got)
	}
	if got := m.Remove(lower+StackGuardBytes, stride-StackGuardBytes, true); got != PermissionDenied {
		t.Fatalf("removing the stack alone = %d", got)
	}
	if got := m.Restrict(lower+StackGuardBytes, PageSize, Read); got != PermissionDenied {
		t.Fatalf("Restrict = %d", got)
	}
	expect(t, m, r)
	if got := m.Remove(lower, stride, true); got != OK {
		t.Fatalf("removing the whole region = %d", got)
	}
	expect(t, m)
}
