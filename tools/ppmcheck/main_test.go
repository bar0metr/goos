package main

import "testing"

func TestValidateAcceptsExactLowerRightCheckerboard(t *testing.T) {
	const width, height = 20, 18
	header := []byte("P6\n# fixture\n20 18\n255\n")
	data := append(header, make([]byte, width*height*3)...)
	for y := 0; y < markerSize; y++ {
		for x := 0; x < markerSize; x++ {
			index := len(header) + (((height-markerSize+y)*width + width - markerSize + x) * 3)
			color := [3]byte{255, 0, 255}
			if (x+y)&1 != 0 {
				color = [3]byte{0, 255, 255}
			}
			copy(data[index:index+3], color[:])
		}
	}
	if err := validate(data); err != nil {
		t.Fatal(err)
	}
	data[len(header)+((height-1)*width+width-1)*3] = 1
	if err := validate(data); err == nil {
		t.Fatal("corrupted marker accepted")
	}
}

func TestValidateRejectsMalformedImages(t *testing.T) {
	for _, data := range [][]byte{
		[]byte("P3\n16 16\n255\n"),
		[]byte("P6\n15 16\n255\n"),
		[]byte("P6\n16 16\n31\n"),
		[]byte("P6\n16 16\n255\nshort"),
	} {
		if err := validate(data); err == nil {
			t.Fatalf("malformed image accepted: %q", data)
		}
	}
}
