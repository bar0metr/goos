package linuxsock

import (
	"goos/kernel/ipc"
	"goos/userspace/linuxsockapi"
)

// Proto names the native session a request goes to.
type Proto uint8

const (
	TCP Proto = iota + 1
	UDP
)

// NativeOp is one operation of the native TCP or UDP protocol, in the form
// the service needs from GTCP version 2 and GUDP version 2 (ADR 0210).
// Every operation but WaitReady completes at once in netd.
type NativeOp uint8

const (
	// NativeOpen creates an endpoint; the completion's Endpoint names it.
	NativeOpen NativeOp = iota + 1
	// NativeBind binds Endpoint to Address and Port, either zero for the
	// wildcard or an ephemeral port. ReuseAddr carries SO_REUSEADDR. The
	// completion carries the bound address and port in Local, LocalPort.
	NativeBind
	// NativeListen makes a bound TCP endpoint a listener with Length as the
	// backlog.
	NativeListen
	// NativeAccept takes one established child: Endpoint in the completion,
	// its peer in Address and Port, its local end in Local and LocalPort.
	// NoWait answers WouldBlock when none is queued.
	NativeAccept
	// NativeConnect connects a TCP endpoint, or fixes a UDP endpoint's peer.
	// For TCP with NoWait the result is OK, InProgress or an error; both OK
	// and InProgress carry the local end the connection uses.
	NativeConnect
	// NativeRead reads up to Length bytes of a TCP stream, or one datagram
	// from a UDP endpoint. NoWait answers WouldBlock when nothing is queued,
	// Peek leaves the data. The completion's Data holds the bytes; for a
	// datagram, Full is its whole length and Address and Port its source.
	// A stream at its end answers EOF.
	NativeRead
	// NativeWrite sends Data on a connected endpoint. For TCP with NoWait the
	// completion's Length is what the stream took; WouldBlock means nothing
	// fit. A datagram goes whole or answers MessageSize.
	NativeWrite
	// NativeSendTo sends Data as one datagram to Address and Port.
	NativeSendTo
	// NativeShutdownWrite half-closes a TCP stream.
	NativeShutdownWrite
	// NativeClose closes Endpoint. A WaitReady held on it is answered.
	NativeClose
	// NativeReady answers the endpoint's mask and readable bytes now.
	NativeReady
	// NativeWaitReady is held by netd until the endpoint's mask differs from
	// Mask, then answers like NativeReady. One per endpoint.
	NativeWaitReady
	// NativeError returns and clears the endpoint's pending connection or
	// transport error in the completion's Value, as a NativeResult.
	NativeError
	// NativeOption reads the effective value of Option into Value.
	NativeOption
)

// NativeFlags qualify a request.
type NativeFlags uint8

const (
	NativeNoWait NativeFlags = 1 << iota
	NativePeek
	NativeReuseAddr
)

// NativeMask is an endpoint's readiness as netd reports it.
type NativeMask uint32

const (
	MaskAcceptable    NativeMask = 1 << iota // a listener has a child queued
	MaskConnected                            // a connect finished and the stream is established
	MaskConnectFailed                        // a connect failed; NativeError has the reason
	MaskReadable                             // bytes or a datagram are queued
	MaskReceiveEOF                           // the peer closed its write side
	MaskWritable                             // a write takes at least one byte
	MaskWriteClosed                          // the local write side is closed
	MaskPendingError                         // NativeError has an error to report
	MaskHungUp                               // the transport is gone both ways
)

// NativeOptionID names an option NativeOption reads.
type NativeOptionID uint8

const (
	OptionMSS NativeOptionID = iota + 1
	OptionReceiveBuffer
	OptionSendBuffer
	OptionTTL
)

// NativeResult is a native outcome.
type NativeResult uint8

const (
	NativeOK NativeResult = iota
	NativeWouldBlock
	NativeInProgress
	NativeInvalidArgument
	NativeBusy
	NativeDown
	NativeNoAddress
	NativeNoRoute
	NativeNeighborUnreachable
	NativeReconfigured
	NativeAddressInUse
	NativeRefused
	NativeTimeout
	NativeReset
	NativeDriverFault
	NativeClosed
	NativeEOF
	NativeMessageSize
	NativeNotSupported
	// NativeLost is never sent by netd: the core reports it for requests
	// whose native session died.
	NativeLost
)

// NativeRequest is one request to a native session. Data is valid only
// during Submit; the backend copies it into its own pool.
type NativeRequest struct {
	Op       NativeOp
	Flags    NativeFlags
	Option   NativeOptionID
	Endpoint uint32
	Address  [4]byte
	Port     uint16
	Length   uint32
	Mask     NativeMask
	Data     []byte
}

// NativeCompletion is the answer to one NativeRequest. Data is valid only
// during Complete; the backend releases its slot afterwards.
type NativeCompletion struct {
	Op        NativeOp
	Result    NativeResult
	Endpoint  uint32
	Address   [4]byte
	Port      uint16
	Local     [4]byte
	LocalPort uint16
	Length    uint32
	Full      uint32
	Mask      NativeMask
	Readable  uint32
	Value     uint32
	Data      []byte
}

// SubmitStatus is what a backend did with a request.
type SubmitStatus uint8

const (
	// SubmitAccepted: the request is on its way; Complete follows.
	SubmitAccepted SubmitStatus = iota
	// SubmitBusy: no native slot or queue room now. The core retries after
	// the next completion from that backend.
	SubmitBusy
	// SubmitDown: the session is gone. The backend reports BackendDown too.
	SubmitDown
	// SubmitUnsupported: the session's protocol version lacks the
	// operation. The core treats it as a NotSupported completion.
	SubmitUnsupported
)

// Backend carries native requests. tag is nonzero and comes back with the
// completion; a backend uses it as the wire correlation.
type Backend interface {
	Submit(proto Proto, tag uint64, request *NativeRequest) SubmitStatus
}

// Transport carries messages to the kernel side of family sessions.
type Transport interface {
	// Send queues message on family's channel and reports false when the
	// queue is full; the core keeps the message and calls Send again from
	// Flush.
	Send(family int, message *ipc.Message) bool
	// Drop ends family's session after the core released everything it
	// held for it: the transport closes the channel and unmaps the pool.
	Drop(family int)
}

// nativeResult maps a native outcome onto GLNXSOCK1.
func nativeResult(result NativeResult) linuxsockapi.Result {
	switch result {
	case NativeOK, NativeEOF:
		return linuxsockapi.ResultOK
	case NativeWouldBlock:
		return linuxsockapi.ResultWouldBlock
	case NativeInProgress:
		return linuxsockapi.ResultInProgress
	case NativeInvalidArgument:
		return linuxsockapi.ResultInvalidArgument
	case NativeBusy:
		return linuxsockapi.ResultNoBuffers
	case NativeDown, NativeDriverFault:
		return linuxsockapi.ResultDown
	case NativeNoAddress:
		return linuxsockapi.ResultNoAddress
	case NativeNoRoute:
		return linuxsockapi.ResultNoRoute
	case NativeNeighborUnreachable:
		return linuxsockapi.ResultHostUnreachable
	case NativeReconfigured:
		return linuxsockapi.ResultReconfigured
	case NativeAddressInUse:
		return linuxsockapi.ResultAddressInUse
	case NativeRefused:
		return linuxsockapi.ResultRefused
	case NativeTimeout:
		return linuxsockapi.ResultTimeout
	case NativeReset:
		return linuxsockapi.ResultReset
	case NativeClosed:
		return linuxsockapi.ResultClosed
	case NativeMessageSize:
		return linuxsockapi.ResultMessageSize
	case NativeNotSupported:
		return linuxsockapi.ResultNotSupported
	}
	return linuxsockapi.ResultServiceLost
}
