package klog

import (
	"goos/internal/logfmt"
	"testing"
)

func TestRingKeepsNewestRecordsInOrder(t *testing.T) {
	var ring Ring
	for index := uint64(1); index <= Capacity+3; index++ {
		ring.Append(logfmt.Record{Time: index, Level: logfmt.Info, Component: []byte("kernel"), Message: []byte("event")})
	}
	if ring.Len() != Capacity || ring.Total() != Capacity+3 {
		t.Fatalf("len %d total %d", ring.Len(), ring.Total())
	}
	first, _ := ring.At(0)
	last, _ := ring.At(Capacity - 1)
	if first.Time != 4 || last.Time != Capacity+3 || string(first.Component) != "kernel" {
		t.Fatalf("first %+v last %+v", first, last)
	}
	if _, ok := ring.At(Capacity); ok {
		t.Fatal("index past the ring accepted")
	}
}
