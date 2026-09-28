package framebuffer

// UTF8Decoder incrementally decodes a console byte stream. Its state survives
// separate ConsoleWrite calls. Decode can return two scalars when a malformed
// sequence is followed by a valid single-byte scalar.
type UTF8Decoder struct {
	value     rune
	minimum   rune
	remaining uint8
}

func (decoder *UTF8Decoder) Decode(value byte) (first, second rune, count int) {
	if decoder.remaining == 0 {
		first, ok := decoder.start(value)
		if ok {
			return first, 0, 1
		}
		return 0, 0, 0
	}
	if value&0xc0 != 0x80 {
		decoder.reset()
		second, ok := decoder.start(value)
		if ok {
			return ReplacementCharacter, second, 2
		}
		return ReplacementCharacter, 0, 1
	}
	decoder.value = decoder.value<<6 | rune(value&0x3f)
	decoder.remaining--
	if decoder.remaining != 0 {
		return 0, 0, 0
	}
	scalar, minimum := decoder.value, decoder.minimum
	decoder.reset()
	if scalar < minimum || scalar > 0x10ffff || scalar >= 0xd800 && scalar <= 0xdfff {
		return ReplacementCharacter, 0, 1
	}
	return scalar, 0, 1
}

// Flush replaces an incomplete trailing sequence and resets the decoder.
func (decoder *UTF8Decoder) Flush() (rune, bool) {
	if decoder.remaining == 0 {
		return 0, false
	}
	decoder.reset()
	return ReplacementCharacter, true
}

func (decoder *UTF8Decoder) start(value byte) (rune, bool) {
	switch {
	case value < 0x80:
		return rune(value), true
	case value >= 0xc2 && value <= 0xdf:
		decoder.value, decoder.minimum, decoder.remaining = rune(value&0x1f), 0x80, 1
	case value >= 0xe0 && value <= 0xef:
		decoder.value, decoder.minimum, decoder.remaining = rune(value&0x0f), 0x800, 2
	case value >= 0xf0 && value <= 0xf4:
		decoder.value, decoder.minimum, decoder.remaining = rune(value&0x07), 0x10000, 3
	default:
		return ReplacementCharacter, true
	}
	return 0, false
}

func (decoder *UTF8Decoder) reset() {
	decoder.value = 0
	decoder.minimum = 0
	decoder.remaining = 0
}
