package linux

import (
	"goos/kernel/compat/linux/abi"
	"goos/userspace/linuxsockapi"
)

// SocketCall is the Linux call a GLNXSOCK1 result answers. Some results
// mean different things to different calls, as they do on Linux.
type SocketCall uint8

const (
	SocketCallOpen SocketCall = iota + 1
	SocketCallBind
	SocketCallListen
	SocketCallAccept
	SocketCallConnect
	SocketCallRead
	// SocketCallWrite is a send on a stream socket, SocketCallSend one on a
	// datagram socket.
	SocketCallWrite
	SocketCallSend
	SocketCallShutdown
	SocketCallName
	SocketCallOption
)

// SocketErrno is the one translation of a GLNXSOCK1 result into a Linux
// errno. Every result has an entry; none falls back to EIO.
//
//	OK                  0
//	WouldBlock          EAGAIN
//	InProgress          EINPROGRESS for connect, EALREADY otherwise
//	Already             EALREADY
//	IsConnected         EISCONN
//	NotConnected        EPIPE for a stream send, EDESTADDRREQ for a datagram send, ENOTCONN otherwise
//	InvalidArgument     EINVAL
//	NotSupported        EOPNOTSUPP
//	NoOption            ENOPROTOOPT
//	FamilyLimit         EMFILE for socket and accept, ENOBUFS otherwise
//	ServiceLimit        ENFILE for socket and accept, ENOBUFS otherwise
//	NoBuffers           ENOBUFS
//	Down                ENETDOWN
//	NoAddress           EADDRNOTAVAIL
//	NoRoute             ENETUNREACH
//	HostUnreachable     EHOSTUNREACH
//	Reconfigured        ENETRESET
//	AddressInUse        EADDRINUSE
//	Refused             ECONNREFUSED
//	Timeout             ETIMEDOUT
//	Reset               ECONNABORTED for accept, ECONNRESET otherwise
//	Closed              EPIPE for a send, EINVAL for listen and accept, ENOTCONN otherwise
//	MessageSize         EMSGSIZE
//	DestinationRequired EDESTADDRREQ
//	ServiceLost         ENETDOWN for socket, ENETRESET otherwise
//	Stale               ENETRESET
func SocketErrno(result linuxsockapi.Result, call SocketCall) abi.Errno {
	switch result {
	case linuxsockapi.ResultOK:
		return 0
	case linuxsockapi.ResultWouldBlock:
		return abi.EAGAIN
	case linuxsockapi.ResultInProgress:
		if call == SocketCallConnect {
			return abi.EINPROGRESS
		}
		return abi.EALREADY
	case linuxsockapi.ResultAlready:
		return abi.EALREADY
	case linuxsockapi.ResultIsConnected:
		return abi.EISCONN
	case linuxsockapi.ResultNotConnected:
		switch call {
		case SocketCallWrite:
			return abi.EPIPE
		case SocketCallSend:
			return abi.EDESTADDRREQ
		}
		return abi.ENOTCONN
	case linuxsockapi.ResultInvalidArgument:
		return abi.EINVAL
	case linuxsockapi.ResultNotSupported:
		return abi.EOPNOTSUPP
	case linuxsockapi.ResultNoOption:
		return abi.ENOPROTOOPT
	case linuxsockapi.ResultFamilyLimit:
		if call == SocketCallOpen || call == SocketCallAccept {
			return abi.EMFILE
		}
		return abi.ENOBUFS
	case linuxsockapi.ResultServiceLimit:
		if call == SocketCallOpen || call == SocketCallAccept {
			return abi.ENFILE
		}
		return abi.ENOBUFS
	case linuxsockapi.ResultNoBuffers:
		return abi.ENOBUFS
	case linuxsockapi.ResultDown:
		return abi.ENETDOWN
	case linuxsockapi.ResultNoAddress:
		return abi.EADDRNOTAVAIL
	case linuxsockapi.ResultNoRoute:
		return abi.ENETUNREACH
	case linuxsockapi.ResultHostUnreachable:
		return abi.EHOSTUNREACH
	case linuxsockapi.ResultReconfigured:
		return abi.ENETRESET
	case linuxsockapi.ResultAddressInUse:
		return abi.EADDRINUSE
	case linuxsockapi.ResultRefused:
		return abi.ECONNREFUSED
	case linuxsockapi.ResultTimeout:
		return abi.ETIMEDOUT
	case linuxsockapi.ResultReset:
		if call == SocketCallAccept {
			return abi.ECONNABORTED
		}
		return abi.ECONNRESET
	case linuxsockapi.ResultClosed:
		switch call {
		case SocketCallWrite, SocketCallSend:
			return abi.EPIPE
		case SocketCallListen, SocketCallAccept:
			return abi.EINVAL
		}
		return abi.ENOTCONN
	case linuxsockapi.ResultMessageSize:
		return abi.EMSGSIZE
	case linuxsockapi.ResultDestinationRequired:
		return abi.EDESTADDRREQ
	case linuxsockapi.ResultServiceLost:
		if call == SocketCallOpen {
			return abi.ENETDOWN
		}
		return abi.ENETRESET
	case linuxsockapi.ResultStale:
		return abi.ENETRESET
	}
	// A result outside the protocol never decodes; a session that sent one
	// has already failed.
	return abi.ENETRESET
}
