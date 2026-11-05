package tcpserve

import (
	"goos/kernel/ipc"
	"goos/userspace/network"
	"goos/userspace/shmring"
	"goos/userspace/tcpapi"
)

// Session4 serves one GTCP 4 client. Stream data moves through the rings of
// tcpapi.Control; Handle answers the channel requests, and Deliver runs the
// data plane: it takes the client's ring entries, sends queued TX slots,
// fills RX slots against posted credit, accepts ahead, finishes closes and
// answers parked channel requests. It keeps Session's liveness contract:
// Deliver reports more work past its budget, Progressed reports a call that
// did anything, and Runnable names work a call would do now.
//
// Ownership. netd holds a TX slot from the EntryWrite that names it to the
// EntryWriteDone it publishes, and lends an RX slot from its EntryData to
// the client's EntryRelease. Any ring entry that breaks those rules, or
// names a generation out of turn, ends the session with FaultProtocol.
type Session4 struct {
	owner     int
	tx, rx    []byte
	control   tcpapi.Control
	endpoints [tcpapi.Endpoints4]endpoint4
	txSlots   [tcpapi.TXSlots4]txSlot4
	// txDone holds TX slots finished but not yet on the TXDone ring.
	txDone uint32
	rxGen  [tcpapi.RXSlots4]uint32
	rxLent uint32
	reply  tcpapi.Reply
	// rotor is the endpoint the next RX fill pass starts from, so one busy
	// endpoint cannot keep every RX slot.
	rotor int
	// worked counts the ring entries taken and published, the stack calls
	// that moved bytes and the replies of the pass under way.
	worked     int
	published  bool
	progressed bool
	Counters   Counters4
}

// Counters4 are a session's data-plane totals.
type Counters4 struct {
	TXEntries, ReleaseEntries, WriteDone, Data, Ends, Accepted, Closed uint64
	// Doorbells counts the OpDoorbell notifications netd sent.
	Doorbells uint64
}

// FaultProtocol ends a session whose ring entry broke the data plane rules.
const FaultProtocol Fault = "data plane protocol violation"

// Notifier sends replies and the OpDoorbell notification.
type Notifier interface {
	Replier
	Doorbell() bool
}

type endpointState uint8

const (
	endpointFree endpointState = iota
	endpointOpen
	// endpointClosing: the client closed; netd sends its queued TX slots,
	// then closes the connection.
	endpointClosing
	// endpointClosed: the stack closed the endpoint; the entry waits for
	// the client's EntryForget.
	endpointClosed
)

type endpoint4 struct {
	id    network.TCPEndpointID
	state endpointState
	// announced: EntryClosed is on the ring.
	announced bool
	accept    uint64
	connect   uint64
	// openConnect: the parked connect is an OpOpenConnect.
	openConnect bool
	ready       uint64
	readyMask   uint16
	ahead       uint8
	credit      uint32
	// queued is the stream offset past the last byte the client queued.
	queued         uint32
	txHead, txTail int8
	shutdown       bool
	shown          tcpapi.Readiness
}

type txSlot4 struct {
	held       bool
	generation uint32
	index      uint8
	endpoint   uint32
	length     uint32
	taken      uint32
	next       int8
	result     tcpapi.Result
}

// Start begins a GTCP 4 session over tx and rx, the two pools with their
// control pages. Anything the owner held before is closed first.
func (s *Session4) Start(stack *network.Stack, owner, quota int, tx, rx []byte, now uint64) bool {
	stack.TCPCloseOwner(owner, now)
	control, ok := tcpapi.ControlOf(tx, rx)
	if !ok || uint64(len(tx)) < tcpapi.Layout4.TXPool() || uint64(len(rx)) < tcpapi.Layout4.RXPool() || !stack.SetTCPQuota(owner, quota) {
		stack.SetTCPQuota(owner, 0)
		return false
	}
	*s = Session4{owner: owner, tx: tx, rx: rx, control: control}
	for index := range s.endpoints {
		s.endpoints[index].txHead, s.endpoints[index].txTail = -1, -1
	}
	s.control.InitService()
	return true
}

// End closes everything the session held. The client's slots return to it
// with the session.
func (s *Session4) End(stack *network.Stack, now uint64) {
	stack.TCPCloseOwner(s.owner, now)
	stack.SetTCPQuota(s.owner, 0)
	*s = Session4{owner: s.owner}
}

func (s *Session4) Layout() tcpapi.Layout { return tcpapi.Layout4 }

func (s *Session4) Decode(in *ipc.Message, out *tcpapi.Request) (uint64, bool) {
	return tcpapi.Layout4.DecodeRequest(in, out)
}

func (s *Session4) Encode(reply *tcpapi.Reply, correlation uint64, out *ipc.Message) bool {
	return tcpapi.Layout4.EncodeReply(reply, correlation, out)
}

// Idle arms netd's doorbell before netd sleeps and reports whether the
// client's rings are still empty. When it reports false netd must run
// Deliver first.
func (s *Session4) Idle() bool {
	s.control.ServiceArm()
	return !s.control.ServicePending()
}

// Waiting counts the parked channel requests.
func (s *Session4) Waiting() int {
	count := 0
	for index := range s.endpoints {
		e := &s.endpoints[index]
		for _, correlation := range [...]uint64{e.accept, e.connect, e.ready} {
			if correlation != 0 {
				count++
			}
		}
	}
	return count
}

// TXHeld and RXLent report the slots netd holds and the slots the client
// holds, as bitmaps, for diagnostics.
func (s *Session4) TXHeld() uint32 {
	held := uint32(0)
	for slot := range s.txSlots {
		if s.txSlots[slot].held {
			held |= 1 << slot
		}
	}
	return held
}

func (s *Session4) RXLent() uint32 { return s.rxLent }

// Handle takes the client's ring entries, then answers one channel request.
// The entries the client published before it sent the request go first, so
// a Close follows the Writes before it.
func (s *Session4) Handle(stack *network.Stack, request *tcpapi.Request, correlation, now uint64, out Notifier) Fault {
	if fault := s.takeRings(stack, now); fault != FaultNone {
		return fault
	}
	if request.Opcode == tcpapi.OpDoorbell {
		fault, _ := s.Deliver(stack, now, out)
		return fault
	}
	id := network.TCPEndpointID(request.Endpoint)
	noWait := request.Flags&tcpapi.FlagNoWait != 0
	reply := &s.reply
	*reply = tcpapi.Reply{Opcode: request.Opcode, Endpoint: request.Endpoint}
	held, index := s.find(id)
	switch request.Opcode {
	case tcpapi.OpOpen, tcpapi.OpOpenConnect:
		free := s.free()
		if free < 0 {
			reply.Result, reply.Endpoint = tcpapi.ResultBusy, 0
			break
		}
		opened, result := stack.TCPOpen(s.owner)
		if result != network.ResultOK {
			reply.Result, reply.Endpoint = Result(result), 0
			break
		}
		e := &s.endpoints[free]
		*e = endpoint4{id: opened, state: endpointOpen, txHead: -1, txTail: -1}
		reply.Endpoint, reply.Slot = uint32(opened), uint8(free)
		if request.Opcode == tcpapi.OpOpen {
			break
		}
		tuple, result := stack.TCPConnect(s.owner, opened, network.IPv4(request.Address), request.Port, now)
		if result != network.ResultOK {
			stack.TCPClose(s.owner, opened, now)
			s.forget(free)
			reply.Result, reply.Endpoint, reply.Slot = Result(result), 0, 0
			break
		}
		if !noWait {
			e.connect, e.openConnect = correlation, true
			return s.deliverConnect(stack, free, out)
		}
		reply.Result = tcpapi.ResultInProgress
		reply.Address, reply.Port, reply.Local, reply.LocalPort = tuple.Remote, tuple.RemotePort, tuple.Local, tuple.LocalPort
	case tcpapi.OpBind:
		if held == nil {
			reply.Result = tcpapi.ResultInvalidArgument
			break
		}
		address, port, result := stack.TCPBindReuse(s.owner, id, network.IPv4(request.Address), request.Port, request.Flags&tcpapi.FlagReuseAddr != 0)
		reply.Result = Result(result)
		if result == network.ResultOK {
			reply.Address, reply.Port = address, port
		}
	case tcpapi.OpListen:
		if held == nil {
			reply.Result = tcpapi.ResultInvalidArgument
			break
		}
		reply.Result = Result(stack.TCPListen(s.owner, id, int(request.Backlog)))
	case tcpapi.OpAccept:
		if held == nil || held.accept != 0 {
			reply.Result = tcpapi.ResultBusy
			break
		}
		held.accept = correlation
		if fault := s.deliverAccept(stack, index, out); fault != FaultNone || !noWait || held.accept == 0 {
			return fault
		}
		held.accept = 0
		reply.Result = tcpapi.ResultWouldBlock
	case tcpapi.OpAcceptAhead:
		if ready, result := stack.TCPReadiness(s.owner, id); held == nil || result != network.ResultOK || ready.Mask&network.TCPReadyListening == 0 {
			reply.Result = tcpapi.ResultInvalidArgument
			break
		}
		held.ahead = request.Backlog
	case tcpapi.OpConnect:
		if held == nil || held.connect != 0 {
			reply.Result = tcpapi.ResultBusy
			break
		}
		tuple, result := stack.TCPConnect(s.owner, id, network.IPv4(request.Address), request.Port, now)
		if result != network.ResultOK {
			reply.Result = Result(result)
			break
		}
		held.connect, held.openConnect = correlation, false
		if fault := s.deliverConnect(stack, index, out); fault != FaultNone || !noWait || held.connect == 0 {
			return fault
		}
		held.connect = 0
		reply.Result = tcpapi.ResultInProgress
		reply.Address, reply.Port, reply.Local, reply.LocalPort = tuple.Remote, tuple.RemotePort, tuple.Local, tuple.LocalPort
	case tcpapi.OpShutdownWrite:
		switch {
		case held == nil:
			reply.Result = tcpapi.ResultInvalidArgument
		case held.txHead >= 0:
			// The queued slots go first; the FIN follows them.
			held.shutdown = true
		default:
			reply.Result = Result(stack.TCPShutdownWrite(s.owner, id, now))
		}
	case tcpapi.OpReady:
		if held == nil {
			reply.Result = tcpapi.ResultInvalidArgument
			break
		}
		ready, result := stack.TCPReadiness(s.owner, id)
		reply.Result = Result(result)
		if result == network.ResultOK {
			reply.Mask, reply.Value, reply.Room = uint16(ready.Mask), ready.Readable, ready.Writable
		}
	case tcpapi.OpWaitReady:
		if held == nil {
			reply.Result = tcpapi.ResultInvalidArgument
			break
		}
		if held.ready != 0 {
			reply.Result = tcpapi.ResultBusy
			break
		}
		held.ready, held.readyMask = correlation, request.Mask
		return s.deliverReady(stack, index, out)
	case tcpapi.OpError:
		if held == nil {
			reply.Result = tcpapi.ResultInvalidArgument
			break
		}
		pending, result := stack.TCPTakeError(s.owner, id)
		reply.Result = Result(result)
		if result == network.ResultOK {
			reply.Error = Result(pending)
		}
	case tcpapi.OpOption:
		if held == nil {
			reply.Result = tcpapi.ResultInvalidArgument
			break
		}
		options, result := stack.TCPOptions(s.owner, id)
		reply.Result = Result(result)
		if result == network.ResultOK {
			reply.Value = optionValue(&options, request.Option)
		}
	case tcpapi.OpClose:
		if held == nil {
			reply.Result = tcpapi.ResultInvalidArgument
			break
		}
		if fault := s.cancel(held, out); fault != FaultNone {
			return fault
		}
		// The endpoint is netd's now. Its queued slots still go out;
		// Deliver closes the connection after them.
		held.state, held.credit, held.ahead = endpointClosing, 0, 0
	default:
		reply.Result = tcpapi.ResultInvalidArgument
	}
	if !out.Reply(reply, correlation) {
		return FaultUnreachable
	}
	return FaultNone
}

// Deliver runs the data plane and answers the parked channel requests, in
// passes until one does nothing, at most maxDeliverPasses. more reports
// that the last pass still did something.
func (s *Session4) Deliver(stack *network.Stack, now uint64, out Notifier) (fault Fault, more bool) {
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
	s.showReadiness(stack)
	if s.published && s.control.ServiceShouldRing() {
		s.Counters.Doorbells++
		if !out.Doorbell() {
			return FaultUnreachable, false
		}
	}
	return FaultNone, more
}

// Progressed reports whether the last Deliver call did anything.
func (s *Session4) Progressed() bool { return s.progressed }

func (s *Session4) pass(stack *network.Stack, now uint64, out Notifier) Fault {
	if fault := s.takeRings(stack, now); fault != FaultNone {
		return fault
	}
	for index := range s.endpoints {
		if fault := s.deliverConnect(stack, index, out); fault != FaultNone {
			return fault
		}
		if fault := s.deliverAccept(stack, index, out); fault != FaultNone {
			return fault
		}
		s.acceptAhead(stack, index)
		s.send(stack, index, now)
		if fault := s.deliverReady(stack, index, out); fault != FaultNone {
			return fault
		}
	}
	s.fill(stack, now)
	// A closed endpoint's slots go back before its EntryClosed.
	s.flushDone()
	s.finishCloses(stack, now)
	return FaultNone
}

// takeRings consumes the TX and Release rings.
func (s *Session4) takeRings(stack *network.Stack, now uint64) Fault {
	var e tcpapi.Entry
	for {
		status := s.control.PopTX(&e)
		if status == shmring.Broken {
			return FaultProtocol
		}
		if status != shmring.OK {
			break
		}
		s.worked++
		s.Counters.TXEntries++
		if !s.queue(&e) {
			return FaultProtocol
		}
	}
	for {
		status := s.control.PopRelease(&e)
		if status == shmring.Broken {
			return FaultProtocol
		}
		if status != shmring.OK {
			return FaultNone
		}
		s.worked++
		s.Counters.ReleaseEntries++
		if !s.released(stack, &e) {
			return FaultProtocol
		}
	}
}

// queue takes a TX slot from the client and queues it on its endpoint, or
// finishes it at once when the endpoint cannot take it.
func (s *Session4) queue(e *tcpapi.Entry) bool {
	slot := &s.txSlots[e.Slot]
	if slot.held || e.Generation != slot.generation+1 {
		return false
	}
	*slot = txSlot4{held: true, generation: e.Generation, index: e.Index, endpoint: e.Endpoint, length: e.Length, next: -1}
	target := &s.endpoints[e.Index]
	switch {
	case target.id != network.TCPEndpointID(e.Endpoint) || target.state == endpointFree:
		s.finishTX(e.Slot, tcpapi.ResultInvalidArgument)
	case target.state != endpointOpen || target.shutdown:
		s.finishTX(e.Slot, tcpapi.ResultClosed)
	case e.Offset != target.queued:
		s.finishTX(e.Slot, tcpapi.ResultInvalidArgument)
	default:
		target.queued += e.Length
		if target.txTail >= 0 {
			s.txSlots[target.txTail].next = int8(e.Slot)
		} else {
			target.txHead = int8(e.Slot)
		}
		target.txTail = int8(e.Slot)
	}
	return true
}

func (s *Session4) released(stack *network.Stack, e *tcpapi.Entry) bool {
	switch e.Kind {
	case tcpapi.EntryRelease:
		bit := uint32(1) << e.Slot
		if s.rxLent&bit == 0 || s.rxGen[e.Slot] != e.Generation {
			return false
		}
		s.rxLent &^= bit
	case tcpapi.EntryCredit:
		target := &s.endpoints[e.Index]
		if target.id != network.TCPEndpointID(e.Endpoint) || target.state != endpointOpen {
			// The client closed the endpoint after it posted the credit.
			return true
		}
		if target.credit+e.Length > tcpapi.MaxCredit4 {
			return false
		}
		target.credit += e.Length
	case tcpapi.EntryForget:
		target := &s.endpoints[e.Index]
		if target.id != network.TCPEndpointID(e.Endpoint) || target.state != endpointClosed || !target.announced {
			return false
		}
		s.forget(int(e.Index))
	}
	return true
}

func (s *Session4) forget(index int) {
	s.endpoints[index] = endpoint4{txHead: -1, txTail: -1}
	s.control.StoreReadiness(index, &tcpapi.Readiness{})
}

// finishTX marks a held slot finished; flushDone hands it back.
func (s *Session4) finishTX(slot uint8, result tcpapi.Result) {
	s.txSlots[slot].result = result
	s.txDone |= 1 << slot
	s.worked++
}

func (s *Session4) flushDone() {
	for slot := uint8(0); s.txDone != 0 && slot < tcpapi.TXSlots4; slot++ {
		if s.txDone&(1<<slot) == 0 {
			continue
		}
		t := &s.txSlots[slot]
		e := tcpapi.Entry{Kind: tcpapi.EntryWriteDone, Slot: slot, Index: t.index, Endpoint: t.endpoint, Generation: t.generation,
			Result: t.result, Length: t.taken}
		if s.control.PushTXDone(&e) != shmring.OK {
			return
		}
		s.txDone &^= 1 << slot
		t.held = false
		s.Counters.WriteDone++
		s.published = true
	}
}

// send offers the endpoint's queued TX slots to the stack in order.
func (s *Session4) send(stack *network.Stack, index int, now uint64) {
	target := &s.endpoints[index]
	if target.state != endpointOpen && target.state != endpointClosing {
		return
	}
	if target.txHead >= 0 {
		// Slots queued while the handshake runs wait for it.
		if ready, result := stack.TCPReadiness(s.owner, target.id); result == network.ResultOK && ready.Mask&network.TCPReadyConnecting != 0 {
			return
		}
	}
	for target.txHead >= 0 {
		slot := uint8(target.txHead)
		t := &s.txSlots[slot]
		if t.taken < t.length {
			base := tcpapi.Layout4.Offset(slot)
			written, result := stack.TCPWrite(s.owner, target.id, s.tx[base+uint64(t.taken):base+uint64(t.length)], now)
			t.taken += uint32(written)
			if written != 0 {
				s.worked++
			}
			if result == network.ResultBusy || result == network.ResultOK && t.taken < t.length {
				return
			}
			if result != network.ResultOK {
				s.failQueue(stack, index, result)
				return
			}
		}
		target.txHead = t.next
		if target.txHead < 0 {
			target.txTail = -1
		}
		s.finishTX(slot, tcpapi.ResultOK)
	}
	if target.shutdown {
		target.shutdown = false
		stack.TCPShutdownWrite(s.owner, target.id, now)
		s.worked++
	}
}

// failQueue finishes every queued slot of the endpoint with result: the
// head with the bytes it got in, the rest with none.
func (s *Session4) failQueue(stack *network.Stack, index int, result network.Result) {
	target := &s.endpoints[index]
	s.reportedID(stack, target.id, result)
	for target.txHead >= 0 {
		slot := uint8(target.txHead)
		target.txHead = s.txSlots[slot].next
		s.finishTX(slot, Result(result))
	}
	target.txTail, target.shutdown = -1, false
}

func (s *Session4) reportedID(stack *network.Stack, id network.TCPEndpointID, result network.Result) {
	switch result {
	case network.ResultOK, network.ResultBusy, network.ResultEOF, network.ResultClosed, network.ResultInvalidArgument:
		return
	}
	stack.TCPTakeError(s.owner, id)
}

// fill copies arrived bytes into free RX slots for endpoints with credit,
// starting at the rotor.
func (s *Session4) fill(stack *network.Stack, now uint64) {
	for step := range tcpapi.Endpoints4 {
		index := (s.rotor + step) % tcpapi.Endpoints4
		target := &s.endpoints[index]
		for target.state == endpointOpen && target.credit != 0 {
			slot, free := s.freeRX()
			if !free || s.control.RXRoom() == 0 {
				s.rotor = index
				return
			}
			base := tcpapi.Layout4.Offset(slot)
			read, result := stack.TCPRead(s.owner, target.id, s.rx[base:base+tcpapi.SlotSize4], now)
			if result == network.ResultBusy || result == network.ResultOK && read == 0 {
				break
			}
			e := tcpapi.Entry{Index: uint8(index), Endpoint: uint32(target.id)}
			if result == network.ResultOK {
				s.rxGen[slot]++
				e.Kind, e.Slot, e.Length, e.Generation = tcpapi.EntryData, slot, uint32(read), s.rxGen[slot]
				s.Counters.Data++
			} else {
				s.reportedID(stack, target.id, result)
				e.Kind, e.Result = tcpapi.EntryEnd, Result(result)
				s.Counters.Ends++
			}
			if s.control.PushRX(&e) != shmring.OK {
				// RXRoom said there was room, so the client broke the head;
				// the bytes stay lost with the session it ends.
				return
			}
			if e.Kind == tcpapi.EntryData {
				s.rxLent |= 1 << slot
			}
			target.credit--
			s.worked++
			s.published = true
		}
	}
	s.rotor = (s.rotor + 1) % tcpapi.Endpoints4
}

func (s *Session4) freeRX() (uint8, bool) {
	for slot := uint8(0); slot < tcpapi.RXSlots4; slot++ {
		if s.rxLent&(1<<slot) == 0 {
			return slot, true
		}
	}
	return 0, false
}

// acceptAhead takes established children of a listener the client asked to
// accept ahead and publishes each on the RX ring.
func (s *Session4) acceptAhead(stack *network.Stack, index int) {
	listener := &s.endpoints[index]
	for listener.state == endpointOpen && listener.ahead != 0 && listener.accept == 0 {
		free := s.free()
		if free < 0 || s.control.RXRoom() == 0 {
			return
		}
		child, tuple, result := stack.TCPAccept(s.owner, listener.id)
		if result == network.ResultBusy {
			return
		}
		if result != network.ResultOK {
			listener.ahead = 0
			return
		}
		s.endpoints[free] = endpoint4{id: child, state: endpointOpen, txHead: -1, txTail: -1}
		e := tcpapi.Entry{Kind: tcpapi.EntryAccepted, Index: uint8(free), Endpoint: uint32(child), Slot: uint8(index),
			Generation: uint32(listener.id), Address: tuple.Remote, Port: tuple.RemotePort, Local: tuple.Local, LocalPort: tuple.LocalPort}
		if s.control.PushRX(&e) != shmring.OK {
			stack.TCPClose(s.owner, child, 0)
			s.forget(free)
			return
		}
		listener.ahead--
		s.Counters.Accepted++
		s.worked++
		s.published = true
	}
}

// finishCloses closes the connections of closed endpoints whose queues
// drained, and announces each closed endpoint once on the TXDone ring.
func (s *Session4) finishCloses(stack *network.Stack, now uint64) {
	for index := range s.endpoints {
		target := &s.endpoints[index]
		if target.state == endpointClosing && target.txHead < 0 {
			stack.TCPClose(s.owner, target.id, now)
			target.state = endpointClosed
			s.worked++
		}
		if target.state != endpointClosed || target.announced || s.control.TXDoneRoom() == 0 || s.finishing(index) {
			continue
		}
		e := tcpapi.Entry{Kind: tcpapi.EntryClosed, Index: uint8(index), Endpoint: uint32(target.id)}
		if s.control.PushTXDone(&e) != shmring.OK {
			return
		}
		target.announced = true
		s.Counters.Closed++
		s.worked++
		s.published = true
	}
}

// finishing reports whether a finished TX slot of the endpoint still waits
// for room on the TXDone ring.
func (s *Session4) finishing(index int) bool {
	for slot := range s.txSlots {
		if s.txDone&(1<<slot) != 0 && int(s.txSlots[slot].index) == index {
			return true
		}
	}
	return false
}

// showReadiness rewrites the readiness entries that changed.
func (s *Session4) showReadiness(stack *network.Stack) {
	for index := range s.endpoints {
		target := &s.endpoints[index]
		if target.state == endpointFree {
			continue
		}
		shown := tcpapi.Readiness{Endpoint: uint32(target.id), Credit: target.credit}
		if target.state == endpointOpen {
			if ready, result := stack.TCPReadiness(s.owner, target.id); result == network.ResultOK {
				shown.Mask, shown.Readable, shown.Room = uint16(ready.Mask), ready.Readable, ready.Writable
			}
		}
		if shown != target.shown {
			target.shown = shown
			s.control.StoreReadiness(index, &shown)
		}
	}
}

func (s *Session4) deliverConnect(stack *network.Stack, index int, out Notifier) Fault {
	target := &s.endpoints[index]
	if target.connect == 0 {
		return FaultNone
	}
	tuple, result, done := stack.TCPConnectResult(s.owner, target.id)
	if !done {
		return FaultNone
	}
	reply := tcpapi.Reply{Opcode: tcpapi.OpConnect, Result: Result(result), Endpoint: uint32(target.id)}
	if target.openConnect {
		reply.Opcode = tcpapi.OpOpenConnect
		reply.Slot = uint8(index)
	}
	if result == network.ResultOK {
		reply.Address, reply.Port, reply.Local, reply.LocalPort = tuple.Remote, tuple.RemotePort, tuple.Local, tuple.LocalPort
	}
	s.reportedID(stack, target.id, result)
	correlation := target.connect
	target.connect = 0
	if target.openConnect && result != network.ResultOK {
		// The client never saw this endpoint; it goes with the failure.
		stack.TCPClose(s.owner, target.id, 0)
		s.forget(index)
		reply.Endpoint, reply.Slot = 0, 0
	}
	s.worked++
	if !out.Reply(&reply, correlation) {
		return FaultUnreachable
	}
	return FaultNone
}

func (s *Session4) deliverAccept(stack *network.Stack, index int, out Notifier) Fault {
	listener := &s.endpoints[index]
	if listener.accept == 0 {
		return FaultNone
	}
	free := s.free()
	if free < 0 {
		return FaultNone
	}
	child, tuple, result := stack.TCPAccept(s.owner, listener.id)
	if result == network.ResultBusy {
		return FaultNone
	}
	reply := tcpapi.Reply{Opcode: tcpapi.OpAccept, Result: Result(result), Endpoint: uint32(listener.id)}
	if result == network.ResultOK {
		s.endpoints[free] = endpoint4{id: child, state: endpointOpen, txHead: -1, txTail: -1}
		reply.Endpoint, reply.Slot = uint32(child), uint8(free)
		reply.Address, reply.Port, reply.Local, reply.LocalPort = tuple.Remote, tuple.RemotePort, tuple.Local, tuple.LocalPort
	}
	correlation := listener.accept
	listener.accept = 0
	s.worked++
	if !out.Reply(&reply, correlation) {
		return FaultUnreachable
	}
	return FaultNone
}

func (s *Session4) deliverReady(stack *network.Stack, index int, out Notifier) Fault {
	target := &s.endpoints[index]
	if target.ready == 0 {
		return FaultNone
	}
	ready, result := stack.TCPReadiness(s.owner, target.id)
	if result == network.ResultOK && uint16(ready.Mask) == target.readyMask {
		return FaultNone
	}
	reply := tcpapi.Reply{Opcode: tcpapi.OpWaitReady, Result: Result(result), Endpoint: uint32(target.id)}
	if result == network.ResultOK {
		reply.Mask, reply.Value, reply.Room = uint16(ready.Mask), ready.Readable, ready.Writable
	}
	correlation := target.ready
	target.ready, target.readyMask = 0, 0
	s.worked++
	if !out.Reply(&reply, correlation) {
		return FaultUnreachable
	}
	return FaultNone
}

// cancel answers the endpoint's parked channel requests with ResultClosed.
func (s *Session4) cancel(target *endpoint4, out Notifier) Fault {
	connect := tcpapi.OpConnect
	if target.openConnect {
		connect = tcpapi.OpOpenConnect
	}
	for _, pending := range [...]struct {
		opcode      tcpapi.Opcode
		correlation uint64
	}{{tcpapi.OpWaitReady, target.ready}, {tcpapi.OpAccept, target.accept}, {connect, target.connect}} {
		if pending.correlation == 0 {
			continue
		}
		reply := tcpapi.Reply{Opcode: pending.opcode, Result: tcpapi.ResultClosed, Endpoint: uint32(target.id)}
		if pending.opcode == tcpapi.OpOpenConnect {
			reply.Endpoint = 0
		}
		if !out.Reply(&reply, pending.correlation) {
			return FaultUnreachable
		}
	}
	target.ready, target.accept, target.connect = 0, 0, 0
	return FaultNone
}

// find returns the open endpoint id names and its table index.
func (s *Session4) find(id network.TCPEndpointID) (*endpoint4, int) {
	if id == 0 {
		return nil, -1
	}
	for index := range s.endpoints {
		if s.endpoints[index].id == id && s.endpoints[index].state == endpointOpen {
			return &s.endpoints[index], index
		}
	}
	return nil, -1
}

func (s *Session4) free() int {
	for index := range s.endpoints {
		if s.endpoints[index].state == endpointFree {
			return index
		}
	}
	return -1
}

// Runnable names work a Deliver pass would do now, with the endpoint it
// concerns. It changes nothing.
func (s *Session4) Runnable(stack *network.Stack) (endpoint uint32, op string, found bool) {
	if s.control.ServicePending() {
		return 0, "rings", true
	}
	if s.txDone != 0 && s.control.TXDoneRoom() != 0 {
		return 0, "write-done", true
	}
	_, rxFree := s.freeRX()
	rxRoom := s.control.RXRoom() != 0
	for index := range s.endpoints {
		target := &s.endpoints[index]
		if target.state == endpointFree {
			continue
		}
		if target.state == endpointClosing && target.txHead < 0 || target.state == endpointClosed && !target.announced && s.control.TXDoneRoom() != 0 && !s.finishing(index) {
			return uint32(target.id), "close", true
		}
		ready, result := stack.TCPReadiness(s.owner, target.id)
		if target.connect != 0 {
			if _, _, done := stack.TCPConnectResult(s.owner, target.id); done {
				return uint32(target.id), "connect", true
			}
		}
		acceptable := result != network.ResultOK || ready.Mask&network.TCPReadyAcceptable != 0
		if target.accept != 0 && acceptable && s.free() >= 0 {
			return uint32(target.id), "accept", true
		}
		if target.ahead != 0 && target.accept == 0 && result == network.ResultOK && ready.Mask&network.TCPReadyAcceptable != 0 &&
			s.free() >= 0 && rxRoom {
			return uint32(target.id), "accept-ahead", true
		}
		if target.txHead >= 0 && (result != network.ResultOK || ready.Mask&network.TCPReadyConnecting == 0 &&
			ready.Mask&(network.TCPReadyWritable|network.TCPReadyWriteClosed|network.TCPReadyError|network.TCPReadyHangUp) != 0) {
			return uint32(target.id), "write", true
		}
		if target.state == endpointOpen && target.credit != 0 && rxFree && rxRoom &&
			(result != network.ResultOK || ready.Mask&(network.TCPReadyReadable|network.TCPReadyReadEOF|network.TCPReadyError|network.TCPReadyHangUp) != 0) {
			return uint32(target.id), "read", true
		}
		if target.ready != 0 && (result != network.ResultOK || uint16(ready.Mask) != target.readyMask) {
			return uint32(target.id), "wait-ready", true
		}
	}
	return 0, "", false
}

// EndpointSnapshot4 is one table entry, for diagnostics.
type EndpointSnapshot4 struct {
	ID                  uint32
	State               uint8
	Credit, Queued      uint32
	Ahead               uint8
	TXQueued, Announced bool
}

// Snapshot reports table entry index.
func (s *Session4) Snapshot(index int) EndpointSnapshot4 {
	e := &s.endpoints[index]
	return EndpointSnapshot4{ID: uint32(e.id), State: uint8(e.state), Credit: e.credit, Queued: e.queued, Ahead: e.ahead,
		TXQueued: e.txHead >= 0, Announced: e.announced}
}
