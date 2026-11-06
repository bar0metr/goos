package linux

import (
	"encoding/binary"

	"goos/kernel/compat/linux/abi"
)

// The wire forms of the socket calls: struct sockaddr_in, struct msghdr,
// and the checks Linux makes on their lengths before it reads them. Every
// helper works on bytes the kernel already copied, so a bad pointer is the
// caller's EFAULT and never reaches them.

const (
	// SockaddrInSize is struct sockaddr_in: family, port, address, and
	// eight bytes of padding.
	SockaddrInSize = 16
	// SockaddrStorageSize is the longest address Linux copies in.
	SockaddrStorageSize = 128
	// MsghdrSize is struct msghdr on x86_64.
	MsghdrSize = 56
	// CmsghdrSize is struct cmsghdr, the smallest control message.
	CmsghdrSize = 16
	// MsghdrNameLength, MsghdrControlLength and MsghdrFlags are the offsets
	// recvmsg writes back.
	MsghdrNameLength    = 8
	MsghdrControlLength = 40
	MsghdrFlags         = 48
)

// SocketAddress is an IPv4 address and port in host order.
type SocketAddress struct {
	Address [4]byte
	Port    uint16
}

// AddressUse names the call an address is for; Linux checks the family a
// little differently for each.
type AddressUse uint8

const (
	AddressBind AddressUse = iota + 1
	AddressConnect
	AddressSend
)

// SockaddrInputLength checks the length a program passes with an address and
// returns how many bytes to copy. Linux reads it as an int and refuses one
// that is negative or longer than a sockaddr_storage before it copies
// anything; it then copies the whole length, of which an IPv4 address uses
// the first SockaddrInSize bytes.
func SockaddrInputLength(length uint64) (int, abi.Errno) {
	value := int32(uint32(length))
	if value < 0 || value > SockaddrStorageSize {
		return 0, abi.EINVAL
	}
	return int(min(value, SockaddrInSize)), 0
}

// DecodeSockaddrIn reads an IPv4 address from the copied bytes, which are
// the caller's whole length or its first SockaddrInSize bytes. unspecified
// reports AF_UNSPEC, which connect takes as a request to dissolve the
// association rather than as an address.
func DecodeSockaddrIn(wire []byte, use AddressUse) (address SocketAddress, unspecified bool, errno abi.Errno) {
	if use == AddressConnect && len(wire) >= 2 && binary.LittleEndian.Uint16(wire) == abi.FamilyUnspec {
		return SocketAddress{}, true, 0
	}
	if len(wire) < SockaddrInSize {
		return SocketAddress{}, false, abi.EINVAL
	}
	family := binary.LittleEndian.Uint16(wire)
	address.Port = binary.BigEndian.Uint16(wire[2:])
	copy(address.Address[:], wire[4:8])
	switch {
	case family == abi.FamilyInet:
	case family == abi.FamilyUnspec && use == AddressBind && address.Address == [4]byte{}:
		// Linux binds an AF_UNSPEC INADDR_ANY as AF_INET, for old programs.
	case family == abi.FamilyUnspec && use == AddressSend:
		// sendto takes AF_UNSPEC as AF_INET.
	default:
		return SocketAddress{}, false, abi.EAFNOSUPPORT
	}
	return address, false, 0
}

// UnprivilegedPortStart is Linux's ip_unprivileged_port_start. A Linux
// program on GoOS runs as uid 1000 without CAP_NET_BIND_SERVICE.
const UnprivilegedPortStart = 1024

// BindPortErrno refuses a bind to a privileged port with EACCES, as Linux
// refuses it to an unprivileged process. Port 0 picks an ephemeral port.
func BindPortErrno(port uint16) abi.Errno {
	if port != 0 && port < UnprivilegedPortStart {
		return abi.EACCES
	}
	return 0
}

// EncodeSockaddrIn writes address as a complete struct sockaddr_in. Every
// byte of target is set.
func EncodeSockaddrIn(target *[SockaddrInSize]byte, address SocketAddress) {
	*target = [SockaddrInSize]byte{}
	binary.LittleEndian.PutUint16(target[0:], abi.FamilyInet)
	binary.BigEndian.PutUint16(target[2:], address.Port)
	copy(target[4:8], address.Address[:])
}

// SockaddrOutputLength is the check Linux makes on the length a program
// gives for an address the kernel returns: it reads it as an int, refuses a
// negative one, and copies at most that much of the actual address. The
// caller then reports actual, the full length, whatever it copied.
func SockaddrOutputLength(capacity uint32, actual int) (int, abi.Errno) {
	if int32(capacity) < 0 {
		return 0, abi.EINVAL
	}
	if int(capacity) < actual {
		return int(capacity), 0
	}
	return actual, 0
}

// MessageHeader is a decoded struct msghdr.
type MessageHeader struct {
	Name          uint64
	NameLength    uint32
	IOVec         uint64
	IOVecCount    uint64
	Control       uint64
	ControlLength uint64
	Flags         uint32
}

// DecodeMsghdr reads a struct msghdr and applies the checks Linux makes on
// it: a negative name length is EINVAL and a longer one than a
// sockaddr_storage is cut to it, a name length goes with no name, and a
// vector of more than MaxIOVec elements is EMSGSIZE. A send with a control
// length past INT_MAX is ENOBUFS.
func DecodeMsghdr(wire *[MsghdrSize]byte, send bool) (MessageHeader, abi.Errno) {
	header := MessageHeader{
		Name:          binary.LittleEndian.Uint64(wire[0:]),
		NameLength:    binary.LittleEndian.Uint32(wire[8:]),
		IOVec:         binary.LittleEndian.Uint64(wire[16:]),
		IOVecCount:    binary.LittleEndian.Uint64(wire[24:]),
		Control:       binary.LittleEndian.Uint64(wire[32:]),
		ControlLength: binary.LittleEndian.Uint64(wire[40:]),
		Flags:         binary.LittleEndian.Uint32(wire[48:]),
	}
	if int32(header.NameLength) < 0 {
		return MessageHeader{}, abi.EINVAL
	}
	if header.NameLength > SockaddrStorageSize {
		header.NameLength = SockaddrStorageSize
	}
	if header.Name == 0 {
		header.NameLength = 0
	}
	if header.IOVecCount > MaxIOVec {
		return MessageHeader{}, abi.EMSGSIZE
	}
	if send && header.ControlLength > 1<<31-1 {
		return MessageHeader{}, abi.ENOBUFS
	}
	return header, 0
}

// SendControlErrno answers the control part of a sendmsg. No control
// message has any meaning here: a buffer too short to hold one is ignored,
// as Linux ignores it, and anything longer asks for ancillary data GoOS
// does not carry.
func SendControlErrno(header MessageHeader) abi.Errno {
	if header.ControlLength < CmsghdrSize {
		return 0
	}
	return abi.EOPNOTSUPP
}

// SendFlags checks the flags of a send call. MSG_OOB and MSG_DONTROUTE have
// effects GoOS does not implement and fail; MSG_FASTOPEN too. The hints
// MSG_MORE, MSG_EOR and MSG_CONFIRM change nothing here and are accepted,
// with MSG_DONTWAIT and MSG_NOSIGNAL, which the kernel applies. Linux ignores
// the other bits, and so does this.
func SendFlags(flags uint64) abi.Errno {
	const fastOpen = uint64(0x20000000)
	if flags&(abi.MessageOOB|abi.MessageDontRoute|fastOpen) != 0 {
		return abi.EOPNOTSUPP
	}
	return 0
}

// ReceiveFlags checks the flags of a receive call. MSG_OOB fails; the error
// queue is always empty, so MSG_ERRQUEUE answers EAGAIN as Linux does for an
// empty queue. MSG_PEEK, MSG_TRUNC, MSG_WAITALL, MSG_DONTWAIT and
// MSG_CMSG_CLOEXEC are applied or harmless.
func ReceiveFlags(flags uint64) abi.Errno {
	const errorQueue = uint64(0x2000)
	switch {
	case flags&abi.MessageOOB != 0:
		return abi.EOPNOTSUPP
	case flags&errorQueue != 0:
		return abi.EAGAIN
	}
	return 0
}

// SocketType splits the type argument of socket and the flags of accept4.
type SocketType struct {
	Datagram    bool
	NonBlock    bool
	CloseOnExec bool
}

// DecodeSocket checks the arguments of socket(2) in the order Linux does:
// the type's flags and range, the family, then the type and the protocol.
// Only AF_INET with SOCK_STREAM over TCP and SOCK_DGRAM over UDP exist here;
// IPv6, local, netlink and packet sockets are EAFNOSUPPORT, raw and
// sequenced-packet sockets ESOCKTNOSUPPORT.
func DecodeSocket(family uint64, kind uint64, protocol uint64) (SocketType, abi.Errno) {
	const typeLimit = 11 // SOCK_MAX
	flags := kind &^ abi.SocketTypeMask
	if flags&^(abi.SocketNonBlock|abi.SocketCloseExec) != 0 || kind&abi.SocketTypeMask >= typeLimit {
		return SocketType{}, abi.EINVAL
	}
	if family != uint64(abi.FamilyInet) {
		return SocketType{}, abi.EAFNOSUPPORT
	}
	result := SocketType{NonBlock: flags&abi.SocketNonBlock != 0, CloseOnExec: flags&abi.SocketCloseExec != 0}
	switch kind & abi.SocketTypeMask {
	case abi.SocketStream:
		if protocol != abi.ProtocolIP && protocol != abi.ProtocolTCP {
			return SocketType{}, abi.EPROTONOSUPPORT
		}
	case abi.SocketDatagram:
		if protocol != abi.ProtocolIP && protocol != abi.ProtocolUDP {
			return SocketType{}, abi.EPROTONOSUPPORT
		}
		result.Datagram = true
	default:
		return SocketType{}, abi.ESOCKTNOSUPPORT
	}
	return result, 0
}

// AcceptFlags checks the flags of accept4: SOCK_NONBLOCK and SOCK_CLOEXEC,
// nothing else.
func AcceptFlags(flags uint64) (SocketType, abi.Errno) {
	if flags&^(abi.SocketNonBlock|abi.SocketCloseExec) != 0 {
		return SocketType{}, abi.EINVAL
	}
	return SocketType{NonBlock: flags&abi.SocketNonBlock != 0, CloseOnExec: flags&abi.SocketCloseExec != 0}, 0
}
