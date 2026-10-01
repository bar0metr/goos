// Package release reads and writes the installation record of an installed
// system, the file /system/release on the volume (ADR 0175). The record says
// what the system was built as: its format, architecture, release, and the
// versions of the interfaces between PID 1 and the programs it starts. PID 1
// compares it with its own before it trusts anything else under /system.
//
// This is a compatibility check, not verification: the record proves nothing
// about who wrote the volume.
package release

import (
	"goos/internal/version"
	"goos/kernel/loader"
	"goos/userspace/commands"
	"goos/userspace/startup"
)

const (
	// Format is the record format this package reads and writes.
	Format = 1
	// MaxSize bounds the record. A larger file is refused unread.
	MaxSize = 512
	// MaxValue bounds one value.
	MaxValue = 32
	// maxNumber bounds a numeric value, so it always fits a uint32.
	maxNumber = 999999999
)

// Record is a decoded installation record. Text values are copied out of the
// file, so a record does not alias the bytes it was read from.
type Record struct {
	Format uint32
	// Arch and Release are the machine and release the system was built for.
	Arch, Release         [MaxValue]byte
	ArchSize, ReleaseSize uint8
	// Startup, Image, and Index are the versions of the startup block a
	// program is launched with, of the native image format, and of the
	// command index format.
	Startup, Image, Index uint32
}

// Problem says why a record was refused.
type Problem uint8

const (
	OK Problem = iota
	ProblemTooLarge
	ProblemMalformed
	ProblemDuplicate
	ProblemUnknownKey
	ProblemMissing
	ProblemFormat
	ProblemArch
	ProblemRelease
	ProblemStartup
	ProblemImage
	ProblemIndex
)

// String is the reason a boot that refused the record reports.
func (problem Problem) String() string {
	switch problem {
	case OK:
		return "compatible"
	case ProblemTooLarge:
		return "record too large"
	case ProblemMalformed:
		return "record malformed"
	case ProblemDuplicate:
		return "record repeats a field"
	case ProblemUnknownKey:
		return "record has an unknown field"
	case ProblemMissing:
		return "record lacks a field"
	case ProblemFormat:
		return "unsupported record format"
	case ProblemArch:
		return "architecture mismatch"
	case ProblemRelease:
		return "release mismatch"
	case ProblemStartup:
		return "startup interface mismatch"
	case ProblemImage:
		return "native image format mismatch"
	case ProblemIndex:
		return "command index format mismatch"
	}
	return "record refused"
}

// Field identifiers, in the order Encode writes them.
const (
	fieldFormat = iota
	fieldArch
	fieldRelease
	fieldStartup
	fieldImage
	fieldIndex
	fieldCount
)

var fieldNames = [fieldCount]string{"format", "arch", "release", "startup", "image", "index"}

// Current is the record of the system this build is: what PID 1 requires an
// installed system to match, and what the build writes onto the volume.
func Current() Record {
	record := Record{Format: Format, Startup: startup.Version, Image: uint32(loader.NativeVersion), Index: commands.IndexFormat}
	record.ArchSize = uint8(copy(record.Arch[:], version.Machine))
	record.ReleaseSize = uint8(copy(record.Release[:], version.Release))
	return record
}

// ArchText and ReleaseText return the text values.
func (record *Record) ArchText() []byte    { return record.Arch[:record.ArchSize] }
func (record *Record) ReleaseText() []byte { return record.Release[:record.ReleaseSize] }

// Parse decodes a record. Each line is a field name, one space, and a value,
// ending in a line feed. The first line must be the format; a format this
// package does not know is refused before anything else is read, so a later
// format may change the rest. Every field must appear exactly once, and no
// other may appear.
func Parse(data []byte) (Record, Problem) {
	if len(data) > MaxSize {
		return Record{}, ProblemTooLarge
	}
	var record Record
	var seen [fieldCount]bool
	for offset, line := 0, 0; offset < len(data); line++ {
		end := offset
		for end < len(data) && data[end] != '\n' {
			end++
		}
		if end == len(data) {
			return Record{}, ProblemMalformed
		}
		field, value, ok := split(data[offset:end])
		if !ok {
			return Record{}, ProblemMalformed
		}
		if line == 0 && field != fieldFormat {
			return Record{}, ProblemMissing
		}
		if field < 0 {
			return Record{}, ProblemUnknownKey
		}
		if seen[field] {
			return Record{}, ProblemDuplicate
		}
		seen[field] = true
		switch field {
		case fieldArch:
			record.ArchSize = uint8(copy(record.Arch[:], value))
		case fieldRelease:
			record.ReleaseSize = uint8(copy(record.Release[:], value))
		default:
			number, valid := decimal(value)
			if !valid {
				return Record{}, ProblemMalformed
			}
			switch field {
			case fieldFormat:
				if number != Format {
					return Record{}, ProblemFormat
				}
				record.Format = number
			case fieldStartup:
				record.Startup = number
			case fieldImage:
				record.Image = number
			case fieldIndex:
				record.Index = number
			}
		}
		offset = end + 1
	}
	for _, present := range seen {
		if !present {
			return Record{}, ProblemMissing
		}
	}
	return record, OK
}

// Check compares an installed record with the one this build requires and
// returns the first difference. The release must match exactly: the first
// installed systems are tied to the boot archive that starts them.
func Check(installed, required *Record) Problem {
	switch {
	case installed.Format != required.Format:
		return ProblemFormat
	case !equal(installed.ArchText(), required.ArchText()):
		return ProblemArch
	case !equal(installed.ReleaseText(), required.ReleaseText()):
		return ProblemRelease
	case installed.Startup != required.Startup:
		return ProblemStartup
	case installed.Image != required.Image:
		return ProblemImage
	case installed.Index != required.Index:
		return ProblemIndex
	}
	return OK
}

// Encode writes a record in the form Parse reads, fields in a fixed order.
func Encode(record *Record) []byte {
	output := make([]byte, 0, MaxSize)
	for field := 0; field < fieldCount; field++ {
		output = append(output, fieldNames[field]...)
		output = append(output, ' ')
		switch field {
		case fieldFormat:
			output = appendDecimal(output, record.Format)
		case fieldArch:
			output = append(output, record.ArchText()...)
		case fieldRelease:
			output = append(output, record.ReleaseText()...)
		case fieldStartup:
			output = appendDecimal(output, record.Startup)
		case fieldImage:
			output = appendDecimal(output, record.Image)
		case fieldIndex:
			output = appendDecimal(output, record.Index)
		}
		output = append(output, '\n')
	}
	return output
}

// split separates one line into its field and value. The field is -1 for a
// name this format does not define. A value is 1 to MaxValue characters of
// letters, digits, '.', '-', and '_'.
func split(line []byte) (int, []byte, bool) {
	space := -1
	for index, value := range line {
		if value == ' ' {
			space = index
			break
		}
	}
	if space <= 0 {
		return 0, nil, false
	}
	name, value := line[:space], line[space+1:]
	if len(value) == 0 || len(value) > MaxValue {
		return 0, nil, false
	}
	for _, character := range value {
		if !(character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' ||
			character == '.' || character == '-' || character == '_') {
			return 0, nil, false
		}
	}
	for field := range fieldNames {
		if equal(name, []byte(fieldNames[field])) {
			return field, value, true
		}
	}
	for _, character := range name {
		if !(character >= 'a' && character <= 'z') {
			return 0, nil, false
		}
	}
	return -1, value, true
}

// decimal reads a number without sign or leading zeros.
func decimal(value []byte) (uint32, bool) {
	if len(value) == 0 || len(value) > 1 && value[0] == '0' {
		return 0, false
	}
	number := uint64(0)
	for _, digit := range value {
		if digit < '0' || digit > '9' {
			return 0, false
		}
		number = number*10 + uint64(digit-'0')
		if number > maxNumber {
			return 0, false
		}
	}
	return uint32(number), true
}

func appendDecimal(output []byte, value uint32) []byte {
	var digits [10]byte
	size := len(digits)
	for {
		size--
		digits[size] = byte('0' + value%10)
		value /= 10
		if value == 0 {
			break
		}
	}
	return append(output, digits[size:]...)
}

func equal(left, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
