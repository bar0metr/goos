package dnsstub

import (
	"goos/userspace/dns"
	"goos/userspace/dnstcp"
	"goos/userspace/udpapi"
	"goos/userspace/upstream"
)

// The datagram side: one endpoint bound to Address:Port. Each datagram that
// arrives takes a request slot; its reply goes back to the address and port
// it came from with SendTo, one at a time.

// BindUDP starts using the resolver's UDP session, whose TX pool is writable
// and RX pool readable, beside client, which must be bound to it already.
// The endpoint opens at the next FlushUDP.
func (s *Server) BindUDP(tx, rx []byte, client *upstream.Client, now uint64) {
	s.udp = udpSide{online: true, tx: tx, rx: rx, slots: client.TXSlots(), upstream: client, retry: now}
}

// UnbindUDP forgets the session and its endpoint. Datagram queries in
// progress end without a reply; their clients ask again.
func (s *Server) UnbindUDP(resolver *dns.Resolver) {
	s.udp = udpSide{}
	for index := 0; index < MaxUDPRequests; index++ {
		s.cancel(resolver, index)
	}
}

func (s *Server) sendUDP(out UDPSender, correlation uint64) bool {
	return out.Send(&s.udpRequest, correlation)
}

// freeUDP returns a free datagram request slot, or -1.
func (s *Server) freeUDP() int {
	for index := 0; index < MaxUDPRequests; index++ {
		if s.requests[index].phase == requestFree {
			return index
		}
	}
	return -1
}

func (s *Server) readyUDP() int {
	for index := 0; index < MaxUDPRequests; index++ {
		if s.requests[index].phase == requestReady {
			return index
		}
	}
	return -1
}

func (s *Server) freeTX() int {
	count := 0
	for slot := uint8(0); slot < udpapi.TXSlots; slot++ {
		if !s.udp.slots.Lent(slot) {
			count++
		}
	}
	return count
}

// FlushUDP opens the endpoint when it is due, keeps a RecvFrom out while a
// request slot is free, and sends the next ready reply. It reports false
// when a request could not be sent; the caller then drops the session.
func (s *Server) FlushUDP(now uint64, out UDPSender) bool {
	side := &s.udp
	if !side.online {
		return true
	}
	switch {
	case side.phase == endpointClosed && now >= side.retry:
		side.phase, side.pending = endpointOpening, s.next()
		s.udpRequest = udpapi.Request{Opcode: udpapi.OpOpen}
		if !s.sendUDP(out, side.pending) {
			return false
		}
	case side.phase == endpointReady && side.recv == 0 && s.freeUDP() >= 0:
		side.recv = s.next()
		s.udpRequest = udpapi.Request{Opcode: udpapi.OpRecvFrom, Endpoint: side.endpoint}
		if !s.sendUDP(out, side.recv) {
			return false
		}
	}
	// A query still opening sent its Open before this SendTo, so its reply,
	// and the slot it needs, come after this SendTo's slot is back: leaving
	// one free slot for each is enough.
	index := s.readyUDP()
	if side.phase != endpointReady || side.held != 0 || index < 0 || s.freeTX() <= side.upstream.Opening() {
		return true
	}
	slot, _ := side.slots.Free()
	current := &s.requests[index]
	copy(side.tx[udpapi.Offset(slot):], current.reply[dnstcp.PrefixSize:dnstcp.PrefixSize+current.length])
	side.slots.Lend(slot)
	side.held++
	current.phase, current.send = requestSending, s.next()
	s.udpRequest = udpapi.Request{Opcode: udpapi.OpSendTo, Endpoint: side.endpoint, Slot: slot,
		Length: uint16(current.length), Address: current.address, Port: current.port}
	return s.sendUDP(out, current.send)
}

// AnswerUDP handles one decoded reply whose correlation Owns. It reports
// false for a protocol violation or a failed send.
func (s *Server) AnswerUDP(resolver *dns.Resolver, reply *udpapi.Reply, correlation, now uint64, out UDPSender) bool {
	side := &s.udp
	if !side.online {
		return true
	}
	switch reply.Opcode {
	case udpapi.OpOpen:
		if side.phase != endpointOpening || correlation != side.pending {
			if reply.Result == udpapi.ResultOK {
				return s.closeUDP(reply.Endpoint, out)
			}
			return true
		}
		if reply.Result != udpapi.ResultOK {
			side.phase, side.pending, side.retry = endpointClosed, 0, now+RetryDelay
			return true
		}
		side.endpoint, side.phase, side.pending = reply.Endpoint, endpointBinding, s.next()
		s.udpRequest = udpapi.Request{Opcode: udpapi.OpBind, Endpoint: side.endpoint, Address: Address, Port: Port}
		return s.sendUDP(out, side.pending)
	case udpapi.OpBind:
		if side.phase != endpointBinding || correlation != side.pending {
			return true
		}
		if reply.Result != udpapi.ResultOK {
			return s.dropUDP(now, out)
		}
		side.phase, side.pending = endpointReady, 0
	case udpapi.OpRecvFrom:
		mine := side.recv != 0 && correlation == side.recv
		if mine {
			side.recv = 0
		}
		if reply.Result != udpapi.ResultOK {
			if mine && side.phase == endpointReady {
				return s.dropUDP(now, out)
			}
			return true
		}
		if mine && reply.Endpoint == side.endpoint {
			s.received(resolver, reply, now)
		}
		s.udpRequest = udpapi.Request{Opcode: udpapi.OpRelease, Slot: reply.Slot}
		return s.sendUDP(out, 0)
	case udpapi.OpSend, udpapi.OpSendTo:
		if side.held == 0 || !side.slots.Return(reply.Slot) {
			return false
		}
		side.held--
		for index := 0; index < MaxUDPRequests; index++ {
			if current := &s.requests[index]; current.phase == requestSending && current.send == correlation {
				current.phase = requestFree
				if reply.Result == udpapi.ResultOK {
					increment(&s.stats.Replies)
				}
			}
		}
	}
	return true
}

// received takes one datagram. Only a loopback client can reach an endpoint
// bound to Address; the check keeps it so if the stack ever changes.
func (s *Server) received(resolver *dns.Resolver, reply *udpapi.Reply, now uint64) {
	index := s.freeUDP()
	if index < 0 || reply.Address[0] != Address[0] || reply.Port == 0 {
		increment(&s.stats.Ignored)
		return
	}
	increment(&s.stats.UDPQueries)
	current := &s.requests[index]
	current.address, current.port = reply.Address, reply.Port
	offset := udpapi.Offset(reply.Slot)
	s.take(resolver, index, s.udp.rx[offset:offset+uint64(reply.Length)], now)
}

// dropUDP closes the endpoint after a failure and opens a new one after
// RetryDelay. Queries in progress keep their slots and are answered from the
// new endpoint, which has the same address and port.
func (s *Server) dropUDP(now uint64, out UDPSender) bool {
	side := &s.udp
	endpoint := side.endpoint
	side.phase, side.endpoint, side.pending, side.recv, side.retry = endpointClosed, 0, 0, 0, now+RetryDelay
	if endpoint == 0 {
		return true
	}
	return s.closeUDP(endpoint, out)
}

func (s *Server) closeUDP(endpoint uint32, out UDPSender) bool {
	s.udpRequest = udpapi.Request{Opcode: udpapi.OpClose, Endpoint: endpoint}
	return s.sendUDP(out, s.next())
}
