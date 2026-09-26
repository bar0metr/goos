package loader

import "testing"

func TestImageExtentStaysInsideTheImage(t *testing.T) {
	image := []byte("0123456789")
	data, key, ok := ImageExtent(image, 2, 3)
	if !ok || string(data) != "234" || key == 0 {
		t.Fatalf("extent = %q, %d, %v", data, key, ok)
	}
	if data, _, ok := ImageExtent(image, 0, uint64(len(image))); !ok || len(data) != len(image) {
		t.Fatal("whole image refused")
	}
	for _, extent := range [][2]uint64{
		{0, 0},                      // empty
		{10, 1},                     // starts at the end
		{11, 0},                     // starts past the end
		{9, 2},                      // runs past the end
		{1, ^uint64(0)},             // wraps
		{^uint64(0), 1},             // offset wraps
		{MaxImageExtent + 1, 1},     // offset not keyable
		{0, MaxImageExtent + 1},     // size not keyable
		{5, ^uint64(0) - 4},         // sum overflows to a small value
		{uint64(len(image)) + 1, 0}, // past the end and empty
	} {
		if _, _, ok := ImageExtent(image, extent[0], extent[1]); ok {
			t.Fatalf("accepted extent %d+%d", extent[0], extent[1])
		}
	}
}

func TestImageExtentKeysDistinguishLengths(t *testing.T) {
	image := make([]byte, 64)
	_, short, _ := ImageExtent(image, 8, 8)
	_, long, _ := ImageExtent(image, 8, 16)
	_, moved, _ := ImageExtent(image, 16, 8)
	_, again, _ := ImageExtent(image, 8, 8)
	if short == 0 || short == long || short == moved || short != again {
		t.Fatalf("keys = %d, %d, %d, %d", short, long, moved, again)
	}
}

// paged splits data into whole pages of pageSize bytes, the tail zero padded,
// and returns them by number.
func paged(data []byte, pageSize int) func(uint64) ([]byte, bool) {
	var pages [][]byte
	for start := 0; start < len(data); start += pageSize {
		page := make([]byte, pageSize)
		copy(page, data[start:])
		pages = append(pages, page)
	}
	return pageList(pages)
}

func pageList(pages [][]byte) func(uint64) ([]byte, bool) {
	return func(index uint64) ([]byte, bool) {
		if index >= uint64(len(pages)) {
			return nil, false
		}
		return pages[index], true
	}
}

func TestPagedSourceReadsLikeContiguous(t *testing.T) {
	data := make([]byte, 1000)
	for index := range data {
		data[index] = byte(index * 7)
	}
	contiguous := Contiguous(data)
	source, ok := Paged(paged(data, 64), 64, uint64(len(data)))
	if !ok || source.Len() != uint64(len(data)) {
		t.Fatal("paged source refused")
	}
	scratch := make([]byte, len(data))
	for offset := uint64(0); offset <= uint64(len(data)); offset += 13 {
		for _, length := range []uint64{0, 1, 63, 64, 65, 200} {
			want, wantOK := contiguous.Read(offset, length, nil)
			got, gotOK := source.Read(offset, length, scratch)
			if gotOK != wantOK || string(got) != string(want) {
				t.Fatalf("read %d+%d = %v %v, want %v %v", offset, length, got, gotOK, want, wantOK)
			}
		}
	}
	if _, ok := source.Read(0, 200, make([]byte, 199)); ok {
		t.Fatal("a crossing read used a scratch too small for it")
	}
	if _, ok := source.Read(990, 11, scratch); ok {
		t.Fatal("read past the end of the source")
	}
	if _, ok := source.Read(^uint64(0), 2, scratch); ok {
		t.Fatal("wrapping read accepted")
	}
}

func TestPagedSourceRequiresWholePages(t *testing.T) {
	pages := paged(make([]byte, 128), 64)
	for _, test := range []struct {
		page     func(uint64) ([]byte, bool)
		pageSize uint64
		size     uint64
	}{
		{pages, 64, 0},  // empty
		{pages, 0, 128}, // no page size
		{nil, 64, 128},  // no pages
	} {
		if _, ok := Paged(test.page, test.pageSize, test.size); ok {
			t.Fatalf("accepted page size %d for %d bytes", test.pageSize, test.size)
		}
	}
	whole := make([]byte, 64)
	short, _ := Paged(pageList([][]byte{whole, whole[:63]}), 64, 127)
	if _, ok := short.Read(60, 10, make([]byte, 10)); ok {
		t.Fatal("a short page was read")
	}
	missing, _ := Paged(pageList([][]byte{whole}), 64, 128)
	if missing.CopyOut(0, make([]byte, 128)) {
		t.Fatal("a missing page was copied")
	}
	data := make([]byte, 200)
	for index := range data {
		data[index] = byte(index)
	}
	source, _ := Paged(paged(data, 64), 64, 200)
	target := make([]byte, 100)
	if !source.CopyOut(50, target) || target[0] != 50 || target[99] != 149 {
		t.Fatal("copy across pages")
	}
}

func TestParseSourceMatchesParseNativeAcrossPages(t *testing.T) {
	data := validNativeImage(t)
	want, wantOK := ParseNative(data, testPageSize, testUserStart, testUserEnd)
	// Pages smaller than the segment table make the envelope cross pages.
	source, ok := Paged(paged(data, 48), 48, uint64(len(data)))
	if !ok {
		t.Fatal("paged source refused")
	}
	got, gotOK := ParseSource(source, testPageSize, testUserStart, testUserEnd)
	if !wantOK || gotOK != wantOK || got != want {
		t.Fatalf("paged parse = %+v %v, contiguous %+v %v", got, gotOK, want, wantOK)
	}
	truncated, _ := Paged(paged(data, 48), 48, uint64(len(data))-1)
	if _, ok := ParseSource(truncated, testPageSize, testUserStart, testUserEnd); ok {
		t.Fatal("a source one byte short of its last segment was accepted")
	}
}
