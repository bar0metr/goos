package terminal

// Width returns the columns value takes on the console: one per character.
// The console's glyphs are all one cell wide, so this is the count of UTF-8
// sequences; a byte that starts none counts as one, as the console shows it
// as one replacement glyph.
func Width(value []byte) int {
	columns := 0
	for index := 0; index < len(value); {
		index += characterSize(value, index)
		columns++
	}
	return columns
}

// Cut returns the longest prefix of value that fits in columns, ending on a
// character boundary.
func Cut(value []byte, columns int) []byte {
	index := 0
	for used := 0; index < len(value) && used < columns; used++ {
		index += characterSize(value, index)
	}
	return value[:index]
}

// characterSize returns the length of the well-formed UTF-8 sequence at
// value[index], or 1 for a byte that does not start one.
func characterSize[T ~string | ~[]byte](value T, index int) int {
	lead := value[index]
	size := 1
	switch {
	case lead < 0x80:
		return 1
	case lead >= 0xc2 && lead <= 0xdf:
		size = 2
	case lead >= 0xe0 && lead <= 0xef:
		size = 3
	case lead >= 0xf0 && lead <= 0xf4:
		size = 4
	default:
		return 1
	}
	if index+size > len(value) {
		return 1
	}
	for offset := 1; offset < size; offset++ {
		if value[index+offset]&0xc0 != 0x80 {
			return 1
		}
	}
	return size
}

// questionMark stands in for a character a terminal could obey.
var questionMark = []byte{'?'}

// Printable returns the character of value at index as it may be shown, and
// the index of the next one. C0 controls, DEL, the C1 controls U+0080 to
// U+009F and bytes that start no valid UTF-8 character come back as '?':
// some terminals obey a C1 control or a lone 0x9b as CSI, so text from a file
// name or a volume label must reach them as data only.
func Printable(value []byte, index int) ([]byte, int) {
	size := characterSize(value, index)
	character := value[index : index+size]
	lead := character[0]
	switch {
	case size == 1 && (lead < 0x20 || lead >= 0x7f):
		return questionMark, index + 1
	case size == 2 && lead == 0xc2 && character[1] < 0xa0:
		return questionMark, index + 2
	}
	return character, index + size
}
