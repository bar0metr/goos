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

func TestSpleenFontGeometry(t *testing.T) {
	if GlyphWidth != 12 || GlyphHeight != 24 {
		t.Fatalf("glyph geometry = %dx%d, want 12x24", GlyphWidth, GlyphHeight)
	}
	if glyph := GlyphFor('A'); glyph[4] != 0x1f80 || glyph[18] != 0x6060 {
		t.Fatalf("Spleen A signature changed: %#v", glyph)
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
