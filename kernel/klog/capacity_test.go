package klog

import (
	"goos/internal/logfmt"
	"goos/internal/timekeeping"
	"goos/kernel/kcall"
	"sync"
	"testing"
	"unsafe"
)

func TestCapacityMatchesTheABIAndItsBSSCost(t *testing.T) {
	if Capacity != kcall.LogRingCapacity || Capacity < 2048 {
		t.Fatalf("capacity %d, ABI %d", Capacity, kcall.LogRingCapacity)
	}
	if size := unsafe.Sizeof(entry{}); size != 208 {
		t.Fatalf("entry is %d bytes; update the BSS cost in klog.go", size)
	}
	var ring Ring
	if size := unsafe.Sizeof(ring); size > 2048*208+64 {
		t.Fatalf("ring is %d bytes", size)
	}
}

// Writers on several processors append at once. Every record must land
// whole, the total must count each one, and each writer's records must keep
// their order.
func TestConcurrentWritersKeepWholeOrderedRecords(t *testing.T) {
	const writers, each = 8, 600
	var ring Ring
	var wg sync.WaitGroup
	for writer := 0; writer < writers; writer++ {
		wg.Add(1)
		go func(writer int) {
			defer wg.Done()
			var fields [16]byte
			for index := 0; index < each; index++ {
				value := logfmt.AppendUint(fields[:0], uint64(index))
				ring.Append(logfmt.Record{Time: timekeeping.Time{Seconds: int64(index)}, Process: uint64(writer), Level: logfmt.Info,
					Component: []byte("writer"), Message: []byte("burst"), Fields: value})
			}
		}(writer)
	}
	wg.Wait()
	oldest, total := ring.Bounds()
	if total != writers*each || total-oldest != Capacity || oldest != writers*each-Capacity {
		t.Fatalf("oldest %d total %d", oldest, total)
	}
	var copies [kcall.LogReadMax]Copy
	var last [writers]int64
	for index := range last {
		last[index] = -1
	}
	for cursor := oldest; cursor != total; {
		_, end, first, count := ring.Read(cursor, copies[:])
		if end != total || first != cursor || count == 0 {
			t.Fatalf("read at %d: end %d first %d count %d", cursor, end, first, count)
		}
		for index := 0; index < count; index++ {
			record := copies[index].Record()
			if record.Process >= writers || string(record.Component) != "writer" || string(record.Message) != "burst" {
				t.Fatalf("torn record %+v", record)
			}
			if record.Time.Seconds <= last[record.Process] {
				t.Fatalf("writer %d went back from %d to %d", record.Process, last[record.Process], record.Time.Seconds)
			}
			last[record.Process] = record.Time.Seconds
		}
		cursor += uint64(count)
	}
}

// A batch smaller than the ring returns the first records past the cursor
// and reports the ring's end, so a reader knows to continue.
func TestReadBatchSmallerThanTheRing(t *testing.T) {
	var ring Ring
	for index := uint64(0); index < 1000; index++ {
		appendSequence(&ring, index)
	}
	var copies [kcall.LogReadMax]Copy
	oldest, total, first, count := ring.Read(10, copies[:])
	if oldest != 0 || total != 1000 || first != 10 || count != kcall.LogReadMax || copies[count-1].Record().Time.Seconds != 10+kcall.LogReadMax-1 {
		t.Fatalf("oldest %d total %d first %d count %d", oldest, total, first, count)
	}
}
