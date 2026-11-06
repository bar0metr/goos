package linuxsock

// UDPRingLink is the service's end of linuxsockd's GUDP 3 session (ADR
// 0216): the core's native UDP operations mapped onto the session's rings,
// so a datagram costs no channel message while both sides keep busy.
// cmd/linuxsockd owns the channel and the mappings and drives the link from
// its event loop.
//
//   - NativeWrite and NativeSendTo copy the datagram into a free TX slot and
//     publish EntrySend. EntrySent completes the send with its result and
//     hands the slot back.
//   - NativeRead posts one receive of credit. EntryDatagram completes the
//     endpoint's oldest read with the RX slot's bytes and the endpoint's
//     readiness after it, and the link hands the slot back once the core
//     copied them. A NoWait read with no read posted asks netd with an
//     OpReady probe first and posts credit only when a datagram waits.
//   - NativeClose sends OpClose; the reads the link holds answer Closed.
//     netd takes every send published before the close first.
//
// Everything else (open, bind, connect, Ready, WaitReady, option) is a
// channel request whose reply completes the operation. After it publishes,
// the link rings netd's doorbell when netd armed it. Before linuxsockd
// sleeps it calls Idle, which arms the link's doorbell and takes whatever
// arrived meanwhile.

import (
	"math/bits"

	"goos/kernel/ipc"
	"goos/userspace/shmring"
	"goos/userspace/udpapi"
)

// UDPRingStats counts what a GUDP 3 link did.
type UDPRingStats struct {
	Requests, Replies, DoorbellsSent, DoorbellsReceived uint64
	Sends, Sent, Datagrams, Credits, Releases, Probes   uint64
	LocalCompletions, Overflows                         uint64
}

type udpRingEndpoint struct {
	id uint32
	// reads are the core's reads, oldest first, one receive of credit each.
	reads     [udpapi.MaxCredit3]uint64
	readFirst uint8
	readCount uint8
	// probe is a NoWait read waiting for its OpReady probe, zero for none.
	probe uint64
}

// UDPRingLink holds one GUDP 3 session. Reset starts it.
type UDPRingLink struct {
	Generation uint64
	tx, rx     []byte
	rings      udpapi.Rings3
	txGen      [udpapi.TXSlots3]uint32
	txTag      [udpapi.TXSlots3]uint64
	txOp       [udpapi.TXSlots3]NativeOp
	txLent     uint32
	rxGen      [udpapi.RXSlots3]uint32
	rxHeld     uint32
	endpoints  []udpRingEndpoint
	// local holds completions the link answered itself, for the next poll.
	local, spare []ringCompletion
	// release holds Release ring entries that found the ring full, in order.
	release      []udpapi.Entry
	writeBlocked bool
	broken       bool
	// published: the link put entries on TX or Release since it last rang.
	published  bool
	out        ipc.Message
	request    udpapi.Request
	reply      udpapi.Reply
	completion NativeCompletion
	Stats      UDPRingStats
}

// Reset starts a session over tx, the TX pool with its control page that
// the service writes, and rx, the RX pool with netd's control page. It
// reports false when the pools are short or netd has not set them up.
func (l *UDPRingLink) Reset(generation uint64, tx, rx []byte) bool {
	if len(tx) < udpapi.TXPoolPages3*4096 || len(rx) < udpapi.RXPoolPages3*4096 || !udpapi.ServiceReady(rx[udpapi.RXControl3:]) {
		return false
	}
	rings, ok := udpapi.ClientRings3(tx[udpapi.TXControl3:], rx[udpapi.RXControl3:])
	if !ok {
		return false
	}
	l.Generation, l.tx, l.rx, l.rings = generation, tx, rx, rings
	l.txGen, l.txTag, l.txOp, l.rxGen = [udpapi.TXSlots3]uint32{}, [udpapi.TXSlots3]uint64{}, [udpapi.TXSlots3]NativeOp{}, [udpapi.RXSlots3]uint32{}
	l.txLent, l.rxHeld = 0, 0
	l.endpoints, l.local, l.release = l.endpoints[:0], l.local[:0], l.release[:0]
	l.writeBlocked, l.broken, l.published = false, false, false
	l.Stats = UDPRingStats{}
	return true
}

// Broken reports whether netd broke the protocol.
func (l *UDPRingLink) Broken() bool { return l.broken }

// WantsWritable reports whether the link waits for room in netd's queue.
func (l *UDPRingLink) WantsWritable() bool { return l.writeBlocked }

// Backlogged reports Release ring entries that wait for room, which no
// doorbell announces.
func (l *UDPRingLink) Backlogged() bool { return len(l.release) != 0 }

// Snapshot reports how many TX slots netd holds and RX slots the link
// holds, and whether it waits for room, for the family snapshots.
func (l *UDPRingLink) Snapshot() (uint8, uint8, bool) {
	return uint8(bits.OnesCount32(l.txLent)), uint8(bits.OnesCount32(l.rxHeld)), l.writeBlocked
}

func (l *UDPRingLink) find(id uint32) int {
	if id == 0 {
		return -1
	}
	for index := range l.endpoints {
		if l.endpoints[index].id == id {
			return index
		}
	}
	return -1
}

func (l *UDPRingLink) claim(id uint32) bool {
	if id == 0 || l.find(id) >= 0 {
		return l.fail()
	}
	for index := range l.endpoints {
		if l.endpoints[index].id == 0 {
			l.endpoints[index] = udpRingEndpoint{id: id}
			return true
		}
	}
	l.endpoints = append(l.endpoints, udpRingEndpoint{id: id})
	return true
}

func (l *UDPRingLink) fail() bool {
	l.broken = true
	return false
}

func (l *UDPRingLink) answer(tag uint64, c NativeCompletion) {
	l.local = append(l.local, ringCompletion{tag: tag, c: c})
	l.Stats.LocalCompletions++
}

// call encodes l.request and sends it under correlation.
func (l *UDPRingLink) call(ch NativeChannel, correlation uint64) SendStatus {
	if !udpapi.EncodeRequest(&l.request, correlation, &l.out) {
		return SendGone
	}
	status := ch.Send(&l.out)
	switch status {
	case SendOK:
		l.Stats.Requests++
	case SendFull:
		l.writeBlocked = true
	}
	return status
}

// notify sends OpDoorbell when the link published and netd armed since the
// link last rang. A full queue drops it: netd takes the rings before each
// queued request.
func (l *UDPRingLink) notify(ch NativeChannel) {
	if !l.published {
		return
	}
	l.published = false
	if !l.rings.ShouldRing() {
		return
	}
	udpapi.EncodeDoorbell(&l.out)
	if ch.Send(&l.out) == SendOK {
		l.Stats.DoorbellsSent++
	}
}

func (l *UDPRingLink) pushRelease(e *udpapi.Entry) {
	if len(l.release) == 0 {
		switch l.rings.Push(udpapi.RingRelease, e) {
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

func (l *UDPRingLink) flushRelease() bool {
	sent := 0
	for sent < len(l.release) {
		status := l.rings.Push(udpapi.RingRelease, &l.release[sent])
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

// Submit is Backend.Submit for the UDP session.
func (l *UDPRingLink) Submit(ch NativeChannel, tag uint64, r *NativeRequest) SubmitStatus {
	if l.broken {
		return SubmitDown
	}
	switch r.Op {
	case NativeWrite, NativeSendTo:
		return l.send(ch, tag, r)
	case NativeRead:
		return l.read(ch, tag, r)
	case NativeClose:
		return l.close(ch, tag, r)
	}
	if status := udpRequest(r, 0, &l.request); status != SubmitAccepted {
		return status
	}
	switch l.call(ch, tag) {
	case SendOK:
		return SubmitAccepted
	case SendGone:
		return SubmitDown
	}
	return SubmitBusy
}

func (l *UDPRingLink) send(ch NativeChannel, tag uint64, r *NativeRequest) SubmitStatus {
	if len(r.Data) > udpapi.SlotSize {
		return SubmitUnsupported
	}
	if l.find(r.Endpoint) < 0 {
		l.answer(tag, NativeCompletion{Op: r.Op, Endpoint: r.Endpoint, Result: NativeInvalidArgument})
		return SubmitAccepted
	}
	free := bits.TrailingZeros32(^l.txLent)
	if free >= udpapi.TXSlots3 {
		return SubmitBusy
	}
	slot := uint8(free)
	base := udpapi.Offset(slot)
	length := copy(l.tx[base:base+udpapi.SlotSize], r.Data)
	l.txGen[slot]++
	if l.txGen[slot] == 0 {
		l.txGen[slot] = 1
	}
	entry := udpapi.Entry{Kind: udpapi.EntrySend, Slot: slot, Endpoint: r.Endpoint, Length: uint16(length), Generation: l.txGen[slot]}
	if r.Op == NativeSendTo {
		entry.Flags, entry.Address, entry.Port = udpapi.FlagTo, r.Address, r.Port
	}
	if l.rings.Push(udpapi.RingTX, &entry) != shmring.OK {
		// The TX ring holds every TX slot; anything else is a broken ring
		// or a request the protocol refuses.
		if r.Op == NativeSendTo && (r.Address == [4]byte{} || r.Port == 0) {
			l.txGen[slot]--
			l.answer(tag, NativeCompletion{Op: r.Op, Endpoint: r.Endpoint, Result: NativeInvalidArgument})
			return SubmitAccepted
		}
		l.fail()
		return SubmitDown
	}
	l.txLent |= 1 << slot
	l.txTag[slot], l.txOp[slot] = tag, r.Op
	l.published = true
	l.Stats.Sends++
	l.notify(ch)
	return SubmitAccepted
}

func (l *UDPRingLink) read(ch NativeChannel, tag uint64, r *NativeRequest) SubmitStatus {
	if r.Flags&NativePeek != 0 {
		return SubmitUnsupported
	}
	index := l.find(r.Endpoint)
	if index < 0 {
		l.answer(tag, NativeCompletion{Op: NativeRead, Endpoint: r.Endpoint, Result: NativeInvalidArgument})
		return SubmitAccepted
	}
	e := &l.endpoints[index]
	if int(e.readCount) == len(e.reads) || e.probe != 0 {
		return SubmitBusy
	}
	if r.Flags&NativeNoWait != 0 && e.readCount == 0 {
		// netd fills credit only when a datagram waits, so credit posted
		// with nothing queued would make this read block. Ask first.
		l.request = udpapi.Request{Opcode: udpapi.OpReady, Endpoint: e.id}
		switch l.call(ch, ringTag(tagRingProbe, index, e.id)) {
		case SendOK:
			e.probe = tag
			l.Stats.Probes++
			return SubmitAccepted
		case SendGone:
			return SubmitDown
		}
		return SubmitBusy
	}
	l.post(ch, index, tag)
	return SubmitAccepted
}

// post queues a read on endpoint index and gives netd one receive of
// credit for it.
func (l *UDPRingLink) post(ch NativeChannel, index int, tag uint64) {
	e := &l.endpoints[index]
	e.reads[(int(e.readFirst)+int(e.readCount))%len(e.reads)] = tag
	e.readCount++
	l.pushRelease(&udpapi.Entry{Kind: udpapi.EntryCredit, Endpoint: e.id, Value: 1})
	l.Stats.Credits++
	l.notify(ch)
}

func (l *UDPRingLink) close(ch NativeChannel, tag uint64, r *NativeRequest) SubmitStatus {
	index := l.find(r.Endpoint)
	if index < 0 {
		l.answer(tag, NativeCompletion{Op: NativeClose, Endpoint: r.Endpoint, Result: NativeInvalidArgument})
		return SubmitAccepted
	}
	l.request = udpapi.Request{Opcode: udpapi.OpClose, Endpoint: r.Endpoint}
	switch l.call(ch, tag) {
	case SendOK:
	case SendGone:
		return SubmitDown
	default:
		return SubmitBusy
	}
	e := &l.endpoints[index]
	for ; e.readCount != 0; e.readCount-- {
		l.answer(e.reads[e.readFirst], NativeCompletion{Op: NativeRead, Endpoint: e.id, Result: NativeClosed})
		e.readFirst = (e.readFirst + 1) % uint8(len(e.reads))
	}
	if e.probe != 0 {
		l.answer(e.probe, NativeCompletion{Op: NativeRead, Endpoint: e.id, Result: NativeClosed})
	}
	*e = udpRingEndpoint{}
	return SubmitAccepted
}

// Receive takes one message from netd's channel: a doorbell or a reply.
// It reports false when netd broke the protocol.
func (l *UDPRingLink) Receive(ch NativeChannel, in *ipc.Message, core Completer) bool {
	if l.broken {
		return false
	}
	if in.Kind == ipc.KindNotification {
		if !udpapi.IsDoorbell(in) {
			return l.fail()
		}
		l.Stats.DoorbellsReceived++
		return l.Poll(ch, core)
	}
	l.reply = udpapi.Reply{}
	correlation, ok := udpapi.DecodeReply(in, &l.reply)
	if !ok {
		return l.fail()
	}
	l.Stats.Replies++
	if correlation>>56 == tagRingProbe {
		l.probed(ch, correlation)
		return l.Poll(ch, core)
	}
	r := &l.reply
	c := &l.completion
	*c = NativeCompletion{Result: udpResult(r.Result), Endpoint: r.Endpoint}
	switch r.Opcode {
	case udpapi.OpOpen:
		c.Op = NativeOpen
		if r.Result == udpapi.ResultOK && !l.claim(r.Endpoint) {
			return false
		}
	case udpapi.OpBind:
		c.Op, c.Local, c.LocalPort = NativeBind, r.Address, r.Port
	case udpapi.OpConnect:
		c.Op, c.Local, c.LocalPort = NativeConnect, r.Local, r.LocalPort
	case udpapi.OpClose:
		c.Op = NativeClose
	case udpapi.OpReady:
		c.Op, c.Mask, c.Readable = NativeReady, coreUDPMask(r.Mask), r.Value
	case udpapi.OpWaitReady:
		c.Op, c.Mask, c.Readable = NativeWaitReady, coreUDPMask(r.Mask), r.Value
	case udpapi.OpOption:
		c.Op, c.Value = NativeOption, r.Value
	default:
		return l.fail()
	}
	core.Complete(UDP, l.Generation, correlation, c)
	return l.Poll(ch, core)
}

// probed takes the answer to a NoWait read's probe: credit when a datagram
// waits, WouldBlock otherwise.
func (l *UDPRingLink) probed(ch NativeChannel, correlation uint64) {
	index, id := int(uint16(correlation>>32)), uint32(correlation)
	if index >= len(l.endpoints) || l.endpoints[index].id != id || l.endpoints[index].probe == 0 {
		return
	}
	e := &l.endpoints[index]
	tag := e.probe
	e.probe = 0
	r := &l.reply
	switch {
	case r.Result == udpapi.ResultOK && r.Mask&udpapi.ReadyReadable != 0:
		l.post(ch, index, tag)
	case r.Result == udpapi.ResultOK:
		l.answer(tag, NativeCompletion{Op: NativeRead, Endpoint: id, Result: NativeWouldBlock})
	default:
		l.answer(tag, NativeCompletion{Op: NativeRead, Endpoint: id, Result: udpResult(r.Result)})
	}
}

// Poll takes what netd published and answers what the link answered
// itself. It reports false when netd broke the protocol.
func (l *UDPRingLink) Poll(ch NativeChannel, core Completer) bool {
	_, ok := l.poll(ch, core)
	return ok
}

// Idle arms the link's doorbell before linuxsockd sleeps, then polls once
// more. It reports true when nothing moved, so linuxsockd may sleep: netd
// rings once it publishes.
func (l *UDPRingLink) Idle(ch NativeChannel, core Completer) bool {
	if l.broken {
		return true
	}
	l.rings.Arm()
	moved, ok := l.poll(ch, core)
	return !ok || !moved
}

// Writable retries what a full queue refused once netd's channel has room.
func (l *UDPRingLink) Writable(ch NativeChannel, core *Service) {
	l.writeBlocked = false
	core.BackendWritable(UDP)
	l.Poll(ch, core)
}

func (l *UDPRingLink) poll(ch NativeChannel, core Completer) (moved, ok bool) {
	var e udpapi.Entry
	for !l.broken {
		step := l.flushRelease()
		for !l.broken {
			status := l.rings.Pop(udpapi.RingDone, &e)
			if status == shmring.Empty {
				break
			}
			if status != shmring.OK {
				l.fail()
				break
			}
			step = true
			l.sent(&e, core)
		}
		for !l.broken {
			status := l.rings.Pop(udpapi.RingRX, &e)
			if status == shmring.Empty {
				break
			}
			if status != shmring.OK {
				l.fail()
				break
			}
			step = true
			l.received(&e, core)
		}
		if len(l.local) != 0 {
			step = true
			batch := l.local
			l.local = l.spare[:0]
			for index := range batch {
				core.Complete(UDP, l.Generation, batch[index].tag, &batch[index].c)
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

func (l *UDPRingLink) sent(e *udpapi.Entry, core Completer) {
	bit := uint32(1) << e.Slot
	if l.txLent&bit == 0 || l.txGen[e.Slot] != e.Generation {
		l.fail()
		return
	}
	l.txLent &^= bit
	l.Stats.Sent++
	c := &l.completion
	*c = NativeCompletion{Op: l.txOp[e.Slot], Endpoint: e.Endpoint, Result: udpResult(e.Result)}
	core.Complete(UDP, l.Generation, l.txTag[e.Slot], c)
}

func (l *UDPRingLink) received(e *udpapi.Entry, core Completer) {
	bit := uint32(1) << e.Slot
	if l.rxHeld&bit != 0 || e.Generation != l.rxGen[e.Slot]+1 && !(l.rxGen[e.Slot] == ^uint32(0) && e.Generation == 1) {
		l.fail()
		return
	}
	l.rxHeld |= bit
	l.rxGen[e.Slot] = e.Generation
	l.Stats.Datagrams++
	index := l.find(e.Endpoint)
	if index < 0 {
		// netd filled credit the endpoint posted before it closed.
		l.releaseRX(e.Slot)
		return
	}
	target := &l.endpoints[index]
	if target.readCount == 0 {
		l.fail()
		return
	}
	tag := target.reads[target.readFirst]
	target.readFirst = (target.readFirst + 1) % uint8(len(target.reads))
	target.readCount--
	base := udpapi.Offset(e.Slot)
	c := &l.completion
	*c = NativeCompletion{Op: NativeRead, Endpoint: e.Endpoint, Result: NativeOK, Address: e.Address, Port: e.Port,
		Full: uint32(e.Length), Ready: true, Mask: coreUDPMask(e.Mask), Readable: e.Value,
		Data: l.rx[base : base+uint64(e.Length)]}
	core.Complete(UDP, l.Generation, tag, c)
	c.Data = nil
	l.releaseRX(e.Slot)
}

func (l *UDPRingLink) releaseRX(slot uint8) {
	l.rxHeld &^= 1 << slot
	l.Stats.Releases++
	l.pushRelease(&udpapi.Entry{Kind: udpapi.EntryRelease, Slot: slot, Generation: l.rxGen[slot]})
}
