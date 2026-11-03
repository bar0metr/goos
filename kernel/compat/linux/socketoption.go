package linux

import (
	"encoding/binary"

	"goos/internal/timekeeping"
	"goos/kernel/compat/linux/abi"
	"goos/userspace/linuxsockapi"
)

// OptionPlace says who answers a socket option.
type OptionPlace uint8

const (
	// OptionKernel is answered from the socket table: SO_TYPE, SO_DOMAIN,
	// SO_PROTOCOL, SO_ACCEPTCONN, SO_REUSEADDR (sent as the Bind flag), the
	// two timeouts, and the options GoOS only has off.
	OptionKernel OptionPlace = iota + 1
	// OptionService goes to linuxsockd as GetOption or SetOption with the
	// Linux level and name: the buffer sizes, TCP_NODELAY, TCP_MAXSEG and
	// IP_TTL.
	OptionService
	// OptionPendingError is SO_ERROR, read and cleared with
	// GetPendingError.
	OptionPendingError
)

// ClassifyOption finds who answers an option, or the error Linux gives for
// one that does not exist: EOPNOTSUPP for an unknown level, ENOPROTOOPT for
// an unknown name. Options with behavior GoOS lacks, SO_REUSEPORT,
// SO_BINDTODEVICE, multicast and the rest, are ENOPROTOOPT rather than
// accepted and ignored. SO_KEEPALIVE, SO_LINGER, SO_BROADCAST, SO_OOBINLINE,
// SO_DEBUG and SO_DONTROUTE read as off and accept only off.
func ClassifyOption(level uint64, name uint64, datagram bool, set bool) (OptionPlace, abi.Errno) {
	switch level {
	case abi.LevelSocket:
		switch name {
		case abi.OptionType, abi.OptionDomain, abi.OptionProtocol, abi.OptionAcceptConn:
			if set {
				return 0, abi.ENOPROTOOPT
			}
			return OptionKernel, 0
		case abi.OptionError:
			if set {
				return 0, abi.ENOPROTOOPT
			}
			return OptionPendingError, 0
		case abi.OptionReuseAddr, abi.OptionRecvTimeout, abi.OptionSendTimeout,
			abi.OptionKeepAlive, abi.OptionLinger, abi.OptionBroadcast, abi.OptionOOBInline, abi.OptionDebug, abi.OptionDontRoute:
			return OptionKernel, 0
		case abi.OptionSendBuffer, abi.OptionRecvBuffer:
			return OptionService, 0
		}
	case abi.LevelTCP:
		if !datagram && (name == abi.OptionTCPNoDelay || name == abi.OptionTCPMaxSeg) {
			return OptionService, 0
		}
	case abi.LevelIP:
		if name == abi.OptionIPTTL {
			return OptionService, 0
		}
	case abi.LevelUDP:
	default:
		// Linux 6.12 answers a level no protocol of the socket knows with
		// EOPNOTSUPP, a name its level does not know with ENOPROTOOPT.
		return 0, abi.EOPNOTSUPP
	}
	return 0, abi.ENOPROTOOPT
}

// OffOption reports an option GoOS only has off.
func OffOption(name uint64) bool {
	switch name {
	case abi.OptionKeepAlive, abi.OptionLinger, abi.OptionBroadcast, abi.OptionOOBInline, abi.OptionDebug, abi.OptionDontRoute:
		return true
	}
	return false
}

// OptionInputLength checks the length setsockopt passes: an int on Linux,
// negative is EINVAL. It returns how many bytes to copy, at most 16.
func OptionInputLength(length uint64) (int, abi.Errno) {
	value := int32(uint32(length))
	if value < 0 {
		return 0, abi.EINVAL
	}
	return int(min(value, 16)), 0
}

// DecodeOptionInt reads an int option. Linux refuses a shorter value.
func DecodeOptionInt(wire []byte) (int32, abi.Errno) {
	if len(wire) < 4 {
		return 0, abi.EINVAL
	}
	return int32(binary.LittleEndian.Uint32(wire)), 0
}

// DecodeLingerOff reads a struct linger and reports whether it asks for
// lingering.
func DecodeLingerOff(wire []byte) (bool, abi.Errno) {
	if len(wire) < 8 {
		return false, abi.EINVAL
	}
	return binary.LittleEndian.Uint32(wire) == 0, 0
}

// ImmediateTimeout is the stored timeout of a negative timeval, which Linux
// keeps as a zero wait: the call times out at once.
const ImmediateTimeout = uint64(1)

// DecodeTimeout reads SO_RCVTIMEO or SO_SNDTIMEO as Linux does: a timeval
// that is short is EINVAL, microseconds out of range EDOM, zero means no
// limit and a negative time an immediate one. The result is nanoseconds.
func DecodeTimeout(wire []byte) (uint64, abi.Errno) {
	if len(wire) < TimevalSize {
		return 0, abi.EINVAL
	}
	seconds := int64(binary.LittleEndian.Uint64(wire[0:]))
	micros := int64(binary.LittleEndian.Uint64(wire[8:]))
	if micros < 0 || micros >= 1000000 {
		return 0, abi.EDOM
	}
	if seconds < 0 {
		return ImmediateTimeout, 0
	}
	const limit = uint64(1) << 62
	if uint64(seconds) > limit/uint64(timekeeping.NanosPerSecond) {
		return limit, 0
	}
	return uint64(seconds)*uint64(timekeeping.NanosPerSecond) + uint64(micros)*1000, 0
}

// EncodeTimeout writes a stored timeout back as a struct timeval.
func EncodeTimeout(target *[TimevalSize]byte, nanos uint64) {
	if nanos == ImmediateTimeout {
		nanos = 0
	}
	binary.LittleEndian.PutUint64(target[0:], nanos/uint64(timekeeping.NanosPerSecond))
	binary.LittleEndian.PutUint64(target[8:], nanos%uint64(timekeeping.NanosPerSecond)/1000)
}

// OptionOutputLength is the check getsockopt makes on the caller's length:
// an int, negative is EINVAL, and the value is cut to it. The caller writes
// back the length it copied, as Linux does.
func OptionOutputLength(capacity uint32, actual int) (int, abi.Errno) {
	if int32(capacity) < 0 {
		return 0, abi.EINVAL
	}
	return min(int(capacity), actual), 0
}

// PollReadHangup is POLLRDHUP.
const PollReadHangup = uint16(0x2000)

// SocketPollRevents turns a socket's readiness into the events poll
// returns for one that asked for requested. A socket whose session failed,
// or whose entry the service no longer publishes, is in error and hung up,
// and readable and writable too, since a read or write fails at once.
// POLLERR and POLLHUP are reported whether asked for or not; a peer's
// half-close makes the socket readable, as on Linux.
func SocketPollRevents(requested uint16, ready linuxsockapi.Ready, live bool, shutRead bool) uint16 {
	if !live {
		ready = linuxsockapi.ReadyIn | linuxsockapi.ReadyOut | linuxsockapi.ReadyError | linuxsockapi.ReadyHangup
	}
	if shutRead {
		ready |= linuxsockapi.ReadyIn | linuxsockapi.ReadyReadHangup
	}
	result := uint16(0)
	if ready&(linuxsockapi.ReadyIn|linuxsockapi.ReadyReadHangup) != 0 {
		result |= PollIn | PollReadNormal
	}
	if ready&linuxsockapi.ReadyOut != 0 {
		result |= PollOut | PollWriteNormal
	}
	if ready&linuxsockapi.ReadyReadHangup != 0 {
		result |= PollReadHangup
	}
	result &= requested
	if ready&linuxsockapi.ReadyError != 0 {
		result |= PollError
	}
	if ready&linuxsockapi.ReadyHangup != 0 {
		result |= PollHangup
	}
	return result
}
