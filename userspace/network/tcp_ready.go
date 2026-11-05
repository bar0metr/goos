package network

// TCPReady is an endpoint's readiness mask. Each bit is a fact a caller can
// test without consuming anything; the caller derives poll events from them.
type TCPReady uint16

const (
	// TCPReadyAcceptable: a listener has an established child to accept.
	TCPReadyAcceptable TCPReady = 1 << iota
	// TCPReadyConnected: the three-way handshake completed. It stays set
	// after the connection ends.
	TCPReadyConnected
	// TCPReadyConnectFailed: an active open ended without a connection; the
	// reason is the pending error.
	TCPReadyConnectFailed
	// TCPReadyReadable: received bytes are buffered.
	TCPReadyReadable
	// TCPReadyReadEOF: the peer's FIN arrived and every byte before it is
	// buffered. Reads return the buffered bytes and then EOF.
	TCPReadyReadEOF
	// TCPReadyWritable: the send buffer takes at least one byte now.
	TCPReadyWritable
	// TCPReadyWriteClosed: no byte can be written again: the write side was
	// shut down, or the connection ended or failed.
	TCPReadyWriteClosed
	// TCPReadyError: a pending error waits for TCPTakeError.
	TCPReadyError
	// TCPReadyHangUp: the connection ended, or both directions are closed.
	TCPReadyHangUp
	// TCPReadyConnecting: an active open is in progress.
	TCPReadyConnecting
	// TCPReadyListening: the endpoint is a listener.
	TCPReadyListening

	TCPReadyAll = TCPReadyListening<<1 - 1
)

// TCPReadiness is the mask and the byte counts behind it: Readable is the
// buffered byte count a read returns now, Writable the send buffer room.
type TCPReadiness struct {
	Mask     TCPReady
	Readable uint32
	Writable uint32
}

// TCPReadiness reports owner's endpoint's readiness. It changes nothing.
func (stack *Stack) TCPReadiness(owner int, id TCPEndpointID) (TCPReadiness, Result) {
	entry, _ := stack.tcpEndpoint(owner, id)
	if entry == nil {
		return TCPReadiness{}, ResultInvalidArgument
	}
	var ready TCPReadiness
	switch {
	case entry.state == TCPListen:
		ready.Mask |= TCPReadyListening
		if stack.tcpAcceptable(id) {
			ready.Mask |= TCPReadyAcceptable
		}
	case entry.state == TCPSynSent:
		ready.Mask |= TCPReadyConnecting
	case entry.state == TCPClosed && !entry.synchronized:
		ready.Mask |= TCPReadyConnectFailed | TCPReadyWriteClosed | TCPReadyHangUp
	case entry.synchronized:
		ready.Mask |= TCPReadyConnected
		writable := !entry.shutdownWrite && (entry.state == TCPEstablished || entry.state == TCPCloseWait)
		if writable {
			if room := stack.tcpSendRoom(entry); room > 0 {
				ready.Mask |= TCPReadyWritable
				ready.Writable = uint32(room)
			}
		} else {
			ready.Mask |= TCPReadyWriteClosed
		}
		if entry.finReceived {
			ready.Mask |= TCPReadyReadEOF
		}
		if entry.state == TCPClosed || (entry.finReceived && !writable) {
			ready.Mask |= TCPReadyHangUp
		}
	}
	if entry.recvLen > 0 {
		ready.Mask |= TCPReadyReadable
		ready.Readable = uint32(entry.recvLen)
	}
	if entry.pendingError != ResultOK {
		ready.Mask |= TCPReadyError
	}
	return ready, ResultOK
}

// TCPTakeError returns the pending error and clears it. OK means none.
func (stack *Stack) TCPTakeError(owner int, id TCPEndpointID) (Result, Result) {
	entry, _ := stack.tcpEndpoint(owner, id)
	if entry == nil {
		return ResultOK, ResultInvalidArgument
	}
	pending := entry.pendingError
	entry.pendingError = ResultOK
	return pending, ResultOK
}

// TCPPeek is TCPRead that leaves the bytes buffered and sends nothing.
func (stack *Stack) TCPPeek(owner int, id TCPEndpointID, dst []byte) (int, Result) {
	entry, _ := stack.tcpEndpoint(owner, id)
	if entry == nil || len(dst) == 0 {
		return 0, ResultInvalidArgument
	}
	if entry.recvLen == 0 {
		if entry.finReceived {
			return 0, ResultEOF
		}
		if entry.lastResult != ResultOK {
			return 0, entry.lastResult
		}
		return 0, ResultBusy
	}
	return ringLoad(dst, entry.receive[:], int(entry.recvHead), min(len(dst), int(entry.recvLen))), ResultOK
}

// TCPOptions are the effective values behind the socket options a caller
// may read.
type TCPOptions struct {
	// SendMSS is the sender maximum segment size of a connection, or the
	// default MSS before a connection exists.
	SendMSS       uint32
	SendBuffer    uint32
	ReceiveBuffer uint32
	TTL           uint32
}

func (stack *Stack) TCPOptions(owner int, id TCPEndpointID) (TCPOptions, Result) {
	entry, _ := stack.tcpEndpoint(owner, id)
	if entry == nil {
		return TCPOptions{}, ResultInvalidArgument
	}
	options := TCPOptions{SendMSS: tcpDefaultMSS, TTL: uint32(stack.tuning.TTL),
		SendBuffer:    uint32(min(int(stack.tuning.TCP.SendBuffer), TCPSendCapacity)),
		ReceiveBuffer: uint32(min(int(stack.tuning.TCP.ReceiveBuffer), TCPReceiveCapacity))}
	if entry.state >= TCPSynSent {
		options.SendMSS = tcpSMSS(entry)
	}
	return options, ResultOK
}

func (stack *Stack) tcpSendRoom(entry *tcpEndpoint) int {
	limit := min(int(stack.tuning.TCP.SendBuffer), TCPSendCapacity)
	return max(limit-int(entry.sendLen), 0)
}

func (stack *Stack) tcpAcceptable(listener TCPEndpointID) bool {
	for index := stack.nextTCP(0); index >= 0; index = stack.nextTCP(index + 1) {
		child := &stack.tcp[index]
		if child.listener == listener && child.state >= TCPEstablished && child.state < TCPClosed && !child.accepted {
			return true
		}
	}
	return false
}
