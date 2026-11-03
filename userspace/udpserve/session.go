// Package udpserve is the network service's side of one GUDP session
// (ADR 0195): it turns decoded requests into stack calls, holds each
// endpoint's waiting RecvFrom and WaitReady, and tracks the RX slots the
// client holds. A WaitReady parks while the endpoint's mask equals the mask
// it names, so a datagram that arrives between a client's Ready and its
// WaitReady answers the wait at once. It
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
	id        network.EndpointID
	recv      uint64
	recvPeek  bool
	ready     uint64
	readyMask uint16
}

// Session is one client's GUDP1 state. Owner is the stack owner its
// endpoints are opened under.
type Session struct {
	owner     int
	tx, rx    []byte
	rxSlots   udpapi.Slots
	endpoints [network.MaxUDPSessionQuota]waiter
	reply     udpapi.Reply
}

// Start begins a session on owner's endpoints with the ordinary quota.
func (s *Session) Start(stack *network.Stack, owner int, tx, rx []byte) {
	s.StartQuota(stack, owner, 0, tx, rx)
}

// StartQuota begins a session on owner's endpoints, with the client's TX
// pool mapped read-only and the RX pool writable. The owner may hold quota
// endpoints, 1 to network.MaxUDPSessionQuota; zero is the ordinary quota.
// Anything the owner held before is closed first.
func (s *Session) StartQuota(stack *network.Stack, owner, quota int, tx, rx []byte) bool {
	stack.UDPCloseOwner(owner)
	if !stack.SetUDPQuota(owner, quota) {
		stack.SetUDPQuota(owner, 0)
		return false
	}
	s.owner, s.tx, s.rx = owner, tx, rx
	s.rxSlots.Reset(udpapi.RXSlots)
	for index := range s.endpoints {
		s.endpoints[index] = waiter{}
	}
	return true
}

// End closes every endpoint of the session. Waiting receives end with it
// unanswered: the client is gone.
func (s *Session) End(stack *network.Stack) {
	stack.UDPCloseOwner(s.owner)
	stack.SetUDPQuota(s.owner, 0)
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
		if s.endpoints[index].ready != 0 {
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
		reuse := request.Flags&udpapi.FlagReuseAddr != 0
		address, port, result := stack.UDPBindReuse(s.owner, id, network.IPv4(request.Address), request.Port, reuse)
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
		case request.Flags&udpapi.FlagNoWait == 0:
			held.recv, held.recvPeek = correlation, request.Flags&udpapi.FlagPeek != 0
			return s.deliver(stack, held, out)
		default:
			if _, free := s.rxSlots.Free(); !free {
				// The client holds every RX slot; nothing could be lent.
				reply.Result = udpapi.ResultBusy
				break
			}
			held.recv, held.recvPeek = correlation, request.Flags&udpapi.FlagPeek != 0
			if fault := s.deliver(stack, held, out); fault != FaultNone || held.recv == 0 {
				return fault
			}
			held.recv, held.recvPeek = 0, false
			reply.Result = udpapi.ResultWouldBlock
		}
	case udpapi.OpReady:
		ready, result := stack.UDPReadiness(s.owner, id)
		reply.Result = Result(result)
		if result == network.ResultOK {
			reply.Mask, reply.Value, reply.Count = uint16(ready.Mask), ready.Next, ready.Queued
		}
	case udpapi.OpWaitReady:
		held := s.find(id)
		if _, result := stack.UDPReadiness(s.owner, id); held == nil || id == 0 || result != network.ResultOK {
			reply.Result = udpapi.ResultInvalidArgument
			break
		}
		if held.ready != 0 {
			reply.Result = udpapi.ResultBusy
			break
		}
		held.ready, held.readyMask = correlation, request.Mask
		return s.deliverReady(stack, held, out)
	case udpapi.OpOption:
		var value int
		_, result := stack.UDPReadiness(s.owner, id)
		if result != network.ResultOK {
			result = network.ResultInvalidArgument
		} else if request.Option == udpapi.OptionTTL {
			value = int(stack.Tuning().TTL)
		} else {
			value, result = stack.UDPMaxPayload(s.owner, id, network.IPv4(request.Address))
		}
		reply.Result = Result(result)
		if result == network.ResultOK {
			reply.Value = uint32(value)
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
			if held.ready != 0 {
				closed := udpapi.Reply{Opcode: udpapi.OpWaitReady, Result: udpapi.ResultClosed, Endpoint: request.Endpoint}
				if !out.Reply(&closed, held.ready) {
					return FaultUnreachable
				}
			}
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
// while the client has an RX slot free for it, then every WaitReady whose
// endpoint's mask changed.
func (s *Session) Deliver(stack *network.Stack, out Replier) Fault {
	for index := range s.endpoints {
		if fault := s.deliver(stack, &s.endpoints[index], out); fault != FaultNone {
			return fault
		}
		if fault := s.deliverReady(stack, &s.endpoints[index], out); fault != FaultNone {
			return fault
		}
	}
	return FaultNone
}

func (s *Session) deliverReady(stack *network.Stack, held *waiter, out Replier) Fault {
	if held.ready == 0 {
		return FaultNone
	}
	ready, result := stack.UDPReadiness(s.owner, held.id)
	if result == network.ResultOK && uint16(ready.Mask) == held.readyMask {
		return FaultNone
	}
	reply := udpapi.Reply{Opcode: udpapi.OpWaitReady, Result: Result(result), Endpoint: uint32(held.id)}
	if result == network.ResultOK {
		reply.Mask, reply.Value, reply.Count = uint16(ready.Mask), ready.Next, ready.Queued
	}
	correlation := held.ready
	held.ready, held.readyMask = 0, 0
	if !out.Reply(&reply, correlation) {
		return FaultUnreachable
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
	dst := s.rx[offset : offset+udpapi.SlotSize]
	var received network.UDPReceived
	var found bool
	if held.recvPeek {
		received, found = stack.UDPPeek(s.owner, held.id, dst)
	} else {
		received, found = stack.UDPReceive(s.owner, held.id, dst)
	}
	if !found {
		return FaultNone
	}
	s.rxSlots.Lend(slot)
	reply := udpapi.Reply{Opcode: udpapi.OpRecvFrom, Endpoint: uint32(held.id), Slot: slot,
		Address: received.Source, Port: received.SourcePort, Length: uint16(received.Length), Local: received.Destination}
	correlation := held.recv
	held.recv, held.recvPeek = 0, false
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
	case network.ResultMessageSize:
		return udpapi.ResultMessageSize
	}
	return udpapi.ResultInvalidArgument
}
