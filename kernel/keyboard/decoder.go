package keyboard

// MaxBytes bounds the terminal bytes one scan-code byte may produce. The
// longest sequence the decoder emits is a function key, ESC [ 2 4 ~.
const MaxBytes = 8

// Output is the bounded terminal-byte result of one scan-code byte. A key's
// sequence is produced whole by one Feed, so the IRQ path can enqueue it all
// or drop it all.
type Output struct {
	Bytes     [MaxBytes]byte
	Length    uint8
	Interrupt bool
}

// Decoder converts PS/2 scan-code set 1 into bytes for the terminal input
// stream. Hardware transport and keyboard layout intentionally remain separate.
type Decoder struct {
	extended   bool
	leftShift  bool
	rightShift bool
	leftCtrl   bool
	rightCtrl  bool
	leftAlt    bool
	rightAlt   bool
	capsLock   bool
	pauseBytes uint8
}

func (decoder *Decoder) Feed(value byte) Output {
	if decoder.pauseBytes != 0 {
		decoder.pauseBytes--
		return Output{}
	}
	if value == 0xe0 {
		decoder.extended = true
		return Output{}
	}
	if value == 0xe1 {
		decoder.extended = false
		decoder.pauseBytes = 5
		return Output{}
	}
	extended := decoder.extended
	decoder.extended = false
	released := value&0x80 != 0
	code := value & 0x7f

	if extended {
		switch code {
		case 0x1d:
			decoder.rightCtrl = !released
		case 0x38:
			decoder.rightAlt = !released
		case 0x48:
			if !released {
				return sequence('A')
			}
		case 0x50:
			if !released {
				return sequence('B')
			}
		case 0x4d:
			if !released {
				return sequence('C')
			}
		case 0x4b:
			if !released {
				return sequence('D')
			}
		case 0x47:
			if !released {
				return sequence('H')
			}
		case 0x4f:
			if !released {
				return sequence('F')
			}
		case 0x52, 0x53, 0x49, 0x51:
			if !released {
				return tilde(editingKeys[code-0x49])
			}
		}
		return Output{}
	}

	switch code {
	case 0x2a:
		decoder.leftShift = !released
		return Output{}
	case 0x36:
		decoder.rightShift = !released
		return Output{}
	case 0x1d:
		decoder.leftCtrl = !released
		return Output{}
	case 0x38:
		decoder.leftAlt = !released
		return Output{}
	case 0x3a:
		if !released {
			decoder.capsLock = !decoder.capsLock
		}
		return Output{}
	}
	if released {
		return Output{}
	}
	if number := functionKey(code); number != 0 {
		// Modifiers do not change a function key's sequence.
		return tilde(number)
	}

	shift := decoder.leftShift || decoder.rightShift
	value, letter := translateUS(code, shift)
	if value == 0 {
		return Output{}
	}
	if letter && decoder.capsLock {
		if value >= 'a' && value <= 'z' {
			value -= 'a' - 'A'
		} else if value >= 'A' && value <= 'Z' {
			value += 'a' - 'A'
		}
	}
	if decoder.leftCtrl || decoder.rightCtrl {
		upper := value
		if upper >= 'a' && upper <= 'z' {
			upper -= 'a' - 'A'
		}
		if upper >= 'A' && upper <= 'Z' {
			value = upper - 'A' + 1
			if value == 3 {
				return Output{Interrupt: true}
			}
		}
	}
	return Output{Bytes: [MaxBytes]byte{value}, Length: 1}
}

func sequence(final byte) Output {
	return Output{Bytes: [MaxBytes]byte{0x1b, '[', final}, Length: 3}
}

// tilde encodes the xterm key sequence ESC [ number ~.
func tilde(number byte) Output {
	output := Output{Bytes: [MaxBytes]byte{0x1b, '['}, Length: 2}
	if number >= 10 {
		output.Bytes[output.Length] = '0' + number/10
		output.Length++
	}
	output.Bytes[output.Length] = '0' + number%10
	output.Bytes[output.Length+1] = '~'
	output.Length += 2
	return output
}

// editingKeys maps the extended codes 0x49..0x53 to their xterm numbers:
// PageUp 5, PageDown 6, Insert 2, Delete 3. Other slots are unused.
var editingKeys = [0x53 - 0x49 + 1]byte{0x49 - 0x49: 5, 0x51 - 0x49: 6, 0x52 - 0x49: 2, 0x53 - 0x49: 3}

// functionKey returns the xterm number of the F key with make code code, or
// zero: F1..F12 are ESC [ 11 ~ .. ESC [ 24 ~, skipping 16 and 22.
func functionKey(code byte) byte {
	switch {
	case code >= 0x3b && code <= 0x3f:
		return 11 + code - 0x3b
	case code >= 0x40 && code <= 0x44:
		return 17 + code - 0x40
	case code == 0x57:
		return 23
	case code == 0x58:
		return 24
	}
	return 0
}

func translateUS(code byte, shift bool) (byte, bool) {
	if int(code) >= len(unshifted) {
		return 0, false
	}
	value := unshifted[code]
	if shift {
		value = shifted[code]
	}
	return value, value >= 'A' && value <= 'Z' || value >= 'a' && value <= 'z'
}

var unshifted = [128]byte{
	0x01: 0x1b, 0x02: '1', 0x03: '2', 0x04: '3', 0x05: '4', 0x06: '5', 0x07: '6', 0x08: '7', 0x09: '8', 0x0a: '9', 0x0b: '0', 0x0c: '-', 0x0d: '=', 0x0e: '\b', 0x0f: '\t',
	0x10: 'q', 0x11: 'w', 0x12: 'e', 0x13: 'r', 0x14: 't', 0x15: 'y', 0x16: 'u', 0x17: 'i', 0x18: 'o', 0x19: 'p', 0x1a: '[', 0x1b: ']', 0x1c: '\n',
	0x1e: 'a', 0x1f: 's', 0x20: 'd', 0x21: 'f', 0x22: 'g', 0x23: 'h', 0x24: 'j', 0x25: 'k', 0x26: 'l', 0x27: ';', 0x28: '\'', 0x29: '`', 0x2b: '\\',
	0x2c: 'z', 0x2d: 'x', 0x2e: 'c', 0x2f: 'v', 0x30: 'b', 0x31: 'n', 0x32: 'm', 0x33: ',', 0x34: '.', 0x35: '/', 0x39: ' ',
}

var shifted = [128]byte{
	0x01: 0x1b, 0x02: '!', 0x03: '@', 0x04: '#', 0x05: '$', 0x06: '%', 0x07: '^', 0x08: '&', 0x09: '*', 0x0a: '(', 0x0b: ')', 0x0c: '_', 0x0d: '+', 0x0e: '\b', 0x0f: '\t',
	0x10: 'Q', 0x11: 'W', 0x12: 'E', 0x13: 'R', 0x14: 'T', 0x15: 'Y', 0x16: 'U', 0x17: 'I', 0x18: 'O', 0x19: 'P', 0x1a: '{', 0x1b: '}', 0x1c: '\n',
	0x1e: 'A', 0x1f: 'S', 0x20: 'D', 0x21: 'F', 0x22: 'G', 0x23: 'H', 0x24: 'J', 0x25: 'K', 0x26: 'L', 0x27: ':', 0x28: '"', 0x29: '~', 0x2b: '|',
	0x2c: 'Z', 0x2d: 'X', 0x2e: 'C', 0x2f: 'V', 0x30: 'B', 0x31: 'N', 0x32: 'M', 0x33: '<', 0x34: '>', 0x35: '?', 0x39: ' ',
}
