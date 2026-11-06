// Package tcpserve owns one client's GTCP service state. It contains no
// channel, handle, or VM mapping code, so host tests exercise the same request
// sequencing netd uses.
//
// Each endpoint holds at most one parked Accept, Connect, Write and
// WaitReady, and as many parked Reads as its session's Layout allows: one in
// version 2, tcpapi.QueuedReads3 in version 3, answered in arrival order. A
// request with FlagNoWait never parks. A version 3 endpoint counts the bytes
// it accepted and takes a Write only at that stream offset. A WaitReady parks while
// the endpoint's mask equals the mask the request names; Deliver answers it
// once the mask differs. If the endpoint changes between a client's Ready
// and its WaitReady, netd answers the WaitReady at once.
package tcpserve

import (
	"goos/kernel/ipc"
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

// parkedRead is one Read waiting for bytes.
type parkedRead struct {
	correlation uint64
	max         uint16
	peek        bool
}

type waiter struct {
	id      network.TCPEndpointID
	accept  uint64
	connect uint64
	// reads[:readCount] wait in arrival order.
	reads       [tcpapi.QueuedReads3]parkedRead
	readCount   uint8
	write       uint64
	writeSlot   uint8
	writeLength uint16
	// accepted counts the bytes the endpoint took from the client, mod
	// 2^32: the stream offset a version 3 Write must name.
	accepted  uint32
	ready     uint64
	readyMask uint16
}

// Session's endpoint table fits the session's quota, sized when it starts;
// the stack enforces the quota.
type Session struct {
	owner     int
	layout    tcpapi.Layout
	tx, rx    []byte
	rxSlots   tcpapi.Slots
	endpoints []waiter
	reply     tcpapi.Reply
	// delivered counts the answers of the Deliver pass under way.
	delivered int
	// progressed records whether the last Deliver call answered a request
	// or closed a child.
	progressed bool
}

// Start begins a version 2 session with the ordinary endpoint quota.
func (s *Session) Start(stack *network.Stack, owner int, tx, rx []byte, now uint64) {
	s.StartQuota(stack, owner, 0, tx, rx, now)
}

// StartQuota begins a version 2 session whose owner may hold quota
// endpoints, 1 to network.MaxTCPSessionQuota; zero is the ordinary quota.
func (s *Session) StartQuota(stack *network.Stack, owner, quota int, tx, rx []byte, now uint64) bool {
	return s.StartLayout(stack, owner, quota, tcpapi.Layout2, tx, rx, now)
}

// StartLayout begins a session that speaks layout's version over tx and rx,
// which must hold layout's pools. Anything the owner held before is closed
// first.
func (s *Session) StartLayout(stack *network.Stack, owner, quota int, layout tcpapi.Layout, tx, rx []byte, now uint64) bool {
	stack.TCPCloseOwner(owner, now)
	known, ok := tcpapi.LayoutFor(layout.Version)
	if !ok || known != layout || uint64(len(tx)) < layout.TXPool() || uint64(len(rx)) < layout.RXPool() ||
		!stack.SetTCPQuota(owner, quota) {
		stack.SetTCPQuota(owner, 0)
		return false
	}
	s.owner, s.layout, s.tx, s.rx = owner, layout, tx, rx
	s.rxSlots.Reset(layout.RXSlots)
	s.endpoints = tableFor(s.endpoints, max(quota, network.MaxTCPEndpointsPerSession))
	for index := range s.endpoints {
		s.endpoints[index] = waiter{}
	}
	return true
}

func (s *Session) End(stack *network.Stack, now uint64) {
	stack.TCPCloseOwner(s.owner, now)
	stack.SetTCPQuota(s.owner, 0)
	s.tx, s.rx = nil, nil
	s.rxSlots.Reset(s.layout.RXSlots)
	for index := range s.endpoints {
		s.endpoints[index] = waiter{}
	}
}

func (s *Session) RXLent(slot uint8) bool { return s.rxSlots.Lent(slot) }

// Layout is the version and pools the session speaks.
func (s *Session) Layout() tcpapi.Layout { return s.layout }

// Decode decodes a client message of the session's version. A message of
// another version fails, and netd ends the session.
func (s *Session) Decode(in *ipc.Message, out *tcpapi.Request) (uint64, bool) {
	return s.layout.DecodeRequest(in, out)
}

// Encode encodes a reply in the session's version.
func (s *Session) Encode(reply *tcpapi.Reply, correlation uint64, out *ipc.Message) bool {
	return s.layout.EncodeReply(reply, correlation, out)
}

func (s *Session) Waiting() int {
	count := 0
	for index := range s.endpoints {
		w := &s.endpoints[index]
		count += int(w.readCount)
		for _, correlation := range [...]uint64{w.accept, w.connect, w.write, w.ready} {
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
	// The slots a request returns go back before it runs, so a read it
	// posts can take one of them. The parked requests a free slot lets go
	// wait for the event loop's Deliver.
	for slot := uint8(0); request.Release>>slot != 0; slot++ {
		if request.Release&(1<<slot) != 0 && !s.rxSlots.Return(slot) {
			return FaultRelease
		}
	}
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
		reply.Slot = request.Slot
		if s.layout.StreamOffsets() {
			if ahead := request.Offset - held.accepted; ahead != 0 {
				// Ahead: an earlier Write left bytes the client resends
				// first. Behind: bytes the endpoint already took.
				reply.Result = tcpapi.ResultWouldBlock
				if ahead >= 1<<31 {
					reply.Result = tcpapi.ResultInvalidArgument
				}
				break
			}
		}
		written, result := s.write(stack, held, request.Slot, request.Length, now)
		if result == network.ResultBusy {
			if noWait {
				reply.Result = tcpapi.ResultWouldBlock
				break
			}
			held.write, held.writeSlot, held.writeLength = correlation, request.Slot, request.Length
			return FaultNone
		}
		s.reported(stack, id, result)
		reply.Result, reply.Length = Result(result), uint16(written)
	case tcpapi.OpRead:
		held := s.find(id)
		if held == nil || held.readCount >= s.layout.QueuedReads {
			reply.Result = tcpapi.ResultBusy
			break
		}
		if noWait && held.readCount != 0 {
			// Bytes that arrive go to the parked Reads first.
			reply.Result = tcpapi.ResultWouldBlock
			break
		}
		if _, free := s.rxSlots.Free(); noWait && !free {
			// The client holds every RX slot; nothing could be lent.
			reply.Result = tcpapi.ResultBusy
			break
		}
		held.reads[held.readCount] = parkedRead{correlation: correlation, max: request.Length, peek: request.Flags&tcpapi.FlagPeek != 0}
		held.readCount++
		if fault := s.deliverRead(stack, held, now, out); fault != FaultNone || !noWait || held.readCount == 0 {
			return fault
		}
		held.readCount, held.reads[0] = 0, parkedRead{}
		reply.Result = tcpapi.ResultWouldBlock
	case tcpapi.OpRelease:
		if !s.rxSlots.Return(request.Slot) {
			return FaultRelease
		}
		// The event loop calls Deliver again before it sleeps, so work
		// left over after this call's budget waits only for that call.
		fault, _ := s.Deliver(stack, now, out)
		return fault
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

// Deliver answers every parked request that can finish now. One answer can
// make another possible, as a read that empties a peer's receive buffer
// gives the other end send room, so passes repeat until one answers
// nothing. A call runs at most maxDeliverPasses passes, so one session
// cannot hold netd's loop. More reports that the last pass still answered
// something: requests may be runnable, and the caller must call Deliver
// again before it sleeps.
func (s *Session) Deliver(stack *network.Stack, now uint64, out Replier) (fault Fault, more bool) {
	s.progressed = false
	for pass := 0; pass < maxDeliverPasses; pass++ {
		s.delivered = 0
		if fault := s.deliverPass(stack, now, out); fault != FaultNone {
			return fault, false
		}
		if s.delivered == 0 {
			return FaultNone, false
		}
		s.progressed = true
	}
	return FaultNone, true
}

// Progressed reports whether the last Deliver call answered a request or
// closed a child. Its reads and writes reach loopback peers at once, so a
// parked request of another session may finish now.
func (s *Session) Progressed() bool { return s.progressed }

// maxDeliverPasses is the work budget of one Deliver call, in passes over
// the endpoint table.
const maxDeliverPasses = 16

// Runnable names a parked request a Deliver pass would answer now, with
// the endpoint it waits on. It changes nothing, and it finds nothing after
// a Deliver call that reported no more work.
func (s *Session) Runnable(stack *network.Stack) (endpoint uint32, op string, found bool) {
	for index := range s.endpoints {
		held := &s.endpoints[index]
		if op := s.runnable(stack, held); op != "" {
			return uint32(held.id), op, true
		}
	}
	return 0, "", false
}

// runnable names the first parked request of held that its deliver
// function would answer now, or returns "". It mirrors their busy tests.
func (s *Session) runnable(stack *network.Stack, held *waiter) string {
	if held.connect == 0 && held.accept == 0 && held.readCount == 0 && held.write == 0 && held.ready == 0 {
		return ""
	}
	ready, result := stack.TCPReadiness(s.owner, held.id)
	if held.connect != 0 {
		if _, _, done := stack.TCPConnectResult(s.owner, held.id); done {
			return "connect"
		}
	}
	if held.accept != 0 && (result != network.ResultOK || ready.Mask&network.TCPReadyListening == 0 || ready.Mask&network.TCPReadyAcceptable != 0) {
		return "accept"
	}
	if _, free := s.rxSlots.Free(); held.readCount != 0 && free {
		var probe [1]byte
		if held.reads[0].max == 0 {
			return "read"
		}
		if _, peeked := stack.TCPPeek(s.owner, held.id, probe[:]); peeked != network.ResultBusy {
			return "read"
		}
	}
	if held.write != 0 {
		busy := result == network.ResultOK && held.writeLength != 0 && ready.Mask&network.TCPReadyConnected != 0 &&
			ready.Mask&(network.TCPReadyWritable|network.TCPReadyWriteClosed) == 0
		if !busy {
			return "write"
		}
	}
	if held.ready != 0 && (result != network.ResultOK || uint16(ready.Mask) != held.readyMask) {
		return "wait-ready"
	}
	return ""
}

// deliver sends one answer of a Deliver pass and counts it.
func (s *Session) deliver(out Replier, reply *tcpapi.Reply, correlation uint64) bool {
	s.delivered++
	return out.Reply(reply, correlation)
}

func (s *Session) deliverPass(stack *network.Stack, now uint64, out Replier) Fault {
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
	if !s.deliver(out, &reply, correlation) {
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
			// Closing the child is progress: the listener may hold another
			// child for the next pass.
			stack.TCPClose(s.owner, child, 0)
			s.delivered++
			return FaultNone
		}
		*free = waiter{id: child}
		reply.Endpoint = uint32(child)
		reply.Address, reply.Port, reply.Local, reply.LocalPort = tuple.Remote, tuple.RemotePort, tuple.Local, tuple.LocalPort
	}
	correlation := held.accept
	held.accept = 0
	if !s.deliver(out, &reply, correlation) {
		return FaultUnreachable
	}
	return FaultNone
}

// deliverRead answers parked Reads in arrival order while bytes, an end of
// stream or an error, and free RX slots, allow.
func (s *Session) deliverRead(stack *network.Stack, held *waiter, now uint64, out Replier) Fault {
	for held.readCount != 0 {
		slot, free := s.rxSlots.Free()
		if !free {
			return FaultNone
		}
		head := held.reads[0]
		offset := s.layout.Offset(slot)
		dst := s.rx[offset : offset+uint64(head.max)]
		var read int
		var result network.Result
		if head.peek {
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
		for index := uint8(1); index < held.readCount; index++ {
			held.reads[index-1] = held.reads[index]
		}
		held.readCount--
		held.reads[held.readCount] = parkedRead{}
		if !s.deliver(out, &reply, head.correlation) {
			return FaultUnreachable
		}
	}
	return FaultNone
}

// write offers one TX slot's bytes to the stack and counts what it took.
func (s *Session) write(stack *network.Stack, held *waiter, slot uint8, length uint16, now uint64) (int, network.Result) {
	offset := s.layout.Offset(slot)
	written, result := stack.TCPWrite(s.owner, held.id, s.tx[offset:offset+uint64(length)], now)
	held.accepted += uint32(written)
	return written, result
}

func (s *Session) deliverWrite(stack *network.Stack, held *waiter, now uint64, out Replier) Fault {
	if held.write == 0 {
		return FaultNone
	}
	written, result := s.write(stack, held, held.writeSlot, held.writeLength, now)
	if result == network.ResultBusy {
		return FaultNone
	}
	s.reported(stack, held.id, result)
	reply := tcpapi.Reply{Opcode: tcpapi.OpWrite, Result: Result(result), Endpoint: uint32(held.id), Slot: held.writeSlot, Length: uint16(written)}
	correlation := held.write
	held.write, held.writeLength = 0, 0
	if !s.deliver(out, &reply, correlation) {
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
	if !s.deliver(out, &reply, correlation) {
		return FaultUnreachable
	}
	return FaultNone
}

// cancel answers everything parked on held with ResultClosed, the queued
// Reads in arrival order.
func (s *Session) cancel(held *waiter, out Replier) Fault {
	for _, pending := range [...]struct {
		opcode      tcpapi.Opcode
		correlation uint64
		slot        uint8
	}{
		{tcpapi.OpWaitReady, held.ready, 0},
		{tcpapi.OpAccept, held.accept, 0}, {tcpapi.OpConnect, held.connect, 0},
		{tcpapi.OpWrite, held.write, held.writeSlot},
	} {
		if pending.correlation == 0 {
			continue
		}
		reply := tcpapi.Reply{Opcode: pending.opcode, Result: tcpapi.ResultClosed, Endpoint: uint32(held.id), Slot: pending.slot}
		if !out.Reply(&reply, pending.correlation) {
			return FaultUnreachable
		}
	}
	for index := uint8(0); index < held.readCount; index++ {
		reply := tcpapi.Reply{Opcode: tcpapi.OpRead, Result: tcpapi.ResultClosed, Endpoint: uint32(held.id)}
		if !out.Reply(&reply, held.reads[index].correlation) {
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

// RXLentBits reports the RX slots the client holds, for diagnostics.
func (s *Session) RXLentBits() uint8 { return s.rxSlots.Bits() }

// tableFor returns table resized to size entries, reusing its storage when
// it is large enough.
func tableFor(table []waiter, size int) []waiter {
	if cap(table) < size {
		return make([]waiter, size)
	}
	return table[:size]
}
