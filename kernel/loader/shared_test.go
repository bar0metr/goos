package loader

import "testing"

func TestSharedPagesRecordsAndFindsSegments(t *testing.T) {
	var cache SharedPages
	frames, ok := cache.Reserve(3)
	if !ok || len(frames) != 3 {
		t.Fatal("reserve refused")
	}
	for index := range frames {
		frames[index] = uint64(index+1) * 0x1000
	}
	if !cache.Commit(7, 0, 3) {
		t.Fatal("commit refused")
	}
	found, hit := cache.Lookup(7, 0, 3)
	if !hit || len(found) != 3 || found[2] != 0x3000 || cache.Hits() != 1 {
		t.Fatalf("lookup = %v %v", found, hit)
	}
	for _, miss := range []struct {
		key            uint64
		segment, pages int
	}{{7, 1, 3}, {8, 0, 3}, {7, 0, 2}, {0, 0, 3}} {
		if _, hit := cache.Lookup(miss.key, miss.segment, miss.pages); hit {
			t.Fatalf("%+v hit", miss)
		}
	}
	if next, ok := cache.Reserve(1); !ok || &next[0] == &found[0] {
		t.Fatal("a reservation overlaps committed frames")
	}
}

func TestSharedPagesIsBounded(t *testing.T) {
	var cache SharedPages
	if _, ok := cache.Reserve(MaxSharedPages + 1); ok {
		t.Fatal("reservation beyond the frame bound")
	}
	if _, ok := cache.Reserve(0); ok {
		t.Fatal("empty reservation")
	}
	for index := 0; index < MaxSharedSegments; index++ {
		if _, ok := cache.Reserve(1); !ok || !cache.Commit(uint64(index+1), 0, 1) {
			t.Fatalf("segment %d refused", index)
		}
	}
	if _, ok := cache.Reserve(1); ok || cache.Commit(999, 0, 1) {
		t.Fatal("segment beyond the bound recorded")
	}
	if cache.Commit(0, 0, 1) {
		t.Fatal("zero key recorded")
	}
}

func TestOnlyReadOnlySegmentsAreShareable(t *testing.T) {
	if !Shareable(Segment{Flags: SegmentReadable | SegmentExecutable}) || !Shareable(Segment{Flags: SegmentReadable}) {
		t.Fatal("read-only segment not shareable")
	}
	if Shareable(Segment{Flags: SegmentReadable | SegmentWritable}) {
		t.Fatal("writable segment shareable")
	}
}
