package framebuffer

const (
	GlyphWidth  = 8
	GlyphHeight = 16

	ReplacementCharacter  = '\uFFFD'
	replacementGlyphIndex = 511
)

// Glyph is one fixed-width bitmap cell. Bit 7 is the leftmost pixel. Each
// source row is doubled vertically to produce an 8x16 cell without rendering
// allocation or runtime font loading.
type Glyph [GlyphHeight]uint8

// GlyphFor returns the built-in glyph for scalar. Unsupported scalars use the
// visible replacement glyph.
func GlyphFor(scalar rune) Glyph {
	index := replacementGlyphIndex
	switch {
	case scalar >= 0x20 && scalar <= 0x7e:
		index = int(scalar)
	case scalar >= 0x0410 && scalar <= 0x044f:
		index = int(cyrillicIndices[scalar-0x0410])
	case scalar == 0x0401:
		index = cyrillicCapitalYoIndex
	case scalar == 0x0451:
		index = cyrillicSmallYoIndex
	}
	var glyph Glyph
	for row, bits := range fontData[index] {
		glyph[row*2] = bits
		glyph[row*2+1] = bits
	}
	return glyph
}

// DrawGlyph paints one complete cell. Buffer clipping keeps partially visible
// cells inside the framebuffer and the operation allocates no memory.
func (buffer *Buffer) DrawGlyph(x, y int, glyph Glyph, foreground, background uint32) {
	if buffer == nil {
		return
	}
	for row, bits := range glyph {
		for column := 0; column < GlyphWidth; column++ {
			pixel := background
			if bits&(0x80>>column) != 0 {
				pixel = foreground
			}
			buffer.PutPixel(x+column, y+row, pixel)
		}
	}
}
