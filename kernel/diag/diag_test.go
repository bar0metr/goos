package diag

import "testing"

func TestRingKeepsNewestInOrder(t *testing.T) {
	if !Enabled {
		t.Skip("diagnostics are compiled out without the goos_diag tag")
	}
	var ring Ring
	for tick := uint64(1); tick <= RingSize+5; tick++ {
		ring.Record(Event{Time: tick, Kind: WaitBegin})
	}
	var seen []uint64
	for index := 0; index < ring.Len(); index++ {
		seen = append(seen, ring.At(index).Time)
	}
	if len(seen) != RingSize || seen[0] != 6 || seen[len(seen)-1] != RingSize+5 {
		t.Fatalf("retained %d events from %d to %d", len(seen), seen[0], seen[len(seen)-1])
	}
}

func TestRingCompilesAwayWhenDisabled(t *testing.T) {
	if Enabled {
		t.Skip("diagnostics enabled")
	}
	var ring Ring
	ring.Record(Event{Kind: WaitBegin})
	if ring.Total() != 0 {
		t.Fatal("disabled build recorded an event")
	}
}

func TestNamesCoverEveryKind(t *testing.T) {
	for kind := WaitBegin; kind <= ProcessSpawned; kind++ {
		if kind.String() == "unknown" {
			t.Fatalf("kind %d has no name", kind)
		}
	}
	for deny := DenyOwner; deny <= DenyType; deny++ {
		if deny.String() == "unknown" {
			t.Fatalf("deny %d has no name", deny)
		}
	}
}
