// Package tcpserve owns one client's GTCP service state. It contains no
// channel, handle, or VM mapping code, so host tests exercise the same request
// sequencing netd uses.
//
// Each endpoint holds at most one parked Accept, Connect, Read, Write and
// WaitReady. A request with FlagNoWait never parks. A WaitReady parks while
// the endpoint's mask equals the mask the request names; Deliver answers it
// once the mask differs, so a change that happens between a client's Ready
// and its WaitReady is answered at once rather than lost.
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
	readPeek    bool
	write       uint64
	writeSlot   uint8
	writeLength uint16
	ready       uint64
	readyMask   uint16
}

// Session's endpoint table fits the largest quota a session can be granted;
// the stack enforces the session's own quota.
type Session struct {
	owner     int
	tx, rx    []byte
	rxSlots   tcpapi.Slots
	endpoints [network.MaxTCPSessionQuota]waiter
	reply     tcpapi.Reply
}

// Start begins a session with the ordinary endpoint quota.
func (s *Session) Start(stack *network.Stack, owner int, tx, rx []byte, now uint64) {
	s.StartQuota(stack, owner, 0, tx, rx, now)
}

// StartQuota begins a session whose owner may hold quota endpoints, 1 to
// network.MaxTCPSessionQuota; zero is the ordinary quota. Anything the owner
// held before is closed first.
func (s *Session) StartQuota(stack *network.Stack, owner, quota int, tx, rx []byte, now uint64) bool {
	stack.TCPCloseOwner(owner, now)
	if !stack.SetTCPQuota(owner, quota) {
		stack.SetTCPQuota(owner, 0)
		return false
	}
	s.owner, s.tx, s.rx = owner, tx, rx
	s.rxSlots.Reset(tcpapi.RXSlots)
	for index := range s.endpoints {
		s.endpoints[index] = waiter{}
	}
	return true
}

func (s *Session) End(stack *network.Stack, now uint64) {
	stack.TCPCloseOwner(s.owner, now)
	stack.SetTCPQuota(s.owner, 0)
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
		for _, correlation := range [...]uint64{w.accept, w.connect, w.read, w.write, w.ready} {
			if correlation != 0 {
				count++
			}
		}
	}
	return count
}

func (s *Session) Handle(stack *network.Stack, request *tcpapi.Request, correlation, now uint64, out Replier) Fault {
	id := network.TCPEndpointID(request.Endpoint)
	noWait := request.Flags&tcpapi.FlagNoWait != 0
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
		reuse := request.Flags&tcpapi.FlagReuseAddr != 0
		address, port, result := stack.TCPBindReuse(s.owner, id, network.IPv4(request.Address), request.Port, reuse)
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
		if fault := s.deliverAccept(stack, held, out); fault != FaultNone || !noWait || held.accept == 0 {
			return fault
		}
		held.accept = 0
		reply.Result = tcpapi.ResultWouldBlock
	case tcpapi.OpConnect:
		held := s.find(id)
		if held == nil || held.connect != 0 {
			reply.Result = tcpapi.ResultBusy
			break
		}
		tuple, result := stack.TCPConnect(s.owner, id, network.IPv4(request.Address), request.Port, now)
		if result != network.ResultOK {
			reply.Result = Result(result)
			break
		}
		held.connect = correlation
		if fault := s.deliverConnect(stack, held, out); fault != FaultNone || !noWait || held.connect == 0 {
			return fault
		}
		held.connect = 0
		reply.Result = tcpapi.ResultInProgress
		reply.Address, reply.Port, reply.Local, reply.LocalPort = tuple.Remote, tuple.RemotePort, tuple.Local, tuple.LocalPort
	case tcpapi.OpWrite:
		held := s.find(id)
		if held == nil || held.write != 0 {
			reply.Result, reply.Slot = tcpapi.ResultBusy, request.Slot
			break
		}
		offset := tcpapi.Offset(request.Slot)
		written, result := stack.TCPWrite(s.owner, id, s.tx[offset:offset+uint64(request.Length)], now)
		if result == network.ResultBusy {
			if noWait {
				reply.Result, reply.Slot = tcpapi.ResultWouldBlock, request.Slot
				break
			}
			held.write, held.writeSlot, held.writeLength = correlation, request.Slot, request.Length
			return FaultNone
		}
		s.reported(stack, id, result)
		reply.Result, reply.Slot, reply.Length = Result(result), request.Slot, uint16(written)
	case tcpapi.OpRead:
		held := s.find(id)
		if held == nil || held.read != 0 {
			reply.Result = tcpapi.ResultBusy
			break
		}
		if _, free := s.rxSlots.Free(); noWait && !free {
			// The client holds every RX slot; nothing could be lent.
			reply.Result = tcpapi.ResultBusy
			break
		}
		held.read, held.readMax, held.readPeek = correlation, request.Length, request.Flags&tcpapi.FlagPeek != 0
		if fault := s.deliverRead(stack, held, now, out); fault != FaultNone || !noWait || held.read == 0 {
			return fault
		}
		held.read, held.readMax, held.readPeek = 0, 0, false
		reply.Result = tcpapi.ResultWouldBlock
	case tcpapi.OpRelease:
		if !s.rxSlots.Return(request.Slot) {
			return FaultRelease
		}
		return s.Deliver(stack, now, out)
	case tcpapi.OpShutdownWrite:
		reply.Result = Result(stack.TCPShutdownWrite(s.owner, id, now))
	case tcpapi.OpReady:
		ready, result := stack.TCPReadiness(s.owner, id)
		reply.Result = Result(result)
		if result == network.ResultOK {
			reply.Mask, reply.Value, reply.Room = uint16(ready.Mask), ready.Readable, ready.Writable
		}
	case tcpapi.OpWaitReady:
		held := s.find(id)
		if _, result := stack.TCPReadiness(s.owner, id); held == nil || result != network.ResultOK {
			reply.Result = tcpapi.ResultInvalidArgument
			break
		}
		if held.ready != 0 {
			reply.Result = tcpapi.ResultBusy
			break
		}
		held.ready, held.readyMask = correlation, request.Mask
		return s.deliverReady(stack, held, out)
	case tcpapi.OpError:
		pending, result := stack.TCPTakeError(s.owner, id)
		reply.Result = Result(result)
		if result == network.ResultOK {
			reply.Error = Result(pending)
		}
	case tcpapi.OpOption:
		options, result := stack.TCPOptions(s.owner, id)
		reply.Result = Result(result)
		if result == network.ResultOK {
			reply.Value = optionValue(&options, request.Option)
		}
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

func optionValue(options *network.TCPOptions, option uint8) uint32 {
	switch option {
	case tcpapi.OptionSendMSS:
		return options.SendMSS
	case tcpapi.OptionSendBuffer:
		return options.SendBuffer
	case tcpapi.OptionReceiveBuffer:
		return options.ReceiveBuffer
	case tcpapi.OptionTTL:
		return options.TTL
	}
	return 0
}

// reported clears the pending error once a Connect, Read or Write reply has
// carried it to the client, as a Linux call that returns the error clears
// SO_ERROR.
func (s *Session) reported(stack *network.Stack, id network.TCPEndpointID, result network.Result) {
	switch result {
	case network.ResultOK, network.ResultBusy, network.ResultEOF, network.ResultClosed, network.ResultInvalidArgument:
		return
	}
	stack.TCPTakeError(s.owner, id)
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
		if fault := s.deliverReady(stack, held, out); fault != FaultNone {
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
	s.reported(stack, held.id, result)
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
	dst := s.rx[offset : offset+uint64(held.readMax)]
	var read int
	var result network.Result
	if held.readPeek {
		read, result = stack.TCPPeek(s.owner, held.id, dst)
	} else {
		read, result = stack.TCPRead(s.owner, held.id, dst, now)
	}
	if result == network.ResultBusy {
		return FaultNone
	}
	reply := tcpapi.Reply{Opcode: tcpapi.OpRead, Result: Result(result), Endpoint: uint32(held.id)}
	if result == network.ResultOK {
		s.rxSlots.Lend(slot)
		reply.Slot, reply.Length = slot, uint16(read)
	}
	s.reported(stack, held.id, result)
	correlation := held.read
	held.read, held.readMax, held.readPeek = 0, 0, false
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
	s.reported(stack, held.id, result)
	reply := tcpapi.Reply{Opcode: tcpapi.OpWrite, Result: Result(result), Endpoint: uint32(held.id), Slot: held.writeSlot, Length: uint16(written)}
	correlation := held.write
	held.write, held.writeLength = 0, 0
	if !out.Reply(&reply, correlation) {
		return FaultUnreachable
	}
	return FaultNone
}

// deliverReady answers a parked WaitReady once the endpoint's mask differs
// from the one the client saw.
func (s *Session) deliverReady(stack *network.Stack, held *waiter, out Replier) Fault {
	if held.ready == 0 {
		return FaultNone
	}
	ready, result := stack.TCPReadiness(s.owner, held.id)
	if result == network.ResultOK && uint16(ready.Mask) == held.readyMask {
		return FaultNone
	}
	reply := tcpapi.Reply{Opcode: tcpapi.OpWaitReady, Result: Result(result), Endpoint: uint32(held.id)}
	if result == network.ResultOK {
		reply.Mask, reply.Value, reply.Room = uint16(ready.Mask), ready.Readable, ready.Writable
	}
	correlation := held.ready
	held.ready, held.readyMask = 0, 0
	if !out.Reply(&reply, correlation) {
		return FaultUnreachable
	}
	return FaultNone
}

func (s *Session) cancel(held *waiter, out Replier) Fault {
	for _, pending := range [...]struct {
		opcode      tcpapi.Opcode
		correlation uint64
		slot        uint8
	}{
		{tcpapi.OpWaitReady, held.ready, 0},
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
