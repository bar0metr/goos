package terminal

// Key is a logical key a program reacts to.
type Key uint8

const (
	// KeyRune is a character; Event.Rune holds it. With Ctrl set it is a
	// control key such as Ctrl-R, named by its lower-case letter.
	KeyRune Key = iota + 1
	KeyEnter
	KeyBackspace
	KeyTab
	KeyEscape
	KeyUp
	KeyDown
	KeyLeft
	KeyRight
	KeyHome
	KeyEnd
	KeyPageUp
	KeyPageDown
	KeyInsert
	KeyDelete
	KeyF1
	KeyF2
	KeyF3
	KeyF4
	KeyF5
	KeyF6
	KeyF7
	KeyF8
	KeyF9
	KeyF10
	KeyF11
	KeyF12
)

// Event is one decoded key press.
type Event struct {
	Key  Key
	Rune rune
	Ctrl bool
	Alt  bool
}

const (
	parserGround uint8 = iota
	parserEscape
	parserCSI
	parserSS3
	// parserLinux follows ESC [ [, the Linux console's F1-F5.
	parserLinux
	parserUTF8
	// parserDiscard skips the rest of a sequence too long to hold, up to its
	// final byte, so its tail is not taken for typed text.
	parserDiscard
)

// maxKeyParameters bounds the parameters of one key sequence: a key number
// and a modifier are all any terminal sends.
const (
	maxKeyParameters = 2
	maxKeyDigits     = 3
)

// Parser turns terminal input bytes into key events, one byte at a time, so a
// sequence split across reads decodes as one key. It is bounded and
// allocation-free, and a malformed sequence is dropped without affecting what
// follows.
//
// ESC alone is the Escape key, but it also begins every key sequence. GoOS
// delivers each key's sequence in one piece, so an ESC that ends a read is
// the Escape key: the caller says so with Flush once it has fed everything a
// read returned. No timing is involved.
type Parser struct {
	state      uint8
	parameter  [maxKeyParameters]uint16
	parameters uint8
	digits     uint8
	unicode    [4]byte
	have, need uint8
}

// Feed consumes one byte and returns the event it completes, if any.
func (parser *Parser) Feed(value byte) (Event, bool) {
	switch parser.state {
	case parserEscape:
		return parser.escape(value)
	case parserCSI:
		return parser.csi(value)
	case parserSS3:
		parser.state = parserGround
		return ss3Key(value)
	case parserLinux:
		parser.state = parserGround
		if value >= 'A' && value <= 'E' {
			return Event{Key: KeyF1 + Key(value-'A')}, true
		}
		return Event{}, false
	case parserUTF8:
		return parser.continuation(value)
	case parserDiscard:
		if value >= 0x40 && value <= 0x7e || value < 0x20 {
			parser.state = parserGround
		}
		return Event{}, false
	}
	return parser.ground(value)
}

// Flush ends a read: an ESC waiting for the rest of a sequence was the Escape
// key. A sequence that has got further than ESC is kept, since its remainder
// may still come.
func (parser *Parser) Flush() (Event, bool) {
	if parser.state == parserEscape {
		parser.state = parserGround
		return Event{Key: KeyEscape}, true
	}
	return Event{}, false
}

func (parser *Parser) ground(value byte) (Event, bool) {
	switch {
	case value == 0x1b:
		parser.state = parserEscape
		return Event{}, false
	case value == '\r' || value == '\n':
		return Event{Key: KeyEnter}, true
	case value == 0x08 || value == 0x7f:
		return Event{Key: KeyBackspace}, true
	case value == '\t':
		return Event{Key: KeyTab}, true
	case value >= 0x01 && value <= 0x1a:
		return Event{Key: KeyRune, Rune: rune('a' + value - 1), Ctrl: true}, true
	case value < 0x20:
		return Event{}, false
	case value < 0x80:
		return Event{Key: KeyRune, Rune: rune(value)}, true
	}
	return parser.startUTF8(value)
}

func (parser *Parser) escape(value byte) (Event, bool) {
	switch value {
	case '[':
		parser.state, parser.parameters, parser.digits = parserCSI, 0, 0
		parser.parameter = [maxKeyParameters]uint16{}
		return Event{}, false
	case 'O':
		parser.state = parserSS3
		return Event{}, false
	case 0x1b:
		// ESC ESC: the first was the Escape key; the second may begin a
		// sequence.
		return Event{Key: KeyEscape}, true
	}
	parser.state = parserGround
	if value >= 0x20 && value < 0x7f {
		return Event{Key: KeyRune, Rune: rune(value), Alt: true}, true
	}
	event, ok := parser.ground(value)
	event.Alt = ok
	return event, ok
}

func (parser *Parser) csi(value byte) (Event, bool) {
	switch {
	case value >= '0' && value <= '9':
		if parser.parameters == 0 {
			parser.parameters = 1
		}
		if parser.digits == maxKeyDigits {
			parser.state = parserDiscard
			return Event{}, false
		}
		last := &parser.parameter[parser.parameters-1]
		*last = *last*10 + uint16(value-'0')
		parser.digits++
		return Event{}, false
	case value == ';':
		if parser.parameters == 0 {
			parser.parameters = 1
		}
		if parser.parameters == maxKeyParameters {
			parser.state = parserDiscard
			return Event{}, false
		}
		parser.parameters++
		parser.digits = 0
		return Event{}, false
	case value == '[' && parser.parameters == 0:
		parser.state = parserLinux
		return Event{}, false
	}
	parser.state = parserGround
	switch value {
	case 'A':
		return Event{Key: KeyUp}, true
	case 'B':
		return Event{Key: KeyDown}, true
	case 'C':
		return Event{Key: KeyRight}, true
	case 'D':
		return Event{Key: KeyLeft}, true
	case 'H':
		return Event{Key: KeyHome}, true
	case 'F':
		return Event{Key: KeyEnd}, true
	case 'P', 'Q', 'R', 'S':
		// ESC [ 1 ; m P: a modified F1-F4.
		return Event{Key: KeyF1 + Key(value-'P')}, true
	case '~':
		if key := tildeKey(parser.parameter[0]); key != 0 {
			return Event{Key: key}, true
		}
	}
	return Event{}, false
}

// tildeKey names the key of ESC [ number ~, as xterm and GoOS number them.
func tildeKey(number uint16) Key {
	switch number {
	case 1, 7:
		return KeyHome
	case 2:
		return KeyInsert
	case 3:
		return KeyDelete
	case 4, 8:
		return KeyEnd
	case 5:
		return KeyPageUp
	case 6:
		return KeyPageDown
	case 11, 12, 13, 14, 15:
		return KeyF1 + Key(number-11)
	case 17, 18, 19, 20, 21:
		return KeyF6 + Key(number-17)
	case 23, 24:
		return KeyF11 + Key(number-23)
	}
	return 0
}

// ss3Key names the key of ESC O final, which terminals in application mode
// send for the arrows, Home, End and F1-F4.
func ss3Key(value byte) (Event, bool) {
	switch value {
	case 'A':
		return Event{Key: KeyUp}, true
	case 'B':
		return Event{Key: KeyDown}, true
	case 'C':
		return Event{Key: KeyRight}, true
	case 'D':
		return Event{Key: KeyLeft}, true
	case 'H':
		return Event{Key: KeyHome}, true
	case 'F':
		return Event{Key: KeyEnd}, true
	case 'P', 'Q', 'R', 'S':
		return Event{Key: KeyF1 + Key(value-'P')}, true
	}
	return Event{}, false
}

func (parser *Parser) startUTF8(value byte) (Event, bool) {
	switch {
	case value >= 0xc2 && value <= 0xdf:
		parser.need = 2
	case value >= 0xe0 && value <= 0xef:
		parser.need = 3
	case value >= 0xf0 && value <= 0xf4:
		parser.need = 4
	default:
		return Event{}, false
	}
	parser.unicode[0], parser.have, parser.state = value, 1, parserUTF8
	return Event{}, false
}

func (parser *Parser) continuation(value byte) (Event, bool) {
	if value&0xc0 != 0x80 {
		// A truncated character is dropped; the byte starts something new.
		parser.state = parserGround
		return parser.ground(value)
	}
	parser.unicode[parser.have] = value
	parser.have++
	if parser.have < parser.need {
		return Event{}, false
	}
	parser.state = parserGround
	scalar := rune(parser.unicode[0]) & (0x7f >> parser.need)
	for index := uint8(1); index < parser.need; index++ {
		scalar = scalar<<6 | rune(parser.unicode[index]&0x3f)
	}
	minimum := [5]rune{0, 0, 0x80, 0x800, 0x10000}[parser.need]
	if scalar < minimum || scalar > 0x10ffff || scalar >= 0xd800 && scalar <= 0xdfff {
		return Event{}, false
	}
	return Event{Key: KeyRune, Rune: scalar}, true
}
