package capability

import "testing"

func TestCapabilityCannotEscalateRights(t *testing.T) {
	parent := New(42, Read|Write)
	child, ok := parent.Derive(Read)
	if !ok || child.Object() != 42 || !child.Allows(Read) || child.Allows(Write) {
		t.Fatalf("unexpected derived capability: %#v, ok=%v", child, ok)
	}
	if _, ok := parent.Derive(Read | Execute); ok {
		t.Fatal("derived capability escalated rights")
	}
}

func TestEmptyRightSetIsRejected(t *testing.T) {
	cap := New(1, Read)
	if cap.Allows(0) {
		t.Fatal("empty rights must not authorize an operation")
	}
	if _, ok := cap.Derive(0); ok {
		t.Fatal("empty derived capability must be rejected")
	}
}

func TestClockSetIsExplicitAuthority(t *testing.T) {
	clock := New(5, ClockSet)
	if !clock.Allows(ClockSet) || clock.Allows(Write) || New(5, Write).Allows(ClockSet) {
		t.Fatal("clock-setting authority aliases generic write authority")
	}
}
