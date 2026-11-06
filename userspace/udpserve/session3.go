package udpserve

import (
	"goos/userspace/network"
	"goos/userspace/shmring"
	"goos/userspace/udpapi"
)

// Session3 serves one GUDP 3 client (ADR 0216). Datagrams move through the
// rings of udpapi.Rings3; Handle answers the channel requests, and Deliver
// runs the data plane: it takes the client's ring entries, sends each TX
// slot at once and hands it back, fills RX slots against posted credit and
// answers parked WaitReady requests. It keeps Session's liveness contract:
// Deliver reports more work past its budget, Progressed reports a call that
// did anything, Runnable names work a call would do now, and Idle arms
// netd's doorbell before netd sleeps.
//
// Ownership. netd holds a TX slot from the EntrySend that names it to the
// EntrySent it publishes, which is within the same pass, and lends an RX
// slot from its EntryDatagram to the client's EntryRelease. A ring entry
// that breaks those rules, or names a generation out of turn, ends the
// session with FaultProtocol.
type Session3 struct {
	owner     int
	tx, rx    []byte
	rings     udpapi.Rings3
	endpoints []waiter3
	limit     int
	txGen     [udpapi.TXSlots3]uint32
	rxGen     [udpapi.RXSlots3]uint32
	rxLent    uint32
	reply     udpapi.Reply
	// rotor is the endpoint the next fill starts from, so one busy endpoint
	// cannot keep every RX slot.
	rotor int
	// worked counts the entries taken and published and the answers of the
	// pass under way.
	worked     int
	published  bool
	progressed bool
	Counters   Counters3
}

// Counters3 are a session's data-plane totals.
type Counters3 struct {
	Sends, Releases, Credits, Datagrams, Doorbells uint64
}

type waiter3 struct {
	id        network.EndpointID
	credit    uint32
	ready     uint64
	readyMask uint16
}

// FaultProtocol ends a session whose ring entry broke the data plane rules.
const FaultProtocol Fault = "data plane protocol violation"

// Notifier sends replies and the OpDoorbell notification.
type Notifier interface {
	Replier
	Doorbell() bool
}

// Start begins a GUDP 3 session over tx and rx, the two whole pools with
// their control pages: tx mapped read-only, rx writable. quota is as for
// Session.StartQuota.
func (s *Session3) Start(stack *network.Stack, owner, quota int, tx, rx []byte) bool {
	if len(tx) < udpapi.TXPoolPages3*4096 || len(rx) < udpapi.RXPoolPages3*4096 {
		return false
	}
	stack.UDPCloseOwner(owner)
	if !stack.SetUDPQuota(owner, quota) {
		stack.SetUDPQuota(owner, 0)
		return false
	}
	udpapi.InitService(rx[udpapi.RXControl3:])
	rings, ok := udpapi.ServiceRings3(tx[udpapi.TXControl3:], rx[udpapi.RXControl3:])
	if !ok {
		return false
	}
	*s = Session3{owner: owner, tx: tx, rx: rx, rings: rings, endpoints: s.endpoints}
	size := min(max(quota, network.MaxUDPSessionEndpoints), network.MaxUDPEndpoints)
	if cap(s.endpoints) < size {
		s.endpoints = make([]waiter3, size)
	}
	s.endpoints = s.endpoints[:size]
	clear(s.endpoints)
	return true
}

// End closes every endpoint of the session.
func (s *Session3) End(stack *network.Stack) {
	stack.UDPCloseOwner(s.owner)
	stack.SetUDPQuota(s.owner, 0)
	endpoints := s.endpoints
	clear(endpoints)
	*s = Session3{endpoints: endpoints}
}

// Idle arms netd's doorbell before netd sleeps and reports whether the
// client's rings are still empty. false means netd must Deliver first.
func (s *Session3) Idle() bool {
	s.rings.Arm()
	return !s.rings.Pending()
}

// Waiting counts the parked WaitReady requests.
func (s *Session3) Waiting() int {
	count := 0
	for index := range s.endpoints[:s.limit] {
		if s.endpoints[index].ready != 0 {
			count++
		}
	}
	return count
}

// RXLent reports the RX slots the client holds, as a bitmap.
func (s *Session3) RXLent() uint32 { return s.rxLent }

// Progressed reports whether the last Deliver call did anything.
func (s *Session3) Progressed() bool { return s.progressed }

// Handle takes the client's ring entries, then answers one channel request.
// The entries the client published before it sent the request go first, so
// a Close follows the sends before it.
func (s *Session3) Handle(stack *network.Stack, request *udpapi.Request, correlation, now uint64, out Notifier) Fault {
	s.published = false
	if fault := s.takeRings(stack, now); fault != FaultNone {
		return fault
	}
	if !udpapi.Version3Request(request) {
		return FaultProtocol
	}
	id := network.EndpointID(request.Endpoint)
	reply := &s.reply
	*reply = udpapi.Reply{Opcode: request.Opcode, Endpoint: request.Endpoint}
	switch request.Opcode {
	case udpapi.OpOpen:
		opened, result := stack.UDPOpen(s.owner)
		free := s.free()
		if result == network.ResultOK && free < 0 {
			stack.UDPClose(s.owner, opened)
			result = network.ResultBusy
		}
		reply.Result = Result(result)
		if result == network.ResultOK {
			s.endpoints[free] = waiter3{id: opened}
			s.limit = max(s.limit, free+1)
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
	case udpapi.OpReady:
		ready, result := stack.UDPReadiness(s.owner, id)
		reply.Result = Result(result)
		if result == network.ResultOK {
			reply.Mask, reply.Value, reply.Count = uint16(ready.Mask), ready.Next, ready.Queued
		}
	case udpapi.OpWaitReady:
		held := s.find(id)
		if _, result := stack.UDPReadiness(s.owner, id); held == nil || result != network.ResultOK {
			reply.Result = udpapi.ResultInvalidArgument
			break
		}
		if held.ready != 0 {
			reply.Result = udpapi.ResultBusy
			break
		}
		held.ready, held.readyMask = correlation, request.Mask
		if fault := s.deliverReady(stack, held, out); fault != FaultNone {
			return fault
		}
		return s.ring(out)
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
	case udpapi.OpClose:
		held := s.find(id)
		result := stack.UDPClose(s.owner, id)
		if held != nil {
			if held.ready != 0 {
				closed := udpapi.Reply{Opcode: udpapi.OpWaitReady, Result: udpapi.ResultClosed, Endpoint: request.Endpoint}
				if !out.Reply(&closed, held.ready) {
					return FaultUnreachable
				}
			}
			*held = waiter3{}
			for s.limit > 0 && s.endpoints[s.limit-1].id == 0 {
				s.limit--
			}
		}
		*reply = udpapi.Reply{Opcode: udpapi.OpClose, Endpoint: request.Endpoint, Result: Result(result)}
	}
	if !out.Reply(reply, correlation) {
		return FaultUnreachable
	}
	return s.ring(out)
}

// ring sends the doorbell when this call published and the client armed.
func (s *Session3) ring(out Notifier) Fault {
	if s.published && s.rings.ShouldRing() {
		s.Counters.Doorbells++
		if !out.Doorbell() {
			return FaultUnreachable
		}
	}
	s.published = false
	return FaultNone
}

// Deliver runs the data plane until a pass does nothing, at most
// maxDeliverPasses passes. more reports that the last pass still worked.
func (s *Session3) Deliver(stack *network.Stack, now uint64, out Notifier) (fault Fault, more bool) {
	s.progressed, s.published = false, false
	more = true
	for pass := 0; pass < maxDeliverPasses; pass++ {
		s.worked = 0
		if fault = s.pass(stack, now, out); fault != FaultNone {
			return fault, false
		}
		if s.worked == 0 {
			more = false
			break
		}
		s.progressed = true
	}
	return s.ring(out), more
}

func (s *Session3) pass(stack *network.Stack, now uint64, out Notifier) Fault {
	if fault := s.takeRings(stack, now); fault != FaultNone {
		return fault
	}
	s.fill(stack)
	for index := range s.endpoints[:s.limit] {
		if fault := s.deliverReady(stack, &s.endpoints[index], out); fault != FaultNone {
			return fault
		}
	}
	return FaultNone
}

// takeRings consumes the TX and Release rings. A send goes to the stack at
// once and its slot comes back on Done in the same step.
func (s *Session3) takeRings(stack *network.Stack, now uint64) Fault {
	var e udpapi.Entry
	for {
		status := s.rings.Pop(udpapi.RingTX, &e)
		if status == shmring.Empty {
			break
		}
		expected := s.txGen[e.Slot] + 1
		if expected == 0 {
			expected = 1
		}
		if status != shmring.OK || e.Generation != expected {
			return FaultProtocol
		}
		s.txGen[e.Slot] = e.Generation
		s.worked++
		s.Counters.Sends++
		sent := udpapi.Entry{Kind: udpapi.EntrySent, Slot: e.Slot, Endpoint: e.Endpoint, Generation: e.Generation,
			Result: s.send(stack, &e, now)}
		if s.rings.Push(udpapi.RingDone, &sent) != shmring.OK {
			return FaultProtocol
		}
		s.published = true
	}
	for {
		status := s.rings.Pop(udpapi.RingRelease, &e)
		if status == shmring.Empty {
			return FaultNone
		}
		if status != shmring.OK {
			return FaultProtocol
		}
		s.worked++
		switch e.Kind {
		case udpapi.EntryRelease:
			bit := uint32(1) << e.Slot
			if s.rxLent&bit == 0 || s.rxGen[e.Slot] != e.Generation {
				return FaultProtocol
			}
			s.rxLent &^= bit
			s.Counters.Releases++
		case udpapi.EntryCredit:
			held := s.find(network.EndpointID(e.Endpoint))
			if held == nil {
				// Credit for an endpoint closed meanwhile has nothing to fill.
				continue
			}
			if held.credit+e.Value > udpapi.MaxCredit3 {
				return FaultProtocol
			}
			held.credit += e.Value
			s.Counters.Credits++
		}
	}
}

// send hands one EntrySend's datagram to the stack.
func (s *Session3) send(stack *network.Stack, e *udpapi.Entry, now uint64) udpapi.Result {
	id := network.EndpointID(e.Endpoint)
	if s.find(id) == nil {
		return udpapi.ResultInvalidArgument
	}
	offset := udpapi.Offset(e.Slot)
	payload := s.tx[offset : offset+uint64(e.Length)]
	if e.Flags&udpapi.FlagTo != 0 {
		return Result(stack.UDPSendTo(s.owner, id, network.IPv4(e.Address), e.Port, payload, now))
	}
	return Result(stack.UDPSend(s.owner, id, payload, now))
}

// freeRX returns the lowest RX slot netd holds.
func (s *Session3) freeRX() (uint8, bool) {
	for slot := uint8(0); slot < udpapi.RXSlots3; slot++ {
		if s.rxLent&(1<<slot) == 0 {
			return slot, true
		}
	}
	return 0, false
}

// fill lends RX slots to credited endpoints with datagrams queued, one
// datagram per endpoint per round, starting at the rotor.
func (s *Session3) fill(stack *network.Stack) {
	for s.limit != 0 {
		filled := false
		for step := 0; step < s.limit; step++ {
			index := (s.rotor + step) % s.limit
			held := &s.endpoints[index]
			if held.id == 0 || held.credit == 0 {
				continue
			}
			slot, free := s.freeRX()
			if !free {
				return
			}
			offset := udpapi.Offset(slot)
			received, found := stack.UDPReceive(s.owner, held.id, s.rx[offset:offset+udpapi.SlotSize])
			if !found {
				continue
			}
			s.rxGen[slot]++
			if s.rxGen[slot] == 0 {
				s.rxGen[slot] = 1
			}
			e := udpapi.Entry{Kind: udpapi.EntryDatagram, Slot: slot, Endpoint: uint32(held.id), Generation: s.rxGen[slot],
				Address: received.Source, Port: received.SourcePort, Length: uint16(received.Length), Local: received.Destination}
			if ready, result := stack.UDPReadiness(s.owner, held.id); result == network.ResultOK {
				e.Mask, e.Value = uint16(ready.Mask), ready.Next
			}
			// The RX ring holds every RX slot, so a push finds room.
			if s.rings.Push(udpapi.RingRX, &e) != shmring.OK {
				return
			}
			s.rxLent |= 1 << slot
			held.credit--
			s.worked++
			s.published = true
			s.Counters.Datagrams++
			s.rotor = (index + 1) % s.limit
			filled = true
		}
		if !filled {
			return
		}
	}
}

func (s *Session3) deliverReady(stack *network.Stack, held *waiter3, out Replier) Fault {
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
	s.worked++
	if !out.Reply(&reply, correlation) {
		return FaultUnreachable
	}
	return FaultNone
}

// Runnable names work a Deliver call would do now. It changes nothing; it
// finds nothing after a Deliver call that reported no more work and an Idle
// that found the rings empty.
func (s *Session3) Runnable(stack *network.Stack) (endpoint uint32, op string, found bool) {
	if s.rings.Pending() {
		return 0, "ring", true
	}
	_, free := s.freeRX()
	for index := range s.endpoints[:s.limit] {
		held := &s.endpoints[index]
		if held.id == 0 {
			continue
		}
		ready, result := stack.UDPReadiness(s.owner, held.id)
		if held.credit != 0 && free && result == network.ResultOK && ready.Mask&network.UDPReadyReadable != 0 {
			return uint32(held.id), "receive", true
		}
		if held.ready != 0 && (result != network.ResultOK || uint16(ready.Mask) != held.readyMask) {
			return uint32(held.id), "wait-ready", true
		}
	}
	return 0, "", false
}

func (s *Session3) find(id network.EndpointID) *waiter3 {
	if id == 0 {
		return nil
	}
	for index := range s.endpoints[:s.limit] {
		if s.endpoints[index].id == id {
			return &s.endpoints[index]
		}
	}
	return nil
}

func (s *Session3) free() int {
	for index := range s.endpoints {
		if s.endpoints[index].id == 0 {
			return index
		}
	}
	return -1
}
