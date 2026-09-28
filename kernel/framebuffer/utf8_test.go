package framebuffer

import "testing"

func decodeBytes(values ...byte) []rune {
	decoder := UTF8Decoder{}
	result := make([]rune, 0, len(values))
	for _, value := range values {
		first, second, count := decoder.Decode(value)
		if count >= 1 {
			result = append(result, first)
		}
		if count == 2 {
			result = append(result, second)
		}
	}
	if scalar, ok := decoder.Flush(); ok {
		result = append(result, scalar)
	}
	return result
}

func TestUTF8DecoderPreservesSplitSequences(t *testing.T) {
	decoder := UTF8Decoder{}
	for _, value := range []byte{0xd0, 0x9f, 0xd1} {
		first, _, count := decoder.Decode(value)
		if value == 0x9f {
			if count != 1 || first != 'П' {
				t.Fatalf("first split scalar = U+%04X count %d", first, count)
			}
		} else if count != 0 {
			t.Fatalf("partial sequence emitted %d scalars", count)
		}
	}
	first, _, count := decoder.Decode(0x80)
	if count != 1 || first != 'р' {
		t.Fatalf("second split scalar = U+%04X count %d", first, count)
	}
}

func TestUTF8DecoderRecoversAfterMalformedInput(t *testing.T) {
	want := []rune{ReplacementCharacter, '(', ReplacementCharacter, 'A'}
	got := decodeBytes(0xe2, '(', 0xa1, 'A')
	if len(got) != len(want) {
		t.Fatalf("decoded %d scalars, want %d: %U", len(got), len(want), got)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("scalar %d = U+%04X, want U+%04X", index, got[index], want[index])
		}
	}
}

func TestUTF8DecoderRejectsInvalidScalarsAndIncompleteTail(t *testing.T) {
	for _, values := range [][]byte{
		{0xc0, 0x80},
		{0xed, 0xa0, 0x80},
		{0xf4, 0x90, 0x80, 0x80},
		{0xe2, 0x82},
	} {
		got := decodeBytes(values...)
		if len(got) == 0 {
			t.Fatalf("malformed input %x emitted nothing", values)
		}
		for _, scalar := range got {
			if scalar != ReplacementCharacter {
				t.Fatalf("malformed input %x emitted U+%04X", values, scalar)
			}
		}
	}
}
