// Package tcpserve owns one client's GTCP1 service state. It contains no
// channel, handle, or VM mapping code, so host tests exercise the same request
// sequencing netd uses.
package tcpserve

import (
	"goos/userspace/network"
	"goos/userspace/tcpapi"
)

type Replier interface {
	Reply(reply *tcpapi.Reply, correlation uint64) bool
}

type Fault string

const (
	FaultNone        Fault = ""
	FaultRelease     Fault = "release of a slot the client does not hold"
	FaultUnreachable Fault = "client unreachable"
)

type waiter struct {
	id          network.TCPEndpointID
	accept      uint64
	connect     uint64
	read        uint64
	readMax     uint16
	write       uint64
	writeSlot   uint8
	writeLength uint16
}

type Session struct {
	owner     int
	tx, rx    []byte
	rxSlots   tcpapi.Slots
	endpoints [network.MaxTCPEndpointsPerSession]waiter
	reply     tcpapi.Reply
}

func (s *Session) Start(stack *network.Stack, owner int, tx, rx []byte, now uint64) {
	stack.TCPCloseOwner(owner, now)
	s.owner, s.tx, s.rx = owner, tx, rx
	s.rxSlots.Reset(tcpapi.RXSlots)
	for index := range s.endpoints {
		s.endpoints[index] = waiter{}
	}
}

func (s *Session) End(stack *network.Stack, now uint64) {
	stack.TCPCloseOwner(s.owner, now)
	s.tx, s.rx = nil, nil
	s.rxSlots.Reset(tcpapi.RXSlots)
	for index := range s.endpoints {
		s.endpoints[index] = waiter{}
	}
}

func (s *Session) RXLent(slot uint8) bool { return s.rxSlots.Lent(slot) }

func (s *Session) Waiting() int {
	count := 0
	for index := range s.endpoints {
		w := &s.endpoints[index]
		if w.accept != 0 {
			count++
		}
		if w.connect != 0 {
			count++
		}
		if w.read != 0 {
			count++
		}
		if w.write != 0 {
			count++
		}
	}
	return count
}

func (s *Session) Handle(stack *network.Stack, request *tcpapi.Request, correlation, now uint64, out Replier) Fault {
	id := network.TCPEndpointID(request.Endpoint)
	reply := &s.reply
	*reply = tcpapi.Reply{Opcode: request.Opcode, Endpoint: request.Endpoint}
	switch request.Opcode {
	case tcpapi.OpOpen:
		opened, result := stack.TCPOpen(s.owner)
		free := s.find(0)
		if result == network.ResultOK && free == nil {
			stack.TCPClose(s.owner, opened, now)
			result = network.ResultBusy
		}
		reply.Result = Result(result)
		if result == network.ResultOK {
			*free = waiter{id: opened}
			reply.Endpoint = uint32(opened)
		}
	case tcpapi.OpBind:
		address, port, result := stack.TCPBind(s.owner, id, network.IPv4(request.Address), request.Port)
		reply.Result = Result(result)
		if result == network.ResultOK {
			reply.Address, reply.Port = address, port
		}
	case tcpapi.OpListen:
		reply.Result = Result(stack.TCPListen(s.owner, id, int(request.Backlog)))
	case tcpapi.OpAccept:
		held := s.find(id)
		if held == nil || held.accept != 0 {
			reply.Result = tcpapi.ResultBusy
			break
		}
		held.accept = correlation
		return s.deliverAccept(stack, held, out)
	case tcpapi.OpConnect:
		held := s.find(id)
		if held == nil || held.connect != 0 {
			reply.Result = tcpapi.ResultBusy
			break
		}
		_, result := stack.TCPConnect(s.owner, id, network.IPv4(request.Address), request.Port, now)
		if result != network.ResultOK {
			reply.Result = Result(result)
			break
		}
		held.connect = correlation
		return s.deliverConnect(stack, held, out)
	case tcpapi.OpWrite:
		held := s.find(id)
		if held == nil || held.write != 0 {
			reply.Result, reply.Slot = tcpapi.ResultBusy, request.Slot
			break
		}
		offset := tcpapi.Offset(request.Slot)
		written, result := stack.TCPWrite(s.owner, id, s.tx[offset:offset+uint64(request.Length)], now)
		if result == network.ResultBusy {
			held.write, held.writeSlot, held.writeLength = correlation, request.Slot, request.Length
			return FaultNone
		}
		reply.Result, reply.Slot, reply.Length = Result(result), request.Slot, uint16(written)
	case tcpapi.OpRead:
		held := s.find(id)
		if held == nil || held.read != 0 {
			reply.Result = tcpapi.ResultBusy
			break
		}
		held.read, held.readMax = correlation, request.Length
		return s.deliverRead(stack, held, now, out)
	case tcpapi.OpRelease:
		if !s.rxSlots.Return(request.Slot) {
			return FaultRelease
		}
		return s.Deliver(stack, now, out)
	case tcpapi.OpShutdownWrite:
		reply.Result = Result(stack.TCPShutdownWrite(s.owner, id, now))
	case tcpapi.OpClose:
		held := s.find(id)
		if held == nil {
			reply.Result = tcpapi.ResultInvalidArgument
			break
		}
		if fault := s.cancel(held, out); fault != FaultNone {
			return fault
		}
		reply.Result = Result(stack.TCPClose(s.owner, id, now))
		*held = waiter{}
	}
	if !out.Reply(reply, correlation) {
		return FaultUnreachable
	}
	return FaultNone
}

func (s *Session) Deliver(stack *network.Stack, now uint64, out Replier) Fault {
	for index := range s.endpoints {
		held := &s.endpoints[index]
		if fault := s.deliverConnect(stack, held, out); fault != FaultNone {
			return fault
		}
		if fault := s.deliverAccept(stack, held, out); fault != FaultNone {
			return fault
		}
		if fault := s.deliverRead(stack, held, now, out); fault != FaultNone {
			return fault
		}
		if fault := s.deliverWrite(stack, held, now, out); fault != FaultNone {
			return fault
		}
	}
	return FaultNone
}

func (s *Session) deliverConnect(stack *network.Stack, held *waiter, out Replier) Fault {
	if held.connect == 0 {
		return FaultNone
	}
	tuple, result, done := stack.TCPConnectResult(s.owner, held.id)
	if !done {
		return FaultNone
	}
	reply := tcpapi.Reply{Opcode: tcpapi.OpConnect, Result: Result(result), Endpoint: uint32(held.id)}
	if result == network.ResultOK {
		reply.Address, reply.Port, reply.Local, reply.LocalPort = tuple.Remote, tuple.RemotePort, tuple.Local, tuple.LocalPort
	}
	correlation := held.connect
	held.connect = 0
	if !out.Reply(&reply, correlation) {
		return FaultUnreachable
	}
	return FaultNone
}

func (s *Session) deliverAccept(stack *network.Stack, held *waiter, out Replier) Fault {
	if held.accept == 0 {
		return FaultNone
	}
	child, tuple, result := stack.TCPAccept(s.owner, held.id)
	if result == network.ResultBusy {
		return FaultNone
	}
	reply := tcpapi.Reply{Opcode: tcpapi.OpAccept, Result: Result(result), Endpoint: uint32(held.id)}
	if result == network.ResultOK {
		free := s.find(0)
		if free == nil {
			stack.TCPClose(s.owner, child, 0)
			return FaultNone
		}
		*free = waiter{id: child}
		reply.Endpoint = uint32(child)
		reply.Address, reply.Port, reply.Local, reply.LocalPort = tuple.Remote, tuple.RemotePort, tuple.Local, tuple.LocalPort
	}
	correlation := held.accept
	held.accept = 0
	if !out.Reply(&reply, correlation) {
		return FaultUnreachable
	}
	return FaultNone
}

func (s *Session) deliverRead(stack *network.Stack, held *waiter, now uint64, out Replier) Fault {
	if held.read == 0 {
		return FaultNone
	}
	slot, free := s.rxSlots.Free()
	if !free {
		return FaultNone
	}
	offset := tcpapi.Offset(slot)
	read, result := stack.TCPRead(s.owner, held.id, s.rx[offset:offset+uint64(held.readMax)], now)
	if result == network.ResultBusy {
		return FaultNone
	}
	reply := tcpapi.Reply{Opcode: tcpapi.OpRead, Result: Result(result), Endpoint: uint32(held.id)}
	if result == network.ResultOK {
		s.rxSlots.Lend(slot)
		reply.Slot, reply.Length = slot, uint16(read)
	}
	correlation := held.read
	held.read, held.readMax = 0, 0
	if !out.Reply(&reply, correlation) {
		return FaultUnreachable
	}
	return FaultNone
}

func (s *Session) deliverWrite(stack *network.Stack, held *waiter, now uint64, out Replier) Fault {
	if held.write == 0 {
		return FaultNone
	}
	offset := tcpapi.Offset(held.writeSlot)
	written, result := stack.TCPWrite(s.owner, held.id, s.tx[offset:offset+uint64(held.writeLength)], now)
	if result == network.ResultBusy {
		return FaultNone
	}
	reply := tcpapi.Reply{Opcode: tcpapi.OpWrite, Result: Result(result), Endpoint: uint32(held.id), Slot: held.writeSlot, Length: uint16(written)}
	correlation := held.write
	held.write, held.writeLength = 0, 0
	if !out.Reply(&reply, correlation) {
		return FaultUnreachable
	}
	return FaultNone
}

func (s *Session) cancel(held *waiter, out Replier) Fault {
	for _, pending := range []struct {
		opcode      tcpapi.Opcode
		correlation uint64
		slot        uint8
	}{
		{tcpapi.OpAccept, held.accept, 0}, {tcpapi.OpConnect, held.connect, 0},
		{tcpapi.OpRead, held.read, 0}, {tcpapi.OpWrite, held.write, held.writeSlot},
	} {
		if pending.correlation == 0 {
			continue
		}
		reply := tcpapi.Reply{Opcode: pending.opcode, Result: tcpapi.ResultClosed, Endpoint: uint32(held.id), Slot: pending.slot}
		if !out.Reply(&reply, pending.correlation) {
			return FaultUnreachable
		}
	}
	return FaultNone
}

func (s *Session) find(id network.TCPEndpointID) *waiter {
	for index := range s.endpoints {
		if s.endpoints[index].id == id {
			return &s.endpoints[index]
		}
	}
	return nil
}

func Result(result network.Result) tcpapi.Result {
	switch result {
	case network.ResultOK:
		return tcpapi.ResultOK
	case network.ResultBusy:
		return tcpapi.ResultBusy
	case network.ResultDown:
		return tcpapi.ResultDown
	case network.ResultNoAddress:
		return tcpapi.ResultNoAddress
	case network.ResultNoRoute:
		return tcpapi.ResultNoRoute
	case network.ResultNeighborUnreachable:
		return tcpapi.ResultNeighborUnreachable
	case network.ResultReconfigured:
		return tcpapi.ResultReconfigured
	case network.ResultAddressInUse:
		return tcpapi.ResultAddressInUse
	case network.ResultConnectionRefused:
		return tcpapi.ResultConnectionRefused
	case network.ResultTimeout:
		return tcpapi.ResultTimeout
	case network.ResultReset:
		return tcpapi.ResultReset
	case network.ResultDriverFault:
		return tcpapi.ResultDriverFault
	case network.ResultClosed:
		return tcpapi.ResultClosed
	case network.ResultEOF:
		return tcpapi.ResultEOF
	}
	return tcpapi.ResultInvalidArgument
}
