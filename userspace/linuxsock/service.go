// Package linuxsock is the core of linuxsockd (ADR 0210): Linux socket
// objects for the kernel's family sessions, kept over native TCP and UDP
// endpoints in netd.
//
// The core runs in one event loop and never waits. Every GLNXSOCK1 request
// becomes an operation that either answers at once from local state or
// issues one native request that netd completes at once (NoWait), sometimes
// two in a row (an unbound listen binds first). An operation may wait in
// the service only for a resource another prompt exchange frees: a native
// pool slot, a family RX slot the kernel is about to release, or room in the
// family's channel for its reply.
//
// Readiness is learned from one NativeWaitReady held per endpoint. Each
// change is published in the family's readiness page; a ReadyChanged
// notification follows when no other is outstanding, and one that finds
// the queue full is sent again from Flush.
//
// Everything is bounded and static: MaxFamilies families, MaxSockets sockets
// in all and linuxsockapi.MaxSockets per family, FamilyOps requests per
// family in the service at once.
package linuxsock

import (
	"goos/kernel/ipc"
	"goos/userspace/linuxsockapi"
)

const (
	MaxFamilies = 16
	MaxSockets  = 64
	// FamilyOps bounds a family's requests in the service. The event loop
	// stops reading a family's channel while it has that many.
	FamilyOps = 16
	// FamilyParked bounds a family's reads that wait for a Release; a read
	// past it answers WouldBlock. With FamilyOps twice as large, half of a
	// family's operations always make progress, so the Releases behind them
	// are always read.
	FamilyParked = linuxsockapi.RXSlots
	// MaxOps holds every family's requests and two internal operations per
	// socket, a pending-error fetch and a readable-count refresh.
	MaxOps = MaxFamilies*FamilyOps + 2*MaxSockets
	// MaxBacklog is the native listen backlog limit; Linux backlogs are
	// clamped to it.
	MaxBacklog = 8
	// closeSlots holds native closes waiting for room on their session: at
	// most every endpoint of both protocols.
	closeSlots = 2 * MaxSockets
)

type sockState uint8

const (
	stateUnbound sockState = iota
	stateBound
	stateListening
	stateConnecting
	stateConnected
	// stateFailed: a stream connect failed after InProgress.
	stateFailed
)

type socket struct {
	used    bool
	stream  bool
	opening bool // its Open or Accept has not completed
	// dead: the native session the socket lived on is gone.
	dead      bool
	readShut  bool
	writeShut bool
	waiting   bool // a NativeWaitReady is held
	needWait  bool // the wait could not be sent and is retried
	noWait    bool // the backend cannot wait; readiness comes from replies only
	fetching  bool
	// refreshing: a kindRefresh operation is out.
	refreshing bool
	// reopen: a connect failed at once and the native endpoint is spent; the
	// next connect replaces it.
	reopen bool
	// connectDone: a nonblocking connect finished and no connect has
	// reported it yet.
	connectDone bool
	reuse       bool
	// lentPeek and lentRetained describe the lent slot: data a native peek
	// left in netd, or bytes the socket had retained.
	lentPeek     bool
	lentRetained bool
	family       int8
	index        uint8
	state        sockState
	proto        Proto
	// lent is the family RX slot a stream read lent to the kernel, retained
	// the slot holding bytes a Release returned unread; -1 for none.
	lent           int8
	retained       int8
	retainedLength uint16
	lentLength     uint16
	// tail counts bytes of the lent slot past lentLength: retained bytes a
	// short read did not hand over.
	tail       uint16
	endpoint   uint32
	serial     uint32
	generation uint64
	id         uint64
	pending    linuxsockapi.Result
	mask       NativeMask
	readable   uint32
	local      [4]byte
	peer       [4]byte
	localPort  uint16
	peerPort   uint16
	backlog    uint32
	published  linuxsockapi.Readiness
}

type family struct {
	used     bool
	notify   bool
	token    uint64
	session  uint32
	pool     []byte
	count    int
	inflight int
	parked   int
	replies  int
	// slots maps a family socket index to a global socket, plus one.
	slots       [linuxsockapi.MaxSockets]int16
	generations [linuxsockapi.MaxSockets]uint32
	rxUsed      uint8
	rxLent      uint8
	txHeld      uint8
	rxOwner     [linuxsockapi.RXSlots]int16
	rxSerial    [linuxsockapi.RXSlots]uint32
}

type opState uint8

const (
	opFree opState = iota
	// opSubmit: waiting to issue its next native request.
	opSubmit
	// opNative: a native request is outstanding.
	opNative
	// opReply: the reply waits for room in the family's channel.
	opReply
)

type opKind uint8

const (
	kindRequest opKind = iota
	kindError
	// kindRefresh reads a stream's readable count again once reads used
	// up the count netd last reported while its Readable bit stays set:
	// netd answers a held wait only when the mask changes.
	kindRefresh
)

type op struct {
	state  opState
	kind   opKind
	orphan bool // the family is gone; the outcome goes nowhere
	parked bool
	family int8
	stage  uint8
	rx     int8
	native NativeOp
	proto  Proto
	socket int16
	child  int16
	serial uint32
	// socketSerial is the serial of socket when the operation took it.
	socketSerial uint32
	childSerial  uint32
	generation   uint64
	correlation  uint64
	request      linuxsockapi.Request
	reply        linuxsockapi.Reply
}

type backendState struct {
	up         bool
	generation uint64
}

type pendingClose struct {
	used       bool
	proto      Proto
	endpoint   uint32
	generation uint64
}

// Native tags: the kind in the top byte, a table index, and a serial.
const (
	tagOp    = 1
	tagWait  = 2
	tagClose = 3
)

func makeTag(kind, index int, serial uint32) uint64 {
	return uint64(kind)<<56 | uint64(uint16(index))<<32 | uint64(serial)
}

func splitTag(tag uint64) (int, int, uint32) {
	return int(tag >> 56), int(uint16(tag >> 32)), uint32(tag)
}

// Service is the whole core. The zero value is unusable; call Init.
type Service struct {
	backend   Backend
	transport Transport
	families  [MaxFamilies]family
	sockets   [MaxSockets]socket
	ops       [MaxOps]op
	backends  [UDP + 1]backendState
	closes    [closeSlots]pendingClose
	serial    uint32
	// Work the pump retries; zero skips the scan.
	submits, waits, closing int
	counters                linuxsockapi.Counters
	message                 ipc.Message
	request                 linuxsockapi.Request
	native                  NativeRequest
}

func (s *Service) Init(backend Backend, transport Transport) {
	s.backend, s.transport = backend, transport
}

func (s *Service) nextSerial() uint32 {
	s.serial++
	if s.serial == 0 {
		s.serial++
	}
	return s.serial
}

// --- families ----------------------------------------------------------------

// AddFamily takes a family session's pool and returns the family's index,
// which the transport and the event loop use to name its channel.
func (s *Service) AddFamily(token uint64, session uint32, pool []byte) (int, linuxsockapi.ControlResult) {
	if token == 0 || session == 0 || len(pool) < linuxsockapi.PoolBytes {
		return -1, linuxsockapi.ControlInvalidArgument
	}
	free := -1
	for index := range s.families {
		f := &s.families[index]
		if f.used && (f.token == token || f.session == session) {
			return -1, linuxsockapi.ControlInvalidArgument
		}
		if !f.used && free < 0 {
			free = index
		}
	}
	if free < 0 {
		return -1, linuxsockapi.ControlBusy
	}
	f := &s.families[free]
	f.used, f.notify, f.token, f.session, f.pool = true, false, token, session, pool[:linuxsockapi.PoolBytes]
	f.count, f.inflight, f.parked, f.replies = 0, 0, 0, 0
	f.rxUsed, f.rxLent, f.txHeld = 0, 0, 0
	for index := range f.slots {
		f.slots[index] = 0
	}
	linuxsockapi.InitReadiness(f.pool, session)
	return free, linuxsockapi.ControlOK
}

// FamilyByToken finds the family PID 1 added under token.
func (s *Service) FamilyByToken(token uint64) (int, bool) {
	for index := range s.families {
		if s.families[index].used && s.families[index].token == token {
			return index, true
		}
	}
	return -1, false
}

// Family reports whether family index is in use.
func (s *Service) Family(index int) bool {
	return index >= 0 && index < MaxFamilies && s.families[index].used
}

// RemoveFamily closes every socket of a family and every native endpoint
// they held, forgets its requests and has the transport drop the session.
func (s *Service) RemoveFamily(index int) {
	if !s.Family(index) {
		return
	}
	f := &s.families[index]
	for slot := range f.slots {
		if g := f.slots[slot]; g != 0 {
			s.releaseSocket(int(g-1), false)
		}
	}
	for index2 := range s.ops {
		o := &s.ops[index2]
		if o.state == opFree || int(o.family) != index {
			continue
		}
		if o.state == opNative {
			o.orphan = true
			continue
		}
		o.orphan = true
		s.freeOp(o)
	}
	f.used, f.pool = false, nil
	s.transport.Drop(index)
}

// failFamily ends a session that sent a malformed message.
func (s *Service) failFamily(index int) {
	s.counters.SessionFailures++
	s.RemoveFamily(index)
}

// CanReceive reports whether the event loop may read family index's
// channel: the family has room for another request.
func (s *Service) CanReceive(index int) bool {
	return s.Family(index) && s.families[index].inflight < FamilyOps
}

// Pending reports whether family index has messages that found its channel
// full; the event loop waits for it to be writable and calls Flush.
func (s *Service) Pending(index int) bool {
	return s.Family(index) && (s.families[index].replies != 0 || s.families[index].notify)
}

// Flush sends what a full channel held back.
func (s *Service) Flush(index int) {
	if !s.Family(index) {
		return
	}
	for i := range s.ops {
		o := &s.ops[i]
		if o.state == opReply && int(o.family) == index && !s.sendReply(o) {
			return
		}
	}
	s.notify(index)
}

// Receive handles one message read from family index's channel.
func (s *Service) Receive(index int, message *ipc.Message) {
	if !s.Family(index) {
		return
	}
	correlation, ok := linuxsockapi.DecodeRequest(message, &s.request)
	if !ok || s.request.Opcode == linuxsockapi.OpReadyChanged {
		s.failFamily(index)
		return
	}
	if s.request.Opcode == linuxsockapi.OpRelease {
		s.release(index)
		s.pump()
		return
	}
	o := s.newOp(index, kindRequest)
	if o == nil {
		// The event loop reads only while CanReceive holds.
		s.failFamily(index)
		return
	}
	o.correlation, o.request = correlation, s.request
	o.reply = linuxsockapi.Reply{Opcode: s.request.Opcode, Socket: s.request.Socket}
	if !s.take(o) {
		return
	}
	s.step(o)
	s.pump()
}

// take records what the request lends and the socket it names. It reports
// false when the request ended the family or was answered.
func (s *Service) take(o *op) bool {
	f := &s.families[o.family]
	r := &o.request
	if r.Opcode == linuxsockapi.OpWrite || r.Opcode == linuxsockapi.OpSendTo {
		bit := uint8(1) << r.Slot
		if f.txHeld&bit != 0 {
			s.failFamily(int(o.family))
			return false
		}
		f.txHeld |= bit
		o.reply.Slot = r.Slot
	}
	switch r.Opcode {
	case linuxsockapi.OpOpen:
		o.reply.Socket = 0
		return true
	case linuxsockapi.OpCancel:
		s.finish(o, linuxsockapi.ResultOK)
		return false
	}
	index, result := s.lookup(int(o.family), r.Socket)
	if result != linuxsockapi.ResultOK {
		s.finish(o, result)
		return false
	}
	o.socket, o.socketSerial = int16(index), s.sockets[index].serial
	return true
}

// lookup finds a family's socket by identity: another session's identity,
// a free slot or an older generation is stale.
func (s *Service) lookup(index int, id uint64) (int, linuxsockapi.Result) {
	f := &s.families[index]
	slot, ok := linuxsockapi.SocketIndex(id)
	if !ok || linuxsockapi.SessionOf(id) != f.session || f.slots[slot] == 0 {
		return -1, linuxsockapi.ResultStale
	}
	global := int(f.slots[slot] - 1)
	if sock := &s.sockets[global]; sock.id != id || sock.opening {
		return -1, linuxsockapi.ResultStale
	}
	return global, linuxsockapi.ResultOK
}

// --- sockets -----------------------------------------------------------------

func (s *Service) allocSocket(index int, stream bool) int {
	f := &s.families[index]
	if f.count >= linuxsockapi.MaxSockets {
		return -1
	}
	global := -1
	for i := range s.sockets {
		if !s.sockets[i].used {
			global = i
			break
		}
	}
	slot := -1
	for i := range f.slots {
		if f.slots[i] == 0 {
			slot = i
			break
		}
	}
	if global < 0 || slot < 0 {
		return -1
	}
	generation := (f.generations[slot] + 1) & 0xffffff
	if generation == 0 {
		generation = 1
	}
	f.generations[slot] = generation
	proto := UDP
	if stream {
		proto = TCP
	}
	sock := &s.sockets[global]
	*sock = socket{used: true, stream: stream, opening: true, family: int8(index), index: uint8(slot), proto: proto,
		lent: -1, retained: -1, serial: s.nextSerial(), id: linuxsockapi.MakeSocket(f.session, generation, slot)}
	f.slots[slot] = int16(global + 1)
	f.count++
	return global
}

// adopt gives an opening socket its native endpoint and starts its wait.
func (s *Service) adopt(global int, endpoint uint32, generation uint64) {
	sock := &s.sockets[global]
	sock.opening, sock.endpoint, sock.generation = false, endpoint, generation
	s.arm(global)
	s.publish(global)
}

// releaseSocket frees a socket and closes its native endpoint. publish
// clears its readiness entry; a family being removed skips that.
func (s *Service) releaseSocket(global int, publish bool) {
	sock := &s.sockets[global]
	if !sock.used {
		return
	}
	f := &s.families[sock.family]
	if sock.endpoint != 0 && !sock.dead {
		s.queueClose(sock.proto, sock.generation, sock.endpoint)
	}
	if sock.retained >= 0 {
		s.freeRX(f, uint8(sock.retained))
	}
	if sock.needWait {
		s.waits--
	}
	if publish && !sock.opening {
		linuxsockapi.Publish(f.pool, int(sock.index), linuxsockapi.Readiness{})
	}
	f.slots[sock.index] = 0
	f.count--
	*sock = socket{}
}

func (s *Service) socketOf(o *op) *socket {
	if o.socket < 0 {
		return nil
	}
	sock := &s.sockets[o.socket]
	if !sock.used || sock.serial != o.socketSerial {
		return nil
	}
	return sock
}

// --- RX slots ------------------------------------------------------------------

func (s *Service) reserveRX(f *family) int8 {
	for slot := uint8(0); slot < linuxsockapi.RXSlots; slot++ {
		if f.rxUsed&(1<<slot) == 0 {
			f.rxUsed |= 1 << slot
			return int8(slot)
		}
	}
	return -1
}

func (s *Service) freeRX(f *family, slot uint8) {
	f.rxUsed &^= 1 << slot
	f.rxOwner[slot], f.rxSerial[slot] = 0, 0
}

func (s *Service) rxSlot(f *family, slot uint8) []byte {
	offset := linuxsockapi.RXOffset(slot)
	return f.pool[offset : offset+linuxsockapi.SlotSize]
}

func (s *Service) txSlot(f *family, slot uint8, length uint32) []byte {
	offset := linuxsockapi.TXOffset(slot)
	return f.pool[offset : offset+uint64(length)]
}

// lend hands a filled RX slot to the kernel with the reply about to go.
func (s *Service) lend(f *family, slot uint8, global int) {
	f.rxLent |= 1 << slot
	f.rxOwner[slot], f.rxSerial[slot] = int16(global+1), s.sockets[global].serial
}

// release takes back an RX slot. Unread bytes of a stream stay with the
// socket, moved to the start of the slot, and are the next read's first.
func (s *Service) release(index int) {
	f := &s.families[index]
	r := &s.request
	bit := uint8(1) << r.Slot
	if f.rxLent&bit == 0 {
		s.failFamily(index)
		return
	}
	f.rxLent &^= bit
	var sock *socket
	global := int(f.rxOwner[r.Slot]) - 1
	if global >= 0 {
		candidate := &s.sockets[global]
		if candidate.used && candidate.serial == f.rxSerial[r.Slot] {
			sock = candidate
		}
	}
	if sock != nil && sock.id != r.Socket {
		s.failFamily(index)
		return
	}
	if sock == nil || !sock.stream || sock.lent != int8(r.Slot) {
		s.freeRX(f, r.Slot)
		return
	}
	sock.lent = -1
	unread, offset := uint32(r.Length), uint32(r.Value)
	lent, tail := uint32(sock.lentLength), uint32(sock.tail)
	if offset+unread > lent || tail != 0 && unread != 0 && offset+unread != lent {
		s.failFamily(index)
		return
	}
	start, end := lent, lent+tail
	switch {
	case sock.lentRetained && sock.lentPeek:
		// A peek of retained bytes consumed nothing.
		start = 0
	case sock.lentPeek:
		// The bytes are still in netd.
		start, end = 0, 0
	case unread != 0:
		start = offset
	}
	if end == start {
		s.freeRX(f, r.Slot)
		s.publish(global)
		return
	}
	data := s.rxSlot(f, r.Slot)
	copy(data, data[start:end])
	sock.retained, sock.retainedLength = int8(r.Slot), uint16(end-start)
	sock.lentLength, sock.tail = 0, 0
	f.rxOwner[r.Slot], f.rxSerial[r.Slot] = 0, 0
	s.publish(global)
}

// --- operations ----------------------------------------------------------------

func (s *Service) newOp(index int, kind opKind) *op {
	if kind == kindRequest && s.families[index].inflight >= FamilyOps {
		return nil
	}
	for i := range s.ops {
		o := &s.ops[i]
		if o.state != opFree {
			continue
		}
		*o = op{state: opSubmit, kind: kind, family: int8(index), rx: -1, socket: -1, child: -1, serial: s.nextSerial()}
		s.submits++
		if kind == kindRequest {
			s.families[index].inflight++
		}
		return o
	}
	return nil
}

func (s *Service) opIndex(o *op) int {
	for i := range s.ops {
		if &s.ops[i] == o {
			return i
		}
	}
	return -1
}

func (s *Service) freeOp(o *op) {
	if o.state == opFree {
		return
	}
	if o.state == opSubmit {
		s.submits--
	}
	if !o.orphan {
		f := &s.families[o.family]
		if o.parked {
			f.parked--
		}
		if o.state == opReply {
			f.replies--
		}
		if o.rx >= 0 {
			s.freeRX(f, uint8(o.rx))
		}
		if o.kind == kindRequest {
			if r := &o.request; r.Opcode == linuxsockapi.OpWrite || r.Opcode == linuxsockapi.OpSendTo {
				f.txHeld &^= 1 << r.Slot
			}
			f.inflight--
		}
	}
	if o.child >= 0 {
		if child := &s.sockets[o.child]; child.used && child.serial == o.childSerial && child.opening {
			if o.orphan {
				*child = socket{}
			} else {
				s.releaseSocket(int(o.child), false)
			}
		}
	}
	if o.kind == kindError {
		if sock := s.socketOf(o); sock != nil {
			sock.fetching = false
		}
	}
	if o.kind == kindRefresh {
		if sock := s.socketOf(o); sock != nil {
			sock.refreshing = false
		}
	}
	o.state = opFree
}

// park keeps a read waiting for a Release. It reports false when the
// family already has FamilyParked of them.
func (s *Service) park(o *op) bool {
	if o.parked {
		return true
	}
	f := &s.families[o.family]
	if f.parked >= FamilyParked {
		return false
	}
	f.parked++
	o.parked = true
	return true
}

func (s *Service) unpark(o *op) {
	if o.parked {
		s.families[o.family].parked--
		o.parked = false
	}
}

// finish answers a request with result. A failed reply keeps only what the
// protocol allows for it.
func (s *Service) finish(o *op, result linuxsockapi.Result) {
	s.unpark(o)
	if o.state == opSubmit {
		s.submits--
	}
	if o.kind != kindRequest || o.orphan {
		o.state = opNative // freeOp below counts nothing for it
		s.freeOp(o)
		return
	}
	switch result {
	case linuxsockapi.ResultWouldBlock, linuxsockapi.ResultInProgress:
		s.counters.WouldBlock++
	}
	r := &o.request
	if result != linuxsockapi.ResultOK {
		socketID := r.Socket
		if r.Opcode == linuxsockapi.OpOpen || r.Opcode == linuxsockapi.OpCancel {
			socketID = 0
		}
		slot := uint8(0)
		if r.Opcode == linuxsockapi.OpWrite || r.Opcode == linuxsockapi.OpSendTo {
			slot = r.Slot
		}
		o.reply = linuxsockapi.Reply{Opcode: r.Opcode, Result: result, Socket: socketID, Slot: slot}
	}
	o.reply.Result = result
	o.state = opReply
	s.families[o.family].replies++
	s.sendReply(o)
}

func (s *Service) sendReply(o *op) bool {
	if !linuxsockapi.EncodeReply(&o.reply, o.correlation, &s.message) {
		// The core built a reply the protocol refuses: answer the request
		// with a bare failure rather than not at all.
		r := &o.request
		o.reply = linuxsockapi.Reply{Opcode: r.Opcode, Result: linuxsockapi.ResultInvalidArgument, Socket: r.Socket}
		if r.Opcode == linuxsockapi.OpOpen || r.Opcode == linuxsockapi.OpCancel {
			o.reply.Socket = 0
		}
		if r.Opcode == linuxsockapi.OpWrite || r.Opcode == linuxsockapi.OpSendTo {
			o.reply.Slot = r.Slot
		}
		if !linuxsockapi.EncodeReply(&o.reply, o.correlation, &s.message) {
			s.freeOp(o)
			return true
		}
	}
	if !s.transport.Send(int(o.family), &s.message) {
		return false
	}
	s.freeOp(o)
	return true
}

// submit issues the native request the operation's step built in s.native.
func (s *Service) submit(o *op, proto Proto) {
	if !s.backends[proto].up {
		s.finishNative(o, NativeLost)
		return
	}
	switch s.backend.Submit(proto, makeTag(tagOp, s.opIndex(o), o.serial), &s.native) {
	case SubmitAccepted:
		if o.state == opSubmit {
			s.submits--
		}
		o.state, o.native, o.proto, o.generation = opNative, s.native.Op, proto, s.backends[proto].generation
	case SubmitBusy:
		if o.state != opSubmit {
			o.state = opSubmit
			s.submits++
		}
	case SubmitDown:
		s.finishNative(o, NativeLost)
	case SubmitUnsupported:
		s.finishNative(o, NativeNotSupported)
	}
}

// wait keeps an operation in opSubmit for the pump to step again.
func (s *Service) wait(o *op) {
	if o.state != opSubmit {
		o.state = opSubmit
		s.submits++
	}
}

func (s *Service) finishNative(o *op, result NativeResult) {
	o.native = s.native.Op
	var c NativeCompletion
	c.Op, c.Result = s.native.Op, result
	if o.state == opSubmit {
		s.submits--
	}
	o.state = opNative
	s.done(o, &c)
}

// --- backends --------------------------------------------------------------------

// BackendUp installs a native session of proto under generation, which must
// be newer than any before it. A session it replaces is taken down first.
func (s *Service) BackendUp(proto Proto, generation uint64) linuxsockapi.ControlResult {
	if proto != TCP && proto != UDP {
		return linuxsockapi.ControlInvalidArgument
	}
	b := &s.backends[proto]
	if generation <= b.generation {
		return linuxsockapi.ControlStale
	}
	if b.up {
		s.BackendDown(proto, b.generation)
	}
	b.up, b.generation = true, generation
	return linuxsockapi.ControlOK
}

// BackendGeneration reports the live generation of proto, zero for none.
func (s *Service) BackendGeneration(proto Proto) uint64 {
	if proto != TCP && proto != UDP || !s.backends[proto].up {
		return 0
	}
	return s.backends[proto].generation
}

// BackendDown fails everything the native session of proto under
// generation backed: each socket on it reports ServiceLost as its pending
// error and readiness wakes every waiter; each request in flight on it is
// answered ServiceLost. Its endpoint identities are never used again.
func (s *Service) BackendDown(proto Proto, generation uint64) {
	if proto != TCP && proto != UDP {
		return
	}
	b := &s.backends[proto]
	if !b.up || b.generation != generation {
		return
	}
	b.up = false
	for i := range s.closes {
		if c := &s.closes[i]; c.used && c.proto == proto && c.generation == generation {
			c.used = false
			s.closing--
		}
	}
	for i := range s.sockets {
		sock := &s.sockets[i]
		if !sock.used || sock.opening || sock.dead || sock.proto != proto || sock.generation != generation {
			continue
		}
		sock.dead, sock.endpoint, sock.waiting = true, 0, false
		if sock.needWait {
			sock.needWait = false
			s.waits--
		}
		if sock.pending == linuxsockapi.ResultOK {
			sock.pending = linuxsockapi.ResultServiceLost
		}
		s.counters.BackendLosses++
		s.publish(i)
	}
	for i := range s.ops {
		o := &s.ops[i]
		if o.state == opNative && o.proto == proto && o.generation == generation {
			var c NativeCompletion
			c.Op, c.Result = o.native, NativeLost
			s.done(o, &c)
		}
	}
	s.pump()
}

// Complete takes the answer to the native request tagged tag from the
// session of proto under generation.
func (s *Service) Complete(proto Proto, generation uint64, tag uint64, c *NativeCompletion) {
	if proto != TCP && proto != UDP || !s.backends[proto].up || s.backends[proto].generation != generation {
		return
	}
	kind, index, serial := splitTag(tag)
	switch kind {
	case tagOp:
		if index < len(s.ops) {
			o := &s.ops[index]
			if o.state == opNative && o.serial == serial && o.proto == proto && o.native == c.Op {
				s.done(o, c)
			}
		}
	case tagWait:
		if index < len(s.sockets) {
			s.waited(index, serial, c)
		}
	}
	s.pump()
}

// queueClose closes a native endpoint, now or once its session has room.
func (s *Service) queueClose(proto Proto, generation uint64, endpoint uint32) {
	if !s.backends[proto].up || s.backends[proto].generation != generation || endpoint == 0 {
		return
	}
	s.native = NativeRequest{Op: NativeClose, Endpoint: endpoint}
	if s.backend.Submit(proto, makeTag(tagClose, 0, 1), &s.native) != SubmitBusy {
		return
	}
	for i := range s.closes {
		if c := &s.closes[i]; !c.used {
			*c = pendingClose{used: true, proto: proto, endpoint: endpoint, generation: generation}
			s.closing++
			return
		}
	}
}

// pump retries what waited for a native slot, a Release, or queue room.
func (s *Service) pump() {
	for progress := true; progress; {
		progress = false
		if s.closing != 0 {
			for i := range s.closes {
				c := &s.closes[i]
				if !c.used {
					continue
				}
				s.native = NativeRequest{Op: NativeClose, Endpoint: c.endpoint}
				if s.backend.Submit(c.proto, makeTag(tagClose, 0, 1), &s.native) != SubmitBusy {
					c.used = false
					s.closing--
				}
			}
		}
		if s.waits != 0 {
			for i := range s.sockets {
				if s.sockets[i].needWait {
					s.sockets[i].needWait = false
					s.waits--
					s.arm(i)
				}
			}
		}
		if s.submits != 0 {
			before := s.submits
			for i := range s.ops {
				if s.ops[i].state == opSubmit {
					s.step(&s.ops[i])
				}
			}
			progress = s.submits < before
		}
	}
}

// Status fills a GLNXCTL1 Status reply.
func (s *Service) Status(reply *linuxsockapi.ControlReply) {
	families, sockets := 0, 0
	for index := range s.families {
		if s.families[index].used {
			families++
		}
	}
	for index := range s.sockets {
		if s.sockets[index].used {
			sockets++
		}
	}
	reply.Families, reply.Sockets, reply.Backends = uint8(families), uint8(sockets), 0
	if s.backends[TCP].up {
		reply.Backends |= linuxsockapi.BackendTCP
	}
	if s.backends[UDP].up {
		reply.Backends |= linuxsockapi.BackendUDP
	}
	reply.Counters = s.counters
}

// Counters returns the totals.
func (s *Service) Counters() linuxsockapi.Counters { return s.counters }
