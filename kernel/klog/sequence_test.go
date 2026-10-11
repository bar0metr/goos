package klog

import (
	"goos/internal/logfmt"
	"goos/internal/timekeeping"
	"sync"
	"testing"
)

func appendSequence(ring *Ring, sequence uint64) {
	ring.Append(logfmt.Record{Time: timekeeping.Time{Seconds: int64(sequence), Nanoseconds: uint32(sequence)}, Process: sequence + 100, Level: logfmt.Info, Component: []byte("test"), Message: []byte("event"), Fields: []byte("n=1")})
}

func TestAbsoluteReadSnapshotWrapAndIndependentCursors(t *testing.T) {
	var ring Ring
	var copies [Capacity]Copy
	for i := uint64(0); i < Capacity+7; i++ {
		appendSequence(&ring, i)
	}
	oldest, total, first, count := ring.Read(0, copies[:])
	if oldest != 7 || total != Capacity+7 || first != 7 || count != Capacity {
		t.Fatalf("window %d %d %d %d", oldest, total, first, count)
	}
	for i := 0; i < count; i++ {
		record := copies[i].Record()
		if record.Time.Seconds != int64(first+uint64(i)) || record.Process != first+uint64(i)+100 || string(record.Fields) != "n=1" {
			t.Fatalf("record %d %+v", i, record)
		}
	}
	appendSequence(&ring, total)
	if copies[0].Record().Time.Seconds != 7 {
		t.Fatal("snapshot was overwritten")
	}
	_, _, first, count = ring.Read(60, copies[:2])
	if first != 60 || count != 2 || copies[1].Record().Time.Seconds != 61 {
		t.Fatal("absolute reader shifted")
	}
	_, total, first, count = ring.Read(Capacity+7, copies[:])
	if total != Capacity+8 || first != Capacity+7 || count != 1 {
		t.Fatal("independent reader cursor lost")
	}
	_, _, first, count = ring.Read(total+1, copies[:])
	if first != total+1 || count != 0 {
		t.Fatal("future cursor overflow")
	}
	_, _, _, count = ring.Read(0, nil)
	if count != 0 {
		t.Fatal("nil output read records")
	}
}

func TestAbsoluteSnapshotConcurrentAppend(t *testing.T) {
	var ring Ring
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := uint64(0); i < 4000; i++ {
			appendSequence(&ring, i)
		}
	}()
	var copies [Capacity]Copy
	for i := 0; i < 4000; i++ {
		oldest, total, first, count := ring.Read(0, copies[:])
		if first != oldest || total-first != uint64(count) {
			t.Fatal("torn window")
		}
		for j := 0; j < count; j++ {
			if got := copies[j].Record().Time.Seconds; got != int64(first+uint64(j)) {
				t.Fatalf("shifted sequence: %d expected %d", got, first+uint64(j))
			}
		}
	}
	wg.Wait()
}

func TestAbsoluteReadDoesNotAllocate(t *testing.T) {
	var ring Ring
	var copies [Capacity]Copy
	appendSequence(&ring, 0)
	if allocations := testing.AllocsPerRun(100, func() { ring.Read(0, copies[:]) }); allocations != 0 {
		t.Fatalf("allocations %v", allocations)
	}
}

func TestSequenceCounterWrapRetainsFullnessAndAbsolutePositions(t *testing.T) {
	var ring Ring
	ring.total = ^uint64(0) - 2
	var copies [Capacity]Copy
	for i := uint64(0); i < 6; i++ {
		appendSequence(&ring, i)
	}
	oldest, total, first, count := ring.Read(^uint64(0), copies[:])
	if ring.Len() != 6 || oldest != ^uint64(0)-2 || total != 3 || first != ^uint64(0) || count != 4 {
		t.Fatalf("wrapped partial window %d %d %d %d len%d", oldest, total, first, count, ring.Len())
	}
	for i := 0; i < count; i++ {
		if copies[i].Record().Time.Seconds != int64(i+2) {
			t.Fatal("wrapped cursor shifted")
		}
	}
	for i := uint64(6); i < 70; i++ {
		appendSequence(&ring, i)
	}
	oldest, total, first, count = ring.Read(^uint64(0), copies[:])
	if ring.Len() != Capacity || oldest != 3 || total != 67 || first != 3 || count != Capacity || first-^uint64(0) != 4 {
		t.Fatalf("wrapped full/gap window %d %d %d %d", oldest, total, first, count)
	}
	if copies[0].Record().Time.Seconds != 6 || copies[63].Record().Time.Seconds != 69 {
		t.Fatal("wrapped retained snapshot")
	}
	var textCopy Copy
	if record, valid := ring.At(0, &textCopy); !valid || record.Time.Seconds != 6 {
		t.Fatal("old relative reader lost records on sequence wrap")
	}
}
