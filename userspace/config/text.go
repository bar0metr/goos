package config

// The exported form is one entry per line:
//
//	system.shell.history uint 32
//	system.log.persist bool true
//	user.name text nikita
//
// It is the import format as well as the export format, and it is what the
// service writes to persistent storage. A line names the type, so a value read
// back is checked against the declaration rather than guessed from its shape.
// Read-only entries are not exported: they come from the schema the running
// image declares, and a stored copy of them would be a second source of truth.

const kindNames = "bool uint text"

// AppendEntry appends one entry's line, including its newline.
func AppendEntry(target []byte, key []byte, value Value) ([]byte, bool) {
	if !validKey(key) || !value.valid() {
		return target, false
	}
	target = append(target, key...)
	target = append(target, ' ')
	switch value.Kind {
	case KindBool:
		target = append(target, "bool "...)
		if value.Uint == 1 {
			target = append(target, "true"...)
		} else {
			target = append(target, "false"...)
		}
	case KindUint:
		target = append(target, "uint "...)
		target = appendUint(target, value.Uint)
	case KindText:
		target = append(target, "text "...)
		target = append(target, value.Bytes()...)
	default:
		return target, false
	}
	return append(target, '\n'), true
}

// ParseEntry reads one line of the exported form. It returns a slice of the
// line for the key, so the caller copies it before the buffer is reused.
func ParseEntry(line []byte) ([]byte, Value, bool) {
	key, rest, ok := field(line)
	if !ok || !validKey(key) {
		return nil, Value{}, false
	}
	kind, rest, ok := field(rest)
	if !ok {
		return nil, Value{}, false
	}
	switch {
	case equal(kind, []byte("bool")):
		if equal(rest, []byte("true")) {
			return key, Bool(true), true
		}
		if equal(rest, []byte("false")) {
			return key, Bool(false), true
		}
		return nil, Value{}, false
	case equal(kind, []byte("uint")):
		number, ok := ParseUint(rest)
		if !ok {
			return nil, Value{}, false
		}
		return key, Uint(number), true
	case equal(kind, []byte("text")):
		value, ok := Text(rest)
		if !ok {
			return nil, Value{}, false
		}
		return key, value, true
	default:
		return nil, Value{}, false
	}
}

// field splits off the next space-separated field. The remainder keeps its own
// spaces, so a text value may contain them while a key and a type may not.
func field(line []byte) ([]byte, []byte, bool) {
	for index, character := range line {
		if character == ' ' {
			if index == 0 {
				return nil, nil, false
			}
			return line[:index], line[index+1:], true
		}
	}
	return nil, nil, false
}

func appendUint(target []byte, value uint64) []byte {
	var digits [20]byte
	size := 0
	for {
		digits[size] = byte('0' + value%10)
		size++
		value /= 10
		if value == 0 {
			break
		}
	}
	for index := size - 1; index >= 0; index-- {
		target = append(target, digits[index])
	}
	return target
}

// ParseUint reads up to 20 decimal digits and refuses any value above the
// uint64 maximum rather than letting it wrap.
func ParseUint(text []byte) (uint64, bool) {
	if len(text) == 0 || len(text) > 20 {
		return 0, false
	}
	value := uint64(0)
	for _, character := range text {
		if character < '0' || character > '9' {
			return 0, false
		}
		digit := uint64(character - '0')
		if value > (^uint64(0)-digit)/10 {
			return 0, false
		}
		value = value*10 + digit
	}
	return value, true
}
