package utility

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

// benchCount parses an operation count between 1 and MaxBenchOps.
func benchCount(value []byte) (uint64, bool) {
	count, ok := parseUint(value)
	return count, ok && count != 0 && count <= MaxBenchOps
}

// appendBenchResult formats one result line without a trailing newline:
// "bench <name>: <ops> ops, total <us> us, avg <ns> ns/op".
func appendBenchResult(target []byte, name string, ops, nanos uint64) []byte {
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
