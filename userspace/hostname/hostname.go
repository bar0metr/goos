// Package hostname is the host-name syntax the resolver and the hosts table
// share: ASCII DNS names of letters, digits and hyphens, compared without
// case. It holds no state and allocates nothing.
package hostname

const (
	// MaxText is the longest name in text form, without a trailing dot.
	MaxText = 253
	// MaxLabel is the longest label between two dots.
	MaxLabel = 63
)

// Name is a normalized host name: lowercase, no trailing dot, every label
// 1..63 bytes of letters, digits and hyphens that neither starts nor ends
// with a hyphen.
type Name struct {
	Text [MaxText]byte
	Size uint8
}

// Bytes returns the name's text.
func (name *Name) Bytes() []byte { return name.Text[:name.Size] }

// Parse normalizes text into a name. It accepts one trailing dot and folds
// upper case to lower case. Anything else outside the syntax fails, so two
// spellings of one name always compare equal and nothing else does.
func Parse(text []byte) (Name, bool) {
	var name Name
	if len(text) != 0 && text[len(text)-1] == '.' {
		text = text[:len(text)-1]
	}
	if len(text) == 0 || len(text) > MaxText {
		return Name{}, false
	}
	label := 0
	for index, character := range text {
		switch {
		case character == '.':
			if label == 0 || text[index-1] == '-' {
				return Name{}, false
			}
			label = 0
		case character == '-':
			if label == 0 {
				return Name{}, false
			}
			label++
		case character >= 'A' && character <= 'Z':
			character += 'a' - 'A'
			label++
		case character >= 'a' && character <= 'z', character >= '0' && character <= '9':
			label++
		default:
			return Name{}, false
		}
		if label > MaxLabel {
			return Name{}, false
		}
		name.Text[index] = character
	}
	if label == 0 || text[len(text)-1] == '-' {
		return Name{}, false
	}
	name.Size = uint8(len(text))
	return name, true
}

// Equal compares two normalized names.
func (name *Name) Equal(other *Name) bool {
	if name.Size != other.Size {
		return false
	}
	for index := 0; index < int(name.Size); index++ {
		if name.Text[index] != other.Text[index] {
			return false
		}
	}
	return true
}

// IsLocalhost reports localhost and every name under it (RFC 6761), which
// always mean the loopback address and never reach a DNS server.
func (name *Name) IsLocalhost() bool { return name.inZone("localhost") }

// IsLocalScope reports .local and every name under it, the multicast DNS
// zone (RFC 6762). Unicast DNS must not be asked about them.
func (name *Name) IsLocalScope() bool { return name.inZone("local") }

// IsText reports whether the name is exactly text, which must be
// normalized.
func (name *Name) IsText(text string) bool {
	if int(name.Size) != len(text) {
		return false
	}
	for index := 0; index < len(text); index++ {
		if name.Text[index] != text[index] {
			return false
		}
	}
	return true
}

func (name *Name) inZone(zone string) bool {
	size := int(name.Size)
	if size < len(zone) {
		return false
	}
	start := size - len(zone)
	if start != 0 && name.Text[start-1] != '.' {
		return false
	}
	for index := 0; index < len(zone); index++ {
		if name.Text[start+index] != zone[index] {
			return false
		}
	}
	return true
}
