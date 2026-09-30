package linux

import (
	"encoding/binary"

	"goos/internal/timekeeping"
	"goos/kernel/compat/linux/abi"
)

const (
	// ParentPID is what getppid reports. PID 1 launches every Linux process
	// and no Linux process tree exists, so every parent is init, as it is
	// for an orphan on Linux.
	ParentPID = 1
	// TimespecSize is the size of struct timespec: seconds and nanoseconds,
	// eight bytes each.
	TimespecSize = 16
	// UnameSize is the size of struct utsname: six fields of 65 bytes.
	UnameSize  = 6 * unameField
	unameField = 65
	// RlimitSize is the size of struct rlimit64: the soft and the hard limit.
	RlimitSize = 16
)

// ClockOf maps a Linux clock to the GoOS clock that keeps the same time.
// GoOS has one realtime and one monotonic clock; the raw, coarse, and
// boot-time variants read those. Process and thread CPU-time clocks have no
// source and are EINVAL.
func ClockOf(id uint64) (timekeeping.Clock, abi.Errno) {
	switch id {
	case abi.ClockRealtime, abi.ClockRealtimeCoarse:
		return timekeeping.Realtime, 0
	case abi.ClockMonotonic, abi.ClockMonotonicRaw, abi.ClockMonotonicCoarse, abi.ClockBoottime:
		return timekeeping.Monotonic, 0
	}
	return 0, abi.EINVAL
}

// EncodeTimespec writes a struct timespec.
func EncodeTimespec(target *[TimespecSize]byte, value timekeeping.Time) {
	binary.LittleEndian.PutUint64(target[0:], uint64(value.Seconds))
	binary.LittleEndian.PutUint64(target[8:], uint64(value.Nanoseconds))
}

// SleepTicks converts a requested sleep to timer ticks. The result rounds up
// and adds one tick, because the tick in progress is already partly spent and
// a sleep must never end early. A sleep longer than maximum ticks is cut to
// it: the caller asked for a time no timer deadline can hold.
func SleepTicks(wire *[TimespecSize]byte, ticksPerSecond uint64, maximum uint64) (uint64, abi.Errno) {
	seconds := int64(binary.LittleEndian.Uint64(wire[0:]))
	nanoseconds := int64(binary.LittleEndian.Uint64(wire[8:]))
	if seconds < 0 || nanoseconds < 0 || nanoseconds >= int64(timekeeping.NanosPerSecond) || ticksPerSecond == 0 {
		return 0, abi.EINVAL
	}
	if seconds == 0 && nanoseconds == 0 {
		return 0, 0
	}
	if uint64(seconds) > maximum/ticksPerSecond {
		return maximum, 0
	}
	tickNanos := uint64(timekeeping.NanosPerSecond) / ticksPerSecond
	ticks := uint64(seconds)*ticksPerSecond + (uint64(nanoseconds)+tickNanos-1)/tickNanos + 1
	if ticks > maximum {
		ticks = maximum
	}
	return ticks, 0
}

// SleepNanos is the duration a validated timespec asks for, saturated at the
// largest value.
func SleepNanos(wire *[TimespecSize]byte) uint64 {
	seconds := binary.LittleEndian.Uint64(wire[0:])
	nanoseconds := binary.LittleEndian.Uint64(wire[8:])
	if seconds > (^uint64(0)-nanoseconds)/uint64(timekeeping.NanosPerSecond) {
		return ^uint64(0)
	}
	return seconds*uint64(timekeeping.NanosPerSecond) + nanoseconds
}

// RemainingTicks is the number of whole ticks that covers what is left of a
// sleep, at least one and at most maximum.
func RemainingTicks(nanos uint64, ticksPerSecond uint64, maximum uint64) uint64 {
	if ticksPerSecond == 0 {
		return 1
	}
	tickNanos := uint64(timekeeping.NanosPerSecond) / ticksPerSecond
	ticks := nanos / tickNanos
	if nanos%tickNanos != 0 || ticks == 0 {
		ticks++
	}
	if ticks > maximum {
		ticks = maximum
	}
	return ticks
}

// EncodeUname writes a struct utsname. The system name is Linux because that
// is the ABI a program built for Linux runs against here; the node, release,
// and version say what implements it. release is the GoOS release.
func EncodeUname(target *[UnameSize]byte, release string) {
	*target = [UnameSize]byte{}
	unameSet(target, 0, "Linux", "", "")
	unameSet(target, 1, "goos", "", "")
	unameSet(target, 2, "6.0.0-goos", "", "")
	unameSet(target, 3, "GoOS ", release, " Linux ABI")
	unameSet(target, 4, "x86_64", "", "")
	unameSet(target, 5, "(none)", "", "")
}

// unameSet fills one field from up to three parts and leaves its last byte
// zero. It takes fixed parameters because a slice of parts would be built on
// the kernel heap.
func unameSet(target *[UnameSize]byte, index int, first string, second string, third string) {
	field := target[index*unameField : (index+1)*unameField-1]
	written := copy(field, first)
	written += copy(field[written:], second)
	copy(field[written:], third)
}

// ResourceLimit reports the limits GoOS applies to a Linux process, as soft
// and hard values that are always equal. Descriptors, the stack, and the
// brk heap have fixed bounds; no core file is ever written. The other
// resources have no limit expressed in Linux terms and report none.
func ResourceLimit(resource uint64, stackBytes uint64) (uint64, abi.Errno) {
	switch resource {
	case abi.ResourceOpenFiles:
		return LinuxMaxFD, 0
	case abi.ResourceStack:
		return stackBytes, 0
	case abi.ResourceData:
		return BrkReserve, 0
	case abi.ResourceCore:
		return 0, 0
	}
	if resource >= abi.ResourceCount {
		return 0, abi.EINVAL
	}
	return abi.ResourceInfinity, 0
}

// MaxRandomBytes bounds what one getrandom call returns. Linux promises
// that a request of up to 256 bytes is filled whole; a longer one may come
// back short, and here it does.
const MaxRandomBytes = 256

// RandomFlags checks the flags of getrandom. GoOS has one source, so the
// pool and blocking choices change nothing. GRND_INSECURE with GRND_RANDOM
// is the one combination Linux refuses among its defined flags.
func RandomFlags(flags uint64) abi.Errno {
	if flags&^(abi.RandomNonBlock|abi.RandomRandom|abi.RandomInsecure) != 0 ||
		flags&(abi.RandomRandom|abi.RandomInsecure) == abi.RandomRandom|abi.RandomInsecure {
		return abi.EINVAL
	}
	return 0
}
