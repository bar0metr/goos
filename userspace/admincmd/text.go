// Package admincmd contains bounded processors for native administration tools.
package admincmd

import "goos/userspace/config"

const (
	ChunkSize  = 256
	MaxInput   = uint64(64 * 1024 * 1024)
	MaxLine    = 8192
	MaxPattern = 256
)

type Status uint8

const (
	OK Status = iota
	EOF
	InputError
	OutputError
	LimitError
	ChangedError
)

type Reader interface{ Read([]byte) (int, Status) }
type Writer interface{ Write([]byte) Status }
type Rewinder interface {
	Reader
	Rewind() Status
}

// Count parses N without allocating a buffer proportional to it.
func Count(text []byte) (uint64, bool) { return config.ParseUint(text) }

func read(reader Reader, buffer []byte, total *uint64) (int, Status) {
	n, status := reader.Read(buffer)
	if status != OK && status != EOF {
		return 0, status
	}
	if n < 0 || n > len(buffer) || n == 0 && status == OK {
		return 0, InputError
	}
	if uint64(n) > MaxInput-*total {
		return 0, LimitError
	}
	*total += uint64(n)
	return n, status
}

func Head(reader Reader, writer Writer, lines uint64) Status {
	if lines == 0 {
		return OK
	}
	var buffer [ChunkSize]byte
	var total uint64
	for {
		n, status := read(reader, buffer[:], &total)
		if status != OK && status != EOF {
			return status
		}
		end := n
		for index, c := range buffer[:n] {
			if c == '\n' {
				lines--
				if lines == 0 {
					end = index + 1
					break
				}
			}
		}
		if end != 0 {
			if status := writer.Write(buffer[:end]); status != OK {
				return status
			}
		}
		if lines == 0 || status == EOF {
			return OK
		}
	}
}

// Tail uses two scans of the same file. Size/line-count changes are errors;
// same-size edits may be visible in the second pass and are not a snapshot.
func Tail(reader Rewinder, writer Writer, wanted uint64) Status {
	if wanted == 0 {
		return OK
	}
	var buffer [ChunkSize]byte
	var bytes, lines uint64
	var last byte
	for {
		n, status := read(reader, buffer[:], &bytes)
		if status != OK && status != EOF {
			return status
		}
		for _, c := range buffer[:n] {
			if c == '\n' {
				lines++
			}
			last = c
		}
		if status == EOF {
			break
		}
	}
	logical := lines
	if bytes != 0 && last != '\n' {
		logical++
	}
	skip := uint64(0)
	if logical > wanted {
		skip = logical - wanted
	}
	if status := reader.Rewind(); status != OK {
		return status
	}
	var scanned, secondLines uint64
	var secondLast byte
	for {
		n, status := read(reader, buffer[:], &scanned)
		if status != OK && status != EOF {
			return status
		}
		if scanned > bytes {
			return ChangedError
		}
		start := 0
		for index, c := range buffer[:n] {
			if c == '\n' {
				secondLines++
				if skip != 0 {
					skip--
					start = index + 1
				}
			}
			secondLast = c
		}
		if skip == 0 && start < n {
			if result := writer.Write(buffer[start:n]); result != OK {
				return result
			}
		}
		if status == EOF {
			if scanned != bytes || secondLines != lines || scanned != 0 && secondLast != last {
				return ChangedError
			}
			return OK
		}
	}
}

// Grep retains one bounded line; matches are literal bytes with optional ASCII
// folding. Unterminated matching lines remain unterminated in the output.
func Grep(reader Reader, writer Writer, pattern []byte, insensitive bool) (bool, Status) {
	if len(pattern) > MaxPattern {
		return false, LimitError
	}
	var buffer [ChunkSize]byte
	var line [MaxLine]byte
	var total uint64
	size := 0
	matched := false
	for {
		n, status := read(reader, buffer[:], &total)
		if status != OK && status != EOF {
			return matched, status
		}
		for _, c := range buffer[:n] {
			if c == '\n' {
				if contains(line[:size], pattern, insensitive) {
					matched = true
					if result := writer.Write(line[:size]); result != OK {
						return matched, result
					}
					if result := writer.Write([]byte{'\n'}); result != OK {
						return matched, result
					}
				}
				size = 0
			} else {
				if size == len(line) {
					return matched, LimitError
				}
				line[size] = c
				size++
			}
		}
		if status == EOF {
			if size != 0 && contains(line[:size], pattern, insensitive) {
				matched = true
				if result := writer.Write(line[:size]); result != OK {
					return matched, result
				}
			}
			return matched, OK
		}
	}
}

func contains(line, pattern []byte, insensitive bool) bool {
	for start := 0; start+len(pattern) <= len(line); start++ {
		match := true
		for index, c := range pattern {
			other := line[start+index]
			if insensitive {
				c, other = fold(c), fold(other)
			}
			if c != other {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

func fold(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + ('a' - 'A')
	}
	return c
}

type Counts struct{ Lines, Words, Bytes uint64 }

// WC counts LF bytes, ASCII-whitespace-delimited words, and bytes in that order.
func WC(reader Reader) (Counts, Status) {
	var buffer [ChunkSize]byte
	var counts Counts
	inWord := false
	for {
		n, status := read(reader, buffer[:], &counts.Bytes)
		if status != OK && status != EOF {
			return counts, status
		}
		for _, c := range buffer[:n] {
			if c == '\n' {
				counts.Lines++
			}
			space := c == ' ' || c >= '\t' && c <= '\r'
			if !space && !inWord {
				counts.Words++
			}
			inWord = !space
		}
		if status == EOF {
			return counts, OK
		}
	}
}

func AppendUint(target []byte, value uint64) []byte {
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
	for size != 0 {
		size--
		target = append(target, digits[size])
	}
	return target
}
