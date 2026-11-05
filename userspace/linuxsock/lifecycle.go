package linuxsock

import "goos/userspace/linuxsockapi"

// Lifecycle fast paths. A socket() takes a native endpoint opened ahead
// from a small pool, so it answers without a round trip to netd; the pool
// counts against the service's native quota and refills in the background.
// A listener with a child queued in netd accepts it ahead into a socket
// held for the listener, so the Linux accept() answers at once; a child
// accepted ahead reads nothing until the kernel took it, since the kernel
// knows its identity only from the Accept reply. Both stay bounded: the
// pool by SetWarm, the children by AcceptAhead per listener and by the
// listener's backlog.

const (
	// MaxWarm bounds the endpoints opened ahead per protocol.
	MaxWarm = 8
	// AcceptAhead bounds the children accepted ahead per listener.
	AcceptAhead = 2
	// warmOps and acceptOps bound their operations in flight.
	warmOps   = 2 * MaxWarm
	acceptOps = MaxSockets
)

type warmPool struct {
	endpoints [MaxWarm]uint32
	count     int
	opening   int
	target    int
	// full: netd refused an endpoint for the quota; the pool waits for a
	// close before it opens more.
	full bool
}

// SetWarm sets how many endpoints of each protocol the core keeps opened
// ahead, at most MaxWarm.
func (s *Service) SetWarm(target int) {
	target = max(0, min(target, MaxWarm))
	for proto := TCP; proto <= UDP; proto++ {
		s.warm[proto].target = target
		s.refill(proto)
	}
}

// refill opens endpoints until the pool reaches its target.
func (s *Service) refill(proto Proto) {
	w := &s.warm[proto]
	for w.count+w.opening < w.target && !w.full && s.backends[proto].up {
		o := s.newOp(-1, kindWarm)
		if o == nil {
			return
		}
		s.native = NativeRequest{Op: NativeOpen}
		if !s.submitData(o, proto) {
			return
		}
		w.opening++
	}
}

// takeWarm hands out an endpoint opened ahead, or reports none.
func (s *Service) takeWarm(proto Proto) (uint32, bool) {
	w := &s.warm[proto]
	if w.count == 0 {
		return 0, false
	}
	w.count--
	endpoint := w.endpoints[w.count]
	w.endpoints[w.count] = 0
	s.refill(proto)
	return endpoint, true
}

// dropWarm forgets the pool of a native session that went away; its
// endpoints went with it.
func (s *Service) dropWarm(proto Proto) {
	w := &s.warm[proto]
	w.count, w.full = 0, false
	clear(w.endpoints[:])
}

func (s *Service) doneWarm(o *op, c *NativeCompletion) {
	proto, generation := o.proto, o.generation
	s.freeOp(o)
	w := &s.warm[proto]
	w.opening--
	switch {
	case c.Result == NativeOK && c.Endpoint != 0 && s.backends[proto].up && s.backends[proto].generation == generation && w.count < w.target:
		w.endpoints[w.count] = c.Endpoint
		w.count++
		s.refill(proto)
	case c.Result == NativeOK && c.Endpoint != 0:
		s.queueClose(proto, generation, c.Endpoint)
	case c.Result == NativeBusy:
		w.full = true
	}
}

// acceptAhead takes children netd has queued for a listener into sockets
// held for it.
func (s *Service) acceptAhead(global int) {
	sock := &s.sockets[global]
	f := &s.families[sock.family]
	if !sock.stream || sock.state != stateListening || sock.readShut || sock.dead || sock.closing || sock.endpoint == 0 ||
		sock.mask&MaskAcceptable == 0 {
		return
	}
	limit := min(AcceptAhead, int(sock.backlog))
	for int(sock.acceptLen)+int(sock.accepting) < limit && f.count < linuxsockapi.MaxSockets-1 {
		child := s.allocSocket(int(sock.family), true)
		if child < 0 {
			return
		}
		o := s.newOp(int(sock.family), kindAccept)
		if o == nil {
			s.releaseSocket(child, false)
			return
		}
		o.socket, o.socketSerial = int16(global), sock.serial
		o.child, o.childSerial = int16(child), s.sockets[child].serial
		s.native = NativeRequest{Op: NativeAccept, Flags: NativeNoWait, Endpoint: sock.endpoint}
		if !s.submitData(o, TCP) {
			s.want(sock, false)
			return
		}
		sock.accepting++
		f.stats.AcceptsAhead++
	}
}

func (s *Service) doneAcceptAhead(o *op, c *NativeCompletion) {
	listener := s.socketOf(o)
	global, childIndex, childSerial, generation := int(o.socket), int(o.child), o.childSerial, o.generation
	// The child stays with the operation's cleanup unless the queue takes
	// it below.
	child := &s.sockets[childIndex]
	ok := c.Result == NativeOK && c.Endpoint != 0
	if listener != nil {
		listener.accepting--
	}
	if !ok || listener == nil || !child.used || child.serial != childSerial || int(listener.acceptLen) >= len(listener.acceptq) {
		if ok {
			s.queueClose(TCP, generation, c.Endpoint)
		}
		s.freeOp(o)
		if listener != nil {
			if c.Result == NativeWouldBlock {
				listener.mask &^= MaskAcceptable
				s.refreshReadable(global)
			}
			s.publish(global)
		}
		return
	}
	o.child = -1
	s.freeOp(o)
	child.endpoint, child.generation = c.Endpoint, generation
	child.state, child.peer, child.peerPort = stateConnected, c.Address, c.Port
	child.local, child.localPort = c.Local, c.LocalPort
	listener.acceptq[listener.acceptLen] = int16(childIndex + 1)
	listener.acceptSerial[listener.acceptLen] = childSerial
	listener.acceptLen++
	s.publish(global)
	s.acceptAhead(global)
}

// takeAccepted hands the oldest child accepted ahead to the kernel's
// Accept, or reports none.
func (s *Service) takeAccepted(o *op, sock *socket) bool {
	for sock.acceptLen != 0 {
		index, serial := int(sock.acceptq[0])-1, sock.acceptSerial[0]
		copy(sock.acceptq[:], sock.acceptq[1:sock.acceptLen])
		copy(sock.acceptSerial[:], sock.acceptSerial[1:sock.acceptLen])
		sock.acceptLen--
		child := &s.sockets[index]
		if !child.used || child.serial != serial {
			continue
		}
		s.adopt(index, child.endpoint, child.generation)
		s.counters.Accepts++
		o.reply.Value, o.reply.Address, o.reply.Port = child.id, child.peer, child.peerPort
		o.reply.Local, o.reply.LocalPort = child.local, child.localPort
		s.publish(int(o.socket))
		s.acceptAhead(int(o.socket))
		s.finish(o, linuxsockapi.ResultOK)
		return true
	}
	return false
}

// dropAccepted closes the children a listener accepted ahead.
func (s *Service) dropAccepted(sock *socket) {
	for position := uint8(0); position < sock.acceptLen; position++ {
		index := int(sock.acceptq[position]) - 1
		if child := &s.sockets[index]; child.used && child.serial == sock.acceptSerial[position] {
			child.opening = false
			s.releaseSocket(index, false)
		}
	}
	sock.acceptLen = 0
}
