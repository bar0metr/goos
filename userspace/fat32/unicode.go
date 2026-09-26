package fat32

import "unicode"

// Native names are UTF-8; VFAT long names are UTF-16LE; 8.3 names are bytes in
// an OEM code page. This file converts between them without allocating and
// with the standard library's generated Unicode simple-case tables.
//
// Policy (docs/userspace/storage.md):
//   - A native name must be valid UTF-8 (no surrogates, no overlong forms, no
//     code point above U+10FFFF). It is stored exactly: no Unicode
//     normalization is applied, so a precomposed and a decomposed spelling are
//     different names.
//   - Names are matched case-insensitively, as VFAT requires, through simple
//     one-to-one Unicode simple upper-case mapping. It is deterministic and
//     locale-independent; multi-code-point full mappings are not used.
//   - 8.3 bytes from 0x80 up are read as code page 437, the Linux default.

const (
	surrogateHigh = 0xd800
	surrogateLow  = 0xdc00
	surrogateEnd  = 0xe000
	maxRune       = 0x10ffff
	replacement   = rune(-1)
)

// decodeRune decodes the UTF-8 code point at the start of data. It returns
// its size, zero for anything that is not valid, shortest-form UTF-8.
func decodeRune(data []byte) (rune, int) {
	if len(data) == 0 {
		return replacement, 0
	}
	first := data[0]
	switch {
	case first < 0x80:
		return rune(first), 1
	case first < 0xc2:
		return replacement, 0
	case first < 0xe0:
		if len(data) < 2 || data[1]&0xc0 != 0x80 {
			return replacement, 0
		}
		return rune(first&0x1f)<<6 | rune(data[1]&0x3f), 2
	case first < 0xf0:
		if len(data) < 3 || data[1]&0xc0 != 0x80 || data[2]&0xc0 != 0x80 {
			return replacement, 0
		}
		value := rune(first&0x0f)<<12 | rune(data[1]&0x3f)<<6 | rune(data[2]&0x3f)
		if value < 0x800 || value >= surrogateHigh && value < surrogateEnd {
			return replacement, 0
		}
		return value, 3
	case first < 0xf5:
		if len(data) < 4 || data[1]&0xc0 != 0x80 || data[2]&0xc0 != 0x80 || data[3]&0xc0 != 0x80 {
			return replacement, 0
		}
		value := rune(first&0x07)<<18 | rune(data[1]&0x3f)<<12 | rune(data[2]&0x3f)<<6 | rune(data[3]&0x3f)
		if value < 0x10000 || value > maxRune {
			return replacement, 0
		}
		return value, 4
	}
	return replacement, 0
}

// encodeRune writes value as UTF-8 at the start of target and returns its
// size, or zero when target is too short.
func encodeRune(target []byte, value rune) int {
	switch {
	case value < 0x80:
		if len(target) < 1 {
			return 0
		}
		target[0] = byte(value)
		return 1
	case value < 0x800:
		if len(target) < 2 {
			return 0
		}
		target[0], target[1] = 0xc0|byte(value>>6), 0x80|byte(value)&0x3f
		return 2
	case value < 0x10000:
		if len(target) < 3 {
			return 0
		}
		target[0], target[1], target[2] = 0xe0|byte(value>>12), 0x80|byte(value>>6)&0x3f, 0x80|byte(value)&0x3f
		return 3
	}
	if len(target) < 4 {
		return 0
	}
	target[0], target[1] = 0xf0|byte(value>>18), 0x80|byte(value>>12)&0x3f
	target[2], target[3] = 0x80|byte(value>>6)&0x3f, 0x80|byte(value)&0x3f
	return 4
}

// utf16To8 converts count UTF-16 units to UTF-8 in target. It fails on an
// unpaired surrogate and when the result does not fit target.
func utf16To8(units []uint16, target []byte) (int, bool) {
	size := 0
	for index := 0; index < len(units); index++ {
		value := rune(units[index])
		switch {
		case value >= surrogateLow && value < surrogateEnd:
			return 0, false
		case value >= surrogateHigh && value < surrogateLow:
			if index+1 >= len(units) {
				return 0, false
			}
			low := rune(units[index+1])
			if low < surrogateLow || low >= surrogateEnd {
				return 0, false
			}
			value = 0x10000 + (value-surrogateHigh)<<10 + (low - surrogateLow)
			index++
		}
		written := encodeRune(target[size:], value)
		if written == 0 {
			return 0, false
		}
		size += written
	}
	return size, true
}

// foldRune maps a code point through Go's generated, compact Unicode simple
// upper-case table. This is one-to-one, deterministic and locale-independent.
func foldRune(value rune) rune {
	return unicode.ToUpper(value)
}

// sameFolded compares two valid UTF-8 names under foldRune.
func sameFolded(first, second []byte) bool {
	for len(first) != 0 && len(second) != 0 {
		a, sizeA := decodeRune(first)
		b, sizeB := decodeRune(second)
		if sizeA == 0 || sizeB == 0 || foldRune(a) != foldRune(b) {
			return false
		}
		first, second = first[sizeA:], second[sizeB:]
	}
	return len(first) == 0 && len(second) == 0
}

// oem437 maps the upper half of code page 437 to Unicode.
var oem437 = [128]uint16{
	0x00c7, 0x00fc, 0x00e9, 0x00e2, 0x00e4, 0x00e0, 0x00e5, 0x00e7, 0x00ea, 0x00eb, 0x00e8, 0x00ef, 0x00ee, 0x00ec, 0x00c4, 0x00c5,
	0x00c9, 0x00e6, 0x00c6, 0x00f4, 0x00f6, 0x00f2, 0x00fb, 0x00f9, 0x00ff, 0x00d6, 0x00dc, 0x00a2, 0x00a3, 0x00a5, 0x20a7, 0x0192,
	0x00e1, 0x00ed, 0x00f3, 0x00fa, 0x00f1, 0x00d1, 0x00aa, 0x00ba, 0x00bf, 0x2310, 0x00ac, 0x00bd, 0x00bc, 0x00a1, 0x00ab, 0x00bb,
	0x2591, 0x2592, 0x2593, 0x2502, 0x2524, 0x2561, 0x2562, 0x2556, 0x2555, 0x2563, 0x2551, 0x2557, 0x255d, 0x255c, 0x255b, 0x2510,
	0x2514, 0x2534, 0x252c, 0x251c, 0x2500, 0x253c, 0x255e, 0x255f, 0x255a, 0x2554, 0x2569, 0x2566, 0x2560, 0x2550, 0x256c, 0x2567,
	0x2568, 0x2564, 0x2565, 0x2559, 0x2558, 0x2552, 0x2553, 0x256b, 0x256a, 0x2518, 0x250c, 0x2588, 0x2584, 0x258c, 0x2590, 0x2580,
	0x03b1, 0x00df, 0x0393, 0x03c0, 0x03a3, 0x03c3, 0x00b5, 0x03c4, 0x03a6, 0x0398, 0x03a9, 0x03b4, 0x221e, 0x03c6, 0x03b5, 0x2229,
	0x2261, 0x00b1, 0x2265, 0x2264, 0x2320, 0x2321, 0x00f7, 0x2248, 0x00b0, 0x2219, 0x00b7, 0x221a, 0x207f, 0x00b2, 0x25a0, 0x00a0,
}

// oemRune is the code point an 8.3 name byte stands for.
func oemRune(value byte) rune {
	if value < 0x80 {
		return rune(value)
	}
	return rune(oem437[value-0x80])
}
