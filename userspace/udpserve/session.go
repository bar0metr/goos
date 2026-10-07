// Package udpserve is the network service's side of one GUDP1 session
// (ADR 0195): it turns decoded requests into stack calls, holds each
// endpoint's waiting RecvFrom, and tracks the RX slots the client holds. It
// knows nothing of channels or memory objects; the service maps the pools,
// decodes requests and sends the replies this package produces, so the
// whole request path runs in host tests against a real stack.
package udpserve

import (
	"goos/userspace/network"
	"goos/userspace/udpapi"
)

// Replier sends one reply on the session's channel and reports whether it
// went.
type Replier interface {
	Reply(reply *udpapi.Reply, correlation uint64) bool
}

// Fault names why a session must end. The empty fault means it goes on.
type Fault string

const (
	FaultNone        Fault = ""
	FaultRelease     Fault = "release of a slot the client does not hold"
	FaultUnreachable Fault = "client unreachable"
)

// waiter is one of the session's endpoints and the correlation of the
// RecvFrom waiting on it, zero when none waits.
type waiter struct {
	id   network.EndpointID
	recv uint64
}

// Session is one client's GUDP1 state. Owner is the stack owner its
// endpoints are opened under.
type Session struct {
	owner     int
	tx, rx    []byte
	rxSlots   udpapi.Slots
	endpoints [network.MaxUDPSessionEndpoints]waiter
	reply     udpapi.Reply
}

// Start begins a session on owner's endpoints, with the client's TX pool
// mapped read-only and the RX pool writable. Anything the owner held before
// is closed first.
func (s *Session) Start(stack *network.Stack, owner int, tx, rx []byte) {
	stack.UDPCloseOwner(owner)
	s.owner, s.tx, s.rx = owner, tx, rx
	s.rxSlots.Reset(udpapi.RXSlots)
	for index := range s.endpoints {
		s.endpoints[index] = waiter{}
	}
}

// End closes every endpoint of the session. Waiting receives end with it
// unanswered: the client is gone.
func (s *Session) End(stack *network.Stack) {
	stack.UDPCloseOwner(s.owner)
	s.tx, s.rx = nil, nil
	s.rxSlots.Reset(udpapi.RXSlots)
	for index := range s.endpoints {
		s.endpoints[index] = waiter{}
	}
}

// Waiting counts the receives the session holds.
func (s *Session) Waiting() int {
	count := 0
	for index := range s.endpoints {
		if s.endpoints[index].recv != 0 {
			count++
		}
	}
	return count
}

// RXLent reports whether the client holds RX slot.
func (s *Session) RXLent(slot uint8) bool { return s.rxSlots.Lent(slot) }

// Handle serves one decoded request. A RecvFrom with nothing queued is held
// and answered later by Deliver or Close. A fault ends the session.
func (s *Session) Handle(stack *network.Stack, request *udpapi.Request, correlation, now uint64, out Replier) Fault {
	id := network.EndpointID(request.Endpoint)
	reply := &s.reply
	*reply = udpapi.Reply{Opcode: request.Opcode, Endpoint: request.Endpoint}
	switch request.Opcode {
	case udpapi.OpOpen:
		opened, result := stack.UDPOpen(s.owner)
		free := s.find(0)
		if result == network.ResultOK && free == nil {
			stack.UDPClose(s.owner, opened)
			result = network.ResultBusy
		}
		reply.Result = Result(result)
		if result == network.ResultOK {
			*free = waiter{id: opened}
			reply.Endpoint = uint32(opened)
		}
	case udpapi.OpBind:
		address, port, result := stack.UDPBind(s.owner, id, network.IPv4(request.Address), request.Port)
		reply.Result = Result(result)
		if result == network.ResultOK {
			reply.Address, reply.Port = address, port
		}
	case udpapi.OpConnect:
		local, port, result := stack.UDPConnect(s.owner, id, network.IPv4(request.Address), request.Port, now)
		reply.Result = Result(result)
		if result == network.ResultOK {
			reply.Address, reply.Port, reply.Local, reply.LocalPort = request.Address, request.Port, local, port
		}
	case udpapi.OpSend, udpapi.OpSendTo:
		offset := udpapi.Offset(request.Slot)
		payload := s.tx[offset : offset+uint64(request.Length)]
		var result network.Result
		if request.Opcode == udpapi.OpSend {
			result = stack.UDPSend(s.owner, id, payload, now)
		} else {
			result = stack.UDPSendTo(s.owner, id, network.IPv4(request.Address), request.Port, payload, now)
		}
		reply.Result, reply.Slot = Result(result), request.Slot
	case udpapi.OpRecvFrom:
		_, result := stack.UDPQueued(s.owner, id)
		held := s.find(id)
		switch {
		case result != network.ResultOK || held == nil || id == 0:
			reply.Result = udpapi.ResultInvalidArgument
		case held.recv != 0:
			reply.Result = udpapi.ResultBusy
		default:
			held.recv = correlation
			return s.deliver(stack, held, out)
		}
	case udpapi.OpRelease:
		if !s.rxSlots.Return(request.Slot) {
			return FaultRelease
		}
		return s.Deliver(stack, out)
	case udpapi.OpClose:
		held := s.find(id)
		if id == 0 {
			held = nil
		}
		result := stack.UDPClose(s.owner, id)
		if held != nil {
			if held.recv != 0 {
				closed := udpapi.Reply{Opcode: udpapi.OpRecvFrom, Result: udpapi.ResultClosed, Endpoint: request.Endpoint}
				if !out.Reply(&closed, held.recv) {
					return FaultUnreachable
				}
			}
			*held = waiter{}
		}
		*reply = udpapi.Reply{Opcode: udpapi.OpClose, Endpoint: request.Endpoint, Result: Result(result)}
	}
	if !out.Reply(reply, correlation) {
		return FaultUnreachable
	}
	return FaultNone
}

// Deliver answers every waiting RecvFrom whose endpoint has a datagram,
// while the client has an RX slot free for it.
func (s *Session) Deliver(stack *network.Stack, out Replier) Fault {
	for index := range s.endpoints {
		if fault := s.deliver(stack, &s.endpoints[index], out); fault != FaultNone {
			return fault
		}
	}
	return FaultNone
}

func (s *Session) deliver(stack *network.Stack, held *waiter, out Replier) Fault {
	if held.recv == 0 {
		return FaultNone
	}
	slot, free := s.rxSlots.Free()
	if !free {
		return FaultNone
	}
	offset := udpapi.Offset(slot)
	received, found := stack.UDPReceive(s.owner, held.id, s.rx[offset:offset+udpapi.SlotSize])
	if !found {
		return FaultNone
	}
	s.rxSlots.Lend(slot)
	reply := udpapi.Reply{Opcode: udpapi.OpRecvFrom, Endpoint: uint32(held.id), Slot: slot,
		Address: received.Source, Port: received.SourcePort, Length: uint16(received.Length), Local: received.Destination}
	correlation := held.recv
	held.recv = 0
	if !out.Reply(&reply, correlation) {
		return FaultUnreachable
	}
	return FaultNone
}

// find returns the entry for id, or a free entry for id 0.
func (s *Session) find(id network.EndpointID) *waiter {
	for index := range s.endpoints {
		if s.endpoints[index].id == id {
			return &s.endpoints[index]
		}
	}
	return nil
}

// Result maps a stack result to its GUDP1 code.
func Result(result network.Result) udpapi.Result {
	switch result {
	case network.ResultOK:
		return udpapi.ResultOK
	case network.ResultBusy:
		return udpapi.ResultBusy
	case network.ResultDown:
		return udpapi.ResultDown
	case network.ResultNoAddress:
		return udpapi.ResultNoAddress
	case network.ResultNoRoute:
		return udpapi.ResultNoRoute
	case network.ResultNeighborUnreachable:
		return udpapi.ResultNeighborUnreachable
	case network.ResultReconfigured:
		return udpapi.ResultReconfigured
	case network.ResultAddressInUse:
		return udpapi.ResultAddressInUse
	case network.ResultDriverFault:
		return udpapi.ResultDriverFault
	}
	return udpapi.ResultInvalidArgument
}
