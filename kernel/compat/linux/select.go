package linux

import (
	"encoding/binary"

	"goos/internal/timekeeping"
	"goos/kernel/compat/linux/abi"
)

// FDSetWords is the number of 64-bit words of an fd_set that can name a
// descriptor of the table. select reads and writes no more of a set.
const FDSetWords = (LinuxMaxFD + 63) / 64

// FDSetSize is the largest part of one fd_set that select copies.
const FDSetSize = FDSetWords * 8

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

// FDSet holds the descriptors one fd_set names, bounded by the table.
type FDSet [FDSetWords]uint64

// Has reports whether fd is in the set.
func (set FDSet) Has(fd int) bool {
	return fd >= 0 && fd < LinuxMaxFD && set[fd/64]&(1<<(fd%64)) != 0
}

// Add puts fd in the set.
func (set *FDSet) Add(fd int) {
	if fd >= 0 && fd < LinuxMaxFD {
		set[fd/64] |= 1 << (fd % 64)
	}
}

// SelectCount bounds the nfds argument of select as Linux does: a negative
// count fails with EINVAL, and a count above the descriptor table is cut to
// the table, whose descriptors are all a set can name.
func SelectCount(argument uint64) (int, abi.Errno) {
	count := int32(argument)
	if count < 0 {
		return 0, abi.EINVAL
	}
	if count > LinuxMaxFD {
		count = LinuxMaxFD
	}
	return int(count), 0
}

// FDSetBytes is the number of bytes of each fd_set that select copies for
// count descriptors: whole 64-bit words, as Linux copies them.
func FDSetBytes(count int) int {
	return (count + 63) / 64 * 8
}

// DecodeFDSet reads the first count descriptors of a copied fd_set. Bits at
// or above count are not part of the set.
func DecodeFDSet(wire []byte, count int) FDSet {
	var set FDSet
	for word := 0; word < FDSetWords && word*8+8 <= len(wire); word++ {
		set[word] = binary.LittleEndian.Uint64(wire[word*8:])
		if left := count - word*64; left <= 0 {
			set[word] = 0
		} else if left < 64 {
			set[word] &= 1<<left - 1
		}
	}
	return set
}

// EncodeFDSet writes set over a copied fd_set. Every byte select copied is
// written, so bits above the count come back clear, as on Linux.
func EncodeFDSet(wire []byte, set *FDSet) {
	for word := 0; word < FDSetWords && word*8+8 <= len(wire); word++ {
		binary.LittleEndian.PutUint64(wire[word*8:], set[word])
	}
}

// SelectEvents is the poll event mask select asks a descriptor for, given
// the sets that name it. The exceptional events are always asked for, as on
// Linux.
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
// tv_usec of a million or more is a longer timeout, not an error. What is
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
