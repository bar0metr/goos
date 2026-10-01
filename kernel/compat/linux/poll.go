package linux

import (
	"encoding/binary"

	"goos/internal/timekeeping"
)

// poll events.
const (
	PollIn          = uint16(0x001)
	PollPriority    = uint16(0x002)
	PollOut         = uint16(0x004)
	PollError       = uint16(0x008)
	PollHangup      = uint16(0x010)
	PollInvalid     = uint16(0x020)
	PollReadNormal  = uint16(0x040)
	PollWriteNormal = uint16(0x100)
)

// PollFDSize is the size of struct pollfd.
const PollFDSize = 8

// LinuxMaxPollFD bounds the descriptors one poll call may name, the size of
// a descriptor table.
const LinuxMaxPollFD = LinuxMaxFD

// PollReadiness is what one descriptor can do now.
type PollReadiness struct {
	Readable bool
	Writable bool
	Hangup   bool
	Error    bool
}

// PollRevents reports the events poll returns for a descriptor that asked for
// requested. POLLERR and POLLHUP are reported whether asked for or not.
func PollRevents(requested uint16, ready PollReadiness) uint16 {
	result := uint16(0)
	if ready.Readable {
		result |= PollIn | PollReadNormal
	}
	if ready.Writable {
		result |= PollOut | PollWriteNormal
	}
	result &= requested
	if ready.Hangup {
		result |= PollHangup
	}
	if ready.Error {
		result |= PollError
	}
	return result
}

// PollEntry reads one struct pollfd from a copied array.
func PollEntry(wire []byte, index int) (int32, uint16) {
	entry := wire[index*PollFDSize:]
	return int32(binary.LittleEndian.Uint32(entry[0:4])), binary.LittleEndian.Uint16(entry[4:6])
}

// SetPollRevents writes revents of one struct pollfd in a copied array.
func SetPollRevents(wire []byte, index int, revents uint16) {
	binary.LittleEndian.PutUint16(wire[index*PollFDSize+6:], revents)
}

// PollTimeoutNanos converts a poll timeout in milliseconds. A negative
// timeout waits without limit, as on Linux.
func PollTimeoutNanos(milliseconds int32) (uint64, bool) {
	if milliseconds < 0 {
		return 0, false
	}
	return uint64(milliseconds) * uint64(timekeeping.NanosPerSecond) / 1000, true
}
