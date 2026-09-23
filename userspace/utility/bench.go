package utility

import "math/bits"

// MaxBenchOps bounds one bench run. The shell stops waiting for a command after
// about 30 seconds, so large counts at a low timer rate outlive the prompt.
const MaxBenchOps = 10000

func parseUint(value []byte) (uint64, bool) {
	if len(value) == 0 {
		return 0, false
	}
	var result uint64
	for _, digit := range value {
		if digit < '0' || digit > '9' || result > (^uint64(0)-uint64(digit-'0'))/10 {
			return 0, false
		}
		result = result*10 + uint64(digit-'0')
	}
	return result, true
}

// BenchCount parses an operation count between 1 and MaxBenchOps.
func BenchCount(value []byte) (uint64, bool) {
	count, ok := parseUint(value)
	return count, ok && count != 0 && count <= MaxBenchOps
}

// AppendBenchResult formats one result line without a trailing newline:
// "bench <name>: <ops> ops, total <us> us, avg <ns> ns/op".
func AppendBenchResult(target []byte, name string, ops, nanos uint64) []byte {
	average := uint64(0)
	if ops != 0 {
		average = nanos / ops
	}
	target = append(target, "bench "...)
	target = append(target, name...)
	target = append(target, ": "...)
	target = appendUint(target, ops)
	target = append(target, " ops, total "...)
	target = appendUint(target, nanos/1000)
	target = append(target, " us, avg "...)
	target = appendUint(target, average)
	return append(target, " ns/op"...)
}

func appendUint(target []byte, value uint64) []byte {
	var digits [20]byte
	position := len(digits)
	for {
		position--
		digits[position] = byte(value%10) + '0'
		value /= 10
		if value == 0 {
			return append(target, digits[position:]...)
		}
	}
}

// MaxBenchChunk bounds the bytes one stream bench operation moves; it is the
// most one native stream call accepts.
const MaxBenchChunk = 256

// BenchChunk parses a per-operation byte count between 1 and MaxBenchChunk.
func BenchChunk(value []byte) (int, bool) {
	size, ok := parseUint(value)
	return int(size), ok && size != 0 && size <= MaxBenchChunk
}

// AppendBenchRate appends ", <size> B/op, <rate> KiB/s" to a result line for
// ops operations of size bytes each that took nanos.
func AppendBenchRate(target []byte, ops, size, nanos uint64) []byte {
	rate := uint64(0)
	if nanos != 0 {
		// A 128-bit product keeps large files from overflowing; a quotient
		// that would not fit saturates.
		high, low := bits.Mul64(ops*size, 1_000_000_000)
		if high >= nanos {
			rate = ^uint64(0)
		} else {
			rate, _ = bits.Div64(high, low, nanos)
			rate /= 1024
		}
	}
	target = append(target, ", "...)
	target = appendUint(target, size)
	target = append(target, " B/op, "...)
	target = appendUint(target, rate)
	return append(target, " KiB/s"...)
}

// MaxBenchSamples bounds the per-operation intervals one run keeps.
const MaxBenchSamples = 512

// Median sorts values in place and returns the middle one, or the lower
// middle one for an even count; it returns 0 for none.
func Median(values []uint64) uint64 {
	if len(values) == 0 {
		return 0
	}
	for index := 1; index < len(values); index++ {
		value, position := values[index], index
		for ; position > 0 && values[position-1] > value; position-- {
			values[position] = values[position-1]
		}
		values[position] = value
	}
	return values[(len(values)-1)/2]
}

// AppendBenchCount appends ", <value> <label>" to a result line.
func AppendBenchCount(target []byte, value uint64, label string) []byte {
	target = append(target, ", "...)
	target = appendUint(target, value)
	target = append(target, ' ')
	return append(target, label...)
}
