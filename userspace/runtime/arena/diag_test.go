package arena

import (
	"strconv"
	"strings"
	"testing"
)

func TestDiagnosticsCountWhatTheAllocatorDid(t *testing.T) {
	th := newTestHeap(t, 256, 64)
	small := th.mustAlloc(t, 17)
	th.mustAlloc(t, 32)
	run := th.mustAlloc(t, 3*pageBytes+1)
	var d Diagnostics
	th.diagnostics(&d)
	if d.SizeCounts[1] != 2 || d.SizeBytes[1] != 49 || d.LargeAllocs != 1 || d.LargeRequested != 3*uint64(pageBytes)+1 || d.LargePages != 4 {
		t.Fatalf("request histogram %v %v large %d %d %d", d.SizeCounts[:3], d.SizeBytes[:3], d.LargeAllocs, d.LargeRequested, d.LargePages)
	}
	// Slots of a slab on a zeroed page are not cleared.
	if d.NewSlabs != 1 || d.PagesFromFresh != 5 || d.PagesFromRuns != 0 || d.ClearedBytes != 0 {
		t.Fatalf("sources: slabs %d runs %d fresh %d cleared %d", d.NewSlabs, d.PagesFromRuns, d.PagesFromFresh, d.ClearedBytes)
	}
	if d.SlabMetadataChunks != 1 || d.SlabChunksTouched != 1 {
		t.Fatalf("slab chunks %d touched %d", d.SlabMetadataChunks, d.SlabChunksTouched)
	}
	if d.PartialSlabs[sizeClass(32)] != 1 || d.FreeSlots[sizeClass(32)] != slotsPerPage(uint8(sizeClass(32)))-2 || d.RunPages != 4 {
		t.Fatalf("snapshot: %+v", d)
	}

	// Freeing the run and taking it again reuses dirty pages: they are
	// cleared, not recommitted.
	bytesAt(run, 8)[0] = 1
	th.free(run)
	th.mustAlloc(t, 2*pageBytes)
	th.diagnostics(&d)
	if d.PagesFromRuns != 2 || d.PagesRecommitted != 0 || d.ClearedBytes != 2*uint64(pageBytes) || d.FreeRuns != 1 || d.DirtyFreePages != 2 {
		t.Fatalf("reuse: %+v", d)
	}

	// A collection discards the free pages; taking them again recommits.
	th.collect(roots{small})
	th.diagnostics(&d)
	if d.DiscardCalls == 0 || d.DiscardedPages == 0 || d.DirtyFreePages != 0 {
		t.Fatalf("discard: %+v", d)
	}
	th.refuse = true
	big := th.mustAlloc(t, 4*pageBytes)
	th.diagnostics(&d)
	if d.PagesRecommitted == 0 {
		t.Fatalf("a discarded page taken again was not counted: %+v", d)
	}
	th.free(big)
	th.releaseFree()
	th.diagnostics(&d)
	if d.DiscardFailures == 0 || d.DirtyFreePages == 0 {
		t.Fatalf("a refused discard: %+v", d)
	}
	if d.MetadataPages != uint64((metaBytes(th.frontier)+pageBytes-1)>>pageShift) || d.Frontier != th.frontier {
		t.Fatalf("metadata: %+v", d)
	}
}

func TestDiagnosticsCountSlabTransitions(t *testing.T) {
	th := newTestHeap(t, 64, 16)
	class := uint8(sizeClass(1024))
	var held []uintptr
	for index := uint32(0); index < slotsPerPage(class); index++ {
		held = append(held, th.mustAlloc(t, 1000))
	}
	var d Diagnostics
	th.diagnostics(&d)
	if d.PartialToFull != 1 || d.FullSlabs[class] != 1 || d.PartialSlabs[class] != 0 {
		t.Fatalf("filling: %+v", d)
	}
	th.free(held[0])
	th.diagnostics(&d)
	if d.FullToPartial != 1 || d.PartialSlabs[class] != 1 || d.FreeSlots[class] != 1 {
		t.Fatalf("freeing: %+v", d)
	}
	th.collect(roots{held[1]})
	th.diagnostics(&d)
	if d.PartialSlabs[class] != 1 || d.FreeSlots[class] != slotsPerPage(class)-1 {
		t.Fatalf("sweeping: %+v", d)
	}
}

var profileLines []string

func keepProfileLine(line []byte) { profileLines = append(profileLines, string(line)) }

var (
	profileSink   [120]byte
	profileLength int
)

func countProfileLine(line []byte) { profileLength += len(line) }

func TestProfileLinesFitAndParse(t *testing.T) {
	th := newTestHeap(t, 1024, 64)
	for size := uintptr(1); size <= maxSmallSize; size += 7 {
		th.mustAlloc(t, size)
	}
	th.mustAlloc(t, 5*pageBytes)
	var d Diagnostics
	th.diagnostics(&d)
	profileLines = nil
	var line [120]byte
	d.AppendProfile("vfs", line[:], keepProfileLine)
	counts := map[int]uint64{}
	fields := map[string]bool{}
	for _, text := range profileLines {
		if len(text) > len(line) || !strings.HasSuffix(text, "\n") || !strings.HasPrefix(text, "ALLOC_PROFILE vfs ") {
			t.Fatalf("malformed line %q", text)
		}
		words := strings.Fields(text)
		if words[2] == "sizes" {
			for _, word := range words[3:] {
				parts := strings.Split(word, ":")
				bucket, _ := strconv.Atoi(parts[0])
				count, _ := strconv.ParseUint(parts[1], 10, 64)
				counts[bucket] += count
			}
			continue
		}
		for _, word := range words[2:] {
			if key, _, ok := strings.Cut(word, "="); ok {
				fields[key] = true
			}
		}
	}
	for bucket, count := range d.SizeCounts {
		if counts[bucket] != count {
			t.Fatalf("bucket %d: %d listed, %d counted", bucket, counts[bucket], count)
		}
	}
	last := profileLines[len(profileLines)-1]
	if last != "ALLOC_PROFILE vfs end lines="+strconv.Itoa(len(profileLines)-1)+"\n" {
		t.Fatalf("profile ends with %q after %d lines", last, len(profileLines)-1)
	}
	for _, key := range []string{"large", "new_slabs", "recommitted", "discards", "meta_pages", "dirty_free", "trims"} {
		if !fields[key] {
			t.Fatalf("no %s in %q", key, profileLines)
		}
	}
}

// TestDiagnosticsDoNotAllocate checks that reading and printing the counters
// allocates nothing, so a program may report them from inside its heap.
func TestDiagnosticsDoNotAllocate(t *testing.T) {
	th := newTestHeap(t, 4096, 64)
	for size := uintptr(1); size <= 3*maxSmallSize; size += 13 {
		th.mustAlloc(t, size)
	}
	d := new(Diagnostics)
	allocations := testing.AllocsPerRun(20, func() {
		th.diagnostics(d)
		d.AppendProfile("memreclaim", profileSink[:], countProfileLine)
	})
	if allocations != 0 || profileLength == 0 {
		t.Fatalf("%v allocations, %d bytes", allocations, profileLength)
	}
}
