package partition

// GUID is a GPT GUID as stored on disk: the first three fields little-endian.
type GUID [16]byte

var (
	// TypeBasicData is Microsoft basic data, EBD0A0A2-B9E5-4433-87C0-68B6B72699C7.
	TypeBasicData = GUID{0xa2, 0xa0, 0xd0, 0xeb, 0xe5, 0xb9, 0x33, 0x44, 0x87, 0xc0, 0x68, 0xb6, 0xb7, 0x26, 0x99, 0xc7}
	// TypeEFISystem is the EFI system partition, C12A7328-F81F-11D2-BA4B-00A0C93EC93B.
	TypeEFISystem = GUID{0x28, 0x73, 0x2a, 0xc1, 0x1f, 0xf8, 0xd2, 0x11, 0xba, 0x4b, 0x00, 0xa0, 0xc9, 0x3e, 0xc9, 0x3b}
)

// guidOrder maps the canonical text order of the sixteen bytes to their
// positions on disk.
var guidOrder = [16]int{3, 2, 1, 0, 5, 4, 7, 6, 8, 9, 10, 11, 12, 13, 14, 15}

const hexDigits = "0123456789ABCDEF"

// String formats the GUID canonically, upper case.
func (guid GUID) String() string {
	var text [36]byte
	at := 0
	for index, position := range guidOrder {
		if index == 4 || index == 6 || index == 8 || index == 10 {
			text[at] = '-'
			at++
		}
		value := guid[position]
		text[at], text[at+1] = hexDigits[value>>4], hexDigits[value&0x0f]
		at += 2
	}
	return string(text[:])
}

// ParseGUID reads the canonical text form, in either case.
func ParseGUID(text string) (GUID, bool) {
	var guid GUID
	if len(text) != 36 {
		return guid, false
	}
	at := 0
	for index, position := range guidOrder {
		if index == 4 || index == 6 || index == 8 || index == 10 {
			if text[at] != '-' {
				return guid, false
			}
			at++
		}
		high, okHigh := hexValue(text[at])
		low, okLow := hexValue(text[at+1])
		if !okHigh || !okLow {
			return guid, false
		}
		guid[position] = high<<4 | low
		at += 2
	}
	return guid, true
}

func hexValue(value byte) (byte, bool) {
	switch {
	case value >= '0' && value <= '9':
		return value - '0', true
	case value >= 'a' && value <= 'f':
		return value - 'a' + 10, true
	case value >= 'A' && value <= 'F':
		return value - 'A' + 10, true
	}
	return 0, false
}

// zero reports whether the GUID is all zeros, which marks an unused entry.
func (guid GUID) zero() bool { return guid == GUID{} }
