package linux

import (
	"encoding/binary"

	"goos/internal/timekeeping"
	"goos/kernel/compat/linux/abi"
)

// select reads and writes the fd_sets of a call a window at a time, so its
// scratch stays the same size whatever the descriptor limit. A window covers
// FDSetWindow descriptors starting at a multiple of FDSetWindow.
const (
	FDSetWords  = 8
	FDSetWindow = FDSetWords * 64
	// FDSetSize is the bytes of one window of one fd_set.
	FDSetSize = FDSetWords * 8
)

// SelectArgumentsSize is the size of the sixth pselect6 argument: a pointer
// to the signal set and the size of that set.
const SelectArgumentsSize = 16

// select readiness: Linux puts a descriptor in the read set for the poll
// events of POLLIN_SET, in the write set for POLLOUT_SET, and in the
// exceptional set for POLLPRI.
const (
	SelectReadEvents   = PollIn | PollReadNormal | PollHangup | PollError
	SelectWriteEvents  = PollOut | PollWriteNormal | PollError
	SelectExceptEvents = PollPriority
)

// FDSet holds one window of an fd_set. Bit n names the descriptor at the
// window's base plus n.
type FDSet [FDSetWords]uint64

// Has reports whether bit is in the set.
func (set *FDSet) Has(bit int) bool {
	return bit >= 0 && bit < FDSetWindow && set[bit/64]&(1<<(bit%64)) != 0
}

// Add puts bit in the set.
func (set *FDSet) Add(bit int) {
	if bit >= 0 && bit < FDSetWindow {
		set[bit/64] |= 1 << (bit % 64)
	}
}

// SelectCount bounds the nfds argument of select as Linux does: a negative
// count fails with EINVAL, and a count above capacity, the descriptors the
// table has entries for, is cut to it. No descriptor past capacity can be
// open.
func SelectCount(argument uint64, capacity int) (int, abi.Errno) {
	count := int32(argument)
	if count < 0 {
		return 0, abi.EINVAL
	}
	if int(count) > capacity {
		return capacity, 0
	}
	return int(count), 0
}

// FDSetBytes is the number of bytes of each fd_set that select copies for
// count descriptors: whole 64-bit words, as Linux copies them.
func FDSetBytes(count int) int {
	return (count + 63) / 64 * 8
}

// DecodeFDSet reads the first count descriptors of one copied window into
// set. Bits at or above count are not part of the set.
func DecodeFDSet(wire []byte, count int, set *FDSet) {
	for word := 0; word < FDSetWords; word++ {
		set[word] = 0
		if word*8+8 > len(wire) {
			continue
		}
		left := count - word*64
		if left <= 0 {
			continue
		}
		set[word] = binary.LittleEndian.Uint64(wire[word*8:])
		if left < 64 {
			set[word] &= 1<<left - 1
		}
	}
}

// EncodeFDSet writes set over one copied window. Every byte select copied is
// written, so bits above the count come back clear, as on Linux.
func EncodeFDSet(wire []byte, set *FDSet) {
	for word := 0; word < FDSetWords && word*8+8 <= len(wire); word++ {
		binary.LittleEndian.PutUint64(wire[word*8:], set[word])
	}
}

// SelectEvents is the poll event mask select asks a descriptor for, given
// the sets that name it. It includes the exceptional events whatever the
// sets, as Linux does.
func SelectEvents(read, write bool) uint16 {
	events := SelectExceptEvents
	if read {
		events |= SelectReadEvents
	}
	if write {
		events |= SelectWriteEvents
	}
	return events
}

// SelectTimevalNanos reads the struct timeval timeout of select. Linux
// carries whole seconds out of tv_usec before it checks the value, so a
// tv_usec of a million or more adds whole seconds to the timeout. What is
// left must be a valid time: no negative seconds, and microseconds that are
// neither negative nor a whole second. The result saturates at the largest
// duration.
func SelectTimevalNanos(wire *[TimevalSize]byte) (uint64, abi.Errno) {
	const microsPerSecond = int64(timekeeping.NanosPerSecond) / 1000
	seconds := int64(binary.LittleEndian.Uint64(wire[0:]))
	micros := int64(binary.LittleEndian.Uint64(wire[8:]))
	seconds += micros / microsPerSecond
	nanos := micros % microsPerSecond * 1000
	if seconds < 0 || nanos < 0 {
		return 0, abi.EINVAL
	}
	if uint64(seconds) > (^uint64(0)-uint64(nanos))/uint64(timekeeping.NanosPerSecond) {
		return ^uint64(0), 0
	}
	return uint64(seconds)*uint64(timekeeping.NanosPerSecond) + uint64(nanos), 0
}

// EncodeRemainingTimeval writes the time a select has left as a struct
// timeval, rounding microseconds down as Linux does.
func EncodeRemainingTimeval(target *[TimevalSize]byte, nanos uint64) {
	binary.LittleEndian.PutUint64(target[0:], nanos/uint64(timekeeping.NanosPerSecond))
	binary.LittleEndian.PutUint64(target[8:], nanos%uint64(timekeeping.NanosPerSecond)/1000)
}

// EncodeRemainingTimespec writes the time a pselect6 or ppoll has left as a
// struct timespec.
func EncodeRemainingTimespec(target *[TimespecSize]byte, nanos uint64) {
	binary.LittleEndian.PutUint64(target[0:], nanos/uint64(timekeeping.NanosPerSecond))
	binary.LittleEndian.PutUint64(target[8:], nanos%uint64(timekeeping.NanosPerSecond))
}

// DecodeSelectArguments reads the sixth pselect6 argument: the address of
// the signal set and its size.
func DecodeSelectArguments(wire *[SelectArgumentsSize]byte) (uint64, uint64) {
	return binary.LittleEndian.Uint64(wire[0:]), binary.LittleEndian.Uint64(wire[8:])
}
