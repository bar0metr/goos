package dnsstub

import (
	"goos/userspace/dns"
	"goos/userspace/dnstcp"
	"goos/userspace/tcpapi"
	"goos/userspace/tcpupstream"
)

// The stream side (RFC 7766): a listener on Address:Port and up to
// MaxConnections accepted connections. A connection carries any number of
// queries, one at a time: the server reads exactly one length-prefixed
// query, so a pipelining client's next one stays in the stream, answers it,
// writes the framed reply and reads again. The client closes when it is
// done; a connection idle past IdleTimeout is closed by the server.

// BindTCP starts using the resolver's TCP session beside fallback, which
// must be bound to it already. The listener opens at the next FlushTCP.
func (s *Server) BindTCP(tx, rx []byte, fallback *tcpupstream.Client, now uint64) {
	side := &s.tcp
	side.online, side.tx, side.rx, side.slots = true, tx, rx, fallback.TXSlots()
	side.phase, side.listener, side.pending, side.accept, side.held, side.retry = endpointClosed, 0, 0, 0, 0, now
	for index := range side.connections {
		side.connections[index].phase = connectionFree
		side.connections[index].pending = 0
	}
}

// UnbindTCP forgets the session. Its connections are gone with it, and
// their queries in progress end.
func (s *Server) UnbindTCP(resolver *dns.Resolver) {
	side := &s.tcp
	side.online, side.tx, side.rx, side.slots = false, nil, nil, nil
	side.phase, side.listener, side.pending, side.accept, side.held = endpointClosed, 0, 0, 0, 0
	for index := range side.connections {
		s.cancel(resolver, MaxUDPRequests+index)
		side.connections[index].phase = connectionFree
		side.connections[index].pending = 0
	}
}

func (s *Server) sendTCP(out TCPSender, correlation uint64) bool {
	return out.Send(&s.tcpRequest, correlation)
}

func (s *Server) freeConnection() int {
	for index := range s.tcp.connections {
		if s.tcp.connections[index].phase == connectionFree {
			return index
		}
	}
	return -1
}

func (s *Server) owning(correlation uint64) int {
	for index := range s.tcp.connections {
		if current := &s.tcp.connections[index]; current.phase != connectionFree && current.pending == correlation {
			return index
		}
	}
	return -1
}

// FlushTCP closes connections past their deadline, opens the listener when
// it is due, keeps an Accept out while a connection slot is free, and starts
// the writes that waited for a TX slot. It reports false when a request
// could not be sent; the caller then drops the session.
func (s *Server) FlushTCP(resolver *dns.Resolver, now uint64, out TCPSender) bool {
	side := &s.tcp
	if !side.online {
		return true
	}
	for index := range side.connections {
		current := &side.connections[index]
		if (current.phase == connectionReading || current.phase == connectionWriting) && now >= current.deadline {
			increment(&s.stats.Timeouts)
			if !s.closeConnection(resolver, index, out) {
				return false
			}
		}
	}
	switch {
	case side.phase == endpointClosed && now >= side.retry:
		side.phase, side.pending = endpointOpening, s.next()
		s.tcpRequest = tcpapi.Request{Opcode: tcpapi.OpOpen}
		if !s.sendTCP(out, side.pending) {
			return false
		}
	case side.phase == endpointReady && side.accept == 0 && s.freeConnection() >= 0:
		side.accept = s.next()
		s.tcpRequest = tcpapi.Request{Opcode: tcpapi.OpAccept, Endpoint: side.listener}
		if !s.sendTCP(out, side.accept) {
			return false
		}
	}
	for index := range side.connections {
		if current := &side.connections[index]; current.phase == connectionWriting && current.pending == 0 && !s.write(index, out) {
			return false
		}
	}
	return true
}

// write offers the next piece of connection index's framed reply, when the
// server may take a TX slot. Otherwise the piece waits for FlushTCP.
func (s *Server) write(index int, out TCPSender) bool {
	side := &s.tcp
	if side.held >= maxTCPSlots {
		return true
	}
	slot, free := side.slots.Free()
	if !free {
		return true
	}
	current := &side.connections[index]
	reply := &s.requests[MaxUDPRequests+index]
	offset := tcpapi.Offset(slot)
	length := copy(side.tx[offset:offset+tcpapi.SlotSize], reply.reply[current.written:dnstcp.PrefixSize+reply.length])
	side.slots.Lend(slot)
	side.held++
	current.offered, current.pending = length, s.next()
	s.tcpRequest = tcpapi.Request{Opcode: tcpapi.OpWrite, Endpoint: current.endpoint, Slot: slot, Length: uint16(length)}
	return s.sendTCP(out, current.pending)
}

func (s *Server) read(current *connection, out TCPSender) bool {
	current.pending = s.next()
	s.tcpRequest = tcpapi.Request{Opcode: tcpapi.OpRead, Endpoint: current.endpoint,
		Length: uint16(current.receiver.Exact(tcpapi.SlotSize))}
	return s.sendTCP(out, current.pending)
}

// closeConnection ends connection index and its query. Requests still out
// on it are answered when the endpoint closes; their replies match nothing
// and only settle the slots they hold.
func (s *Server) closeConnection(resolver *dns.Resolver, index int, out TCPSender) bool {
	current := &s.tcp.connections[index]
	s.cancel(resolver, MaxUDPRequests+index)
	current.phase, current.pending = connectionFree, 0
	return s.closeTCP(current.endpoint, out)
}

func (s *Server) closeTCP(endpoint uint32, out TCPSender) bool {
	s.tcpRequest = tcpapi.Request{Opcode: tcpapi.OpClose, Endpoint: endpoint}
	return s.sendTCP(out, s.next())
}

func (s *Server) releaseTCP(slot uint8, out TCPSender) bool {
	s.tcpRequest = tcpapi.Request{Opcode: tcpapi.OpRelease, Slot: slot}
	return s.sendTCP(out, 0)
}

// dropListener closes the listener after a failure and opens a new one
// after RetryDelay. Accepted connections go on.
func (s *Server) dropListener(now uint64, out TCPSender) bool {
	side := &s.tcp
	listener := side.listener
	side.phase, side.listener, side.pending, side.accept, side.retry = endpointClosed, 0, 0, 0, now+RetryDelay
	if listener == 0 {
		return true
	}
	return s.closeTCP(listener, out)
}

// AnswerTCP handles one decoded reply whose correlation Owns. TX slots come
// back with every Write reply and every successful Read lends an RX slot,
// whatever the connection became since; both are settled first. It reports
// false for a protocol violation or a failed send.
func (s *Server) AnswerTCP(resolver *dns.Resolver, reply *tcpapi.Reply, correlation, now uint64, out TCPSender) bool {
	side := &s.tcp
	if !side.online {
		return true
	}
	if reply.Opcode == tcpapi.OpWrite {
		if side.held == 0 || !side.slots.Return(reply.Slot) {
			return false
		}
		side.held--
	}
	lent := reply.Opcode == tcpapi.OpRead && reply.Result == tcpapi.ResultOK
	switch reply.Opcode {
	case tcpapi.OpOpen, tcpapi.OpBind, tcpapi.OpListen:
		return s.listenerReply(reply, correlation, now, out)
	case tcpapi.OpAccept:
		return s.accepted(reply, correlation, now, out)
	case tcpapi.OpClose:
		return true
	}
	index := s.owning(correlation)
	if index < 0 {
		if lent {
			return s.releaseTCP(reply.Slot, out)
		}
		return true
	}
	current := &side.connections[index]
	current.pending = 0
	switch {
	case reply.Opcode == tcpapi.OpRead && current.phase == connectionReading && reply.Endpoint == current.endpoint:
		if !lent {
			// End of stream, a reset or a closed endpoint.
			return s.closeConnection(resolver, index, out)
		}
		if int(reply.Length) > current.receiver.Exact(tcpapi.SlotSize) {
			return false
		}
		return s.readPiece(resolver, index, reply, now, out)
	case reply.Opcode == tcpapi.OpWrite && current.phase == connectionWriting && reply.Endpoint == current.endpoint:
		if reply.Result != tcpapi.ResultOK {
			return s.closeConnection(resolver, index, out)
		}
		if int(reply.Length) > current.offered {
			return false
		}
		current.written += int(reply.Length)
		request := &s.requests[MaxUDPRequests+index]
		if current.written < dnstcp.PrefixSize+request.length {
			return s.write(index, out)
		}
		request.phase = requestFree
		increment(&s.stats.Replies)
		current.phase, current.deadline = connectionReading, now+IdleTimeout
		current.receiver.Reset()
		return s.read(current, out)
	}
	return false
}

// readPiece takes one Read's bytes into the connection's query and gives
// the RX slot back. A whole query is answered or looked up.
func (s *Server) readPiece(resolver *dns.Resolver, index int, reply *tcpapi.Reply, now uint64, out TCPSender) bool {
	side := &s.tcp
	current := &side.connections[index]
	offset := tcpapi.Offset(reply.Slot)
	complete, valid := current.receiver.TakeInto(current.buffer[:], side.rx[offset:offset+uint64(reply.Length)])
	if !s.releaseTCP(reply.Slot, out) {
		return false
	}
	switch {
	case !valid:
		increment(&s.stats.Ignored)
		return s.closeConnection(resolver, index, out)
	case !complete:
		return s.read(current, out)
	}
	increment(&s.stats.TCPQueries)
	number := MaxUDPRequests + index
	if !s.take(resolver, number, current.buffer[:current.receiver.Length()], now) {
		return s.closeConnection(resolver, index, out)
	}
	if s.requests[number].phase == requestResolving {
		current.phase = connectionResolving
		return true
	}
	current.phase, current.deadline = connectionWriting, now+IdleTimeout
	current.written, current.offered = 0, 0
	return s.write(index, out)
}

// listenerReply moves the listener through Open, Bind and Listen.
func (s *Server) listenerReply(reply *tcpapi.Reply, correlation, now uint64, out TCPSender) bool {
	side := &s.tcp
	ok := reply.Result == tcpapi.ResultOK
	switch reply.Opcode {
	case tcpapi.OpOpen:
		if side.phase != endpointOpening || correlation != side.pending {
			if ok {
				return s.closeTCP(reply.Endpoint, out)
			}
			return true
		}
		if !ok {
			side.phase, side.pending, side.retry = endpointClosed, 0, now+RetryDelay
			return true
		}
		side.listener, side.phase, side.pending = reply.Endpoint, endpointBinding, s.next()
		s.tcpRequest = tcpapi.Request{Opcode: tcpapi.OpBind, Endpoint: side.listener, Address: Address, Port: Port}
		return s.sendTCP(out, side.pending)
	case tcpapi.OpBind:
		if side.phase != endpointBinding || correlation != side.pending {
			return true
		}
		if !ok {
			return s.dropListener(now, out)
		}
		side.phase, side.pending = endpointListening, s.next()
		s.tcpRequest = tcpapi.Request{Opcode: tcpapi.OpListen, Endpoint: side.listener, Backlog: Backlog}
		return s.sendTCP(out, side.pending)
	}
	if side.phase != endpointListening || correlation != side.pending {
		return true
	}
	if !ok {
		return s.dropListener(now, out)
	}
	side.phase, side.pending = endpointReady, 0
	return true
}

// accepted takes a new connection into a free slot. One nobody can serve
// is closed at once.
func (s *Server) accepted(reply *tcpapi.Reply, correlation, now uint64, out TCPSender) bool {
	side := &s.tcp
	mine := side.accept != 0 && correlation == side.accept
	if mine {
		side.accept = 0
	}
	if reply.Result != tcpapi.ResultOK {
		if mine && side.phase == endpointReady {
			return s.dropListener(now, out)
		}
		return true
	}
	index := s.freeConnection()
	if !mine || index < 0 || reply.Address[0] != Address[0] {
		return s.closeTCP(reply.Endpoint, out)
	}
	increment(&s.stats.Connections)
	current := &side.connections[index]
	current.phase, current.endpoint, current.deadline = connectionReading, reply.Endpoint, now+IdleTimeout
	current.receiver.Reset()
	return s.read(current, out)
}
