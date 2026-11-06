// Package linuxsock is the core of linuxsockd (ADR 0210): Linux socket
// objects for the kernel's family sessions, kept over native TCP and UDP
// endpoints in netd.
//
// The core runs in one event loop and never waits. Every GLNXSOCK3 request
// becomes an operation that either answers at once from local state or
// issues one native request that netd completes at once (NoWait), sometimes
// two in a row (an unbound listen binds first). On GTCP 4 (SetStreamRing) a
// stream socket opens its endpoint with the first request that needs one,
// and a connect opens and connects in one request. An operation may wait in
// the service only for something another prompt exchange brings: a native
// pool slot, a read ahead under way, a TX queue draining before SHUT_WR, or
// room in the family's channel for its reply.
//
// Payload never travels in a request (dataplane.go). The kernel commits TX
// slots on the family's submit ring; the core queues them per socket and
// drains each queue into netd with pipelined NoWait writes, handing every
// slot back on the complete ring once netd took all of it. For each
// readable socket the core keeps reads parked in netd that fill RX slots,
// and publishes each filled slot on the complete ring.
//
// Readiness is learned from one NativeWaitReady held per endpoint; a GTCP 4
// link answers most of them from netd's readiness page (ringlink.go). Each
// change is published in the family's readiness page; a ReadyChanged
// notification follows when no other is outstanding, and one that finds
// the queue full is sent again from Flush. The same notification covers
// the complete ring.
//
// Everything is bounded: MaxFamilies families, FamilyOps requests per
// family in the service at once, and the socket quotas of Limits
// (registry.go). The socket and operation tables grow in chunks up to the
// bounds the quotas set and never shrink.
package linuxsock

import (
	"goos/kernel/ipc"
	"goos/userspace/linuxsockapi"
)

const (
	MaxFamilies = 16
	// FamilyOps bounds a family's requests in the service. The event loop
	// stops reading a family's channel while it has that many.
	FamilyOps = 16
	// FamilyParked bounds a family's requests that wait for a read ahead
	// or a free RX slot; one past it answers WouldBlock. With FamilyOps
	// twice as large, half of a family's operations always make progress.
	FamilyParked = linuxsockapi.RXSlots
	// dataOps bounds the native reads and writes of the data plane: every
	// family RX slot under a read, and NativeWriteDepth writes for as many
	// sockets as the native TX slots of both sessions can carry at once.
	dataOps = MaxFamilies*linuxsockapi.RXSlots + 2*nativeTXSlots
	// MaxBacklog is the widest backlog a GTCP listen request carries, one
	// byte. A Linux backlog is clamped to it, and netd clamps again to
	// system.net.tcp.listen-backlog, as Linux clamps to somaxconn.
	MaxBacklog = 255
)

// MaxBacklog fits the GTCP listen request's backlog byte.
var _ = uint8(MaxBacklog)

// The status reply has an entry for every family.
var _ [linuxsockapi.StatusFamilies - MaxFamilies]byte

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
	// closing: the kernel closed the socket while its TX queue still held
	// bytes. It stays until netd took them, then closes its endpoint.
	closing bool
	// eof: a native read reported the end of the stream.
	eof bool
	// txBlocked: netd's send buffer was full; the queue waits for a mask
	// with Writable. txStall: a write took less than it carried, so the
	// writes after it took nothing; the queue sends again from the first
	// byte netd did not take once every write answered.
	txBlocked bool
	txStall   bool
	// txDropped: an error dropped the queue while writes were in netd; the
	// slots they carry go back as they answer.
	txDropped bool
	// needRefresh, needFetch: a readiness refresh or an error fetch is still
	// owed and goes out once an operation is free or the one out answered.
	needRefresh bool
	needFetch   bool
	// shutQueued: SHUT_WR waits for the TX queue to drain into netd.
	shutQueued bool
	// Children a listener accepted ahead, oldest first, and its accepts
	// ahead in netd.
	acceptq      [AcceptAhead]int16
	acceptSerial [AcceptAhead]uint32
	acceptLen    uint8
	accepting    uint8
	// rxBurst: the last read ahead filled its slot, so more may follow.
	rxBurst bool
	// wantTX, wantRX: a native slot was busy; the pump tries again.
	wantTX bool
	wantRX bool
	// dead: the native session the socket lived on is gone.
	dead      bool
	readShut  bool
	writeShut bool
	waiting   bool // a NativeWaitReady is held
	// wakeWait: the held wait carries NativeWake.
	wakeWait bool
	needWait bool // the wait could not be sent and is retried
	noWait   bool // the backend cannot wait; readiness comes from replies only
	fetching bool
	// refreshing: a kindRefresh operation is out.
	refreshing bool
	// reopen: a connect failed at once and the native endpoint is spent; the
	// next connect replaces it.
	reopen bool
	// connectDone: a nonblocking connect finished and no connect has
	// reported it yet.
	connectDone bool
	reuse       bool
	family      int8
	index       uint16
	state       sockState
	proto       Proto
	// The TX queue: family TX slots in commit order. txAccepted counts the
	// head slot's bytes netd took, txSent the bytes past them in writes not
	// yet answered; txStream is the stream offset netd took up to, which
	// every write names.
	txq        [linuxsockapi.TXSlots]uint8
	txqLen     uint8
	writesOut  uint8
	readsOut   uint8
	txAccepted uint32
	txSent     uint32
	txStream   uint32
	endpoint   uint32
	serial     uint32
	generation uint64
	id         uint64
	pending    linuxsockapi.Result
	mask       NativeMask
	// armedMask is the mask the held wait names; netd answers when the
	// endpoint's differs from it.
	armedMask NativeMask
	readable  uint32
	local     [4]byte
	peer      [4]byte
	localPort uint16
	peerPort  uint16
	backlog   uint32
	published linuxsockapi.Readiness
}

type family struct {
	used     bool
	notify   bool
	token    uint64
	session  uint32
	pool     []byte
	rings    []byte
	count    int
	inflight int
	parked   int
	replies  int
	// slots maps a family socket index to a global socket, plus one;
	// generations holds each index's last identity generation. freeSlots
	// stacks the free indexes, freeCount of them, lowest on top.
	slots       [linuxsockapi.MaxSockets]int16
	generations [linuxsockapi.MaxSockets]uint32
	freeSlots   [linuxsockapi.MaxSockets]uint16
	freeCount   int
	// TX slots the kernel committed and the core still holds, with what
	// each carries. txInFlight marks a datagram slot under a write.
	txHeld     uint8
	txInFlight uint8
	txGen      [linuxsockapi.TXSlots]uint32
	txLength   [linuxsockapi.TXSlots]uint32
	txDest     [linuxsockapi.TXSlots]bool
	txAddress  [linuxsockapi.TXSlots][4]byte
	txPort     [linuxsockapi.TXSlots]uint16
	// RX slots: rxUsed holds the ones under a read or published, rxPub the
	// published ones the kernel has not handed back, each with its owner
	// and the generation the kernel repeats.
	rxUsed   uint8
	rxPub    uint8
	rxOwner  [linuxsockapi.RXSlots]int16
	rxSerial [linuxsockapi.RXSlots]uint32
	rxGen    [linuxsockapi.RXSlots]uint32
	stats    linuxsockapi.ServiceCounters
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
	// kindTX is one native write of a socket's TX queue; length is what it
	// carries and slot the datagram's TX slot.
	kindTX
	// kindRX is one native read into RX slot rx.
	kindRX
	// kindShutdown is the half-close a SHUT_WR left for after the queue.
	kindShutdown
	// kindWarm opens an endpoint for the pool; it belongs to no family.
	kindWarm
	// kindAccept accepts a child ahead for listener socket into child.
	kindAccept
)

type op struct {
	state  opState
	kind   opKind
	orphan bool // the family is gone; the outcome goes nowhere
	parked bool
	family int8
	stage  uint8
	rx     int8
	slot   uint8
	// index is the operation's place in the table, its native tags' index.
	index  int16
	length uint32
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
	limits    Limits
	sockets   socketTable
	ops       opTable
	backends  [UDP + 1]backendState
	warm      [UDP + 1]warmPool
	linkView  LinkView
	// closes queues native closes waiting for room on their session, at
	// most one per endpoint of both protocols.
	closes []pendingClose
	serial uint32
	// Work the pump retries; zero skips the scan.
	submits, waits, kicks int
	// retryInternal: a refresh or an error fetch found no free operation.
	retryInternal bool
	// streamRing: the TCP session is GTCP 4. A stream socket opens its
	// endpoint only when it needs one, and a connect opens and connects in
	// one request.
	streamRing bool
	counters   linuxsockapi.Counters
	message    ipc.Message
	request    linuxsockapi.Request
	native     NativeRequest
}

func (s *Service) Init(backend Backend, transport Transport) {
	s.backend, s.transport = backend, transport
	s.limits = DefaultLimits()
}

// SetStreamRing tells the core its TCP session is GTCP 4 (RingLink).
func (s *Service) SetStreamRing(on bool) {
	s.streamRing = on
	if on {
		s.dropWarm(TCP)
	}
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
	generations := f.generations
	*f = family{}
	f.generations = generations
	f.used, f.token, f.session, f.pool = true, token, session, pool[:linuxsockapi.PoolBytes]
	f.rings = f.pool[linuxsockapi.RingPage*4096 : (linuxsockapi.RingPage+1)*4096]
	f.initSlots()
	s.sockets.reserved += s.lacking(0)
	linuxsockapi.InitReadiness(f.pool, session)
	linuxsockapi.InitRings(f.rings)
	clear(f.pool[linuxsockapi.SnapshotPage*4096 : (linuxsockapi.SnapshotPage+linuxsockapi.SnapshotPages)*4096])
	s.snapshotFamily(free)
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
	for slot := 0; slot < len(f.slots) && f.count != 0; slot++ {
		if g := f.slots[slot]; g != 0 {
			s.releaseSocket(int(g-1), false)
		}
	}
	s.sockets.reserved -= s.lacking(f.count)
	for index2 := 0; index2 < s.opCap(); index2++ {
		o := s.op(index2)
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
	f.used, f.pool, f.rings = false, nil, nil
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
	for i := 0; i < s.opCap(); i++ {
		o := s.op(i)
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
	// The kernel commits to the rings before it sends, so what a request
	// follows is in the queues before the request runs.
	if !s.drainSubmit(index) {
		return
	}
	if s.request.Opcode == linuxsockapi.OpDoorbell {
		s.families[index].stats.Doorbells++
		s.pump()
		s.notify(index)
		return
	}
	s.families[index].stats.Requests++
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
	r := &o.request
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
	o.socket, o.socketSerial = int16(index), s.sock(index).serial
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
	if sock := s.sock(global); sock.id != id || sock.opening || sock.closing {
		return -1, linuxsockapi.ResultStale
	}
	return global, linuxsockapi.ResultOK
}

// --- sockets -----------------------------------------------------------------

// allocSocket takes a socket for family index, or answers the quota it
// would break.
func (s *Service) allocSocket(index int, stream bool) (int, linuxsockapi.Result) {
	if result := s.admit(index); result != linuxsockapi.ResultOK {
		return -1, result
	}
	global := s.takeGlobal()
	if global < 0 {
		return -1, linuxsockapi.ResultServiceLimit
	}
	f := &s.families[index]
	slot := f.takeSlot()
	generation := (f.generations[slot] + 1) & linuxsockapi.GenerationMask
	if generation == 0 {
		generation = 1
	}
	f.generations[slot] = generation
	proto := UDP
	if stream {
		proto = TCP
	}
	sock := s.sock(global)
	*sock = socket{used: true, stream: stream, opening: true, family: int8(index), index: uint16(slot), proto: proto,
		serial: s.nextSerial(), id: linuxsockapi.MakeSocket(f.session, generation, slot)}
	f.slots[slot] = int16(global + 1)
	s.countSocket(f)
	s.snapshotFamily(index)
	return global, linuxsockapi.ResultOK
}

// snapshotPage is the pool page holding the snapshot of family index
// slot.
func (f *family) snapshotPage(slot int) []byte {
	page := int(linuxsockapi.SocketSnapshotPage(slot))
	return f.pool[page*4096 : (page+1)*4096]
}

// adopt gives an opening socket its native endpoint and starts its wait.
func (s *Service) adopt(global int, endpoint uint32, generation uint64) {
	sock := s.sock(global)
	sock.opening, sock.endpoint, sock.generation = false, endpoint, generation
	s.arm(global)
	s.publish(global)
	s.kick(global)
}

// releaseSocket frees a socket and closes its native endpoint. publish
// clears its readiness entry; a family being removed skips that.
func (s *Service) releaseSocket(global int, publish bool) {
	sock := s.sock(global)
	if !sock.used {
		return
	}
	f := &s.families[sock.family]
	if sock.endpoint != 0 && !sock.dead {
		s.queueClose(sock.proto, sock.generation, sock.endpoint)
	}
	s.dropTX(global)
	s.dropAccepted(sock)
	if sock.needWait {
		s.waits--
	}
	if sock.wantTX || sock.wantRX {
		s.kicks--
	}
	if publish && !sock.opening && !sock.closing {
		linuxsockapi.Publish(f.pool, int(sock.index), linuxsockapi.Readiness{})
	}
	if f.pool != nil {
		linuxsockapi.StoreSocketSnapshot(f.snapshotPage(int(sock.index)), int(sock.index), &linuxsockapi.SocketSnapshot{})
	}
	family := int(sock.family)
	f.slots[sock.index] = 0
	f.putSlot(int(sock.index))
	s.uncountSocket(f)
	*sock = socket{}
	s.sockets.free = append(s.sockets.free, int16(global))
	if f.used && f.pool != nil {
		s.snapshotFamily(family)
	}
}

func (s *Service) socketOf(o *op) *socket {
	if o.socket < 0 {
		return nil
	}
	sock := s.sock(int(o.socket))
	if !sock.used || sock.serial != o.socketSerial {
		return nil
	}
	return sock
}

// --- RX slots ------------------------------------------------------------------

// reserveRX takes a free RX slot for a read into it, leaving reserve slots
// free.
func (s *Service) reserveRX(f *family, global int, reserve int) int8 {
	free := 0
	for slot := uint8(0); slot < linuxsockapi.RXSlots; slot++ {
		if f.rxUsed&(1<<slot) == 0 {
			free++
		}
	}
	if free <= reserve {
		return -1
	}
	for slot := uint8(0); slot < linuxsockapi.RXSlots; slot++ {
		if f.rxUsed&(1<<slot) == 0 {
			f.rxUsed |= 1 << slot
			f.rxOwner[slot], f.rxSerial[slot] = int16(global+1), s.sock(global).serial
			return int8(slot)
		}
	}
	return -1
}

func (s *Service) freeRX(f *family, slot uint8) {
	f.rxUsed &^= 1 << slot
	f.rxPub &^= 1 << slot
	f.rxOwner[slot], f.rxSerial[slot] = 0, 0
}

// rxOf reports the RX slots that belong to a socket: under a read for it
// or published to the kernel for it.
func (s *Service) rxOf(f *family, global int) (held int, published int) {
	sock := s.sock(global)
	for slot := 0; slot < linuxsockapi.RXSlots; slot++ {
		if f.rxUsed&(1<<slot) != 0 && int(f.rxOwner[slot]) == global+1 && f.rxSerial[slot] == sock.serial {
			held++
			if f.rxPub&(1<<slot) != 0 {
				published++
			}
		}
	}
	return held, published
}

func (s *Service) rxSlot(f *family, slot uint8) []byte {
	offset := linuxsockapi.RXOffset(slot)
	return f.pool[offset : offset+linuxsockapi.SlotSize]
}

func (s *Service) txSlot(f *family, slot uint8) []byte {
	offset := linuxsockapi.TXOffset(slot)
	return f.pool[offset : offset+uint64(f.txLength[slot])]
}

// --- operations ----------------------------------------------------------------

func (s *Service) newOp(index int, kind opKind) *op {
	if kind == kindRequest && s.families[index].inflight >= FamilyOps {
		return nil
	}
	o := s.takeOp()
	if o == nil {
		return nil
	}
	*o = op{state: opSubmit, kind: kind, family: int8(index), rx: -1, socket: -1, child: -1, serial: s.nextSerial(), index: o.index}
	s.submits++
	if kind == kindRequest {
		s.families[index].inflight++
	}
	return o
}

func (s *Service) opIndex(o *op) int { return int(o.index) }

func (s *Service) freeOp(o *op) {
	if o.state == opFree {
		return
	}
	if o.state == opSubmit {
		s.submits--
	}
	if !o.orphan && o.family >= 0 {
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
			f.inflight--
		}
	} else if o.rx >= 0 {
		// The family is gone and its pool with it; the slot needs no care.
		o.rx = -1
	}
	if o.child >= 0 {
		if child := s.sock(int(o.child)); child.used && child.serial == o.childSerial && child.opening {
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
	s.putOp(o)
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
		o.reply = linuxsockapi.Reply{Opcode: r.Opcode, Result: result, Socket: socketID}
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
	s.warm[proto].full = false
	s.refill(proto)
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
	s.dropWarm(proto)
	kept := s.closes[:0]
	for _, c := range s.closes {
		if c.proto != proto || c.generation != generation {
			kept = append(kept, c)
		}
	}
	s.closes = kept
	for i := 0; i < s.socketCap(); i++ {
		sock := s.sock(i)
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
		s.dropTX(i)
		s.publish(i)
	}
	for i := 0; i < s.opCap(); i++ {
		o := s.op(i)
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
	if kind == tagOp && index < s.opCap() && s.op(index).family >= 0 && s.op(index).state == opNative && !s.op(index).orphan {
		s.families[s.op(index).family].stats.Completions++
	}
	switch kind {
	case tagOp:
		if index < s.opCap() {
			o := s.op(index)
			if o.state == opNative && o.serial == serial && o.proto == proto && o.native == c.Op {
				s.done(o, c)
			}
		}
	case tagWait:
		if index < s.socketCap() {
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
	if s.warm[proto].full {
		s.warm[proto].full = false
		defer s.refill(proto)
	}
	s.native = NativeRequest{Op: NativeClose, Endpoint: endpoint}
	if s.backend.Submit(proto, makeTag(tagClose, 0, 1), &s.native) != SubmitBusy {
		return
	}
	// Every queued close holds a distinct endpoint of a live session, so the
	// queue stays within the endpoints the quotas allow.
	s.closes = append(s.closes, pendingClose{proto: proto, endpoint: endpoint, generation: generation})
}

// pump retries what waited for a native slot, a Release, or queue room.
func (s *Service) pump() {
	for progress := true; progress; {
		progress = false
		if len(s.closes) != 0 {
			kept := s.closes[:0]
			for _, c := range s.closes {
				s.native = NativeRequest{Op: NativeClose, Endpoint: c.endpoint}
				if s.backend.Submit(c.proto, makeTag(tagClose, 0, 1), &s.native) == SubmitBusy {
					kept = append(kept, c)
				}
			}
			s.closes = kept
		}
		if s.waits != 0 {
			for i := 0; i < s.socketCap(); i++ {
				if s.sock(i).needWait {
					s.sock(i).needWait = false
					s.waits--
					s.arm(i)
				}
			}
		}
		if s.retryInternal {
			s.retryInternal = false
			for i := 0; i < s.socketCap(); i++ {
				sock := s.sock(i)
				if sock.needRefresh && !sock.refreshing {
					s.refreshReadable(i)
				}
				if sock.needFetch && !sock.fetching {
					s.fetchError(i)
				}
			}
		}
		kicked := false
		if s.kicks != 0 {
			before := s.kicks
			for i := 0; i < s.socketCap(); i++ {
				if sock := s.sock(i); sock.wantTX || sock.wantRX {
					s.kicks--
					sock.wantTX, sock.wantRX = false, false
					s.kick(i)
				}
			}
			kicked = s.kicks < before
		}
		if s.submits != 0 {
			before := s.submits
			for i := 0; i < s.opCap(); i++ {
				if s.op(i).state == opSubmit {
					s.step(s.op(i))
				}
			}
			progress = s.submits < before
		}
		progress = progress || kicked
	}
}

// BackendWritable tells the core that proto's queue to netd has room again:
// everything that found it full goes now. A request that parks in netd
// draws no reply, so this is the only event that retries what waited for
// room behind such requests.
func (s *Service) BackendWritable(proto Proto) {
	if proto == TCP || proto == UDP {
		s.pump()
	}
}

// ProgressHoles counts live sockets that wait for netd's send buffer with
// nothing registered that would tell them it has room: no held wait, no
// wait to send again, no refresh out or owed. It is zero unless the core
// lost track of a socket; validation builds check it after every event.
func (s *Service) ProgressHoles() int {
	holes := 0
	for i := 0; i < s.socketCap(); i++ {
		sock := s.sock(i)
		if !sock.used || sock.opening || sock.dead || !sock.txBlocked || sock.txqLen == 0 {
			continue
		}
		// A held wait that named Writable answers only when netd's mask
		// loses it, so it does not count for a socket waiting to see it.
		if sock.waiting && sock.armedMask&MaskWritable == 0 || sock.needWait || sock.noWait || sock.refreshing ||
			sock.needRefresh || sock.txDropped {
			continue
		}
		holes++
	}
	return holes
}

// Status fills a GLNXCTL2 Status reply: the table use, the backends, the
// counters, and the socket use against the quotas.
func (s *Service) Status(reply *linuxsockapi.ControlReply) {
	families := 0
	for index := range s.families {
		reply.FamilySockets[index] = 0
		if s.families[index].used {
			families++
			reply.FamilySockets[index] = uint16(s.families[index].count)
		}
	}
	reply.Families, reply.Sockets, reply.Backends = uint8(families), uint16(s.sockets.live), 0
	reply.SocketLimit, reply.FamilyLimit, reply.FamilyReserve = uint16(s.limits.System), uint16(s.limits.Family), uint16(s.limits.Reserve)
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
