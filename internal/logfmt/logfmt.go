// Package logfmt defines the structured log record shared by the kernel log
// and userspace: its bounded wire encoding and its console text form.
package logfmt

type Level uint8

const (
	Debug Level = iota + 1
	Info
	Warn
	Error
)

const (
	Version         = 1
	MaxComponent    = 16
	MaxMessage      = 64
	MaxFields       = 96
	headerSize      = 5
	MaxWire         = headerSize + MaxComponent + MaxMessage + MaxFields
	MaxText         = 240
	nanosPerSecond  = 1_000_000_000
	nanosPerMicro   = 1_000
	fractionDigits  = 6
	secondsMinWidth = 5
)

var levelNames = [...]string{Debug: "DEBUG", Info: "INFO ", Warn: "WARN ", Error: "ERROR"}

func (level Level) Valid() bool { return level >= Debug && level <= Error }

func (level Level) String() string {
	if !level.Valid() {
		return "?    "
	}
	return levelNames[level]
}

// Record is one structured event. Fields holds `key=value` pairs separated by
// single spaces, already validated.
type Record struct {
	Time      uint64
	Process   uint64
	Level     Level
	Component []byte
	Message   []byte
	Fields    []byte
}

// Encode writes the caller-supplied part of a record: level, component,
// message, and fields. Time and process are assigned by the kernel.
func Encode(level Level, component, message, fields []byte, target []byte) (int, bool) {
	if !level.Valid() || !validName(component) || !validText(message, MaxMessage, false) || !validFields(fields) {
		return 0, false
	}
	size := headerSize + len(component) + len(message) + len(fields)
	if len(target) < size {
		return 0, false
	}
	target[0], target[1] = Version, byte(level)
	target[2], target[3], target[4] = byte(len(component)), byte(len(message)), byte(len(fields))
	offset := headerSize
	offset += copy(target[offset:], component)
	offset += copy(target[offset:], message)
	copy(target[offset:], fields)
	return size, true
}

// Decode validates untrusted wire bytes. The returned slices alias data.
func Decode(data []byte) (Record, bool) {
	if len(data) < headerSize || data[0] != Version {
		return Record{}, false
	}
	level := Level(data[1])
	componentSize, messageSize, fieldsSize := int(data[2]), int(data[3]), int(data[4])
	if headerSize+componentSize+messageSize+fieldsSize != len(data) {
		return Record{}, false
	}
	component := data[headerSize : headerSize+componentSize]
	message := data[headerSize+componentSize : headerSize+componentSize+messageSize]
	fields := data[headerSize+componentSize+messageSize:]
	if !level.Valid() || !validName(component) || !validText(message, MaxMessage, false) || !validFields(fields) {
		return Record{}, false
	}
	return Record{Level: level, Component: component, Message: message, Fields: fields}, true
}

// Format renders `[seconds.micros] LEVEL component[pid]: message fields`.
func Format(record Record, target []byte) int {
	if len(target) < MaxText {
		return 0
	}
	size := 0
	target[size] = '['
	size++
	seconds := record.Time / nanosPerSecond
	micros := record.Time % nanosPerSecond / nanosPerMicro
	size += putPadded(target[size:], seconds, secondsMinWidth, ' ')
	target[size] = '.'
	size++
	size += putPadded(target[size:], micros, fractionDigits, '0')
	size += copy(target[size:], "] ")
	size += copy(target[size:], record.Level.String())
	target[size] = ' '
	size++
	size += copy(target[size:], record.Component)
	target[size] = '['
	size++
	size += putPadded(target[size:], record.Process, 1, ' ')
	size += copy(target[size:], "]: ")
	size += copy(target[size:], record.Message)
	if len(record.Fields) != 0 {
		target[size] = ' '
		size++
		size += copy(target[size:], record.Fields)
	}
	target[size] = '\n'
	return size + 1
}

// AppendField appends `key=value` to fields, quoting a value that contains a
// space. It reports false when the result would be invalid or too long.
func AppendField(fields []byte, key string, value []byte) ([]byte, bool) {
	if !validName([]byte(key)) || !validText(value, MaxFields, true) {
		return fields, false
	}
	quoted := false
	for _, character := range value {
		if character == ' ' {
			quoted = true
		}
	}
	need := len(key) + 1 + len(value)
	if quoted {
		need += 2
	}
	if len(fields) != 0 {
		need++
	}
	if len(fields)+need > MaxFields {
		return fields, false
	}
	if len(fields) != 0 {
		fields = append(fields, ' ')
	}
	fields = append(fields, key...)
	fields = append(fields, '=')
	if quoted {
		fields = append(fields, '"')
	}
	fields = append(fields, value...)
	if quoted {
		fields = append(fields, '"')
	}
	return fields, true
}

// AppendUint formats value in decimal into target.
func AppendUint(target []byte, value uint64) []byte {
	var digits [20]byte
	position := len(digits)
	for {
		position--
		digits[position] = byte('0' + value%10)
		value /= 10
		if value == 0 {
			break
		}
	}
	return append(target, digits[position:]...)
}

func putPadded(target []byte, value uint64, width int, pad byte) int {
	var buffer [20]byte
	digits := AppendUint(buffer[:0], value)
	size := 0
	for index := len(digits); index < width; index++ {
		target[size] = pad
		size++
	}
	return size + copy(target[size:], digits)
}

func validName(name []byte) bool {
	if len(name) == 0 || len(name) > MaxComponent {
		return false
	}
	for _, character := range name {
		if !(character >= 'a' && character <= 'z' || character >= '0' && character <= '9' || character == '-' || character == '_') {
			return false
		}
	}
	return true
}

func validText(text []byte, limit int, allowEmpty bool) bool {
	if len(text) > limit || len(text) == 0 && !allowEmpty {
		return false
	}
	for _, character := range text {
		if character < ' ' || character > '~' || character == '"' {
			return false
		}
	}
	return true
}

func validFields(fields []byte) bool {
	if len(fields) > MaxFields {
		return false
	}
	for _, character := range fields {
		if character < ' ' || character > '~' {
			return false
		}
	}
	return true
}
