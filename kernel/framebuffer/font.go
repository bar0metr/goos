package framebuffer

const (
	GlyphWidth  = 12
	GlyphHeight = 24

	ReplacementCharacter   = '\uFFFD'
	asciiGlyphCount        = 0x7f - 0x20
	cyrillicGlyphIndex     = asciiGlyphCount
	cyrillicCapitalYoIndex = cyrillicGlyphIndex + (0x0450 - 0x0410)
	cyrillicSmallYoIndex   = cyrillicCapitalYoIndex + 1
	replacementGlyphIndex  = cyrillicSmallYoIndex + 1
)

// Glyph is one fixed-width Spleen bitmap cell. Bit 15 is the leftmost pixel;
// the low four bits are unused because the source cell is 12 pixels wide.
type Glyph [GlyphHeight]uint16

// GlyphFor returns the built-in glyph for scalar. Unsupported scalars use the
// visible replacement glyph.
func GlyphFor(scalar rune) Glyph {
	index := replacementGlyphIndex
	switch {
	case scalar >= 0x20 && scalar <= 0x7e:
		index = int(scalar - 0x20)
	case scalar >= 0x0410 && scalar <= 0x044f:
		index = cyrillicGlyphIndex + int(scalar-0x0410)
	case scalar == 0x0401:
		index = cyrillicCapitalYoIndex
	case scalar == 0x0451:
		index = cyrillicSmallYoIndex
	}
	return fontData[index]
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
			if bits&(uint16(0x8000)>>column) != 0 {
				pixel = foreground
			}
			buffer.PutPixel(x+column, y+row, pixel)
		}
	}
}
