package tty

// ResetSequence is what the service writes when raw mode ends: default
// colours, a visible cursor, a clear screen and the cursor home. It is written
// however the program ended, so a crashed or stopped one cannot leave the
// terminal unusable.
const ResetSequence = "\x1b[0m\x1b[?25h\x1b[2J\x1b[H"

// RawInput holds what was typed in raw mode until a read takes it. It is a
// bounded ring that allocates nothing; a byte arriving when it is full is
// dropped, as the kernel's input ring drops one.
type RawInput struct {
	data    [MaxLine]byte
	head    int
	size    int
	dropped uint64
}

// Feed queues one typed byte.
func (input *RawInput) Feed(value byte) {
	if input.size == len(input.data) {
		input.dropped++
		return
	}
	input.data[(input.head+input.size)%len(input.data)] = value
	input.size++
}

// Len returns how many bytes are waiting.
func (input *RawInput) Len() int { return input.size }

// Dropped returns how many bytes arrived with the buffer full.
func (input *RawInput) Dropped() uint64 { return input.dropped }

// Take moves up to len(target) waiting bytes into target. When bytes remain
// after it, the part taken never ends in ESC: a reader treats an ESC that ends
// what it read as the Escape key, so the ESC waits to go with the rest of its
// sequence.
func (input *RawInput) Take(target []byte) int {
	count := input.size
	if count > len(target) {
		count = len(target)
	}
	for index := 0; index < count; index++ {
		target[index] = input.data[(input.head+index)%len(input.data)]
	}
	if count < input.size && count > 1 && target[count-1] == 0x1b {
		count--
	}
	input.head = (input.head + count) % len(input.data)
	input.size -= count
	return count
}

// Reset discards what is waiting.
func (input *RawInput) Reset() {
	clear(input.data[:])
	input.head, input.size = 0, 0
}
