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

// paged splits data into whole pages of pageSize bytes, the tail zero padded.
func paged(data []byte, pageSize int) [][]byte {
	var pages [][]byte
	for start := 0; start < len(data); start += pageSize {
		page := make([]byte, pageSize)
		copy(page, data[start:])
		pages = append(pages, page)
	}
	return pages
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

func TestPagedSourceRequiresWholePagesForItsSize(t *testing.T) {
	pages := paged(make([]byte, 128), 64)
	for _, test := range []struct {
		pages    [][]byte
		pageSize uint64
		size     uint64
	}{
		{pages, 64, 0},   // empty
		{pages, 0, 128},  // no page size
		{pages, 64, 129}, // more bytes than pages
		{pages, 64, 64},  // a page too many
		{[][]byte{pages[0], pages[1][:63]}, 64, 127}, // a short page
	} {
		if _, ok := Paged(test.pages, test.pageSize, test.size); ok {
			t.Fatalf("accepted %d pages of %d for %d bytes", len(test.pages), test.pageSize, test.size)
		}
	}
	if _, ok := Paged(pages, 64, 65); !ok {
		t.Fatal("a partly used last page refused")
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
