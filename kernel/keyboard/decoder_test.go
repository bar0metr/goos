package keyboard

import "testing"

func TestDecoderLettersModifiersAndRelease(t *testing.T) {
	var decoder Decoder
	if output := decoder.Feed(0x1e); output.Length != 1 || output.Bytes[0] != 'a' {
		t.Fatalf("a = %#v", output)
	}
	decoder.Feed(0x2a)
	if output := decoder.Feed(0x1e); output.Bytes[0] != 'A' {
		t.Fatalf("shift-a = %#v", output)
	}
	decoder.Feed(0xaa)
	decoder.Feed(0x3a)
	if output := decoder.Feed(0x1e); output.Bytes[0] != 'A' {
		t.Fatalf("caps-a = %#v", output)
	}
	if output := decoder.Feed(0x9e); output.Length != 0 {
		t.Fatalf("release = %#v", output)
	}
}

func TestDecoderControlsAndArrows(t *testing.T) {
	var decoder Decoder
	decoder.Feed(0x1d)
	if output := decoder.Feed(0x2e); !output.Interrupt || output.Length != 0 {
		t.Fatalf("ctrl-c = %#v", output)
	}
	if output := decoder.Feed(0x20); output.Length != 1 || output.Bytes[0] != 4 {
		t.Fatalf("ctrl-d = %#v", output)
	}
	decoder.Feed(0x9d)
	decoder.Feed(0xe0)
	if output := decoder.Feed(0x48); output.Length != 3 || string(output.Bytes[:output.Length]) != "\x1b[A" {
		t.Fatalf("up = %#v", output)
	}
}

func TestDecoderUnknownAndPausePrefixAreIgnored(t *testing.T) {
	var decoder Decoder
	for _, value := range []byte{0xff, 0xe1, 0x1d, 0x45, 0xe1, 0x9d, 0xc5, 0x7f} {
		if output := decoder.Feed(value); output.Length != 0 || output.Interrupt {
			t.Fatalf("%#x = %#v", value, output)
		}
	}
}

func TestDecoderPrintableAndEditingKeys(t *testing.T) {
	tests := []struct {
		name     string
		sequence []byte
		want     byte
	}{
		{name: "lowercase", sequence: []byte{0x1e}, want: 'a'},
		{name: "right shift", sequence: []byte{0x36, 0x1e}, want: 'A'},
		{name: "caps and shift", sequence: []byte{0x3a, 0x2a, 0x1e}, want: 'a'},
		{name: "shifted number", sequence: []byte{0x2a, 0x02}, want: '!'},
		{name: "punctuation", sequence: []byte{0x27}, want: ';'},
		{name: "shifted punctuation", sequence: []byte{0x2a, 0x27}, want: ':'},
		{name: "enter", sequence: []byte{0x1c}, want: '\n'},
		{name: "backspace", sequence: []byte{0x0e}, want: '\b'},
		{name: "tab", sequence: []byte{0x0f}, want: '\t'},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var decoder Decoder
			var output Output
			for _, code := range test.sequence {
				output = decoder.Feed(code)
			}
			if output.Length != 1 || output.Bytes[0] != test.want || output.Interrupt {
				t.Fatalf("output = %#v, want %q", output, test.want)
			}
		})
	}
}

func TestDecoderControlKeys(t *testing.T) {
	tests := []struct {
		code      byte
		want      byte
		interrupt bool
	}{
		{code: 0x2e, interrupt: true},
		{code: 0x20, want: 4},
		{code: 0x26, want: 12},
		{code: 0x16, want: 21},
	}
	for _, test := range tests {
		var decoder Decoder
		decoder.Feed(0x1d)
		output := decoder.Feed(test.code)
		if output.Interrupt != test.interrupt || (!test.interrupt && (output.Length != 1 || output.Bytes[0] != test.want)) {
			t.Fatalf("code %#x = %#v", test.code, output)
		}
	}
}

func TestDecoderAllArrowSequencesAndReleases(t *testing.T) {
	for _, test := range []struct{ code, final byte }{{0x48, 'A'}, {0x50, 'B'}, {0x4d, 'C'}, {0x4b, 'D'}} {
		var decoder Decoder
		if output := decoder.Feed(0xe0); output.Length != 0 {
			t.Fatalf("prefix produced output: %#v", output)
		}
		if output := decoder.Feed(test.code); output != sequence(test.final) {
			t.Fatalf("arrow %#x = %#v", test.code, output)
		}
		decoder.Feed(0xe0)
		if output := decoder.Feed(test.code | 0x80); output.Length != 0 || output.Interrupt {
			t.Fatalf("arrow release %#x = %#v", test.code, output)
		}
	}
}

func TestDecoderToleratesRepeatedModifiersAndReleaseWithoutPress(t *testing.T) {
	var decoder Decoder
	for _, code := range []byte{0xaa, 0x9d, 0xb8, 0x2a, 0x2a} {
		if output := decoder.Feed(code); output.Length != 0 || output.Interrupt {
			t.Fatalf("modifier %#x = %#v", code, output)
		}
	}
	if output := decoder.Feed(0x1e); output.Length != 1 || output.Bytes[0] != 'A' {
		t.Fatalf("repeated shift state = %#v", output)
	}
	decoder.Feed(0xaa)
	decoder.Feed(0xaa)
	if output := decoder.Feed(0x1e); output.Length != 1 || output.Bytes[0] != 'a' {
		t.Fatalf("repeated shift release state = %#v", output)
	}
}

func TestDecoderFunctionKeys(t *testing.T) {
	want := []string{"\x1b[11~", "\x1b[12~", "\x1b[13~", "\x1b[14~", "\x1b[15~", "\x1b[17~",
		"\x1b[18~", "\x1b[19~", "\x1b[20~", "\x1b[21~", "\x1b[23~", "\x1b[24~"}
	codes := []byte{0x3b, 0x3c, 0x3d, 0x3e, 0x3f, 0x40, 0x41, 0x42, 0x43, 0x44, 0x57, 0x58}
	for index, code := range codes {
		var decoder Decoder
		output := decoder.Feed(code)
		if got := string(output.Bytes[:output.Length]); got != want[index] || output.Interrupt {
			t.Fatalf("F%d make = %q, want %q", index+1, got, want[index])
		}
		if int(output.Length) > len(output.Bytes) {
			t.Fatalf("F%d length %d overflows", index+1, output.Length)
		}
		if output := decoder.Feed(code | 0x80); output.Length != 0 || output.Interrupt {
			t.Fatalf("F%d break = %#v", index+1, output)
		}
	}
}

func TestDecoderFunctionKeysIgnoreModifiers(t *testing.T) {
	for _, modifier := range []byte{0x2a, 0x36, 0x1d, 0x38} {
		var decoder Decoder
		decoder.Feed(modifier)
		if output := decoder.Feed(0x44); string(output.Bytes[:output.Length]) != "\x1b[21~" {
			t.Fatalf("modifier %#x F10 = %q", modifier, output.Bytes[:output.Length])
		}
	}
}

func TestDecoderNavigationKeys(t *testing.T) {
	tests := []struct {
		name string
		code byte
		want string
	}{
		{"up", 0x48, "\x1b[A"}, {"down", 0x50, "\x1b[B"}, {"right", 0x4d, "\x1b[C"}, {"left", 0x4b, "\x1b[D"},
		{"home", 0x47, "\x1b[H"}, {"end", 0x4f, "\x1b[F"}, {"insert", 0x52, "\x1b[2~"},
		{"delete", 0x53, "\x1b[3~"}, {"page up", 0x49, "\x1b[5~"}, {"page down", 0x51, "\x1b[6~"},
	}
	for _, test := range tests {
		var decoder Decoder
		// The E0 prefix arrives as its own byte and produces nothing.
		if output := decoder.Feed(0xe0); output.Length != 0 {
			t.Fatalf("%s prefix = %#v", test.name, output)
		}
		output := decoder.Feed(test.code)
		if got := string(output.Bytes[:output.Length]); got != test.want {
			t.Fatalf("%s = %q, want %q", test.name, got, test.want)
		}
		decoder.Feed(0xe0)
		if output := decoder.Feed(test.code | 0x80); output.Length != 0 {
			t.Fatalf("%s release = %#v", test.name, output)
		}
		// Without the prefix the same codes are the keypad, which types nothing.
		if output := decoder.Feed(test.code); output.Length != 0 {
			t.Fatalf("%s keypad = %#v", test.name, output)
		}
	}
}

func TestDecoderUnknownExtendedCodes(t *testing.T) {
	var decoder Decoder
	for _, code := range []byte{0x2a, 0x37, 0x5b, 0x5d, 0x4a, 0x4e} {
		decoder.Feed(0xe0)
		if output := decoder.Feed(code); output.Length != 0 || output.Interrupt {
			t.Fatalf("E0 %#x = %#v", code, output)
		}
	}
	// A fake E0 2A shift must not leave shift pressed.
	if output := decoder.Feed(0x1e); output.Bytes[0] != 'a' {
		t.Fatalf("after E0 2A = %q", output.Bytes[:output.Length])
	}
}
