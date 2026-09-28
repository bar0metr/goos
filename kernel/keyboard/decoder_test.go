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
	if output := decoder.Feed(0x48); output.Length != 3 || output.Bytes != [3]byte{0x1b, '[', 'A'} {
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
