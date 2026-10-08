package dnswire

import "encoding/binary"

// Writer takes presentation text a byte or a constant at a time, so a
// formatter never hands a stack buffer across an interface call.
type Writer interface {
	Byte(character byte)
	Text(text string)
}

// MaxGenericData bounds the bytes of opaque record data shown in hex.
const MaxGenericData = 64

var typeNames = [...]struct {
	kind uint16
	name string
}{
	{TypeA, "A"}, {TypeNS, "NS"}, {TypeCNAME, "CNAME"}, {TypeSOA, "SOA"}, {TypePTR, "PTR"},
	{TypeMX, "MX"}, {TypeTXT, "TXT"}, {TypeAAAA, "AAAA"}, {TypeOPT, "OPT"},
}

// ParseType reads a query type name, ignoring case. OPT is not a query
// type.
func ParseType(text []byte) (uint16, bool) {
	for index := range typeNames {
		entry := &typeNames[index]
		if entry.kind != TypeOPT && equalFold(text, entry.name) {
			return entry.kind, true
		}
	}
	return 0, false
}

func equalFold(text []byte, upper string) bool {
	if len(text) != len(upper) {
		return false
	}
	for index, character := range text {
		if character >= 'a' && character <= 'z' {
			character -= 'a' - 'A'
		}
		if character != upper[index] {
			return false
		}
	}
	return true
}

// FormatType writes a type's mnemonic, or TYPEn for one without.
func FormatType(out Writer, kind uint16) {
	for index := range typeNames {
		if typeNames[index].kind == kind {
			out.Text(typeNames[index].name)
			return
		}
	}
	out.Text("TYPE")
	FormatDecimal(out, uint64(kind))
}

// FormatClass writes IN, CH, HS or CLASSn.
func FormatClass(out Writer, class uint16) {
	switch class {
	case ClassIN:
		out.Text("IN")
	case 3:
		out.Text("CH")
	case 4:
		out.Text("HS")
	default:
		out.Text("CLASS")
		FormatDecimal(out, uint64(class))
	}
}

var rcodeNames = [...]string{"NOERROR", "FORMERR", "SERVFAIL", "NXDOMAIN", "NOTIMP", "REFUSED"}

// FormatRcode writes an rcode's mnemonic, or RCODEn for one without.
func FormatRcode(out Writer, rcode uint16) {
	if int(rcode) < len(rcodeNames) {
		out.Text(rcodeNames[rcode])
		return
	}
	out.Text("RCODE")
	FormatDecimal(out, uint64(rcode))
}

func FormatDecimal(out Writer, value uint64) {
	divisor := uint64(1)
	for value/divisor >= 10 {
		divisor *= 10
	}
	for ; divisor != 0; divisor /= 10 {
		out.Byte(byte('0' + value/divisor%10))
	}
}

const hexDigits = "0123456789abcdef"

// escapeDecimal writes \DDD, the presentation escape for any byte.
func escapeDecimal(out Writer, character byte) {
	out.Byte('\\')
	out.Byte('0' + character/100)
	out.Byte('0' + character/10%10)
	out.Byte('0' + character%10)
}

// FormatName writes a name with a trailing dot. A dot or backslash inside a
// label is escaped with a backslash, and every byte outside printable ASCII,
// space included, as \DDD, so a hostile name cannot move the cursor, change
// colours or split a line.
func FormatName(out Writer, name *Name) {
	if name.size <= 1 {
		out.Byte('.')
		return
	}
	position := 0
	for {
		length := int(name.data[position])
		if length == 0 {
			return
		}
		for index := 1; index <= length; index++ {
			character := name.data[position+index]
			switch {
			case character == '.' || character == '\\':
				out.Byte('\\')
				out.Byte(character)
			case character <= ' ' || character > '~':
				escapeDecimal(out, character)
			default:
				out.Byte(character)
			}
		}
		out.Byte('.')
		position += 1 + length
	}
}

// FormatRData writes the data of a record that passed ValidRData: A, AAAA,
// NS, CNAME, PTR, MX, TXT and SOA in their usual text form, anything else in
// the RFC 3597 form \# LENGTH HEX with at most MaxGenericData bytes shown.
func FormatRData(out Writer, message []byte, item *Record, scratch *Name) {
	data := message[item.Data : item.Data+item.Length]
	if item.Class != ClassIN {
		formatGeneric(out, data)
		return
	}
	switch item.Type {
	case TypeA:
		for index, value := range data {
			if index != 0 {
				out.Byte('.')
			}
			FormatDecimal(out, uint64(value))
		}
	case TypeAAAA:
		formatIPv6(out, data)
	case TypeNS, TypeCNAME, TypePTR:
		DecodeName(message, item.Data, scratch)
		FormatName(out, scratch)
	case TypeMX:
		FormatDecimal(out, uint64(binary.BigEndian.Uint16(data)))
		out.Byte(' ')
		DecodeName(message, item.Data+2, scratch)
		FormatName(out, scratch)
	case TypeTXT:
		for position := 0; position < len(data); {
			if position != 0 {
				out.Byte(' ')
			}
			length := int(data[position])
			formatQuoted(out, data[position+1:position+1+length])
			position += 1 + length
		}
	case TypeSOA:
		next, _ := DecodeName(message, item.Data, scratch)
		FormatName(out, scratch)
		out.Byte(' ')
		next, _ = DecodeName(message, next, scratch)
		FormatName(out, scratch)
		for field := 0; field < SOAFixed; field += 4 {
			out.Byte(' ')
			FormatDecimal(out, uint64(binary.BigEndian.Uint32(message[next+field:])))
		}
	default:
		formatGeneric(out, data)
	}
}

// formatQuoted writes a character-string in double quotes. Quotes and
// backslashes are escaped with a backslash and bytes outside printable ASCII
// as \DDD.
func formatQuoted(out Writer, data []byte) {
	out.Byte('"')
	for _, character := range data {
		switch {
		case character == '"' || character == '\\':
			out.Byte('\\')
			out.Byte(character)
		case character < ' ' || character > '~':
			escapeDecimal(out, character)
		default:
			out.Byte(character)
		}
	}
	out.Byte('"')
}

func formatGeneric(out Writer, data []byte) {
	out.Text("\\# ")
	FormatDecimal(out, uint64(len(data)))
	if len(data) == 0 {
		return
	}
	out.Byte(' ')
	for index, value := range data {
		if index == MaxGenericData {
			out.Text(" ...")
			return
		}
		out.Byte(hexDigits[value>>4])
		out.Byte(hexDigits[value&0x0f])
	}
}

// formatIPv6 writes 16 bytes in the RFC 5952 form: lower-case hex without
// leading zeros, and the first longest run of two or more zero groups as
// "::".
func formatIPv6(out Writer, data []byte) {
	best, bestLength := -1, 1
	for start := 0; start < 8; {
		if binary.BigEndian.Uint16(data[start*2:]) != 0 {
			start++
			continue
		}
		end := start
		for end < 8 && binary.BigEndian.Uint16(data[end*2:]) == 0 {
			end++
		}
		if end-start > bestLength {
			best, bestLength = start, end-start
		}
		start = end
	}
	for group := 0; group < 8; group++ {
		if group == best {
			out.Text("::")
			group += bestLength - 1
			continue
		}
		if group != 0 && group != best+bestLength {
			out.Byte(':')
		}
		value := binary.BigEndian.Uint16(data[group*2:])
		started := false
		for shift := 12; shift >= 0; shift -= 4 {
			digit := value >> shift & 0x0f
			if digit != 0 || started || shift == 0 {
				out.Byte(hexDigits[digit])
				started = true
			}
		}
	}
}
