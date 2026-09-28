package framebuffer

const ValidationMarkerSize = 16

// DrawValidationMarker paints a deterministic checkerboard in the lower-right
// visible corner. Callers gate it on the validation build flag; release builds
// never invoke it.
func DrawValidationMarker(buffer *Buffer) bool {
	if buffer == nil || buffer.Width() < ValidationMarkerSize || buffer.Height() < ValidationMarkerSize {
		return false
	}
	x0 := buffer.Width() - ValidationMarkerSize
	y0 := buffer.Height() - ValidationMarkerSize
	magenta := buffer.Color(255, 0, 255)
	cyan := buffer.Color(0, 255, 255)
	for y := 0; y < ValidationMarkerSize; y++ {
		for x := 0; x < ValidationMarkerSize; x++ {
			color := magenta
			if (x+y)&1 != 0 {
				color = cyan
			}
			buffer.PutPixel(x0+x, y0+y, color)
		}
	}
	return true
}
