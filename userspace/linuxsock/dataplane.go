package linuxsock

import "goos/userspace/linuxsockapi"

// The data plane: the family rings, each socket's TX queue and its reads
// ahead.
//
// TX. The kernel commits TX slots on the submit ring; drainSubmit queues
// each on its socket in ring order. A stream's queue goes to netd in
// pipelined NoWait writes of at most NativeSlotSize bytes, NativeWriteDepth
// at a time, each naming the stream offset of its first byte; netd takes a
// write only at the offset it reached, so a write after one that took a
// prefix takes nothing and the queue sends again from the first byte netd
// did not take. A slot goes back on the complete ring once netd took all of
// it. A datagram slot is one datagram and goes back once netd answered it.
// A transport error drops the queue, hands every slot back and becomes the
// socket's pending error: those bytes were reported written already.
//
// RX. While a socket can receive, the core keeps native reads parked in
// netd, each into an RX slot it reserved, leaving linuxsockapi.RXReserve
// slots free for a read the kernel asks for. A read that netd answers fills
// its slot, which goes on the complete ring for the kernel; the kernel
// hands it back once a Linux read consumed all of it.

const (
	// NativeSlotSize is the most one native write or read carries: the
	// GTCP 3 slot of linuxsockd's session.
	NativeSlotSize = NativeStreamSlot
	// NativeWriteDepth bounds a stream's writes in netd at once, and
	// NativeReadDepth its reads parked there: GTCP 3 accepts pipelined
	// writes with offsets and queues that many reads per endpoint.
	NativeWriteDepth = NativePipelinedWrites
	NativeReadDepth  = NativeQueuedReads
	// datagramWriteDepth bounds a datagram socket's sends in netd at once.
	datagramWriteDepth = 2
	// nativeTXSlots is the TX slots of both native sessions.
	nativeTXSlots = NativeStreamTXSlots + 4
)

// The family slot and the native one match, so a slot never splits.
var _ [NativeSlotSize - linuxsockapi.SlotSize]byte
var _ [linuxsockapi.SlotSize - NativeSlotSize]byte

// drainSubmit takes every entry of a family's submit ring. It reports
// false when an entry broke the protocol and the family is gone.
func (s *Service) drainSubmit(index int) bool {
	f := &s.families[index]
	linuxsockapi.TakeDoorbell(f.rings)
	var entry linuxsockapi.RingEntry
	for {
		switch linuxsockapi.PopSubmit(f.rings, &entry) {
		case linuxsockapi.RingEmpty:
			return true
		case linuxsockapi.RingBroken:
			s.failFamily(index)
			return false
		}
		var ok bool
		if entry.Kind == linuxsockapi.RingTX {
			ok = s.committed(index, &entry)
		} else {
			ok = s.handedBack(index, &entry)
		}
		if !ok {
			s.failFamily(index)
			return false
		}
	}
}

// committed queues a TX slot on its socket. A slot for a socket that is
// gone goes back at once.
func (s *Service) committed(index int, e *linuxsockapi.RingEntry) bool {
	f := &s.families[index]
	bit := uint8(1) << e.Slot
	if f.txHeld&bit != 0 {
		return false
	}
	f.txHeld |= bit
	f.txGen[e.Slot], f.txLength[e.Slot] = e.Generation, e.Length
	f.txDest[e.Slot], f.txAddress[e.Slot], f.txPort[e.Slot] = e.Flags&linuxsockapi.RingDestination != 0, e.Address, e.Port
	f.stats.RingTX++
	global, result := s.lookup(index, e.Socket)
	if result != linuxsockapi.ResultOK {
		s.txDone(index, e.Slot)
		return true
	}
	sock := &s.sockets[global]
	if sock.stream && f.txDest[e.Slot] || !sock.stream && e.Length > linuxsockapi.MaxDatagram ||
		int(sock.txqLen) >= len(sock.txq) {
		return false
	}
	if sock.dead || sock.txDropped || sock.stream && (sock.writeShut || sock.state != stateConnected || sock.pending != linuxsockapi.ResultOK ||
		sock.mask&(MaskHungUp|MaskWriteClosed) != 0) {
		// The socket cannot send any more; the bytes go nowhere, as after a
		// transport error.
		s.txDone(index, e.Slot)
		return true
	}
	sock.txq[sock.txqLen] = e.Slot
	sock.txqLen++
	s.kick(global)
	return true
}

// handedBack frees an RX slot the kernel is done with.
func (s *Service) handedBack(index int, e *linuxsockapi.RingEntry) bool {
	f := &s.families[index]
	bit := uint8(1) << e.Slot
	if f.rxPub&bit == 0 || f.rxGen[e.Slot] != e.Generation {
		return false
	}
	owner, serial := int(f.rxOwner[e.Slot])-1, f.rxSerial[e.Slot]
	s.freeRX(f, e.Slot)
	f.stats.RingReleases++
	if owner >= 0 && s.sockets[owner].used && s.sockets[owner].serial == serial {
		s.kick(owner)
		s.publish(owner)
	}
	return true
}

// txDone hands a TX slot back to the kernel.
func (s *Service) txDone(index int, slot uint8) {
	f := &s.families[index]
	bit := uint8(1) << slot
	if f.txHeld&bit == 0 {
		return
	}
	f.txHeld &^= bit
	f.txInFlight &^= bit
	entry := linuxsockapi.RingEntry{Kind: linuxsockapi.RingTXDone, Slot: slot, Generation: f.txGen[slot]}
	if linuxsockapi.PushComplete(f.rings, &entry) != linuxsockapi.RingOK {
		// Slot ownership bounds the ring; a full one means the kernel
		// broke the layout.
		s.failFamily(index)
		return
	}
	f.stats.RingTXDone++
	f.notify = true
}

// dropTX hands back every slot of a socket's TX queue unsent, except the
// slots under a write, which go back when their write answers.
func (s *Service) dropTX(global int) {
	sock := &s.sockets[global]
	if !sock.used || sock.txqLen == 0 {
		return
	}
	index := int(sock.family)
	kept := uint8(0)
	for position := uint8(0); position < sock.txqLen; position++ {
		slot := sock.txq[position]
		if s.underWrite(global, slot) {
			sock.txq[kept] = slot
			kept++
			continue
		}
		s.txDone(index, slot)
	}
	sock.txqLen = kept
	sock.txDropped = kept != 0
	sock.txAccepted, sock.txSent, sock.txStall = 0, 0, false
	s.notify(index)
}

// underWrite reports whether a native write of the socket carries bytes of
// TX slot.
func (s *Service) underWrite(global int, slot uint8) bool {
	sock := &s.sockets[global]
	for index := range s.ops {
		o := &s.ops[index]
		if o.state == opNative && o.kind == kindTX && int(o.socket) == global && o.socketSerial == sock.serial && o.slot == slot {
			return true
		}
	}
	return false
}

// unqueue takes slot out of a socket's TX queue and hands it back.
func (s *Service) unqueue(global int, slot uint8) {
	sock := &s.sockets[global]
	for position := uint8(0); position < sock.txqLen; position++ {
		if sock.txq[position] == slot {
			copy(sock.txq[position:], sock.txq[position+1:sock.txqLen])
			sock.txqLen--
			break
		}
	}
	s.txDone(int(sock.family), slot)
}

// want marks a socket for the pump after a busy native slot.
func (s *Service) want(sock *socket, tx bool) {
	if !sock.wantTX && !sock.wantRX {
		s.kicks++
	}
	if tx {
		sock.wantTX = true
	} else {
		sock.wantRX = true
	}
}

// kick moves a socket's data plane forward: its TX queue and its reads.
func (s *Service) kick(global int) {
	sock := &s.sockets[global]
	if !sock.used || sock.opening {
		return
	}
	if sock.stream {
		s.sendStream(global)
	} else {
		s.sendDatagrams(global)
	}
	s.readAhead(global)
	s.acceptAhead(global)
	if sock.shutQueued && sock.txqLen == 0 && sock.writesOut == 0 {
		s.shutdownLater(global)
	}
	if sock.closing && sock.txqLen == 0 && sock.writesOut == 0 {
		s.counters.Closes++
		s.releaseSocket(global, false)
		return
	}
	s.notify(int(sock.family))
}

// sendStream issues the writes a stream's queue allows.
func (s *Service) sendStream(global int) {
	sock := &s.sockets[global]
	f := &s.families[sock.family]
	if sock.dead || sock.endpoint == 0 || sock.txqLen == 0 || sock.txDropped {
		return
	}
	if sock.txStall {
		if sock.writesOut != 0 {
			return
		}
		sock.txStall, sock.txSent = false, 0
	}
	if sock.txBlocked {
		return
	}
	for sock.writesOut < NativeWriteDepth {
		// Find the first byte not yet in a write.
		position, skip := uint8(0), sock.txAccepted+sock.txSent
		for position < sock.txqLen && skip >= f.txLength[sock.txq[position]] {
			skip -= f.txLength[sock.txq[position]]
			position++
		}
		if position == sock.txqLen {
			return
		}
		slot := sock.txq[position]
		length := min(f.txLength[slot]-skip, NativeSlotSize)
		o := s.newOp(int(sock.family), kindTX)
		if o == nil {
			s.want(sock, true)
			return
		}
		o.socket, o.socketSerial, o.slot, o.length = int16(global), sock.serial, slot, length
		data := s.txSlot(f, slot)[skip : skip+length]
		s.native = NativeRequest{Op: NativeWrite, Flags: NativeNoWait, Endpoint: sock.endpoint, Data: data,
			Offset: sock.txStream + sock.txSent}
		if !s.submitData(o, sock.proto) {
			s.want(sock, true)
			return
		}
		sock.writesOut++
		sock.txSent += length
		f.stats.NativeWrites++
		f.stats.TXBytesCopied += uint64(length)
	}
}

// sendDatagrams issues a datagram socket's queued sends.
func (s *Service) sendDatagrams(global int) {
	sock := &s.sockets[global]
	f := &s.families[sock.family]
	if sock.dead || sock.endpoint == 0 || sock.txBlocked || sock.txDropped {
		return
	}
	for position := uint8(0); position < sock.txqLen && sock.writesOut < datagramWriteDepth; position++ {
		slot := sock.txq[position]
		if f.txInFlight&(1<<slot) != 0 {
			continue
		}
		o := s.newOp(int(sock.family), kindTX)
		if o == nil {
			s.want(sock, true)
			return
		}
		o.socket, o.socketSerial, o.slot, o.length = int16(global), sock.serial, slot, f.txLength[slot]
		data := s.txSlot(f, slot)
		if f.txDest[slot] && sock.state != stateConnected {
			s.native = NativeRequest{Op: NativeSendTo, Endpoint: sock.endpoint, Address: f.txAddress[slot], Port: f.txPort[slot], Data: data}
		} else {
			s.native = NativeRequest{Op: NativeWrite, Endpoint: sock.endpoint, Data: data}
		}
		if !s.submitData(o, UDP) {
			s.want(sock, true)
			return
		}
		f.txInFlight |= 1 << slot
		sock.writesOut++
		f.stats.NativeWrites++
		f.stats.TXBytesCopied += uint64(len(data))
	}
}

// submitData issues a data-plane operation's native request. It reports
// false, with the operation freed, when the native session had no room.
func (s *Service) submitData(o *op, proto Proto) bool {
	s.submit(o, proto)
	switch o.state {
	case opNative:
		return true
	case opSubmit:
		s.submits--
		o.state = opNative // freeOp counts nothing for it
	}
	if o.state != opFree {
		s.freeOp(o)
	}
	return false
}

// receiving reports whether netd may hold data for the socket to read.
func receiving(sock *socket) bool {
	if sock.dead || sock.closing || sock.endpoint == 0 || sock.eof {
		return false
	}
	if sock.stream {
		return sock.state == stateConnected
	}
	return sock.state == stateBound || sock.state == stateConnected
}

// readAhead parks reads in netd while the socket and its family have room.
func (s *Service) readAhead(global int) {
	sock := &s.sockets[global]
	f := &s.families[sock.family]
	if !receiving(sock) {
		return
	}
	depth := uint8(1)
	if sock.stream && sock.rxBurst {
		depth = NativeReadDepth
	}
	for sock.readsOut < depth {
		if held, _ := s.rxOf(f, global); held >= linuxsockapi.SocketRXSlots {
			return
		}
		slot := s.reserveRX(f, global, linuxsockapi.RXReserve)
		if slot < 0 {
			return
		}
		o := s.newOp(int(sock.family), kindRX)
		if o == nil {
			s.freeRX(f, uint8(slot))
			s.want(sock, false)
			return
		}
		o.socket, o.socketSerial, o.rx = int16(global), sock.serial, slot
		s.native = NativeRequest{Op: NativeRead, Endpoint: sock.endpoint, Length: NativeSlotSize}
		if !s.submitData(o, sock.proto) {
			s.want(sock, false)
			return
		}
		sock.readsOut++
		f.stats.NativeReads++
	}
}

// doneTX takes a native write's answer.
func (s *Service) doneTX(o *op, c *NativeCompletion) {
	sock := s.socketOf(o)
	global, index, slot := int(o.socket), int(o.family), o.slot
	length := o.length
	s.freeOp(o)
	if sock == nil {
		// The socket is gone; its slots went back with it, except this one.
		s.txDone(index, slot)
		s.notify(index)
		return
	}
	sock.writesOut--
	switch {
	case sock.txDropped:
		if !s.underWrite(global, slot) {
			s.unqueue(global, slot)
		}
		if sock.writesOut == 0 {
			s.dropTX(global)
			sock.txDropped = false
		}
	case sock.stream:
		s.doneStreamWrite(global, length, c)
	default:
		s.doneDatagram(global, slot, c)
	}
	s.publish(global)
	s.kick(global)
}

func (s *Service) doneStreamWrite(global int, length uint32, c *NativeCompletion) {
	sock := &s.sockets[global]
	f := &s.families[sock.family]
	sock.txSent -= min(sock.txSent, length)
	switch c.Result {
	case NativeOK, NativeWouldBlock:
	case NativeLost:
		s.dropTX(global)
		return
	default:
		// The bytes were reported written; the error waits in SO_ERROR.
		if sock.pending == linuxsockapi.ResultOK {
			sock.pending = nativeResult(c.Result)
		}
		s.dropTX(global)
		return
	}
	taken := uint32(0)
	if c.Result == NativeOK {
		taken = min(c.Length, length)
	}
	if taken < length {
		sock.txStall = true
		if !sock.txBlocked {
			sock.txBlocked = true
			sock.mask &^= MaskWritable
			s.refreshReadable(global)
		}
	}
	if taken == 0 {
		return
	}
	s.counters.BytesOut += uint64(taken)
	sock.txStream += taken
	sock.txAccepted += taken
	for sock.txqLen != 0 && sock.txAccepted >= f.txLength[sock.txq[0]] {
		sock.txAccepted -= f.txLength[sock.txq[0]]
		s.txDone(int(sock.family), sock.txq[0])
		copy(sock.txq[:], sock.txq[1:sock.txqLen])
		sock.txqLen--
	}
}

func (s *Service) doneDatagram(global int, slot uint8, c *NativeCompletion) {
	sock := &s.sockets[global]
	f := &s.families[sock.family]
	f.txInFlight &^= 1 << slot
	switch c.Result {
	case NativeWouldBlock, NativeBusy:
		// No room in netd now: the datagram stays queued for the next mask.
		sock.txBlocked = true
		sock.mask &^= MaskWritable
		s.refreshReadable(global)
		return
	case NativeOK:
		s.counters.DatagramsOut++
	case NativeLost:
	default:
		if sock.pending == linuxsockapi.ResultOK {
			sock.pending = nativeResult(c.Result)
		}
	}
	s.unqueue(global, slot)
}

// doneRX takes a native read's answer: a filled slot goes to the kernel.
func (s *Service) doneRX(o *op, c *NativeCompletion) {
	sock := s.socketOf(o)
	global, index := int(o.socket), int(o.family)
	if sock == nil {
		s.freeOp(o)
		return
	}
	sock.readsOut--
	f := &s.families[index]
	switch c.Result {
	case NativeOK:
		if len(c.Data) == 0 && sock.stream || sock.closing {
			break
		}
		slot := uint8(o.rx)
		length := min(uint32(len(c.Data)), linuxsockapi.SlotSize)
		copy(s.rxSlot(f, slot), c.Data[:length])
		o.rx = -1
		if !s.publishRX(index, global, slot, length, c) {
			s.freeOp(o)
			return
		}
		if sock.stream {
			s.counters.BytesIn += uint64(length)
			sock.rxBurst = length == NativeSlotSize
			sock.readable -= min(sock.readable, length)
		} else {
			s.counters.DatagramsIn++
			// The next datagram's length is unknown until netd says.
			sock.mask &^= MaskReadable
			sock.readable = 0
			s.refreshReadable(global)
		}
	case NativeEOF:
		sock.eof, sock.rxBurst = true, false
		sock.mask |= MaskReceiveEOF
	case NativeClosed, NativeLost, NativeWouldBlock:
		sock.rxBurst = false
	default:
		sock.rxBurst = false
		if sock.pending == linuxsockapi.ResultOK {
			sock.pending = nativeResult(c.Result)
		}
	}
	s.freeOp(o)
	s.publish(global)
	s.kick(global)
}

// publishRX puts a filled RX slot on the complete ring for the kernel.
func (s *Service) publishRX(index, global int, slot uint8, length uint32, c *NativeCompletion) bool {
	f := &s.families[index]
	sock := &s.sockets[global]
	f.rxGen[slot]++
	entry := linuxsockapi.RingEntry{Kind: linuxsockapi.RingRXData, Slot: slot, Length: length, Socket: sock.id, Generation: f.rxGen[slot]}
	if !sock.stream {
		entry.Address, entry.Port = c.Address, c.Port
	}
	if linuxsockapi.PushComplete(f.rings, &entry) != linuxsockapi.RingOK {
		s.freeRX(f, slot)
		s.failFamily(index)
		return false
	}
	f.rxPub |= 1 << slot
	f.stats.RingRXData++
	f.stats.RXBytesCopied += uint64(length)
	f.notify = true
	return true
}

// shutdownLater sends the half-close a SHUT_WR left behind its queue.
func (s *Service) shutdownLater(global int) {
	sock := &s.sockets[global]
	if sock.dead || sock.endpoint == 0 {
		sock.shutQueued = false
		return
	}
	o := s.newOp(int(sock.family), kindShutdown)
	if o == nil {
		s.want(sock, true)
		return
	}
	o.socket, o.socketSerial = int16(global), sock.serial
	s.native = NativeRequest{Op: NativeShutdownWrite, Endpoint: sock.endpoint}
	if !s.submitData(o, TCP) {
		s.want(sock, true)
		return
	}
	sock.shutQueued = false
}
