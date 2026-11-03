package linuxsock

import "goos/userspace/linuxsockapi"

// readiness is what poll reports for a socket, as Linux computes it:
//
//   - a fresh or bound stream is writable and hung up (TCP_CLOSE);
//   - a listener is readable while a child is queued;
//   - a connecting stream reports nothing until the connect ends;
//   - a failed connect is readable, writable, hung up and in error until
//     SO_ERROR is read;
//   - a connected stream is readable with bytes or at end of file,
//     writable while a write takes a byte or once its write side is closed,
//     read-hung-up once the peer or SHUT_RD closed reading, and hung up
//     once both directions are closed; buffered bytes still count;
//   - a datagram socket is writable, and readable with a datagram queued;
//   - a socket whose native session died reports every condition.
func (s *Service) readiness(sock *socket) linuxsockapi.Readiness {
	var ready linuxsockapi.Ready
	var readable uint32
	m := sock.mask
	const in, out, hangup, readHangup = linuxsockapi.ReadyIn, linuxsockapi.ReadyOut, linuxsockapi.ReadyHangup, linuxsockapi.ReadyReadHangup
	switch {
	case sock.dead:
		ready = in | out | hangup | readHangup
	case !sock.stream:
		ready = out
		if sock.readShut {
			ready |= in | readHangup
		} else if m&MaskReadable != 0 {
			ready |= in
			readable = sock.readable
		}
		if sock.readShut && sock.writeShut {
			ready |= hangup
		}
	case sock.state == stateListening:
		if m&MaskAcceptable != 0 && !sock.readShut {
			ready = in
		}
	case sock.state == stateUnbound, sock.state == stateBound:
		ready = out | hangup
	case sock.state == stateConnecting:
	case sock.state == stateFailed:
		ready = in | out | hangup | readHangup
	default:
		readClosed := sock.readShut || m&MaskReceiveEOF != 0
		writeClosed := sock.writeShut || m&MaskWriteClosed != 0
		if !sock.readShut {
			readable = sock.readable + uint32(sock.retainedLength)
			if m&MaskReadable == 0 {
				readable = uint32(sock.retainedLength)
			}
		}
		if readable != 0 || readClosed || m&MaskHungUp != 0 {
			ready |= in
		}
		if readClosed {
			ready |= readHangup
		}
		if m&MaskWritable != 0 || writeClosed || m&MaskHungUp != 0 {
			ready |= out
		}
		if readClosed && writeClosed || m&MaskHungUp != 0 {
			ready |= hangup
		}
	}
	if sock.pending != linuxsockapi.ResultOK || sock.fetching || m&MaskPendingError != 0 && sock.stream && !sock.dead {
		ready |= linuxsockapi.ReadyError
	}
	return linuxsockapi.Readiness{Socket: sock.id, Ready: ready, Readable: readable, Error: sock.pending}
}

// publish writes a socket's entry when it changed and owes the family a
// notification.
func (s *Service) publish(global int) {
	if global < 0 || global >= MaxSockets {
		return
	}
	sock := &s.sockets[global]
	if !sock.used || sock.opening {
		return
	}
	state := s.readiness(sock)
	if state == sock.published {
		return
	}
	f := &s.families[sock.family]
	linuxsockapi.Publish(f.pool, int(sock.index), state)
	sock.published = state
	f.notify = true
	s.notify(int(sock.family))
}

// notify sends ReadyChanged when none is outstanding. A notification the
// kernel has not taken yet covers this change too, since the kernel takes
// the token before it reads the entries. One that finds the channel full
// stays owed and goes from Flush.
func (s *Service) notify(index int) {
	f := &s.families[index]
	if !f.notify {
		return
	}
	if !linuxsockapi.ClaimToken(f.pool) {
		f.notify = false
		return
	}
	request := linuxsockapi.Request{Opcode: linuxsockapi.OpReadyChanged}
	if linuxsockapi.EncodeRequest(&request, 0, &s.message) && s.transport.Send(index, &s.message) {
		f.notify = false
		s.counters.Notifications++
		return
	}
	linuxsockapi.DropToken(f.pool)
	s.counters.DeferredNotifications++
}

// arm holds a NativeWaitReady on the socket's endpoint, naming the mask
// the service knows, so netd answers on the first difference.
func (s *Service) arm(global int) {
	sock := &s.sockets[global]
	if !sock.used || sock.waiting || sock.needWait || sock.noWait || sock.dead || sock.endpoint == 0 ||
		!s.backends[sock.proto].up || s.backends[sock.proto].generation != sock.generation {
		return
	}
	s.native = NativeRequest{Op: NativeWaitReady, Endpoint: sock.endpoint, Mask: sock.mask}
	switch s.backend.Submit(sock.proto, makeTag(tagWait, global, sock.serial), &s.native) {
	case SubmitAccepted:
		sock.waiting = true
	case SubmitBusy:
		sock.needWait = true
		s.waits++
	case SubmitUnsupported:
		sock.noWait = true
	}
}

// waited takes the answer to a socket's wait and holds the next one.
func (s *Service) waited(global int, serial uint32, c *NativeCompletion) {
	sock := &s.sockets[global]
	if !sock.used || sock.serial != serial || !sock.waiting || c.Op != NativeWaitReady {
		return
	}
	sock.waiting = false
	if c.Result != NativeOK {
		if c.Result != NativeClosed {
			s.counters.NativeFailures++
		}
		return
	}
	s.observe(global, c.Mask, c.Readable)
	s.arm(global)
}

// observe applies a mask from netd: a connect that ended moves the
// stream's state, an error netd holds is fetched, and a stream shut for
// reading drains.
func (s *Service) observe(global int, mask NativeMask, readable uint32) {
	sock := &s.sockets[global]
	sock.mask, sock.readable = mask, readable
	if sock.stream && sock.state == stateConnecting {
		switch {
		case mask&MaskConnected != 0:
			sock.state = stateConnected
		case mask&MaskConnectFailed != 0:
			sock.state = stateFailed
			s.fetchError(global)
		}
	}
	if sock.stream && mask&MaskPendingError != 0 {
		s.fetchError(global)
	}
	if sock.readShut {
		s.drain(global)
	}
	s.publish(global)
}
