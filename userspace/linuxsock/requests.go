package linuxsock

import "goos/userspace/linuxsockapi"

// Linux shutdown directions.
const (
	shutRead  = 0
	shutWrite = 1
	shutBoth  = 2
)

// step moves an operation forward: it answers from local state, or builds
// its next native request in s.native and submits it, or leaves it waiting
// for the pump.
func (s *Service) step(o *op) {
	switch o.kind {
	case kindDrain:
		s.stepDrain(o)
		return
	case kindError:
		s.stepError(o)
		return
	}
	r := &o.request
	if r.Opcode == linuxsockapi.OpOpen {
		s.stepOpen(o)
		return
	}
	sock := s.socketOf(o)
	if sock == nil {
		s.finish(o, linuxsockapi.ResultStale)
		return
	}
	switch r.Opcode {
	case linuxsockapi.OpBind:
		s.stepBind(o, sock)
	case linuxsockapi.OpListen:
		s.stepListen(o, sock)
	case linuxsockapi.OpAccept:
		s.stepAccept(o, sock)
	case linuxsockapi.OpConnect:
		s.stepConnect(o, sock)
	case linuxsockapi.OpRead:
		s.stepRead(o, sock)
	case linuxsockapi.OpWrite:
		s.stepWrite(o, sock, false)
	case linuxsockapi.OpSendTo:
		s.stepWrite(o, sock, true)
	case linuxsockapi.OpShutdown:
		s.stepShutdown(o, sock)
	case linuxsockapi.OpGetSockName:
		o.reply.Local, o.reply.LocalPort = sock.local, sock.localPort
		s.finish(o, linuxsockapi.ResultOK)
	case linuxsockapi.OpGetPeerName:
		if sock.state != stateConnected || sock.dead && sock.stream {
			s.finish(o, linuxsockapi.ResultNotConnected)
			return
		}
		o.reply.Address, o.reply.Port = sock.peer, sock.peerPort
		s.finish(o, linuxsockapi.ResultOK)
	case linuxsockapi.OpGetOption, linuxsockapi.OpSetOption:
		s.stepOption(o, sock)
	case linuxsockapi.OpGetPendingError:
		s.stepPendingError(o, sock)
	case linuxsockapi.OpQueryReady:
		if sock.dead || sock.endpoint == 0 {
			s.answerReady(o, sock)
			return
		}
		s.native = NativeRequest{Op: NativeReady, Endpoint: sock.endpoint}
		s.submit(o, sock.proto)
	case linuxsockapi.OpClose:
		s.counters.Closes++
		s.releaseSocket(int(o.socket), true)
		s.finish(o, linuxsockapi.ResultOK)
	default:
		s.finish(o, linuxsockapi.ResultInvalidArgument)
	}
}

// done handles the completion of an operation's native request.
func (s *Service) done(o *op, c *NativeCompletion) {
	if c.Result != NativeOK && c.Result != NativeWouldBlock && c.Result != NativeInProgress && c.Result != NativeEOF {
		s.counters.NativeFailures++
	}
	// An endpoint a completion created belongs to no one unless a handler
	// takes it.
	created := c.Result == NativeOK && (c.Op == NativeOpen || c.Op == NativeAccept) && c.Endpoint != 0
	if o.orphan {
		if created {
			s.queueClose(o.proto, o.generation, c.Endpoint)
		}
		s.freeOp(o)
		return
	}
	switch o.kind {
	case kindDrain:
		s.freeOp(o)
		return
	case kindError:
		s.doneError(o, c)
		return
	}
	r := &o.request
	sock := s.socketOf(o)
	if r.Opcode == linuxsockapi.OpOpen {
		s.doneOpen(o, c)
		return
	}
	if r.Opcode == linuxsockapi.OpAccept {
		s.doneAccept(o, c)
		return
	}
	if sock == nil {
		if created {
			s.queueClose(o.proto, o.generation, c.Endpoint)
		}
		s.finish(o, linuxsockapi.ResultClosed)
		return
	}
	if c.Result == NativeLost {
		s.finish(o, linuxsockapi.ResultServiceLost)
		return
	}
	switch c.Op {
	case NativeOpen: // a connect replacing a spent endpoint
		if c.Result != NativeOK {
			s.finish(o, nativeResult(c.Result))
			return
		}
		sock.endpoint, sock.generation, sock.state, sock.reopen = c.Endpoint, o.generation, stateUnbound, false
		sock.mask, sock.readable, sock.pending = 0, 0, linuxsockapi.ResultOK
		sock.waiting, sock.noWait = false, false
		s.arm(int(o.socket))
		o.stage++
		s.wait(o)
		s.step(o)
	case NativeBind:
		s.doneBind(o, sock, c)
	case NativeListen:
		if c.Result == NativeOK {
			sock.state = stateListening
		}
		s.finish(o, nativeResult(c.Result))
	case NativeConnect:
		s.doneConnect(o, sock, c)
	case NativeRead:
		s.doneRead(o, sock, c)
	case NativeWrite, NativeSendTo:
		s.doneWrite(o, sock, c)
	case NativeShutdownWrite:
		if c.Result == NativeOK {
			sock.writeShut = true
			s.publish(int(o.socket))
		}
		s.finish(o, nativeResult(c.Result))
	case NativeOption:
		s.doneOption(o, sock, c)
	case NativeError:
		value := sock.pending
		if c.Result == NativeOK && NativeResult(c.Value) != NativeOK {
			value = nativeResult(NativeResult(c.Value))
		}
		sock.pending = linuxsockapi.ResultOK
		sock.mask &^= MaskPendingError
		o.reply.Value = uint64(value)
		s.publish(int(o.socket))
		s.finish(o, linuxsockapi.ResultOK)
	case NativeReady:
		if c.Result == NativeOK {
			s.observe(int(o.socket), c.Mask, c.Readable)
		}
		s.answerReady(o, sock)
	default:
		s.finish(o, nativeResult(c.Result))
	}
}

func (s *Service) stepOpen(o *op) {
	r := &o.request
	stream := r.Length == linuxsockapi.KindStream
	proto := UDP
	if stream {
		proto = TCP
	}
	if o.socket < 0 {
		if !s.backends[proto].up {
			s.finish(o, linuxsockapi.ResultDown)
			return
		}
		if s.families[o.family].count >= linuxsockapi.MaxSockets {
			s.finish(o, linuxsockapi.ResultFamilyLimit)
			return
		}
		global := s.allocSocket(int(o.family), stream)
		if global < 0 {
			s.finish(o, linuxsockapi.ResultServiceLimit)
			return
		}
		o.socket, o.socketSerial = int16(global), s.sockets[global].serial
	}
	s.native = NativeRequest{Op: NativeOpen}
	s.submit(o, proto)
}

func (s *Service) doneOpen(o *op, c *NativeCompletion) {
	sock := s.socketOf(o)
	if c.Result != NativeOK || sock == nil {
		if sock != nil {
			s.releaseSocket(int(o.socket), false)
		}
		result := nativeResult(c.Result)
		switch c.Result {
		case NativeBusy:
			result = linuxsockapi.ResultServiceLimit
		case NativeLost:
			result = linuxsockapi.ResultDown
		}
		s.finish(o, result)
		return
	}
	s.counters.Opens++
	s.adopt(int(o.socket), c.Endpoint, o.generation)
	o.reply.Socket = sock.id
	s.finish(o, linuxsockapi.ResultOK)
}

// usable answers a request on a socket whose native side is gone.
func (s *Service) usable(o *op, sock *socket) bool {
	if sock.dead {
		sock.pending = linuxsockapi.ResultOK
		s.publish(int(o.socket))
		s.finish(o, linuxsockapi.ResultServiceLost)
		return false
	}
	return true
}

func (s *Service) stepBind(o *op, sock *socket) {
	if !s.usable(o, sock) {
		return
	}
	if sock.state != stateUnbound || sock.reopen {
		s.finish(o, linuxsockapi.ResultInvalidArgument)
		return
	}
	r := &o.request
	s.native = NativeRequest{Op: NativeBind, Endpoint: sock.endpoint, Address: r.Address, Port: r.Port}
	if r.Flags&linuxsockapi.FlagReuseAddr != 0 || sock.reuse {
		s.native.Flags = NativeReuseAddr
	}
	s.submit(o, sock.proto)
}

// bindAny binds to an ephemeral port on the wildcard, as Linux does for a
// listen or a send on an unbound socket.
func (s *Service) bindAny(o *op, sock *socket) {
	s.native = NativeRequest{Op: NativeBind, Endpoint: sock.endpoint}
	if sock.reuse {
		s.native.Flags = NativeReuseAddr
	}
	s.submit(o, sock.proto)
}

func (s *Service) doneBind(o *op, sock *socket, c *NativeCompletion) {
	if c.Result != NativeOK {
		s.finish(o, nativeResult(c.Result))
		return
	}
	sock.state, sock.local, sock.localPort = stateBound, c.Local, c.LocalPort
	if o.request.Opcode == linuxsockapi.OpBind {
		o.reply.Local, o.reply.LocalPort = c.Local, c.LocalPort
		s.finish(o, linuxsockapi.ResultOK)
		return
	}
	// A listen or a send that bound first goes on.
	o.stage++
	s.wait(o)
	s.step(o)
}

func clampBacklog(backlog uint32) uint32 {
	if backlog == 0 {
		return 1
	}
	return min(backlog, MaxBacklog)
}

// stepListen binds an unbound stream first. A listener's backlog changes
// only in the service: netd keeps the one it was given.
func (s *Service) stepListen(o *op, sock *socket) {
	if !sock.stream {
		s.finish(o, linuxsockapi.ResultNotSupported)
		return
	}
	if !s.usable(o, sock) {
		return
	}
	backlog := clampBacklog(o.request.Length)
	switch {
	case sock.state == stateListening:
		sock.backlog = backlog
		s.finish(o, linuxsockapi.ResultOK)
	case sock.state == stateUnbound && !sock.reopen:
		s.bindAny(o, sock)
	case sock.state == stateBound:
		sock.backlog = backlog
		s.native = NativeRequest{Op: NativeListen, Endpoint: sock.endpoint, Length: backlog}
		s.submit(o, sock.proto)
	default:
		s.finish(o, linuxsockapi.ResultInvalidArgument)
	}
}

// stepAccept reserves the child's socket before asking netd, so a full
// table leaves the child queued in netd rather than taken and lost.
func (s *Service) stepAccept(o *op, sock *socket) {
	if !sock.stream {
		s.finish(o, linuxsockapi.ResultNotSupported)
		return
	}
	if !s.usable(o, sock) {
		return
	}
	if sock.state != stateListening || sock.readShut {
		s.finish(o, linuxsockapi.ResultInvalidArgument)
		return
	}
	if o.child < 0 {
		if s.families[o.family].count >= linuxsockapi.MaxSockets {
			s.finish(o, linuxsockapi.ResultFamilyLimit)
			return
		}
		child := s.allocSocket(int(o.family), true)
		if child < 0 {
			s.finish(o, linuxsockapi.ResultServiceLimit)
			return
		}
		o.child, o.childSerial = int16(child), s.sockets[child].serial
	}
	s.native = NativeRequest{Op: NativeAccept, Flags: NativeNoWait, Endpoint: sock.endpoint}
	s.submit(o, TCP)
}

func (s *Service) doneAccept(o *op, c *NativeCompletion) {
	child := &s.sockets[o.child]
	if c.Result != NativeOK || !child.used || child.serial != o.childSerial {
		if c.Result == NativeOK && c.Endpoint != 0 {
			s.queueClose(o.proto, o.generation, c.Endpoint)
		}
		result := nativeResult(c.Result)
		if c.Result == NativeOK {
			result = linuxsockapi.ResultClosed
		}
		s.finish(o, result)
		return
	}
	child.state, child.peer, child.peerPort = stateConnected, c.Address, c.Port
	child.local, child.localPort = c.Local, c.LocalPort
	s.adopt(int(o.child), c.Endpoint, o.generation)
	s.counters.Accepts++
	o.reply.Value, o.reply.Address, o.reply.Port = child.id, c.Address, c.Port
	o.reply.Local, o.reply.LocalPort = c.Local, c.LocalPort
	o.child = -1
	s.finish(o, linuxsockapi.ResultOK)
}

// stepConnect follows Linux's state rules for a stream: a connect while
// one is in progress is EALREADY, one on a connected stream or a listener
// EISCONN. A stream whose connect failed connects again on a new native
// endpoint, as Linux's socket returns to its closed state; its earlier bind
// does not carry over.
func (s *Service) stepConnect(o *op, sock *socket) {
	if !s.usable(o, sock) {
		return
	}
	r := &o.request
	if !sock.stream {
		s.native = NativeRequest{Op: NativeConnect, Endpoint: sock.endpoint, Address: r.Address, Port: r.Port}
		s.submit(o, UDP)
		return
	}
	switch sock.state {
	case stateConnecting:
		s.finish(o, linuxsockapi.ResultAlready)
		return
	case stateConnected, stateListening:
		s.finish(o, linuxsockapi.ResultIsConnected)
		return
	}
	if o.stage == 0 && (sock.state == stateFailed || sock.reopen) {
		s.queueClose(TCP, sock.generation, sock.endpoint)
		sock.endpoint = 0
		if sock.needWait {
			sock.needWait = false
			s.waits--
		}
		s.native = NativeRequest{Op: NativeOpen}
		s.submit(o, TCP)
		return
	}
	s.native = NativeRequest{Op: NativeConnect, Flags: NativeNoWait, Endpoint: sock.endpoint, Address: r.Address, Port: r.Port}
	s.submit(o, TCP)
}

func (s *Service) doneConnect(o *op, sock *socket, c *NativeCompletion) {
	r := &o.request
	switch c.Result {
	case NativeOK, NativeInProgress:
		sock.peer, sock.peerPort = r.Address, r.Port
		if c.Local != ([4]byte{}) || c.LocalPort != 0 {
			sock.local, sock.localPort = c.Local, c.LocalPort
		}
		s.counters.Connects++
		if c.Result == NativeInProgress {
			sock.state = stateConnecting
			s.publish(int(o.socket))
			s.finish(o, linuxsockapi.ResultInProgress)
			return
		}
		sock.state = stateConnected
		s.publish(int(o.socket))
		o.reply.Address, o.reply.Port = r.Address, r.Port
		o.reply.Local, o.reply.LocalPort = sock.local, sock.localPort
		s.finish(o, linuxsockapi.ResultOK)
	default:
		if sock.stream {
			sock.reopen = true
		}
		s.finish(o, nativeResult(c.Result))
	}
}

// stepRead serves a stream's retained bytes first and keeps one lent slot
// per stream, so bytes leave in order and a Release always knows its
// socket.
func (s *Service) stepRead(o *op, sock *socket) {
	if !s.usable(o, sock) {
		return
	}
	r := &o.request
	f := &s.families[o.family]
	if sock.stream {
		switch sock.state {
		case stateConnecting:
			s.finish(o, linuxsockapi.ResultWouldBlock)
			return
		case stateConnected:
		default:
			if sock.pending != linuxsockapi.ResultOK {
				s.finishPending(o, sock)
				return
			}
			s.finish(o, linuxsockapi.ResultNotConnected)
			return
		}
	} else if sock.state == stateUnbound && !sock.readShut {
		// Nothing can arrive at a datagram socket without a port.
		if sock.pending != linuxsockapi.ResultOK {
			s.finishPending(o, sock)
			return
		}
		s.finish(o, linuxsockapi.ResultWouldBlock)
		return
	}
	if sock.readShut {
		o.reply.Flags = linuxsockapi.FlagEOF
		s.finish(o, linuxsockapi.ResultOK)
		return
	}
	if sock.stream && sock.lent >= 0 {
		if !s.park(o) {
			s.finish(o, linuxsockapi.ResultWouldBlock)
			return
		}
		s.wait(o)
		return
	}
	if sock.stream && sock.retained >= 0 {
		s.unpark(o)
		s.readRetained(o, sock)
		return
	}
	if o.rx < 0 {
		if o.rx = s.reserveRX(f); o.rx < 0 {
			if !s.park(o) {
				s.finish(o, linuxsockapi.ResultWouldBlock)
				return
			}
			s.wait(o)
			return
		}
	}
	s.unpark(o)
	s.native = NativeRequest{Op: NativeRead, Flags: NativeNoWait, Endpoint: sock.endpoint, Length: r.Length}
	if r.Flags&linuxsockapi.FlagPeek != 0 {
		s.native.Flags |= NativePeek
	}
	s.submit(o, sock.proto)
}

// readRetained lends the slot holding a stream's unread bytes. A short read
// leaves the rest past the reply's length, still in the slot.
func (s *Service) readRetained(o *op, sock *socket) {
	f := &s.families[o.family]
	slot := uint8(sock.retained)
	length := min(uint32(sock.retainedLength), o.request.Length)
	sock.lentLength, sock.tail = uint16(length), sock.retainedLength-uint16(length)
	sock.lentPeek = o.request.Flags&linuxsockapi.FlagPeek != 0
	sock.lentRetained = true
	sock.lent, sock.retained, sock.retainedLength = int8(slot), -1, 0
	s.lend(f, slot, int(o.socket))
	if !sock.lentPeek {
		s.counters.BytesIn += uint64(length)
	}
	o.reply.Slot, o.reply.Length = slot, length
	s.publish(int(o.socket))
	s.finish(o, linuxsockapi.ResultOK)
}

func (s *Service) finishPending(o *op, sock *socket) {
	result := sock.pending
	sock.pending = linuxsockapi.ResultOK
	s.publish(int(o.socket))
	s.finish(o, result)
}

func (s *Service) doneRead(o *op, sock *socket, c *NativeCompletion) {
	f := &s.families[o.family]
	r := &o.request
	peek := r.Flags&linuxsockapi.FlagPeek != 0
	switch c.Result {
	case NativeOK:
	case NativeEOF:
		sock.mask |= MaskReceiveEOF
		o.reply.Flags = linuxsockapi.FlagEOF
		s.publish(int(o.socket))
		s.finish(o, linuxsockapi.ResultOK)
		return
	case NativeWouldBlock:
		if sock.pending != linuxsockapi.ResultOK {
			s.finishPending(o, sock)
			return
		}
		s.finish(o, linuxsockapi.ResultWouldBlock)
		return
	default:
		if sock.pending != linuxsockapi.ResultOK {
			s.finishPending(o, sock)
			return
		}
		s.finish(o, nativeResult(c.Result))
		return
	}
	length := min(uint32(len(c.Data)), r.Length, linuxsockapi.SlotSize)
	if length != 0 {
		slot := uint8(o.rx)
		copy(s.rxSlot(f, slot), c.Data[:length])
		s.lend(f, slot, int(o.socket))
		o.rx = -1
		o.reply.Slot, o.reply.Length = slot, length
	}
	if sock.stream {
		if length != 0 {
			sock.lent, sock.lentLength, sock.tail = int8(o.reply.Slot), uint16(length), 0
			sock.lentPeek, sock.lentRetained = peek, false
		}
		if !peek {
			s.counters.BytesIn += uint64(length)
			sock.readable -= min(sock.readable, length)
		}
		s.publish(int(o.socket))
		s.finish(o, linuxsockapi.ResultOK)
		return
	}
	full := c.Full
	if full < uint32(len(c.Data)) {
		full = uint32(len(c.Data))
	}
	if full > r.Length {
		o.reply.Flags = linuxsockapi.FlagTruncated
	}
	o.reply.Value, o.reply.Address, o.reply.Port = uint64(full), c.Address, c.Port
	if !peek {
		s.counters.DatagramsIn++
	}
	s.finish(o, linuxsockapi.ResultOK)
}

// stepWrite sends from the TX slot the kernel lent. A stream that is not
// connected answers as Linux's does: EPIPE (Closed) unless a connect is
// under way. A datagram sent to an address on a connected socket must name
// its peer: netd keeps a connected endpoint to its peer.
func (s *Service) stepWrite(o *op, sock *socket, addressed bool) {
	if !s.usable(o, sock) {
		return
	}
	r := &o.request
	f := &s.families[o.family]
	if sock.stream {
		switch {
		case sock.pending != linuxsockapi.ResultOK && sock.state != stateConnecting:
			s.finishPending(o, sock)
			return
		case sock.writeShut:
			s.finish(o, linuxsockapi.ResultClosed)
			return
		case sock.state == stateConnecting:
			s.finish(o, linuxsockapi.ResultWouldBlock)
			return
		case sock.state != stateConnected:
			s.finish(o, linuxsockapi.ResultClosed)
			return
		case r.Length == 0:
			s.finish(o, linuxsockapi.ResultOK)
			return
		}
		s.native = NativeRequest{Op: NativeWrite, Flags: NativeNoWait, Endpoint: sock.endpoint, Data: s.txSlot(f, r.Slot, r.Length)}
		s.submit(o, TCP)
		return
	}
	if sock.writeShut {
		s.finish(o, linuxsockapi.ResultClosed)
		return
	}
	if !addressed {
		if sock.state != stateConnected {
			s.finish(o, linuxsockapi.ResultDestinationRequired)
			return
		}
		s.native = NativeRequest{Op: NativeWrite, Endpoint: sock.endpoint, Data: s.txSlot(f, r.Slot, r.Length)}
		s.submit(o, UDP)
		return
	}
	switch {
	case sock.state == stateConnected && (r.Address != sock.peer || r.Port != sock.peerPort):
		s.finish(o, linuxsockapi.ResultIsConnected)
	case sock.state == stateConnected:
		s.native = NativeRequest{Op: NativeWrite, Endpoint: sock.endpoint, Data: s.txSlot(f, r.Slot, r.Length)}
		s.submit(o, UDP)
	case sock.state == stateUnbound:
		s.bindAny(o, sock)
	default:
		s.native = NativeRequest{Op: NativeSendTo, Endpoint: sock.endpoint, Address: r.Address, Port: r.Port,
			Data: s.txSlot(f, r.Slot, r.Length)}
		s.submit(o, UDP)
	}
}

func (s *Service) doneWrite(o *op, sock *socket, c *NativeCompletion) {
	r := &o.request
	if c.Result != NativeOK {
		s.finish(o, nativeResult(c.Result))
		return
	}
	if !sock.stream {
		s.counters.DatagramsOut++
		o.reply.Length = r.Length
		s.finish(o, linuxsockapi.ResultOK)
		return
	}
	taken := min(c.Length, r.Length)
	if taken == 0 && r.Length != 0 {
		s.finish(o, linuxsockapi.ResultWouldBlock)
		return
	}
	s.counters.BytesOut += uint64(taken)
	o.reply.Length = taken
	s.finish(o, linuxsockapi.ResultOK)
}

// stepShutdown: SHUT_RD drops unread bytes and makes later reads end of
// file, draining what netd still receives; SHUT_WR half-closes in netd. A
// listener takes SHUT_RD as the end of its accepting.
func (s *Service) stepShutdown(o *op, sock *socket) {
	how := o.request.Length
	read, write := how == shutRead || how == shutBoth, how == shutWrite || how == shutBoth
	if sock.dead || sock.state != stateConnected && !(sock.stream && sock.state == stateListening) {
		s.finish(o, linuxsockapi.ResultNotConnected)
		return
	}
	if read && !sock.readShut {
		sock.readShut = true
		if sock.retained >= 0 {
			s.freeRX(&s.families[o.family], uint8(sock.retained))
			sock.retained, sock.retainedLength = -1, 0
		}
		s.drain(int(o.socket))
		s.publish(int(o.socket))
	}
	if !write || sock.writeShut || sock.state == stateListening {
		s.finish(o, linuxsockapi.ResultOK)
		return
	}
	if !sock.stream {
		sock.writeShut = true
		s.publish(int(o.socket))
		s.finish(o, linuxsockapi.ResultOK)
		return
	}
	s.native = NativeRequest{Op: NativeShutdownWrite, Endpoint: sock.endpoint}
	s.submit(o, TCP)
}

// stepPendingError returns and clears SO_ERROR: the socket's own error
// first, then whatever netd holds.
func (s *Service) stepPendingError(o *op, sock *socket) {
	if sock.pending != linuxsockapi.ResultOK || !sock.stream || sock.dead || sock.endpoint == 0 ||
		sock.mask&MaskPendingError == 0 && !sock.fetching {
		o.reply.Value = uint64(sock.pending)
		sock.pending = linuxsockapi.ResultOK
		s.publish(int(o.socket))
		s.finish(o, linuxsockapi.ResultOK)
		return
	}
	s.native = NativeRequest{Op: NativeError, Endpoint: sock.endpoint}
	s.submit(o, TCP)
}

func (s *Service) answerReady(o *op, sock *socket) {
	state := s.readiness(sock)
	o.reply.Value, o.reply.Extra = uint64(state.Ready), uint64(state.Readable)
	s.finish(o, linuxsockapi.ResultOK)
}

// --- internal operations -------------------------------------------------------

// drain discards what netd receives for a stream shut for reading.
func (s *Service) drain(global int) {
	sock := &s.sockets[global]
	if sock.draining || sock.dead || sock.endpoint == 0 || !sock.readShut || sock.mask&MaskReadable == 0 {
		return
	}
	o := s.newOp(int(sock.family), kindDrain)
	if o == nil {
		return
	}
	o.socket, o.socketSerial = int16(global), sock.serial
	sock.draining = true
	s.step(o)
}

func (s *Service) stepDrain(o *op) {
	sock := s.socketOf(o)
	if sock == nil || sock.dead || sock.endpoint == 0 {
		s.freeOp(o)
		return
	}
	s.native = NativeRequest{Op: NativeRead, Flags: NativeNoWait, Endpoint: sock.endpoint, Length: linuxsockapi.SlotSize}
	s.submit(o, sock.proto)
}

// fetchError reads netd's pending error into the socket.
func (s *Service) fetchError(global int) {
	sock := &s.sockets[global]
	if sock.fetching || sock.dead || sock.endpoint == 0 || !sock.stream {
		return
	}
	o := s.newOp(int(sock.family), kindError)
	if o == nil {
		return
	}
	o.socket, o.socketSerial = int16(global), sock.serial
	sock.fetching = true
	s.step(o)
}

func (s *Service) stepError(o *op) {
	sock := s.socketOf(o)
	if sock == nil || sock.dead || sock.endpoint == 0 {
		s.freeOp(o)
		return
	}
	s.native = NativeRequest{Op: NativeError, Endpoint: sock.endpoint}
	s.submit(o, TCP)
}

func (s *Service) doneError(o *op, c *NativeCompletion) {
	sock := s.socketOf(o)
	global := int(o.socket)
	s.freeOp(o)
	if sock == nil {
		return
	}
	if c.Result == NativeOK && NativeResult(c.Value) != NativeOK {
		sock.pending = nativeResult(NativeResult(c.Value))
	}
	sock.mask &^= MaskPendingError
	s.publish(global)
}
