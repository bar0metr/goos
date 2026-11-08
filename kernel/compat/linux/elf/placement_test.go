package elf

import (
	"encoding/binary"
	"testing"
)

const testTail = uint64(64 << 20)

// loneSegmentPIE is a static PIE with one executable load segment of
// memorySize bytes at virtual, aligned to alignment.
func loneSegmentPIE(t *testing.T, virtual uint64, memorySize uint64, alignment uint64) []byte {
	t.Helper()
	data := buildELF(t, virtual+0x100, []testProgramHeader{
		{programLoad, FlagReadable | FlagExecutable, 0, virtual, 0x200, memorySize, alignment},
	}, 0x1000)
	binary.LittleEndian.PutUint16(data[16:], typeDyn)
	return data
}

func probe(t *testing.T, data []byte) Image {
	t.Helper()
	image, failure := Parse(data, GoOSConfig(StaticPIEWindowStart))
	if failure != OK {
		t.Fatalf("probe parse failed: %d", failure)
	}
	return image
}

func mustPlace(t *testing.T, image *Image, tail uint64) Placement {
	t.Helper()
	placement, ok := PlacePIE(image, tail)
	if !ok {
		t.Fatalf("no placement for an image ending at %#x", image.LoadEnd())
	}
	return placement
}

// The last base leaves the image and its tail exactly at the window's end.
func lastBase(placement Placement) uint64 {
	return placement.First + (placement.Count-1)*placement.Step
}

func TestPlacementCoversTheWindowForASmallImage(t *testing.T) {
	image := probe(t, pieELF(t))
	placement := mustPlace(t, &image, testTail)
	room := StaticPIEWindowEnd - StaticPIEWindowStart - 0x3000 - testTail
	if placement.First != StaticPIEWindowStart || placement.Step != testPageSize || placement.Count != room/testPageSize+1 {
		t.Fatalf("placement %+v", placement)
	}
	if got := placement.Base(0); got != StaticPIEWindowStart {
		t.Fatalf("lowest draw: base %#x", got)
	}
	if got := placement.Base(^uint64(0)); got != lastBase(placement) || got+0x3000+testTail != StaticPIEWindowEnd {
		t.Fatalf("highest draw: base %#x, last %#x", got, lastBase(placement))
	}
	middle := placement.Base(1 << 63)
	if middle != placement.First+placement.Count/2*placement.Step {
		t.Fatalf("middle draw: base %#x", middle)
	}
}

func TestPlacementBasesParseAndKeepTheImageInsideTheWindow(t *testing.T) {
	data := pieELF(t)
	image := probe(t, data)
	placement := mustPlace(t, &image, testTail)
	for _, draw := range []uint64{0, 1, 1 << 40, 1 << 62, 1 << 63, ^uint64(0) >> 1, ^uint64(0) - 1, ^uint64(0)} {
		base := placement.Base(draw)
		moved, failure := Parse(data, GoOSConfig(base))
		if failure != OK || moved.Base != base {
			t.Fatalf("draw %#x: base %#x failure %d", draw, base, failure)
		}
		end := moved.LoadEnd()
		if base < StaticPIEWindowStart || base%testPageSize != 0 || end+testTail > StaticPIEWindowEnd {
			t.Fatalf("draw %#x: base %#x end %#x", draw, base, end)
		}
		if moved.Entry != base+0x1020 || moved.ProgramHeaderAddress != base+HeaderSize || moved.TLS.VirtualAddress != base+0x2080 ||
			moved.Dynamic.Start != base+0x2100 || moved.RELRO.Start != base+0x2000 {
			t.Fatalf("draw %#x: metadata not relocated: %+v", draw, moved)
		}
	}
}

func TestPlacementStepsByTheLargestAlignment(t *testing.T) {
	const alignment = uint64(2 << 20)
	data := loneSegmentPIE(t, 0, 0x5000, alignment)
	image := probe(t, data)
	placement := mustPlace(t, &image, testTail)
	room := StaticPIEWindowEnd - StaticPIEWindowStart - 0x5000 - testTail
	if placement.Step != alignment || placement.Count != room/alignment+1 {
		t.Fatalf("placement %+v", placement)
	}
	for _, draw := range []uint64{0, 1 << 63, ^uint64(0)} {
		base := placement.Base(draw)
		if base%alignment != 0 {
			t.Fatalf("base %#x breaks the %#x alignment", base, alignment)
		}
		if _, failure := Parse(data, GoOSConfig(base)); failure != OK {
			t.Fatalf("base %#x: failure %d", base, failure)
		}
	}
	// A base between two slots breaks the segment's congruence rule.
	if _, failure := Parse(data, GoOSConfig(placement.First+testPageSize)); failure != BadCongruence {
		t.Fatalf("unaligned base accepted: failure %d", failure)
	}
}

func TestPlacementHonoursMetadataAlignment(t *testing.T) {
	// The TLS header asks for 64 KiB alignment; the load segment only for
	// a page.
	const alignment = uint64(0x10000)
	data := buildELF(t, 0x100, []testProgramHeader{
		{programLoad, FlagReadable | FlagExecutable, 0, 0, 0x200, 0x3000, testPageSize},
		{programTLS, FlagReadable, 0, 0, 0x10, 0x10, alignment},
	}, 0x1000)
	binary.LittleEndian.PutUint16(data[16:], typeDyn)
	image := probe(t, data)
	placement := mustPlace(t, &image, testTail)
	if placement.Step != alignment {
		t.Fatalf("placement %+v ignores the TLS alignment", placement)
	}
	if _, failure := Parse(data, GoOSConfig(placement.Base(1<<63))); failure != OK {
		t.Fatalf("middle base: failure %d", failure)
	}
	if _, failure := Parse(data, GoOSConfig(StaticPIEWindowStart+testPageSize)); failure != MalformedTLS {
		t.Fatalf("a base off the TLS alignment: failure %d", failure)
	}
}

func TestPlacementOfAnImageThatExactlyFits(t *testing.T) {
	size := StaticPIEWindowEnd - StaticPIEWindowStart - testTail
	image := probe(t, loneSegmentPIE(t, 0, size, testPageSize))
	placement := mustPlace(t, &image, testTail)
	if placement.Count != 1 || placement.Base(^uint64(0)) != StaticPIEWindowStart {
		t.Fatalf("placement %+v", placement)
	}
}

func TestPlacementRefusesAnImageWithoutBrkRoom(t *testing.T) {
	size := StaticPIEWindowEnd - StaticPIEWindowStart - testTail + testPageSize
	image := probe(t, loneSegmentPIE(t, 0, size, testPageSize))
	if placement, ok := PlacePIE(&image, testTail); ok {
		t.Fatalf("placement %+v leaves no brk room", placement)
	}
	// Without a tail the same image fits once.
	if placement, ok := PlacePIE(&image, 0); !ok || placement.Count != testTail/testPageSize {
		t.Fatalf("placement without a tail: %+v %v", placement, ok)
	}
}

func TestPlacementRefusesAnImageTooLargeForTheWindow(t *testing.T) {
	size := StaticPIEWindowEnd - StaticPIEWindowStart + testPageSize
	if _, failure := Parse(loneSegmentPIE(t, 0, size, testPageSize), GoOSConfig(StaticPIEWindowStart)); failure != BadMemoryRange {
		t.Fatalf("oversized image: failure %d", failure)
	}
}

func TestPlacementOfASegmentNearTheImageAreaLimit(t *testing.T) {
	// The segment sits at the top of the window when loaded at its start.
	virtual := StaticPIEWindowEnd - StaticPIEWindowStart - 2*testPageSize
	image := probe(t, loneSegmentPIE(t, virtual, testPageSize, testPageSize))
	if _, ok := PlacePIE(&image, testTail); ok {
		t.Fatal("placed an image whose brk crosses the window's end")
	}
	placement, ok := PlacePIE(&image, testPageSize)
	if !ok || placement.Count != 1 {
		t.Fatalf("placement %+v %v", placement, ok)
	}
	// One page higher, the segment leaves the image area.
	if _, failure := Parse(loneSegmentPIE(t, virtual+2*testPageSize, testPageSize, testPageSize), GoOSConfig(StaticPIEWindowStart)); failure != BadMemoryRange {
		t.Fatalf("segment past the limit: failure %d", failure)
	}
}

func TestPlacementRejectsWrappingAddresses(t *testing.T) {
	// The entry stays low, so the load segment is what wraps.
	data := loneSegmentPIE(t, ^uint64(0)-0xfff, 0x1000, testPageSize)
	binary.LittleEndian.PutUint64(data[24:], 0x100)
	if _, failure := Parse(data, GoOSConfig(StaticPIEWindowStart)); failure != BadMemoryRange {
		t.Fatalf("wrapping segment: failure %d", failure)
	}
	image := probe(t, pieELF(t))
	if _, ok := PlacePIE(&image, ^uint64(0)); ok {
		t.Fatal("a tail that wraps was accepted")
	}
}

func TestPlacementIgnoresImagesThatAreNotProbes(t *testing.T) {
	executable := probe(t, validELF(t))
	if executable.Base != 0 {
		t.Fatalf("ET_EXEC moved to %#x", executable.Base)
	}
	if _, ok := PlacePIE(&executable, testTail); ok {
		t.Fatal("placed an ET_EXEC image")
	}
	moved, failure := Parse(pieELF(t), GoOSConfig(StaticPIEWindowStart+testPageSize))
	if failure != OK {
		t.Fatalf("parse: %d", failure)
	}
	if _, ok := PlacePIE(&moved, testTail); ok {
		t.Fatal("placed an image parsed away from the window start")
	}
	if _, ok := PlacePIE(nil, testTail); ok {
		t.Fatal("placed a nil image")
	}
}

func TestPlacementBaseIsUniformAcrossSlots(t *testing.T) {
	placement := Placement{First: StaticPIEWindowStart, Step: testPageSize, Count: 3}
	seen := map[uint64]int{}
	// Equal-width draws land in each slot equally often.
	for index := uint64(0); index < 3*1024; index++ {
		seen[placement.Base(index*(^uint64(0)/(3*1024)))]++
	}
	for slot := uint64(0); slot < placement.Count; slot++ {
		if count := seen[placement.First+slot*placement.Step]; count < 1023 || count > 1025 {
			t.Fatalf("slot %d drawn %d times: %v", slot, count, seen)
		}
	}
	if len(seen) != 3 {
		t.Fatalf("draws left the slots: %v", seen)
	}
}
