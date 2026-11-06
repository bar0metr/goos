package linuxsock

// RingLink is the service's end of linuxsockd's GTCP 4 session (ADR 0214):
// the core's native TCP operations mapped onto the session's rings, so a
// 16 KiB slot costs no channel message. cmd/linuxsockd owns the channel
// and the mappings and drives the link from its event loop.
//
//   - NativeWrite copies the family TX slot's bytes into a free GTCP 4 TX
//     slot and publishes EntryWrite at the endpoint's stream offset, which
//     the link counts. EntryWriteDone completes it once the stack took every
//     byte, or with the error that stopped it.
//   - NativeRead posts one fill of credit. EntryData completes the oldest
//     read of the endpoint with the RX slot's bytes, and the link hands the
//     slot back once the core copied them; EntryEnd completes it with the
//     end of the stream or the error that ended it. A NoWait read with no
//     read ahead posts credit only when the readiness page shows netd holds
//     something to report, and answers WouldBlock otherwise.
//   - NativeWaitReady and NativeReady answer from the readiness page. netd
//     rings the doorbell only for ring entries, so a page change wakes the
//     link only when it comes with one: data or an end for posted credit, a
//     child accepted ahead, a finished write. A wait goes to netd as
//     OpWaitReady when nothing else would wake the link: while a connect
//     runs, and when the core marked it NativeWake and no credit is posted.
//     When the page does not name the endpoint yet, an OpReady
//     probe stands in for it.
//   - A listener has netd accept up to ringAcceptAhead children ahead
//     (OpAcceptAhead). They arrive as EntryAccepted, and NativeAccept takes
//     the oldest without a request; MaskAcceptable shows while one waits.
//   - NativeClose sends OpClose, which hands the endpoint to netd: the link
//     answers its reads and its wait with Closed, netd still sends the TX
//     slots queued before it, and EntryClosed draws an EntryForget. One poll
//     publishes its Forgets together, under one doorbell.
//
// Everything else (open, open-and-connect, bind, listen, connect, shutdown,
// error, option) is a channel request whose reply completes the operation.
// After it publishes, the link rings netd's doorbell when netd armed it.
// Before linuxsockd sleeps it calls Idle, which arms the link's doorbell and
// takes whatever arrived meanwhile.

import (
	"math/bits"

	"goos/kernel/ipc"
	"goos/userspace/shmring"
	"goos/userspace/tcpapi"
)

const (
	// ringAcceptAhead bounds the children netd accepts ahead for one
	// listener, and its backlog bounds them again.
	ringAcceptAhead = 4
	// The link's own requests carry these kinds in the top byte of the
	// correlation, past every core tag.
	tagRingAhead = 0xf1
	tagRingClose = 0xf2
	tagRingProbe = 0xf3
)

// Completer takes a completion; *Service is one.
type Completer interface {
	Complete(proto Proto, generation uint64, tag uint64, c *NativeCompletion)
}

type ringState uint8

const (
	ringFree ringState = iota
	ringOpen
	// ringClosing: OpClose went to netd; EntryClosed follows.
	ringClosing
)

type ringEndpoint struct {
	id    uint32
	state ringState
	// queued is the stream offset past the last byte the link wrote.
	queued uint32
	// reads are the core's reads, oldest first, one fill of credit each.
	reads     [tcpapi.MaxCredit4]uint64
	readFirst uint8
	readCount uint8
	// wait is the core's held wait, zero for none; forwarded says netd holds
	// it.
	wait      uint64
	waitMask  NativeMask
	forwarded bool
	// wake: the wait carries NativeWake.
	wake bool
	// connecting: netd answered a connect InProgress and the page has not
	// shown its end.
	connecting bool
	// probing: an OpReady probe is out. probed: probe holds its answer, used
	// while the page does not name the endpoint.
	probing  bool
	probed   bool
	probe    NativeMask
	probeLen uint32
	// child: netd accepted the endpoint ahead and the core has not taken it.
	child bool
	// A listener's accepts ahead: want children, granted of them asked for
	// and not arrived, the arrived ones oldest first.
	listening   bool
	aheadOff    bool
	aheadOut    bool
	want        uint8
	granted     uint8
	acceptCount uint8
	accepted    [ringAcceptAhead]uint16
	// needClose: the link owes netd the close of an unwanted child.
	needClose bool
}

// ringTuple is the connection a child accepted ahead arrived with.
type ringTuple struct {
	address, local  [4]byte
	port, localPort uint16
}

type ringCompletion struct {
	tag uint64
	c   NativeCompletion
}

// RingStats count the link's channel traffic and ring entries.
type RingStats struct {
	Requests, Replies, DoorbellsSent, DoorbellsReceived           uint64
	Writes, WritesDone, Data, Ends, Credits, Releases, Forgets    uint64
	Accepted, LocalCompletions, ForwardedWaits, Probes, Overflows uint64
}

// RingLink holds one GTCP 4 session. Reset starts it.
type RingLink struct {
	Generation uint64
	tx, rx     []byte
	control    tcpapi.Control
	txGen      [tcpapi.TXSlots4]uint32
	txTag      [tcpapi.TXSlots4]uint64
	txLent     uint32
	rxGen      [tcpapi.RXSlots4]uint32
	rxHeld     uint32
	endpoints  [tcpapi.Endpoints4]ringEndpoint
	tuples     [tcpapi.Endpoints4]ringTuple
	// limit is one past the highest table index in use.
	limit int
	// last caches the index the last lookup found.
	last int
	// waits counts the held waits; owed says an endpoint owes netd a
	// request a full queue refused.
	waits int
	owed  bool
	// local holds completions the link answered itself, for the next poll.
	local, spare []ringCompletion
	// release holds Release ring entries that found the ring full, in order.
	release      []tcpapi.Entry
	writeBlocked bool
	broken       bool
	// published: the link put entries on TX or Release since it last rang.
	published  bool
	out        ipc.Message
	request    tcpapi.Request
	reply      tcpapi.Reply
	completion NativeCompletion
	Stats      RingStats
}

// Reset starts a session over tx, the TX pool with its control page that
// the service writes, and rx, the RX pool with netd's control pages. It
// reports false when the pools are short or netd has not set them up.
func (l *RingLink) Reset(generation uint64, tx, rx []byte) bool {
	control, ok := tcpapi.ControlOf(tx, rx)
	if !ok || !control.ServiceReady() || uint64(len(tx)) < tcpapi.Layout4.TXPool() || uint64(len(rx)) < tcpapi.Layout4.RXPool() {
		return false
	}
	// Field by field: a literal of the link's size would go to the heap
	// under TinyGo.
	for index := range l.endpoints {
		l.endpoints[index] = ringEndpoint{}
	}
	for slot := range l.txGen {
		l.txGen[slot], l.txTag[slot] = 0, 0
	}
	for slot := range l.rxGen {
		l.rxGen[slot] = 0
	}
	l.Generation, l.tx, l.rx, l.control = generation, tx, rx, control
	l.txLent, l.rxHeld, l.limit, l.last, l.waits = 0, 0, 0, 0, 0
	l.owed, l.writeBlocked, l.broken, l.published = false, false, false, false
	l.local, l.release = l.local[:0], l.release[:0]
	l.Stats = RingStats{}
	return true
}

// Broken reports whether netd broke the protocol.
func (l *RingLink) Broken() bool { return l.broken }

// WantsWritable reports whether the link waits for room in netd's queue.
func (l *RingLink) WantsWritable() bool { return l.writeBlocked }

// Backlogged reports Release ring entries that wait for room. netd sends
// nothing when it takes ring entries, so the event loop must come back to
// them on its own.
func (l *RingLink) Backlogged() bool { return len(l.release) != 0 }

// Snapshot reports how many TX slots netd holds and RX slots the link
// holds, and whether it waits for room, for the family snapshots.
func (l *RingLink) Snapshot() (uint8, uint8, bool) {
	return uint8(bits.OnesCount32(l.txLent)), uint8(bits.OnesCount32(l.rxHeld)), l.writeBlocked
}

// Held reports the TX slots netd holds and the RX slots the link holds.
func (l *RingLink) Held() (tx, rx uint32) { return l.txLent, l.rxHeld }

// Open counts the table entries in use.
func (l *RingLink) Open() int {
	count := 0
	for index := 0; index < l.limit; index++ {
		if l.endpoints[index].state != ringFree {
			count++
		}
	}
	return count
}

func (l *RingLink) find(id uint32) int {
	if id == 0 {
		return -1
	}
	if l.last < l.limit && l.endpoints[l.last].id == id && l.endpoints[l.last].state != ringFree {
		return l.last
	}
	for index := 0; index < l.limit; index++ {
		if l.endpoints[index].id == id && l.endpoints[index].state != ringFree {
			l.last = index
			return index
		}
	}
	return -1
}

// claim records the endpoint netd opened at table index. A taken entry
// means netd broke the protocol.
func (l *RingLink) claim(index uint16, id uint32) bool {
	if int(index) >= len(l.endpoints) || id == 0 || l.endpoints[index].state != ringFree {
		return l.fail()
	}
	l.endpoints[index] = ringEndpoint{id: id, state: ringOpen}
	l.limit = max(l.limit, int(index)+1)
	return true
}

func (l *RingLink) free(index int) {
	e := &l.endpoints[index]
	if e.wait != 0 {
		l.waits--
	}
	*e = ringEndpoint{}
	for l.limit > 0 && l.endpoints[l.limit-1].state == ringFree {
		l.limit--
	}
}

func (l *RingLink) fail() bool {
	l.broken = true
	return false
}

func (l *RingLink) answer(tag uint64, c NativeCompletion) {
	l.local = append(l.local, ringCompletion{tag: tag, c: c})
	l.Stats.LocalCompletions++
}

func (l *RingLink) send(ch NativeChannel, out *ipc.Message) SendStatus {
	status := ch.Send(out)
	switch status {
	case SendOK:
		l.Stats.Requests++
	case SendFull:
		l.writeBlocked = true
	}
	return status
}

// call encodes request and sends it under correlation.
func (l *RingLink) call(ch NativeChannel, correlation uint64) SendStatus {
	if !tcpapi.Layout4.EncodeRequest(&l.request, correlation, &l.out) {
		return SendGone
	}
	return l.send(ch, &l.out)
}

// notify sends OpDoorbell when the link published and netd armed since the
// link last rang. A full queue drops it: netd takes the rings before each
// queued request.
func (l *RingLink) notify(ch NativeChannel) {
	if !l.published {
		return
	}
	l.published = false
	if !l.control.ClientShouldRing() {
		return
	}
	if tcpapi.EncodeDoorbell4(&l.out) && ch.Send(&l.out) == SendOK {
		l.Stats.DoorbellsSent++
	}
}

func (l *RingLink) pushRelease(e *tcpapi.Entry) {
	if len(l.release) == 0 {
		switch l.control.PushRelease(e) {
		case shmring.OK:
			l.published = true
			return
		case shmring.Broken:
			l.fail()
			return
		}
	}
	l.release = append(l.release, *e)
	l.Stats.Overflows++
}

// flushRelease moves waiting Release entries onto the ring and reports
// whether it moved any.
func (l *RingLink) flushRelease() bool {
	sent := 0
	for sent < len(l.release) {
		status := l.control.PushRelease(&l.release[sent])
		if status == shmring.Broken {
			l.fail()
			return false
		}
		if status != shmring.OK {
			break
		}
		sent++
	}
	if sent != 0 {
		l.release = append(l.release[:0], l.release[sent:]...)
		l.published = true
	}
	return sent != 0
}

// Submit is Backend.Submit for the TCP session.
func (l *RingLink) Submit(ch NativeChannel, tag uint64, r *NativeRequest) SubmitStatus {
	if l.broken {
		return SubmitDown
	}
	switch r.Op {
	case NativeWrite:
		return l.write(ch, tag, r)
	case NativeRead:
		return l.read(ch, tag, r)
	case NativeWaitReady:
		return l.waitReady(ch, tag, r)
	case NativeReady:
		// With the wait in netd the core asks until netd answers it, so the
		// question goes to netd too.
		if index := l.find(r.Endpoint); index >= 0 && !l.endpoints[index].forwarded {
			if mask, readable, known := l.mask(index); known {
				l.answer(tag, NativeCompletion{Op: NativeReady, Endpoint: r.Endpoint, Mask: mask, Readable: readable})
				return SubmitAccepted
			}
		}
	case NativeAccept:
		if index := l.find(r.Endpoint); index >= 0 && l.endpoints[index].state == ringOpen {
			e := &l.endpoints[index]
			if e.acceptCount != 0 {
				l.takeChild(ch, tag, index)
				return SubmitAccepted
			}
			// netd publishes the next child it accepts ahead, so a NoWait
			// accept that finds none here and no child queued in the stack
			// would block.
			if page, ok := l.control.LoadReadiness(index); r.Flags&NativeNoWait != 0 && e.granted != 0 && ok && page.Endpoint == e.id &&
				page.Mask&tcpapi.ReadyAcceptable == 0 {
				l.answer(tag, NativeCompletion{Op: NativeAccept, Endpoint: r.Endpoint, Result: NativeWouldBlock})
				return SubmitAccepted
			}
		}
	case NativeClose:
		return l.close(ch, tag, r)
	case NativeSendTo:
		return SubmitUnsupported
	}
	if status := tcpRequest(r, &l.request); status != SubmitAccepted {
		return status
	}
	if r.Op == NativeListen {
		if index := l.find(r.Endpoint); index >= 0 {
			l.endpoints[index].want = uint8(min(r.Length, ringAcceptAhead))
		}
	}
	switch l.call(ch, tag) {
	case SendOK:
		return SubmitAccepted
	case SendGone:
		return SubmitDown
	}
	return SubmitBusy
}

func (l *RingLink) write(ch NativeChannel, tag uint64, r *NativeRequest) SubmitStatus {
	index := l.find(r.Endpoint)
	if index < 0 || l.endpoints[index].state != ringOpen || l.endpoints[index].child {
		l.answer(tag, NativeCompletion{Op: NativeWrite, Endpoint: r.Endpoint, Result: NativeInvalidArgument})
		return SubmitAccepted
	}
	free := bits.TrailingZeros32(^l.txLent)
	if free >= tcpapi.TXSlots4 {
		return SubmitBusy
	}
	slot := uint8(free)
	e := &l.endpoints[index]
	base := tcpapi.Layout4.Offset(slot)
	length := copy(l.tx[base:base+tcpapi.SlotSize4], r.Data)
	l.txGen[slot]++
	entry := tcpapi.Entry{Kind: tcpapi.EntryWrite, Slot: slot, Index: uint16(index), Endpoint: e.id, Length: uint32(length),
		Generation: l.txGen[slot], Offset: e.queued}
	if l.control.PushTX(&entry) != shmring.OK {
		// The link holds at most TXSlots4 entries there.
		l.fail()
		return SubmitDown
	}
	l.txLent |= 1 << slot
	l.txTag[slot] = tag
	l.published = true
	e.queued += uint32(length)
	l.Stats.Writes++
	l.notify(ch)
	return SubmitAccepted
}

func (l *RingLink) read(ch NativeChannel, tag uint64, r *NativeRequest) SubmitStatus {
	index := l.find(r.Endpoint)
	if index < 0 || l.endpoints[index].child {
		l.answer(tag, NativeCompletion{Op: NativeRead, Endpoint: r.Endpoint, Result: NativeInvalidArgument})
		return SubmitAccepted
	}
	e := &l.endpoints[index]
	if e.state != ringOpen {
		l.answer(tag, NativeCompletion{Op: NativeRead, Endpoint: r.Endpoint, Result: NativeClosed})
		return SubmitAccepted
	}
	if int(e.readCount) == len(e.reads) {
		return SubmitBusy
	}
	if r.Flags&NativeNoWait != 0 && e.readCount == 0 {
		// netd fills credit only when it has something, so credit posted
		// with nothing there would make this read block.
		page, ok := l.control.LoadReadiness(index)
		if !ok || page.Endpoint != e.id || page.Mask&(tcpapi.ReadyReadable|tcpapi.ReadyReadEOF|tcpapi.ReadyError|tcpapi.ReadyHangUp) == 0 {
			l.answer(tag, NativeCompletion{Op: NativeRead, Endpoint: r.Endpoint, Result: NativeWouldBlock})
			return SubmitAccepted
		}
	}
	e.reads[(int(e.readFirst)+int(e.readCount))%len(e.reads)] = tag
	e.readCount++
	l.pushRelease(&tcpapi.Entry{Kind: tcpapi.EntryCredit, Index: uint16(index), Endpoint: e.id, Length: 1})
	l.Stats.Credits++
	l.notify(ch)
	return SubmitAccepted
}

func (l *RingLink) waitReady(ch NativeChannel, tag uint64, r *NativeRequest) SubmitStatus {
	index := l.find(r.Endpoint)
	if index < 0 || l.endpoints[index].state != ringOpen {
		l.answer(tag, NativeCompletion{Op: NativeWaitReady, Endpoint: r.Endpoint, Result: NativeInvalidArgument})
		return SubmitAccepted
	}
	e := &l.endpoints[index]
	if e.wait == tag && r.Flags&NativeWake != 0 {
		// The core marks the wait it holds; netd answers a forwarded one
		// anyway.
		e.wake = true
		l.checkWait(ch, index)
		return SubmitAccepted
	}
	if e.wait != 0 {
		l.answer(tag, NativeCompletion{Op: NativeWaitReady, Endpoint: r.Endpoint, Result: NativeBusy})
		return SubmitAccepted
	}
	e.wait, e.waitMask, e.forwarded, e.wake = tag, r.Mask, false, r.Flags&NativeWake != 0
	l.waits++
	l.checkWait(ch, index)
	return SubmitAccepted
}

// mask reads the endpoint's mask and readable bytes from the readiness
// page, or from the last probe while the page does not name it. A child
// waiting in the listener's queue shows as MaskAcceptable.
func (l *RingLink) mask(index int) (NativeMask, uint32, bool) {
	e := &l.endpoints[index]
	var mask NativeMask
	var readable uint32
	page, ok := l.control.LoadReadiness(index)
	switch {
	case ok && page.Endpoint == e.id:
		mask, readable = coreTCPMask(page.Mask), page.Readable
	case e.probed:
		mask, readable = e.probe, e.probeLen
	default:
		return 0, 0, false
	}
	if e.acceptCount != 0 {
		mask |= MaskAcceptable
	}
	if mask&MaskConnecting == 0 {
		e.connecting = false
	}
	return mask, readable, true
}

// checkWait answers a held wait whose mask the endpoint no longer has, and
// otherwise holds it here, in netd when no ring entry would come for a
// change, or behind a probe while the mask is unknown.
func (l *RingLink) checkWait(ch NativeChannel, index int) {
	e := &l.endpoints[index]
	if e.wait == 0 || e.forwarded || e.state != ringOpen {
		return
	}
	mask, readable, known := l.mask(index)
	if known && mask != e.waitMask {
		l.answer(e.wait, NativeCompletion{Op: NativeWaitReady, Endpoint: e.id, Mask: mask, Readable: readable})
		e.wait = 0
		l.waits--
		return
	}
	switch {
	case e.connecting || known && e.wake && e.readCount == 0:
		l.request = tcpapi.Request{Opcode: tcpapi.OpWaitReady, Endpoint: e.id, Mask: tcpMask(e.waitMask)}
		if l.call(ch, e.wait) == SendOK {
			e.forwarded = true
			l.Stats.ForwardedWaits++
		}
	case !known && !e.probing:
		l.request = tcpapi.Request{Opcode: tcpapi.OpReady, Endpoint: e.id}
		if l.call(ch, ringTag(tagRingProbe, index, e.id)) == SendOK {
			e.probing = true
			l.Stats.Probes++
		}
	}
}

func ringTag(kind int, index int, id uint32) uint64 {
	return uint64(kind)<<56 | uint64(uint16(index))<<32 | uint64(id)
}

// takeChild answers a NativeAccept with the listener's oldest child.
func (l *RingLink) takeChild(ch NativeChannel, tag uint64, index int) {
	e := &l.endpoints[index]
	child := e.accepted[0]
	copy(e.accepted[:], e.accepted[1:e.acceptCount])
	e.acceptCount--
	l.endpoints[child].child = false
	c := &l.completion
	*c = NativeCompletion{Op: NativeAccept, Endpoint: l.endpoints[child].id}
	c.Address, c.Port, c.Local, c.LocalPort = l.childTuple(int(child))
	l.answer(tag, *c)
	l.topUp(ch, index)
}

// childTuple returns the tuple EntryAccepted carried.
func (l *RingLink) childTuple(index int) (address [4]byte, port uint16, local [4]byte, localPort uint16) {
	t := &l.tuples[index]
	return t.address, t.port, t.local, t.localPort
}

// topUp asks netd to accept ahead again once every child it was granted
// arrived and the listener's queue has room.
func (l *RingLink) topUp(ch NativeChannel, index int) {
	e := &l.endpoints[index]
	if !e.listening || e.aheadOff || e.aheadOut || e.state != ringOpen || e.granted != 0 || e.acceptCount >= e.want {
		return
	}
	count := e.want - e.acceptCount
	l.request = tcpapi.Request{Opcode: tcpapi.OpAcceptAhead, Endpoint: e.id, Backlog: count}
	switch l.call(ch, ringTag(tagRingAhead, index, e.id)) {
	case SendOK:
		e.aheadOut, e.granted = true, count
	case SendFull:
		l.owed = true
	}
}

// closed hands an endpoint to netd after its OpClose went: the reads and
// the wait the link holds answer Closed, and the children accepted ahead
// close too.
func (l *RingLink) closed(ch NativeChannel, index int) {
	e := &l.endpoints[index]
	e.state = ringClosing
	for ; e.readCount != 0; e.readCount-- {
		l.answer(e.reads[e.readFirst], NativeCompletion{Op: NativeRead, Endpoint: e.id, Result: NativeClosed})
		e.readFirst = (e.readFirst + 1) % uint8(len(e.reads))
	}
	if e.wait != 0 && !e.forwarded {
		l.answer(e.wait, NativeCompletion{Op: NativeWaitReady, Endpoint: e.id, Result: NativeClosed})
		e.wait = 0
		l.waits--
	}
	for position := uint8(0); position < e.acceptCount; position++ {
		l.closeChild(ch, int(e.accepted[position]))
	}
	e.acceptCount, e.listening = 0, false
}

func (l *RingLink) close(ch NativeChannel, tag uint64, r *NativeRequest) SubmitStatus {
	index := l.find(r.Endpoint)
	if index < 0 || l.endpoints[index].state != ringOpen {
		l.answer(tag, NativeCompletion{Op: NativeClose, Endpoint: r.Endpoint, Result: NativeInvalidArgument})
		return SubmitAccepted
	}
	l.request = tcpapi.Request{Opcode: tcpapi.OpClose, Endpoint: r.Endpoint}
	switch l.call(ch, tag) {
	case SendOK:
	case SendGone:
		return SubmitDown
	default:
		return SubmitBusy
	}
	l.endpoints[index].child = false
	l.closed(ch, index)
	return SubmitAccepted
}

// closeChild closes a child no one will take, now or once netd's queue has
// room.
func (l *RingLink) closeChild(ch NativeChannel, index int) {
	e := &l.endpoints[index]
	l.request = tcpapi.Request{Opcode: tcpapi.OpClose, Endpoint: e.id}
	switch l.call(ch, ringTag(tagRingClose, index, e.id)) {
	case SendOK:
		e.needClose, e.child = false, false
		l.closed(ch, index)
	case SendFull:
		e.needClose = true
		l.owed = true
	}
}

// Receive takes one message from netd's channel: a doorbell or a reply.
// It reports false when netd broke the protocol.
func (l *RingLink) Receive(ch NativeChannel, in *ipc.Message, core Completer) bool {
	if l.broken {
		return false
	}
	if in.Kind == ipc.KindNotification {
		if !tcpapi.IsDoorbell4(in) {
			return l.fail()
		}
		l.Stats.DoorbellsReceived++
		return l.Poll(ch, core)
	}
	l.reply = tcpapi.Reply{}
	correlation, ok := tcpapi.Layout4.DecodeReply(in, &l.reply)
	if !ok {
		return l.fail()
	}
	l.Stats.Replies++
	if correlation>>56 >= tagRingAhead {
		l.ownReply(ch, correlation)
		return l.Poll(ch, core)
	}
	r := &l.reply
	c := &l.completion
	tcpCompletion(r, c)
	switch r.Opcode {
	case tcpapi.OpOpen, tcpapi.OpAccept:
		if r.Result == tcpapi.ResultOK && !l.claim(r.Index, r.Endpoint) {
			return false
		}
	case tcpapi.OpOpenConnect:
		if r.Endpoint != 0 {
			if !l.claim(r.Index, r.Endpoint) {
				return false
			}
			if r.Result == tcpapi.ResultInProgress {
				l.endpoints[r.Index].connecting = true
				c.Mask = MaskConnecting
			}
		}
	case tcpapi.OpConnect:
		if index := l.find(r.Endpoint); index >= 0 && r.Result == tcpapi.ResultInProgress {
			l.endpoints[index].connecting = true
			l.checkWait(ch, index)
		}
	case tcpapi.OpListen:
		if index := l.find(r.Endpoint); index >= 0 && r.Result == tcpapi.ResultOK {
			l.endpoints[index].listening = true
			l.topUp(ch, index)
		}
	case tcpapi.OpWaitReady:
		if index := l.find(r.Endpoint); index >= 0 && l.endpoints[index].wait == correlation {
			e := &l.endpoints[index]
			e.wait, e.forwarded = 0, false
			l.waits--
			if c.Mask&MaskConnecting == 0 {
				e.connecting = false
			}
		}
	}
	core.Complete(TCP, l.Generation, correlation, c)
	return l.Poll(ch, core)
}

// ownReply takes the answer to one of the link's own requests.
func (l *RingLink) ownReply(ch NativeChannel, correlation uint64) {
	index, id := int(uint16(correlation>>32)), uint32(correlation)
	if index >= l.limit || l.endpoints[index].id != id {
		return
	}
	e := &l.endpoints[index]
	r := &l.reply
	switch correlation >> 56 {
	case tagRingAhead:
		e.aheadOut = false
		if r.Result != tcpapi.ResultOK {
			e.aheadOff, e.granted = true, 0
		}
	case tagRingProbe:
		e.probing = false
		if r.Result == tcpapi.ResultOK {
			e.probed, e.probe, e.probeLen = true, coreTCPMask(r.Mask), r.Value
		}
		l.checkWait(ch, index)
	}
}

// Poll takes what netd published and answers what the link answered
// itself. It reports false when netd broke the protocol.
func (l *RingLink) Poll(ch NativeChannel, core Completer) bool {
	_, ok := l.poll(ch, core)
	return ok
}

// Idle arms the link's doorbell before linuxsockd sleeps, then polls once
// more. It reports true when nothing moved, so linuxsockd may sleep: netd
// rings once it publishes.
func (l *RingLink) Idle(ch NativeChannel, core Completer) bool {
	if l.broken {
		return true
	}
	l.control.ClientArm()
	moved, ok := l.poll(ch, core)
	return !ok || !moved
}

// Writable retries what a full queue refused once netd's channel has room.
func (l *RingLink) Writable(ch NativeChannel, core *Service) {
	l.writeBlocked = false
	l.owed = true
	core.BackendWritable(TCP)
	l.Poll(ch, core)
}

func (l *RingLink) poll(ch NativeChannel, core Completer) (moved, ok bool) {
	var e tcpapi.Entry
	for !l.broken {
		step := l.flushRelease()
		for !l.broken {
			status := l.control.PopTXDone(&e)
			if status == shmring.Broken {
				l.fail()
				break
			}
			if status != shmring.OK {
				break
			}
			step = true
			l.txDone(&e, core)
		}
		for !l.broken {
			status := l.control.PopRX(&e)
			if status == shmring.Broken {
				l.fail()
				break
			}
			if status != shmring.OK {
				break
			}
			step = true
			l.received(ch, &e, core)
		}
		if l.owed {
			l.owed = false
			for index := 0; index < l.limit; index++ {
				if l.endpoints[index].needClose {
					l.closeChild(ch, index)
				}
				l.topUp(ch, index)
			}
		}
		if l.waits != 0 {
			for index := 0; index < l.limit; index++ {
				l.checkWait(ch, index)
			}
		}
		if len(l.local) != 0 {
			step = true
			batch := l.local
			l.local = l.spare[:0]
			for index := range batch {
				core.Complete(TCP, l.Generation, batch[index].tag, &batch[index].c)
			}
			l.spare = batch[:0]
		}
		if !step {
			break
		}
		moved = true
	}
	l.flushRelease()
	l.notify(ch)
	return moved, !l.broken
}

func (l *RingLink) txDone(e *tcpapi.Entry, core Completer) {
	if e.Kind == tcpapi.EntryClosed {
		if int(e.Index) >= l.limit || l.endpoints[e.Index].id != e.Endpoint || l.endpoints[e.Index].state != ringClosing {
			l.fail()
			return
		}
		l.free(int(e.Index))
		l.pushRelease(&tcpapi.Entry{Kind: tcpapi.EntryForget, Index: e.Index, Endpoint: e.Endpoint})
		l.Stats.Forgets++
		return
	}
	bit := uint32(1) << e.Slot
	if l.txLent&bit == 0 || l.txGen[e.Slot] != e.Generation {
		l.fail()
		return
	}
	l.txLent &^= bit
	l.Stats.WritesDone++
	c := &l.completion
	*c = NativeCompletion{Op: NativeWrite, Endpoint: e.Endpoint, Result: tcpResult(e.Result), Length: e.Length}
	core.Complete(TCP, l.Generation, l.txTag[e.Slot], c)
}

func (l *RingLink) received(ch NativeChannel, e *tcpapi.Entry, core Completer) {
	if e.Kind == tcpapi.EntryAccepted {
		l.accepted(ch, e)
		return
	}
	if e.Kind == tcpapi.EntryData {
		bit := uint32(1) << e.Slot
		if l.rxHeld&bit != 0 || e.Generation != l.rxGen[e.Slot]+1 {
			l.fail()
			return
		}
		l.rxHeld |= bit
		l.rxGen[e.Slot] = e.Generation
		l.Stats.Data++
	} else {
		l.Stats.Ends++
	}
	target := &l.endpoints[e.Index]
	if int(e.Index) >= l.limit || target.id != e.Endpoint || target.state != ringOpen {
		// netd filled credit the endpoint posted before it closed.
		if e.Kind == tcpapi.EntryData {
			l.releaseRX(e.Slot)
		}
		return
	}
	if target.readCount == 0 {
		l.fail()
		return
	}
	tag := target.reads[target.readFirst]
	target.readFirst = (target.readFirst + 1) % uint8(len(target.reads))
	target.readCount--
	c := &l.completion
	*c = NativeCompletion{Op: NativeRead, Endpoint: e.Endpoint, Result: tcpResult(e.Result)}
	if e.Kind == tcpapi.EntryData {
		base := tcpapi.Layout4.Offset(e.Slot)
		c.Result, c.Data = NativeOK, l.rx[base:base+uint64(e.Length)]
	}
	core.Complete(TCP, l.Generation, tag, c)
	c.Data = nil
	if e.Kind == tcpapi.EntryData {
		l.releaseRX(e.Slot)
	}
}

func (l *RingLink) releaseRX(slot uint8) {
	l.rxHeld &^= 1 << slot
	l.Stats.Releases++
	l.pushRelease(&tcpapi.Entry{Kind: tcpapi.EntryRelease, Slot: slot, Generation: l.rxGen[slot]})
}

// accepted queues a child netd accepted ahead on its listener, or closes it
// when the listener no longer wants it.
func (l *RingLink) accepted(ch NativeChannel, e *tcpapi.Entry) {
	if !l.claim(e.Index, e.Endpoint) {
		return
	}
	l.Stats.Accepted++
	child := &l.endpoints[e.Index]
	child.child = true
	t := &l.tuples[e.Index]
	t.address, t.port, t.local, t.localPort = e.Address, e.Port, e.Local, e.LocalPort
	listener := &l.endpoints[e.Offset]
	if int(e.Offset) < l.limit && listener.id == e.Generation && listener.granted != 0 {
		listener.granted--
	}
	if int(e.Offset) >= l.limit || listener.id != e.Generation || listener.state != ringOpen || !listener.listening ||
		int(listener.acceptCount) >= len(listener.accepted) {
		l.closeChild(ch, int(e.Index))
		return
	}
	listener.accepted[listener.acceptCount] = e.Index
	listener.acceptCount++
	l.topUp(ch, int(e.Offset))
}
