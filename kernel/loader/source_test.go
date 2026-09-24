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
