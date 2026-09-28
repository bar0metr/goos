package framebuffer

import "testing"

func TestDrawValidationMarkerUsesLowerRightVisiblePixels(t *testing.T) {
	buffer, pixels, ok := testBuffer(640, 400, 643)
	if !ok || !DrawValidationMarker(&buffer) {
		t.Fatal("validation marker was not drawn")
	}
	magenta := buffer.Color(255, 0, 255)
	cyan := buffer.Color(0, 255, 255)
	for y := 0; y < 400; y++ {
		for x := 0; x < 643; x++ {
			pixel := pixels[y*643+x]
			inside := x >= 624 && x < 640 && y >= 384
			if !inside {
				if pixel != 0 {
					t.Fatalf("outside pixel (%d,%d) = %#x", x, y, pixel)
				}
				continue
			}
			expected := magenta
			if ((x-624)+(y-384))&1 != 0 {
				expected = cyan
			}
			if pixel != expected {
				t.Fatalf("marker pixel (%d,%d) = %#x, want %#x", x, y, pixel, expected)
			}
		}
	}
}

func TestDrawValidationMarkerRejectsNilBuffer(t *testing.T) {
	if DrawValidationMarker(nil) {
		t.Fatal("nil buffer accepted")
	}
}
