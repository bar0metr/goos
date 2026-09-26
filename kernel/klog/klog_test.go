package klog

import (
	"goos/internal/logfmt"
	"goos/internal/timekeeping"
	"testing"
)

func TestRingKeepsNewestRecordsInOrder(t *testing.T) {
	var ring Ring
	for index := uint64(1); index <= Capacity+3; index++ {
		ring.Append(logfmt.Record{Time: timekeeping.Time{Seconds: int64(index)}, Level: logfmt.Info, Component: []byte("kernel"), Message: []byte("event")})
	}
	if ring.Len() != Capacity || ring.Total() != Capacity+3 {
		t.Fatalf("len %d total %d", ring.Len(), ring.Total())
	}
	first, _ := ring.At(0)
	last, _ := ring.At(Capacity - 1)
	if first.Time.Seconds != 4 || last.Time.Seconds != Capacity+3 || string(first.Component) != "kernel" {
		t.Fatalf("first %+v last %+v", first, last)
	}
	if _, ok := ring.At(Capacity); ok {
		t.Fatal("index past the ring accepted")
	}
}

func TestOldestTracksWhatWasOverwritten(t *testing.T) {
	var ring Ring
	if ring.Oldest() != 0 || ring.Total() != 0 {
		t.Fatal("an empty ring does not start at zero")
	}
	for index := uint64(1); index <= 5; index++ {
		ring.Append(logfmt.Record{Time: timekeeping.Time{Seconds: int64(index)}, Level: logfmt.Info, Component: []byte("kernel")})
	}
	if ring.Oldest() != 0 || ring.Total() != 5 {
		t.Fatalf("a partly filled ring: oldest %d total %d", ring.Oldest(), ring.Total())
	}
	for index := uint64(0); index < Capacity; index++ {
		ring.Append(logfmt.Record{Time: timekeeping.Time{Seconds: int64(index)}, Level: logfmt.Info, Component: []byte("kernel")})
	}
	// Once the ring has wrapped, the window is the last Capacity records.
	if ring.Total() != Capacity+5 || ring.Oldest() != 5 {
		t.Fatalf("a wrapped ring: oldest %d total %d", ring.Oldest(), ring.Total())
	}
	if ring.Total()-ring.Oldest() != uint64(ring.Len()) {
		t.Fatal("the window does not match the retained count")
	}
}
