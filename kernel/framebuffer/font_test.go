package framebuffer

import "testing"

func TestFontCoversRequiredScalars(t *testing.T) {
	replacement := GlyphFor(ReplacementCharacter)
	if replacement == (Glyph{}) {
		t.Fatal("replacement glyph is blank")
	}
	for scalar := rune(0x20); scalar <= 0x7e; scalar++ {
		if glyph := GlyphFor(scalar); scalar != ' ' && glyph == (Glyph{}) {
			t.Fatalf("ASCII U+%04X is blank", scalar)
		}
	}
	for scalar := rune(0x0410); scalar <= 0x044f; scalar++ {
		if glyph := GlyphFor(scalar); glyph == (Glyph{}) {
			t.Fatalf("Cyrillic U+%04X is blank", scalar)
		}
	}
	for _, scalar := range []rune{'Ё', 'ё'} {
		if glyph := GlyphFor(scalar); glyph == (Glyph{}) {
			t.Fatalf("Cyrillic U+%04X is blank", scalar)
		}
	}
}

func TestGlyphRowsAreDoubledToEightBySixteen(t *testing.T) {
	glyph := GlyphFor('A')
	for row := 0; row < GlyphHeight; row += 2 {
		if glyph[row] != glyph[row+1] {
			t.Fatalf("rows %d and %d differ", row, row+1)
		}
	}
}

func TestUnsupportedScalarUsesReplacement(t *testing.T) {
	replacement := GlyphFor(ReplacementCharacter)
	for _, scalar := range []rune{0, 0x7f, '漢', -1, 0x110000} {
		if glyph := GlyphFor(scalar); glyph != replacement {
			t.Fatalf("U+%04X did not use replacement glyph", scalar)
		}
	}
}
